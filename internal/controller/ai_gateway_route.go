// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwaiev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
	aigv1b1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	// managedByValue is the value stamped on the managedByLabel of resources created by this operator.
	managedByValue                      = "envoy-ai-gateway"
	hostRewriteHTTPFilterName           = "ai-eg-host-rewrite"
	routeNotFoundResponseHTTPFilterName = "ai-eg-route-not-found-response"
	aigatewayUUIDAnnotationKey          = "aigateway.envoyproxy.io/uuid"
	egAnnotationPrefix                  = "gateway.envoyproxy.io/"
	// We use this annotation to ensure that Envoy Gateway reconciles the HTTPRoute when the backend refs change.
	// This will result in metadata being added to the underling Envoy route
	// @see https://gateway.envoyproxy.io/contributions/design/metadata/
	httpRouteBackendRefPriorityAnnotationKey = egAnnotationPrefix + "backend-ref-priority"
	// httpRouteQuotaPolicyHashAnnotationKey carries a hash of the QuotaPolicies targeting the route's backends,
	// so a QuotaPolicy change changes the HTTPRoute and Envoy Gateway re-translates it, calling the extension
	// server's PostTranslateModify with the new policy. The egAnnotationPrefix is what carries the change into
	// Envoy Gateway's IR; an annotation without it is dropped from the IR and no xDS update follows.
	httpRouteQuotaPolicyHashAnnotationKey              = egAnnotationPrefix + "quota-policy-hash"
	httpRouteAnnotationForAIGatewayGeneratedIndication = egAnnotationPrefix + internalapi.AIGatewayGeneratedHTTPRouteAnnotation
	egOwningGatewayNameLabel                           = egAnnotationPrefix + "owning-gateway-name"
	egOwningGatewayNamespaceLabel                      = egAnnotationPrefix + "owning-gateway-namespace"
	// apiKeyInSecret is the key to store OpenAI API key.
	apiKeyInSecret = "apiKey"
	// GatewayConfigAnnotationKey is the annotation key used on Gateway objects to reference a GatewayConfig.
	// The value should be the name of the GatewayConfig resource in the same namespace as the Gateway.
	GatewayConfigAnnotationKey = "aigateway.envoyproxy.io/gateway-config"
)

// AIGatewayRouteController implements [reconcile.TypedReconciler].
//
// This handles the AIGatewayRoute resource and creates the necessary resources for the external process.
//
// Exported for testing purposes.
type AIGatewayRouteController struct {
	client client.Client
	kube   kubernetes.Interface
	logger logr.Logger
	// gatewayEventChan is a channel to send events to the gateway controller.
	gatewayEventChan chan event.GenericEvent
	// rootPrefix is the prefix for the root path of the AI Gateway.
	rootPrefix string
	// referenceGrantValidator validates cross-namespace references using ReferenceGrant.
	referenceGrantValidator *referenceGrantValidator
}

// NewAIGatewayRouteController creates a new reconcile.TypedReconciler[reconcile.Request] for the AIGatewayRoute resource.
func NewAIGatewayRouteController(
	client client.Client, kube kubernetes.Interface, logger logr.Logger,
	gatewayEventChan chan event.GenericEvent,
	rootPrefix string,
) *AIGatewayRouteController {
	return &AIGatewayRouteController{
		client:                  client,
		kube:                    kube,
		logger:                  logger,
		gatewayEventChan:        gatewayEventChan,
		rootPrefix:              rootPrefix,
		referenceGrantValidator: newReferenceGrantValidator(client),
	}
}

// Reconcile implements [reconcile.TypedReconciler].
func (c *AIGatewayRouteController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	c.logger.Info("Reconciling AIGatewayRoute", "namespace", req.Namespace, "name", req.Name)

	var aiGatewayRoute aigv1b1.AIGatewayRoute
	if err := c.client.Get(ctx, req.NamespacedName, &aiGatewayRoute); err != nil {
		if client.IgnoreNotFound(err) == nil {
			c.logger.Info("Deleting AIGatewayRoute",
				"namespace", req.Namespace, "name", req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if err := c.syncAIGatewayRoute(ctx, &aiGatewayRoute); err != nil {
		c.logger.Error(err, "failed to sync AIGatewayRoute")
		c.updateAIGatewayRouteStatus(ctx, &aiGatewayRoute, aigv1b1.ConditionTypeNotAccepted, err.Error())
		return ctrl.Result{}, err
	}
	c.updateAIGatewayRouteStatus(ctx, &aiGatewayRoute, aigv1b1.ConditionTypeAccepted, "AI Gateway Route reconciled successfully")
	return reconcile.Result{}, nil
}

func getHostRewriteFilterName(baseName string) string {
	return fmt.Sprintf("%s-%s", hostRewriteHTTPFilterName, baseName)
}

func getRouteNotFoundFilterName(baseName string) string {
	return fmt.Sprintf("%s-%s", routeNotFoundResponseHTTPFilterName, baseName)
}

// generateHTTPRouteFilters returns two HTTPRouteFilter with the given AIGatewayRoute.
func generateHTTPRouteFilters(aiGatewayRoute *aigv1b1.AIGatewayRoute) []*egv1a1.HTTPRouteFilter {
	ns := aiGatewayRoute.Namespace
	baseName := aiGatewayRoute.Name

	hostRewriteName := getHostRewriteFilterName(baseName)
	notFoundName := getRouteNotFoundFilterName(baseName)

	return []*egv1a1.HTTPRouteFilter{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hostRewriteName,
				Namespace: ns,
			},
			Spec: egv1a1.HTTPRouteFilterSpec{
				URLRewrite: &egv1a1.HTTPURLRewriteFilter{
					Hostname: &egv1a1.HTTPHostnameModifier{
						Type: egv1a1.BackendHTTPHostnameModifier,
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      notFoundName,
				Namespace: ns,
			},
			Spec: egv1a1.HTTPRouteFilterSpec{
				DirectResponse: &egv1a1.HTTPDirectResponseFilter{
					StatusCode: ptr.To(404),
					Body: &egv1a1.CustomResponseBody{
						Inline: ptr.To(
							// "Likely" since the matching rule can be arbitrary, not necessarily matching on the model name.
							`No matching route found. It is likely because the model specified in your request is not configured in the Gateway.`,
						),
					},
				},
			},
		},
	}
}

// syncAIGatewayRoute is the main logic for reconciling the AIGatewayRoute resource.
// This is decoupled from the Reconcile method to centralize the error handling and status updates.
func (c *AIGatewayRouteController) syncAIGatewayRoute(ctx context.Context, aiGatewayRoute *aigv1b1.AIGatewayRoute) error {
	if handleFinalizer(ctx, c.client, c.logger, aiGatewayRoute, c.syncGateways) { // Propagate the AIGatewayRoute deletion all the way up to relevant Gateways.
		return nil
	}

	// Check if the static default HTTPRouteFilters exist per AIGatewayRoute.
	filters := generateHTTPRouteFilters(aiGatewayRoute)
	for _, base := range filters {
		var f egv1a1.HTTPRouteFilter
		if err := c.client.Get(ctx, client.ObjectKey{Name: base.Name, Namespace: base.Namespace}, &f); err != nil {
			if apierrors.IsNotFound(err) {
				if err = ctrlutil.SetControllerReference(aiGatewayRoute, base, c.client.Scheme()); err != nil {
					panic(fmt.Errorf("BUG: failed to set controller reference for HTTPRouteFilter: %w", err))
				}
				// Create the filter if it does not exist.
				if err = c.client.Create(ctx, base); err != nil {
					return fmt.Errorf("failed to create HTTPRouteFilter %s: %w", base.Name, err)
				}
				c.logger.Info("Created HTTPRouteFilter", "name", base.Name, "namespace", base.Namespace)
			} else {
				return fmt.Errorf("failed to get HTTPRouteFilter %s: %w", base.Name, err)
			}
		}
	}

	// Check if the HTTPRoute exists.
	c.logger.Info("syncing AIGatewayRoute", "namespace", aiGatewayRoute.Namespace, "name", aiGatewayRoute.Name)
	var httpRoute gwapiv1.HTTPRoute
	err := c.client.Get(ctx, client.ObjectKey{Name: aiGatewayRoute.Name, Namespace: aiGatewayRoute.Namespace}, &httpRoute)
	existingRoute := err == nil
	if apierrors.IsNotFound(err) {
		// This means that this AIGatewayRoute is a new one.
		httpRoute = gwapiv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:        aiGatewayRoute.Name,
				Namespace:   aiGatewayRoute.Namespace,
				Labels:      make(map[string]string),
				Annotations: make(map[string]string),
			},
			Spec: gwapiv1.HTTPRouteSpec{},
		}

		// Copy labels from AIGatewayRoute to HTTPRoute.
		for k, v := range aiGatewayRoute.Labels {
			httpRoute.Labels[k] = v
		}

		// Copy non-controller annotations from AIGatewayRoute to HTTPRoute.
		for k, v := range aiGatewayRoute.Annotations {
			httpRoute.Annotations[k] = v
		}
		if err = ctrlutil.SetControllerReference(aiGatewayRoute, &httpRoute, c.client.Scheme()); err != nil {
			panic(fmt.Errorf("BUG: failed to set controller reference for HTTPRoute: %w", err))
		}
	} else if err != nil {
		return fmt.Errorf("failed to get HTTPRoute: %w", err)
	}

	// Update the HTTPRoute with the new AIGatewayRoute. A backendRef that fails validation (e.g. a
	// missing or revoked ReferenceGrant) is dropped from the generated rule rather than aborting the
	// whole sync, so the HTTPRoute and the Gateways below still get updated to reflect the now-reduced
	// set of authorized backends instead of staying frozen at their last-good state. The error, if any,
	// is still returned at the end so the caller marks the AIGatewayRoute NotAccepted as before.
	newHTTPRouteErr := c.newHTTPRoute(ctx, &httpRoute, aiGatewayRoute)

	if existingRoute {
		c.logger.Info("updating HTTPRoute", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
		if err = c.client.Update(ctx, &httpRoute); err != nil {
			return fmt.Errorf("failed to update HTTPRoute: %w", err)
		}
	} else {
		c.logger.Info("creating HTTPRoute", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
		if err = c.client.Create(ctx, &httpRoute); err != nil {
			return fmt.Errorf("failed to create HTTPRoute: %w", err)
		}
	}

	if err = c.syncGateways(ctx, aiGatewayRoute); err != nil {
		return fmt.Errorf("failed to sync gw pods: %w", err)
	}

	if newHTTPRouteErr != nil {
		return fmt.Errorf("failed to construct a new HTTPRoute: %w", newHTTPRouteErr)
	}
	return nil
}

// newHTTPRoute updates the HTTPRoute with the new AIGatewayRoute.
//
// A backendRef that fails validation (e.g. a missing or revoked ReferenceGrant for a cross-namespace
// reference) is skipped rather than aborting the whole HTTPRoute construction: dst is still populated
// with every other authorized backendRef, and the validation errors are joined and returned so the
// caller can still surface them (e.g. to mark the AIGatewayRoute NotAccepted) without leaving dst stale.
func (c *AIGatewayRouteController) newHTTPRoute(ctx context.Context, dst *gwapiv1.HTTPRoute, aiGatewayRoute *aigv1b1.AIGatewayRoute) error {
	rewriteFilters := []gwapiv1.HTTPRouteFilter{{
		Type: gwapiv1.HTTPRouteFilterExtensionRef,
		ExtensionRef: &gwapiv1.LocalObjectReference{
			Group: "gateway.envoyproxy.io",
			Kind:  "HTTPRouteFilter",
			Name:  gwapiv1.ObjectName(getHostRewriteFilterName(aiGatewayRoute.Name)),
		},
	}}
	rules := make([]gwapiv1.HTTPRouteRule, 0, len(aiGatewayRoute.Spec.Rules)+1) // +1 for the default rule.
	var errs []error
	for i := range aiGatewayRoute.Spec.Rules {
		rule := &aiGatewayRoute.Spec.Rules[i]
		var backendRefs []gwapiv1.HTTPBackendRef
		for j := range rule.BackendRefs {
			br := &rule.BackendRefs[j]
			backendNamespace := br.GetNamespace(aiGatewayRoute.Namespace)
			dstName := fmt.Sprintf("%s.%s", br.Name, backendNamespace)

			if br.IsInferencePool() {
				// Handle InferencePool backend reference, honoring the (optionally cross-namespace)
				// namespace specified on the backendRef.
				if br.IsCrossNamespace(aiGatewayRoute.Namespace) {
					if err := c.referenceGrantValidator.validateInferencePoolReference(
						ctx,
						aiGatewayRoute.Namespace,
						backendNamespace,
						br.Name,
					); err != nil {
						c.logger.Error(err, "skipping InferencePool backendRef that failed ReferenceGrant validation",
							"namespace", aiGatewayRoute.Namespace, "name", aiGatewayRoute.Name, "backend", dstName)
						errs = append(errs, err)
						continue
					}
				}
				ns := gwapiv1.Namespace(backendNamespace)
				backendRefs = append(backendRefs,
					gwapiv1.HTTPBackendRef{BackendRef: gwapiv1.BackendRef{
						BackendObjectReference: gwapiv1.BackendObjectReference{
							Group:     (*gwapiv1.Group)(br.Group),
							Kind:      (*gwapiv1.Kind)(br.Kind),
							Name:      gwapiv1.ObjectName(br.Name),
							Namespace: &ns,
						},
						Weight: br.Weight,
					}},
				)
			} else {
				// Handle AIServiceBackend reference with cross-namespace validation.
				backend, err := c.validateAndGetBackend(ctx, aiGatewayRoute, br)
				if err != nil {
					c.logger.Error(err, "skipping AIServiceBackend backendRef that failed validation",
						"namespace", aiGatewayRoute.Namespace, "name", aiGatewayRoute.Name, "backend", dstName)
					errs = append(errs, fmt.Errorf("failed to get AIServiceBackend %s: %w", dstName, err))
					continue
				}

				// Copy the BackendObjectReference from the AIServiceBackend.
				backendObjRef := backend.Spec.BackendRef

				// Ensure the namespace is explicitly set in the BackendObjectReference
				// only for cross-namespace references.
				// If the AIServiceBackend is in a different namespace than the AIGatewayRoute,
				// the Backend it references is also in that namespace, and we need to set
				// the namespace explicitly in the HTTPRoute's backendRef.
				if backendObjRef.Namespace == nil && backend.Namespace != "" && backend.Namespace != aiGatewayRoute.Namespace {
					ns := gwapiv1.Namespace(backend.Namespace)
					backendObjRef.Namespace = &ns
				}

				backendRefs = append(backendRefs,
					gwapiv1.HTTPBackendRef{BackendRef: gwapiv1.BackendRef{
						BackendObjectReference: backendObjRef,
						Weight:                 br.Weight,
					}},
				)
			}
		}
		var matches []gwapiv1.HTTPRouteMatch
		for j := range rule.Matches {
			matches = append(matches, gwapiv1.HTTPRouteMatch{
				Headers: rule.Matches[j].Headers,
				Path:    &gwapiv1.HTTPPathMatch{Value: &c.rootPrefix},
			})
		}
		// Envoy Gateway names each mirror cluster `<rule>-mirror-<filterIdx>` where filterIdx is the
		// index across ALL of the rule's filters, and the extension server's mirror-cluster parse
		// (post_translate_modify.go) maps that suffix back to rule.Mirrors[j] assuming exactly the one
		// leading host-rewrite filter (suffix = j+1). Mirrors therefore stay the trailing filters of the
		// rule and are emitted all-or-nothing per rule: skipping only the invalid ones would shift the
		// indices of the remaining mirrors and make the extension server resolve the wrong backend.
		var mirrorFilters []gwapiv1.HTTPRouteFilter
		poolMirrors := 0
		for j := range rule.Mirrors {
			mirror := &rule.Mirrors[j]
			mirrorBR := &mirror.BackendRef
			var mirrorObjRef gwapiv1.BackendObjectReference
			var err error
			if mirrorBR.IsInferencePool() {
				if poolMirrors++; poolMirrors > 1 {
					err = fmt.Errorf("at most one InferencePool mirror per rule (rule %d, mirror %d): each pool mirror needs the mirror endpoint-picker header to itself", i, j)
				} else {
					mirrorObjRef, err = c.inferencePoolMirrorBackendRef(ctx, aiGatewayRoute, mirrorBR)
				}
			} else {
				var mirrorBackend *aigv1b1.AIServiceBackend
				if mirrorBackend, err = c.validateAndGetBackend(ctx, aiGatewayRoute, mirrorBR); err != nil {
					err = fmt.Errorf("failed to get AIServiceBackend for mirror %s.%s: %w",
						mirrorBR.Name, mirrorBR.GetNamespace(aiGatewayRoute.Namespace), err)
				} else {
					mirrorObjRef = mirrorBackend.Spec.BackendRef
					if mirrorObjRef.Namespace == nil && mirrorBackend.Namespace != "" && mirrorBackend.Namespace != aiGatewayRoute.Namespace {
						ns := gwapiv1.Namespace(mirrorBackend.Namespace)
						mirrorObjRef.Namespace = &ns
					}
				}
			}
			if err != nil {
				c.logger.Error(err, "skipping mirrors of rule with a mirror backendRef that failed validation",
					"namespace", aiGatewayRoute.Namespace, "name", aiGatewayRoute.Name, "rule", i, "mirror", j)
				errs = append(errs, err)
				mirrorFilters = nil
				break
			}
			mirrorFilters = append(mirrorFilters, gwapiv1.HTTPRouteFilter{
				Type: gwapiv1.HTTPRouteFilterRequestMirror,
				RequestMirror: &gwapiv1.HTTPRequestMirrorFilter{
					BackendRef: mirrorObjRef,
					Percent:    mirror.Percent,
					Fraction:   mirror.Fraction,
				},
			})
		}
		filters := make([]gwapiv1.HTTPRouteFilter, 0, len(rewriteFilters)+len(mirrorFilters))
		filters = append(filters, rewriteFilters...)
		filters = append(filters, mirrorFilters...)
		rules = append(rules, gwapiv1.HTTPRouteRule{
			Name:        rule.Name,
			BackendRefs: backendRefs,
			Matches:     matches,
			Filters:     filters,
			Timeouts:    rule.GetTimeoutsOrDefault(),
		})
	}

	rules = append(rules, gwapiv1.HTTPRouteRule{
		Name:    ptr.To[gwapiv1.SectionName]("route-not-found"),
		Matches: []gwapiv1.HTTPRouteMatch{{Path: &gwapiv1.HTTPPathMatch{Value: &c.rootPrefix}}},
		Filters: []gwapiv1.HTTPRouteFilter{{
			Type: gwapiv1.HTTPRouteFilterExtensionRef,
			ExtensionRef: &gwapiv1.LocalObjectReference{
				Group: "gateway.envoyproxy.io",
				Kind:  "HTTPRouteFilter",
				Name:  gwapiv1.ObjectName(getRouteNotFoundFilterName(aiGatewayRoute.Name)),
			},
		}},
	})

	dst.Spec.Rules = rules

	// Initialize labels and annotations maps if they don't exist.
	if dst.Labels == nil {
		dst.Labels = make(map[string]string)
	}
	if dst.Annotations == nil {
		dst.Annotations = make(map[string]string)
	}

	// Copy labels from AIGatewayRoute to HTTPRoute.
	for k, v := range aiGatewayRoute.Labels {
		dst.Labels[k] = v
	}

	// Copy non-controller annotations from AIGatewayRoute to HTTPRoute.
	for k, v := range aiGatewayRoute.Annotations {
		dst.Annotations[k] = v
	}

	// HACK: We need to set an annotation so that Envoy Gateway reconciles the HTTPRoute when the backend refs change.
	dst.Annotations[httpRouteBackendRefPriorityAnnotationKey] = buildPriorityAnnotation(aiGatewayRoute.Spec.Rules)
	dst.Annotations[httpRouteAnnotationForAIGatewayGeneratedIndication] = "true"
	// On error, the existing hash is kept: the HTTPRoute is still written, and the next reconcile updates it.
	if quotaPolicyHash, err := c.buildQuotaPolicyHashAnnotation(ctx, aiGatewayRoute); err != nil {
		errs = append(errs, err)
	} else if quotaPolicyHash == "" {
		delete(dst.Annotations, httpRouteQuotaPolicyHashAnnotationKey)
	} else {
		dst.Annotations[httpRouteQuotaPolicyHashAnnotationKey] = quotaPolicyHash
	}

	dst.Spec.ParentRefs = aiGatewayRoute.Spec.ParentRefs

	dst.Spec.Hostnames = aiGatewayRoute.Spec.Hostnames
	return errors.Join(errs...)
}

// syncGateways synchronizes the gateways referenced by the AIGatewayRoute by sending events to the gateway controller.
func (c *AIGatewayRouteController) syncGateways(ctx context.Context, aiGatewayRoute *aigv1b1.AIGatewayRoute) error {
	for _, p := range aiGatewayRoute.Spec.ParentRefs {
		gwNamespace := aiGatewayRoute.Namespace
		if p.Namespace != nil {
			gwNamespace = string(*p.Namespace)
		}
		if err := c.syncGateway(ctx, gwNamespace, string(p.Name)); err != nil {
			if aiGatewayRoute.DeletionTimestamp != nil && apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
	}
	return nil
}

// syncGateway is a helper function for syncGateways that sends one GenericEvent to the gateway controller.
func (c *AIGatewayRouteController) syncGateway(ctx context.Context, namespace, name string) error {
	var gw gwapiv1.Gateway
	if err := c.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			c.logger.Info("Gateway not found", "namespace", namespace, "name", name)
			return fmt.Errorf("gateway %s/%s not found: %w", namespace, name, err)
		}
		c.logger.Error(err, "failed to get Gateway", "namespace", namespace, "name", name)
		return fmt.Errorf("failed to get Gateway %s/%s: %w", namespace, name, err)
	}
	c.logger.Info("syncing Gateway", "namespace", gw.Namespace, "name", gw.Name)
	c.gatewayEventChan <- event.GenericEvent{Object: &gw}
	return nil
}

func (c *AIGatewayRouteController) backend(ctx context.Context, namespace, name string) (*aigv1b1.AIServiceBackend, error) {
	backend := &aigv1b1.AIServiceBackend{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, backend); err != nil {
		return nil, err
	}
	return backend, nil
}

// inferencePoolMirrorBackendRef returns the HTTPRoute RequestMirror backendRef for a mirror leg
// targeting an InferencePool.
//
// A pool mirror's real target is decided per request by its endpoint picker: the extension server
// rewrites the mirror cluster to ORIGINAL_DST keyed on the mirror endpoint-picker header and wires
// the pool's EPP into the downstream chain. Envoy Gateway however refuses non-Service/Backend kinds
// on a RequestMirror backendRef, so the HTTPRoute carries a placeholder Service ref — the pool's own
// endpointPickerRef Service, which always exists alongside the pool. Its endpoints are irrelevant
// once the cluster is ORIGINAL_DST.
func (c *AIGatewayRouteController) inferencePoolMirrorBackendRef(
	ctx context.Context,
	aiGatewayRoute *aigv1b1.AIGatewayRoute,
	mirrorBR *aigv1b1.AIGatewayRouteRuleBackendRef,
) (gwapiv1.BackendObjectReference, error) {
	// The placeholder Service ref lives in the pool's namespace, so a cross-namespace pool mirror
	// would also need a Service ReferenceGrant at the Envoy Gateway level, whose absence fails the
	// RequestMirror filter of the rule. Only same-namespace pool mirrors are supported.
	if mirrorBR.IsCrossNamespace(aiGatewayRoute.Namespace) {
		return gwapiv1.BackendObjectReference{}, fmt.Errorf("mirror InferencePool %s.%s must be in the AIGatewayRoute namespace %s",
			mirrorBR.Name, mirrorBR.GetNamespace(aiGatewayRoute.Namespace), aiGatewayRoute.Namespace)
	}
	pool := &gwaiev1.InferencePool{}
	if err := c.client.Get(ctx, client.ObjectKey{
		Namespace: aiGatewayRoute.Namespace, Name: mirrorBR.Name,
	}, pool); err != nil {
		return gwapiv1.BackendObjectReference{}, fmt.Errorf("failed to get InferencePool for mirror %s.%s: %w",
			mirrorBR.Name, aiGatewayRoute.Namespace, err)
	}
	if pool.Spec.EndpointPickerRef == nil {
		return gwapiv1.BackendObjectReference{}, fmt.Errorf("mirror InferencePool %s.%s has no endpointPickerRef",
			mirrorBR.Name, aiGatewayRoute.Namespace)
	}
	eppPort := gwapiv1.PortNumber(internalapi.DefaultEndpointPickerPort)
	if p := pool.Spec.EndpointPickerRef.Port; p != nil {
		eppPort = gwapiv1.PortNumber(p.Number)
	}
	return gwapiv1.BackendObjectReference{
		Name: gwapiv1.ObjectName(pool.Spec.EndpointPickerRef.Name),
		Port: ptr.To(eppPort),
	}, nil
}

// validateAndGetBackend validates a backend reference (including cross-namespace ReferenceGrant check)
// and returns the AIServiceBackend if valid.
func (c *AIGatewayRouteController) validateAndGetBackend(
	ctx context.Context,
	aiGatewayRoute *aigv1b1.AIGatewayRoute,
	backendRef *aigv1b1.AIGatewayRouteRuleBackendRef,
) (*aigv1b1.AIServiceBackend, error) {
	backendNamespace := backendRef.GetNamespace(aiGatewayRoute.Namespace)

	// Validate cross-namespace reference if applicable
	if backendRef.IsCrossNamespace(aiGatewayRoute.Namespace) {
		if err := c.referenceGrantValidator.validateAIServiceBackendReference(
			ctx,
			aiGatewayRoute.Namespace,
			backendNamespace,
			backendRef.Name,
		); err != nil {
			return nil, err
		}
	}

	// Get the backend
	backend, err := c.backend(ctx, backendNamespace, backendRef.Name)
	if err != nil {
		return nil, fmt.Errorf("AIServiceBackend %s.%s not found", backendRef.Name, backendNamespace)
	}

	return backend, nil
}

// updateAIGatewayRouteStatus updates the status of the AIGatewayRoute.
func (c *AIGatewayRouteController) updateAIGatewayRouteStatus(ctx context.Context, route *aigv1b1.AIGatewayRoute, conditionType string, message string) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.client.Get(ctx, client.ObjectKey{Name: route.Name, Namespace: route.Namespace}, route); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		route.Status.Conditions = newConditions(conditionType, message)
		return c.client.Status().Update(ctx, route)
	})
	if err != nil {
		c.logger.Error(err, "failed to update AIGatewayRoute status")
	}
}

// Build an annotation that contains the priority of each backend ref. This is used to ensure Envoy Gateway reconciles the
// HTTP route when the priorities change.
func buildPriorityAnnotation(rules []aigv1b1.AIGatewayRouteRule) string {
	priorities := make([]string, 0, len(rules))
	for i, rule := range rules {
		for _, br := range rule.BackendRefs {
			var priority uint32
			if br.Priority != nil {
				priority = *br.Priority
			}
			priorities = append(priorities, fmt.Sprintf("%d:%s:%d", i, br.Name, priority))
		}
	}
	return strings.Join(priorities, ",")
}

// buildQuotaPolicyHashAnnotation hashes the specs of the QuotaPolicies that target the route's backends.
// It returns "" when no QuotaPolicy targets them, so a route without quotas carries no annotation.
func (c *AIGatewayRouteController) buildQuotaPolicyHashAnnotation(ctx context.Context, aiGatewayRoute *aigv1b1.AIGatewayRoute) (string, error) {
	policies := make(map[string]*aigv1a1.QuotaPolicy)
	for i := range aiGatewayRoute.Spec.Rules {
		for j := range aiGatewayRoute.Spec.Rules[i].BackendRefs {
			br := &aiGatewayRoute.Spec.Rules[i].BackendRefs[j]
			key := fmt.Sprintf("%s.%s", br.Name, br.GetNamespace(aiGatewayRoute.Namespace))
			var list aigv1a1.QuotaPolicyList
			if err := c.client.List(ctx, &list,
				client.MatchingFields{k8sClientIndexAIServiceBackendToTargetingQuotaPolicy: key}); err != nil {
				return "", fmt.Errorf("failed to list QuotaPolicies for backend %s: %w", key, err)
			}
			for k := range list.Items {
				qp := &list.Items[k]
				policies[qp.Namespace+"/"+qp.Name] = qp
			}
		}
	}
	if len(policies) == 0 {
		return "", nil
	}

	names := make([]string, 0, len(policies))
	for name := range policies {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		spec, err := json.Marshal(policies[name].Spec)
		if err != nil {
			return "", fmt.Errorf("failed to marshal QuotaPolicy %s spec: %w", name, err)
		}
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(spec)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
