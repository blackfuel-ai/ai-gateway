// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fake2 "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwaiev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
	aigv1b1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	internaltesting "github.com/envoyproxy/ai-gateway/internal/testing"
)

// finalizerCase describes one controller that guards its resource with aiGatewayControllerFinalizer.
type finalizerCase struct {
	name string
	// newObject returns a minimal valid object of the controller's kind, without finalizers.
	newObject func(key types.NamespacedName) client.Object
	// reconciler builds the controller over the given client.
	reconciler func(t *testing.T, c client.Client) reconcile.Reconciler
	// conditions returns the status conditions of the object, or nil when the controller writes no
	// status on the deletion path.
	conditions func(o client.Object) []metav1.Condition
	// setAccepted seeds an Accepted=True status, as a previous successful reconcile leaves it.
	setAccepted func(o client.Object)
}

func finalizerCases() []finalizerCase {
	accepted := newConditions(aigv1b1.ConditionTypeAccepted, "reconciled successfully")
	return []finalizerCase{
		{
			name: "AIGatewayRoute",
			newObject: func(key types.NamespacedName) client.Object {
				return &aigv1b1.AIGatewayRoute{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
			},
			reconciler: func(_ *testing.T, c client.Client) reconcile.Reconciler {
				ch := internaltesting.NewControllerEventChan[*gwapiv1.Gateway]()
				return NewAIGatewayRouteController(c, fake2.NewClientset(), ctrl.Log, ch.Ch, "/v1")
			},
			conditions:  func(o client.Object) []metav1.Condition { return o.(*aigv1b1.AIGatewayRoute).Status.Conditions },
			setAccepted: func(o client.Object) { o.(*aigv1b1.AIGatewayRoute).Status.Conditions = accepted },
		},
		{
			name: "AIServiceBackend",
			newObject: func(key types.NamespacedName) client.Object {
				return &aigv1b1.AIServiceBackend{
					ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
					Spec: aigv1b1.AIServiceBackendSpec{
						BackendRef: gwapiv1.BackendObjectReference{Name: "some-backend", Namespace: ptr.To(gwapiv1.Namespace(key.Namespace))},
					},
				}
			},
			reconciler: func(_ *testing.T, c client.Client) reconcile.Reconciler {
				ch := internaltesting.NewControllerEventChan[*aigv1b1.AIGatewayRoute]()
				return NewAIServiceBackendController(c, fake2.NewClientset(), ctrl.Log, ch.Ch)
			},
			conditions:  func(o client.Object) []metav1.Condition { return o.(*aigv1b1.AIServiceBackend).Status.Conditions },
			setAccepted: func(o client.Object) { o.(*aigv1b1.AIServiceBackend).Status.Conditions = accepted },
		},
		{
			name: "BackendSecurityPolicy",
			newObject: func(key types.NamespacedName) client.Object {
				return &aigv1b1.BackendSecurityPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
					Spec: aigv1b1.BackendSecurityPolicySpec{
						Type:   aigv1b1.BackendSecurityPolicyTypeAPIKey,
						APIKey: &aigv1b1.BackendSecurityPolicyAPIKey{SecretRef: &gwapiv1.SecretObjectReference{Name: "mysecret"}},
					},
				}
			},
			reconciler: func(_ *testing.T, c client.Client) reconcile.Reconciler {
				backendCh := internaltesting.NewControllerEventChan[*aigv1b1.AIServiceBackend]()
				poolCh := internaltesting.NewControllerEventChan[*gwaiev1.InferencePool]()
				return NewBackendSecurityPolicyController(c, fake2.NewClientset(), ctrl.Log, backendCh.Ch, poolCh.Ch)
			},
			conditions: func(o client.Object) []metav1.Condition {
				return o.(*aigv1b1.BackendSecurityPolicy).Status.Conditions
			},
			setAccepted: func(o client.Object) { o.(*aigv1b1.BackendSecurityPolicy).Status.Conditions = accepted },
		},
		{
			name: "MCPRoute",
			newObject: func(key types.NamespacedName) client.Object {
				return &aigv1b1.MCPRoute{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
			},
			reconciler: func(_ *testing.T, c client.Client) reconcile.Reconciler {
				ch := internaltesting.NewControllerEventChan[*gwapiv1.Gateway]()
				return NewMCPRouteController(c, fake2.NewClientset(), ctrl.Log, ch.Ch)
			},
			conditions:  func(o client.Object) []metav1.Condition { return o.(*aigv1b1.MCPRoute).Status.Conditions },
			setAccepted: func(o client.Object) { o.(*aigv1b1.MCPRoute).Status.Conditions = accepted },
		},
		{
			name: "QuotaPolicy",
			newObject: func(key types.NamespacedName) client.Object {
				return &aigv1a1.QuotaPolicy{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
			},
			reconciler: func(t *testing.T, c client.Client) reconcile.Reconciler {
				return NewQuotaPolicyController(c, fake2.NewClientset(), ctrl.Log, newTestRunner(t),
					make(chan event.GenericEvent, 100), electedCh())
			},
		},
	}
}

// newFinalizerTestClient returns a fake client serving every kind in finalizerCases, whose first
// Update is answered with the Conflict the API server returns for a stale resourceVersion.
func newFinalizerTestClient(t *testing.T, conflictFirstUpdate bool) client.Client {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(Scheme).
		WithStatusSubresource(&aigv1b1.AIGatewayRoute{}).
		WithStatusSubresource(&aigv1b1.AIServiceBackend{}).
		WithStatusSubresource(&aigv1b1.BackendSecurityPolicy{}).
		WithStatusSubresource(&aigv1b1.MCPRoute{}).
		WithStatusSubresource(&aigv1a1.QuotaPolicy{})
	err := ApplyIndexing(t.Context(), func(_ context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
		builder = builder.WithIndex(obj, field, extractValue)
		return nil
	})
	require.NoError(t, err)
	if !conflictFirstUpdate {
		return builder.Build()
	}
	updates := 0
	return builder.WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates++
			if updates == 1 {
				return apierrors.NewConflict(schema.GroupResource{Group: "aigateway.envoyproxy.io", Resource: "objects"},
					obj.GetName(), fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))
			}
			return cl.Update(ctx, obj, opts...)
		},
	}).Build()
}

// TestReconcile_FinalizerRemovalConflictIsRetried pins, for every controller, that a failed
// finalizer removal fails the reconcile. No watch event re-triggers a terminating object, so the
// error returned here is what makes controller-runtime requeue; a nil result leaves the object
// Terminating with the finalizer until the controller restarts.
func TestReconcile_FinalizerRemovalConflictIsRetried(t *testing.T) {
	for _, tc := range finalizerCases() {
		t.Run(tc.name, func(t *testing.T) {
			c := newFinalizerTestClient(t, true)
			r := tc.reconciler(t, c)
			key := types.NamespacedName{Namespace: "inference", Name: "terminating"}
			obj := tc.newObject(key)
			obj.SetFinalizers([]string{aiGatewayControllerFinalizer})
			require.NoError(t, c.Create(t.Context(), obj))
			require.NoError(t, c.Delete(t.Context(), obj))

			_, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: key})
			require.True(t, apierrors.IsConflict(err), "the conflict must fail the reconcile so it is requeued, got %v", err)

			stuck := tc.newObject(key)
			require.NoError(t, c.Get(t.Context(), key, stuck))
			require.Equal(t, []string{aiGatewayControllerFinalizer}, stuck.GetFinalizers())
			if tc.conditions != nil {
				for _, cond := range tc.conditions(stuck) {
					require.NotEqual(t, aigv1b1.ConditionTypeAccepted, cond.Type,
						"an object whose finalizer removal failed must not read Accepted")
				}
			}

			// The requeued reconcile removes the finalizer and the object is gone.
			_, err = r.Reconcile(t.Context(), reconcile.Request{NamespacedName: key})
			require.NoError(t, err)
			err = c.Get(t.Context(), key, stuck)
			require.True(t, apierrors.IsNotFound(err), "expected the object to be deleted, got %v", err)
		})
	}
}

// TestReconcile_FinalizerAddConflictFailsReconcile pins, for every controller, that a failed
// finalizer add fails the reconcile of a live object instead of syncing it without the finalizer.
func TestReconcile_FinalizerAddConflictFailsReconcile(t *testing.T) {
	for _, tc := range finalizerCases() {
		t.Run(tc.name, func(t *testing.T) {
			c := newFinalizerTestClient(t, true)
			r := tc.reconciler(t, c)
			key := types.NamespacedName{Namespace: "inference", Name: "live"}
			require.NoError(t, c.Create(t.Context(), tc.newObject(key)))

			_, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: key})
			require.True(t, apierrors.IsConflict(err), "the conflict must fail the reconcile so it is requeued, got %v", err)

			live := tc.newObject(key)
			require.NoError(t, c.Get(t.Context(), key, live))
			require.Empty(t, live.GetFinalizers())
		})
	}
}

// TestReconcile_KeptTerminatingReadsNotAccepted pins that an object accepted while live reads
// NotAccepted once it is marked for deletion and another finalizer keeps it readable: nothing
// serves it any more, and a consumer of the Accepted condition must not see it as healthy.
func TestReconcile_KeptTerminatingReadsNotAccepted(t *testing.T) {
	const otherFinalizer = "example.com/other"
	for _, tc := range finalizerCases() {
		if tc.conditions == nil {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			c := newFinalizerTestClient(t, false)
			r := tc.reconciler(t, c)
			key := types.NamespacedName{Namespace: "inference", Name: "kept"}
			obj := tc.newObject(key)
			obj.SetFinalizers([]string{aiGatewayControllerFinalizer, otherFinalizer})
			require.NoError(t, c.Create(t.Context(), obj))
			tc.setAccepted(obj)
			require.NoError(t, c.Status().Update(t.Context(), obj))
			require.NoError(t, c.Delete(t.Context(), obj))

			_, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: key})
			require.NoError(t, err)

			kept := tc.newObject(key)
			require.NoError(t, c.Get(t.Context(), key, kept))
			require.Equal(t, []string{otherFinalizer}, kept.GetFinalizers())
			conds := tc.conditions(kept)
			require.Len(t, conds, 1)
			require.Equal(t, aigv1b1.ConditionTypeNotAccepted, conds[0].Type)
			require.Equal(t, terminatingMessage, conds[0].Message)
		})
	}
}
