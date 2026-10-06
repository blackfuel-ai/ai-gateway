// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"

	rlsconfv3 "github.com/envoyproxy/go-control-plane/ratelimit/config/ratelimit/v3"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
	aigv1b1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	"github.com/envoyproxy/ai-gateway/internal/ratelimit/runner"
	"github.com/envoyproxy/ai-gateway/internal/ratelimit/translator"
)

// QuotaPolicyController implements [reconcile.TypedReconciler] for [aigv1a1.QuotaPolicy].
//
// Unlike the other controllers, this one runs on every replica regardless of
// leadership (registered with NeedLeaderElection=false): its reconcile output
// feeds the per-replica rate limit xDS snapshot cache, and the ratelimit
// service's gRPC connection can land on any replica. Only the mutating writes
// (status, finalizer) are gated on leadership via the elected channel.
type QuotaPolicyController struct {
	client             client.Client
	kube               kubernetes.Interface
	logger             logr.Logger
	rateLimitRunner    *runner.Runner
	aiGatewayRouteChan chan event.GenericEvent
	// elected is closed when this replica acquires leadership (manager.Elected()).
	elected <-chan struct{}
	// configCache stores rate limit configs per QuotaPolicy namespace/name.
	// This allows incremental updates when only one policy changes.
	configCache map[string][]*rlsconfv3.RateLimitConfig
	mu          sync.RWMutex
	// targetBackends stores, per QuotaPolicy namespace/name, the backend index keys
	// of its targetRefs at its last successful reconcile. The routes of a backend a
	// policy no longer targets, or of a deleted policy, are notified from it, routes
	// in other namespaces included.
	targetBackends   map[types.NamespacedName][]string
	targetBackendsMu sync.Mutex
}

// NewQuotaPolicyController creates a new reconciler for QuotaPolicy resources.
//
// elected is the manager's Elected() channel; it is closed when this replica
// becomes the leader. Pass a closed channel to make the controller behave as
// the leader unconditionally (e.g. when leader election is disabled).
func NewQuotaPolicyController(
	client client.Client,
	kube kubernetes.Interface,
	logger logr.Logger,
	rateLimitRunner *runner.Runner,
	aiGatewayRouteChan chan event.GenericEvent,
	elected <-chan struct{},
) *QuotaPolicyController {
	return &QuotaPolicyController{
		client:             client,
		kube:               kube,
		logger:             logger,
		rateLimitRunner:    rateLimitRunner,
		aiGatewayRouteChan: aiGatewayRouteChan,
		elected:            elected,
		configCache:        make(map[string][]*rlsconfv3.RateLimitConfig),
		targetBackends:     make(map[types.NamespacedName][]string),
	}
}

// quotaPolicyBackendKeys returns the k8sClientIndexBackendToReferencingAIGatewayRoute
// keys of the backends the policy targets.
func quotaPolicyBackendKeys(policy *aigv1a1.QuotaPolicy) []string {
	keys := make([]string, 0, len(policy.Spec.TargetRefs))
	for _, ref := range policy.Spec.TargetRefs {
		keys = append(keys, fmt.Sprintf("%s.%s", ref.Name, policy.Namespace))
	}
	return keys
}

// swapTargetBackends records the policy's current backend keys, or forgets the
// policy when keys is nil, and returns the keys recorded before. known is false
// when this replica has no record of the policy.
func (c *QuotaPolicyController) swapTargetBackends(policy types.NamespacedName, keys []string) (previous []string, known bool) {
	c.targetBackendsMu.Lock()
	defer c.targetBackendsMu.Unlock()
	previous, known = c.targetBackends[policy]
	if keys == nil {
		delete(c.targetBackends, policy)
	} else {
		c.targetBackends[policy] = keys
	}
	return previous, known
}

// isLeader reports whether this replica currently holds the leader lease.
// The elected channel is closed on acquisition and leadership is never
// relinquished while the process lives (controller-runtime exits on a lost
// lease), so a closed channel is a stable signal.
func (c *QuotaPolicyController) isLeader() bool {
	select {
	case <-c.elected:
		return true
	default:
		return false
	}
}

// Reconcile implements [reconcile.TypedReconciler] for [aigv1a1.QuotaPolicy].
func (c *QuotaPolicyController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var quotaPolicy aigv1a1.QuotaPolicy
	if err := c.client.Get(ctx, req.NamespacedName, &quotaPolicy); err != nil {
		if client.IgnoreNotFound(err) == nil {
			c.logger.Info("Deleting QuotaPolicy",
				"namespace", req.Namespace, "name", req.Name)
			if err = c.deleteQuotaPolicyConfig(ctx, req.NamespacedName); err != nil {
				return ctrl.Result{}, err
			}
			previousTargets, known := c.swapTargetBackends(req.NamespacedName, nil)
			// The AIGatewayRoute controller only runs on the leader, so its
			// event channel has no consumer on other replicas.
			if c.isLeader() {
				if known {
					c.notifyAIGatewayRoutes(ctx, req.NamespacedName, previousTargets)
				} else {
					c.notifyAllAIGatewayRoutesInNamespace(ctx, req.Namespace)
				}
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	c.logger.Info("Reconciling QuotaPolicy", "namespace", req.Namespace, "name", req.Name)

	if c.isLeader() {
		if handleFinalizer(ctx, c.client, c.logger, &quotaPolicy, func(ctx context.Context, _ *aigv1a1.QuotaPolicy) error {
			return c.deleteQuotaPolicyConfig(ctx, req.NamespacedName)
		}) {
			return ctrl.Result{}, nil
		}
	} else if !quotaPolicy.GetDeletionTimestamp().IsZero() {
		// A non-leader must still purge its local snapshot cache on deletion,
		// but leaves the finalizer to the leader.
		if err := c.deleteQuotaPolicyConfig(ctx, req.NamespacedName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := c.syncQuotaPolicy(ctx, &quotaPolicy); err != nil {
		c.logger.Error(err, "failed to sync QuotaPolicy")
		if c.isLeader() {
			c.updateQuotaPolicyStatus(ctx, &quotaPolicy, aigv1a1.ConditionTypeNotAccepted, err.Error())
		}
		return ctrl.Result{}, err
	}
	targets := quotaPolicyBackendKeys(&quotaPolicy)
	previousTargets, _ := c.swapTargetBackends(req.NamespacedName, targets)
	if c.isLeader() {
		c.updateQuotaPolicyStatus(ctx, &quotaPolicy, aigv1a1.ConditionTypeAccepted, "QuotaPolicy reconciled successfully")
		c.notifyAIGatewayRoutes(ctx, req.NamespacedName, slices.Concat(previousTargets, targets))
	}
	return ctrl.Result{}, nil
}

// syncQuotaPolicy is the main reconciliation logic. It builds rate limit configs
// for the changed QuotaPolicy only, updates the cache, and pushes the merged
// configs to the xDS runner.
func (c *QuotaPolicyController) syncQuotaPolicy(ctx context.Context, policy *aigv1a1.QuotaPolicy) error {
	// Resolve target backends for this policy.
	var backends []*aigv1b1.AIServiceBackend
	for _, ref := range policy.Spec.TargetRefs {
		var backend aigv1b1.AIServiceBackend
		key := client.ObjectKey{
			Namespace: policy.Namespace,
			Name:      string(ref.Name),
		}
		if err := c.client.Get(ctx, key, &backend); err != nil {
			if apierrors.IsNotFound(err) {
				c.logger.Info("AIServiceBackend not found, skipping",
					"namespace", key.Namespace, "name", key.Name,
					"quotaPolicy", policy.Name)
				continue
			}
			return fmt.Errorf("failed to get AIServiceBackend %s: %w", key, err)
		}
		backends = append(backends, &backend)
	}

	if len(backends) == 0 && len(policy.Spec.TargetRefs) > 0 {
		return fmt.Errorf("none of the %d target AIServiceBackends were found for QuotaPolicy %s/%s, will retry",
			len(policy.Spec.TargetRefs), policy.Namespace, policy.Name)
	}

	// Build rate limit configs for this policy.
	var configs []*rlsconfv3.RateLimitConfig
	if len(backends) > 0 {
		var err error
		configs, err = translator.BuildRateLimitConfigs(policy, backends)
		if err != nil {
			return fmt.Errorf("failed to build rate limit configs for QuotaPolicy %s/%s: %w",
				policy.Namespace, policy.Name, err)
		}
	}

	// Update cache and push merged configs to xDS.
	// Hold the lock across both cache update and UpdateConfigs to prevent
	// out-of-order execution where a later reconcile's UpdateConfigs could
	// be overwritten by an earlier one completing after it.
	cacheKey := fmt.Sprintf("%s/%s", policy.Namespace, policy.Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configCache[cacheKey] = configs
	allConfigs := c.getMergedConfigsLocked()

	return c.rateLimitRunner.UpdateConfigs(ctx, allConfigs)
}

// deleteQuotaPolicyConfig removes a QuotaPolicy's configs from the cache
// and updates the xDS snapshot.
func (c *QuotaPolicyController) deleteQuotaPolicyConfig(ctx context.Context, key client.ObjectKey) error {
	cacheKey := fmt.Sprintf("%s/%s", key.Namespace, key.Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.configCache, cacheKey)
	allConfigs := c.getMergedConfigsLocked()

	return c.rateLimitRunner.UpdateConfigs(ctx, allConfigs)
}

// getMergedConfigsLocked merges all cached configs into a single RateLimitConfig.
// When multiple QuotaPolicies define the same descriptor path, the policy whose
// namespace/name is alphabetically first takes precedence. Keys are sorted to
// ensure deterministic snapshot generation.
// Caller must hold c.mu lock.
func (c *QuotaPolicyController) getMergedConfigsLocked() []*rlsconfv3.RateLimitConfig {
	var allDescriptors []*rlsconfv3.RateLimitDescriptor
	keys := make([]string, 0, len(c.configCache))
	for k := range c.configCache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, cfg := range c.configCache[k] {
			allDescriptors = append(allDescriptors, cfg.Descriptors...)
		}
	}
	if len(allDescriptors) == 0 {
		return nil
	}
	merged := translator.MergeDescriptors(allDescriptors)
	return []*rlsconfv3.RateLimitConfig{
		{
			Name:        translator.QuotaDomain,
			Domain:      translator.QuotaDomain,
			Descriptors: merged,
		},
	}
}

// BackendToQuotaPolicy maps AIServiceBackend changes to QuotaPolicy reconcile
// requests. This is used as an EnqueueRequestsFromMapFunc handler so that
// when an AIServiceBackend changes, all QuotaPolicies targeting it are re-reconciled.
func (c *QuotaPolicyController) BackendToQuotaPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	var quotaPolicies aigv1a1.QuotaPolicyList
	key := fmt.Sprintf("%s.%s", obj.GetName(), obj.GetNamespace())
	if err := c.client.List(ctx, &quotaPolicies,
		client.MatchingFields{k8sClientIndexAIServiceBackendToTargetingQuotaPolicy: key}); err != nil {
		c.logger.Error(err, "failed to list QuotaPolicies for backend", "backend", key)
		return nil
	}

	var requests []reconcile.Request
	for i := range quotaPolicies.Items {
		qp := &quotaPolicies.Items[i]
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(qp),
		})
	}
	return requests
}

// notifyAIGatewayRoutes sends one event to the AIGatewayRoute controller for each
// route that references any of the given backends, whatever the route's namespace.
// The route reconcile rewrites the HTTPRoute's quota-policy-hash annotation
// (httpRouteQuotaPolicyHashAnnotationKey), and that change is what makes Envoy
// Gateway re-translate xDS and call PostTranslateModify with the updated QuotaPolicy.
func (c *QuotaPolicyController) notifyAIGatewayRoutes(ctx context.Context, policy types.NamespacedName, backendKeys []string) {
	notified := make(map[types.NamespacedName]struct{})
	for _, key := range backendKeys {
		var aiGatewayRoutes aigv1b1.AIGatewayRouteList
		if err := c.client.List(ctx, &aiGatewayRoutes,
			client.MatchingFields{k8sClientIndexBackendToReferencingAIGatewayRoute: key}); err != nil {
			c.logger.Error(err, "failed to list AIGatewayRoutes for backend", "backend", key)
			continue
		}
		for i := range aiGatewayRoutes.Items {
			route := &aiGatewayRoutes.Items[i]
			routeKey := client.ObjectKeyFromObject(route)
			if _, ok := notified[routeKey]; ok {
				continue
			}
			notified[routeKey] = struct{}{}
			c.logger.Info("notifying AIGatewayRoute of QuotaPolicy change",
				"route", route.Name, "namespace", route.Namespace,
				"quotaPolicy", policy.String())
			c.aiGatewayRouteChan <- event.GenericEvent{Object: route}
		}
	}
}

// notifyAllAIGatewayRoutesInNamespace sends events for all AIGatewayRoutes in
// the given namespace. Used on the deletion of a QuotaPolicy this replica never
// reconciled, whose targetRefs are therefore unknown.
func (c *QuotaPolicyController) notifyAllAIGatewayRoutesInNamespace(ctx context.Context, namespace string) {
	var aiGatewayRoutes aigv1b1.AIGatewayRouteList
	if err := c.client.List(ctx, &aiGatewayRoutes, client.InNamespace(namespace)); err != nil {
		c.logger.Error(err, "failed to list AIGatewayRoutes in namespace", "namespace", namespace)
		return
	}
	for i := range aiGatewayRoutes.Items {
		route := &aiGatewayRoutes.Items[i]
		c.logger.Info("notifying AIGatewayRoute of QuotaPolicy deletion",
			"route", route.Name, "namespace", route.Namespace)
		c.aiGatewayRouteChan <- event.GenericEvent{Object: route}
	}
}

// updateQuotaPolicyStatus updates the status of the QuotaPolicy.
func (c *QuotaPolicyController) updateQuotaPolicyStatus(ctx context.Context, policy *aigv1a1.QuotaPolicy, conditionType string, message string) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.client.Get(ctx, client.ObjectKey{Name: policy.Name, Namespace: policy.Namespace}, policy); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		policy.Status.Conditions = newConditions(conditionType, message)
		return c.client.Status().Update(ctx, policy)
	})
	if err != nil {
		c.logger.Error(err, "failed to update QuotaPolicy status",
			"namespace", policy.Namespace, "name", policy.Name)
	}
}
