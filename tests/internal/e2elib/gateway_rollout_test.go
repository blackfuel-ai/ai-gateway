// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package e2elib

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestCheckGatewayRolloutSettled(t *testing.T) {
	readyPod := func(name string, sidecar bool) corev1.Pod {
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "envoy"}}},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			}},
		}
		if sidecar {
			pod.Spec.InitContainers = []corev1.Container{{Name: "ai-gateway-extproc"}}
		}
		return pod
	}
	settledDeployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "envoy", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
		},
	}

	for _, tc := range []struct {
		name        string
		pods        []corev1.Pod
		deployments []appsv1.Deployment
		daemonSets  []appsv1.DaemonSet
		expErr      string
	}{
		{
			name:        "settled with init-container sidecar",
			pods:        []corev1.Pod{readyPod("new", true)},
			deployments: []appsv1.Deployment{settledDeployment},
		},
		{
			name: "settled with regular-container sidecar",
			pods: []corev1.Pod{func() corev1.Pod {
				p := readyPod("new", false)
				p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "ai-gateway-extproc"})
				return p
			}()},
			deployments: []appsv1.Deployment{settledDeployment},
		},
		{
			name:   "no pods",
			expErr: "no pods",
		},
		{
			name:        "outgoing pod without sidecar still serving",
			pods:        []corev1.Pod{readyPod("old", false), readyPod("new", true)},
			deployments: []appsv1.Deployment{settledDeployment},
			expErr:      `pod "old" has no ai-gateway-extproc container`,
		},
		{
			name: "outgoing pod terminating",
			pods: []corev1.Pod{func() corev1.Pod {
				p := readyPod("old", true)
				p.DeletionTimestamp = ptr.To(metav1.Now())
				return p
			}(), readyPod("new", true)},
			deployments: []appsv1.Deployment{settledDeployment},
			expErr:      `pod "old" is terminating`,
		},
		{
			name: "pod not ready",
			pods: []corev1.Pod{func() corev1.Pod {
				p := readyPod("new", true)
				p.Status.Conditions[0].Status = corev1.ConditionFalse
				return p
			}()},
			deployments: []appsv1.Deployment{settledDeployment},
			expErr:      `pod "new" is not Ready`,
		},
		{
			name: "deployment template patched but not yet observed",
			pods: []corev1.Pod{readyPod("new", true)},
			deployments: []appsv1.Deployment{func() appsv1.Deployment {
				d := settledDeployment
				d.Generation = 3
				return d
			}()},
			expErr: `deployment "envoy" rollout not complete`,
		},
		{
			name: "deployment still has old-template replicas",
			pods: []corev1.Pod{readyPod("new", true)},
			deployments: []appsv1.Deployment{func() appsv1.Deployment {
				d := settledDeployment
				d.Status.Replicas = 2
				return d
			}()},
			expErr: `deployment "envoy" rollout not complete`,
		},
		{
			name: "daemonset still rolling",
			pods: []corev1.Pod{readyPod("new", true)},
			daemonSets: []appsv1.DaemonSet{{
				ObjectMeta: metav1.ObjectMeta{Name: "envoy", Generation: 1},
				Status: appsv1.DaemonSetStatus{
					ObservedGeneration: 1, DesiredNumberScheduled: 2, CurrentNumberScheduled: 2,
					UpdatedNumberScheduled: 1, NumberAvailable: 2,
				},
			}},
			expErr: `daemonset "envoy" rollout not complete`,
		},
		{
			name: "daemonset settled",
			pods: []corev1.Pod{readyPod("new", true)},
			daemonSets: []appsv1.DaemonSet{{
				ObjectMeta: metav1.ObjectMeta{Name: "envoy", Generation: 1},
				Status: appsv1.DaemonSetStatus{
					ObservedGeneration: 1, DesiredNumberScheduled: 1, CurrentNumberScheduled: 1,
					UpdatedNumberScheduled: 1, NumberAvailable: 1,
				},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkGatewayRolloutSettled(tc.pods, tc.deployments, tc.daemonSets)
			if tc.expErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.expErr)
		})
	}
}
