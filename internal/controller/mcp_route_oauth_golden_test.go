// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	aigv1b1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
	internaltesting "github.com/envoyproxy/ai-gateway/internal/testing"
)

// TestMCPRouteController_OAuthSingleIssuerGolden pins every object generated for an MCPRoute that
// accepts a single OAuth issuer: the SecurityPolicy, the WWW-Authenticate BackendTrafficPolicy, both
// published metadata documents, the status and the reconcile result. The golden files are the
// compatibility contract for existing routes; set UPDATE_GOLDEN=1 to rewrite them.
func TestMCPRouteController_OAuthSingleIssuerGolden(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/main/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host + "/realms/main"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 base,
			"authorization_endpoint": base + "/auth",
			"token_endpoint":         base + "/token",
			// HTTPS so that the backend TLS policy lookup runs.
			"jwks_uri": "https://" + r.Host + "/realms/main/certs",
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	issuer := server.URL + "/realms/main"

	metadata := aigv1b1.ProtectedResourceMetadata{
		Resource:                          "https://api.example.com/mcp",
		ResourceName:                      ptr.To("example"),
		ScopesSupported:                   []string{"read", "write"},
		ResourceSigningAlgValuesSupported: []string{"RS256"},
		ResourceDocumentation:             ptr.To("https://api.example.com/docs"),
		ResourcePolicyURI:                 ptr.To("https://api.example.com/policy"),
	}

	for _, tc := range []struct {
		name   string
		policy *aigv1b1.MCPRouteSecurityPolicy
	}{
		{
			name: "remote_jwks",
			policy: &aigv1b1.MCPRouteSecurityPolicy{OAuth: &aigv1b1.MCPRouteOAuth{
				Issuer:    issuer,
				Audiences: []string{"aud-1", "aud-2"},
				JWKS: &aigv1b1.JWKS{RemoteJWKS: &egv1a1.RemoteJWKS{
					URI: server.URL + "/realms/main/certs",
				}},
				ProtectedResourceMetadata: metadata,
				ClaimToHeaders: []egv1a1.ClaimToHeader{
					{Claim: "email", Header: "x-user-email"},
				},
			}},
		},
		{
			name: "local_jwks",
			policy: &aigv1b1.MCPRouteSecurityPolicy{OAuth: &aigv1b1.MCPRouteOAuth{
				Issuer: issuer,
				JWKS: &aigv1b1.JWKS{LocalJWKS: &egv1a1.LocalJWKS{
					Type:   ptr.To(egv1a1.LocalJWKSTypeInline),
					Inline: ptr.To(`{"keys":[]}`),
				}},
				ProtectedResourceMetadata: metadata,
			}},
		},
		{
			name: "discovered_jwks",
			policy: &aigv1b1.MCPRouteSecurityPolicy{OAuth: &aigv1b1.MCPRouteOAuth{
				Issuer:                    issuer,
				Audiences:                 []string{"aud-1"},
				ProtectedResourceMetadata: metadata,
			}},
		},
		{
			name: "combined_with_other_authentication",
			policy: &aigv1b1.MCPRouteSecurityPolicy{
				OAuth: &aigv1b1.MCPRouteOAuth{
					Issuer:                    issuer,
					ProtectedResourceMetadata: metadata,
				},
				APIKeyAuth: &egv1a1.APIKeyAuth{
					CredentialRefs: []gwapiv1.SecretObjectReference{{Name: "client-keys"}},
					ExtractFrom:    []*egv1a1.ExtractFrom{{Headers: []string{"x-api-key"}}},
				},
				MergeType: ptr.To(egv1a1.StrategicMerge),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := requireNewFakeClientWithIndexesForMCP(t)
			eventCh := internaltesting.NewControllerEventChan[*gwapiv1.Gateway]()
			c := NewMCPRouteController(fakeClient, fakekube.NewClientset(), logr.Discard(), eventCh.Ch)

			require.NoError(t, fakeClient.Create(t.Context(), &gwapiv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			}))
			require.NoError(t, fakeClient.Create(t.Context(), &gwapiv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "jwks-backend-tls", Namespace: "default"},
				Spec: gwapiv1.BackendTLSPolicySpec{
					Validation: gwapiv1.BackendTLSPolicyValidation{Hostname: gwapiv1.PreciseHostname(serverURL.Hostname())},
					TargetRefs: []gwapiv1.LocalPolicyTargetReferenceWithSectionName{{
						LocalPolicyTargetReference: gwapiv1.LocalPolicyTargetReference{
							Group: "gateway.envoyproxy.io", Kind: "Backend", Name: "jwks-backend",
						},
					}},
				},
			}))
			route := &aigv1b1.MCPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "default"},
				Spec: aigv1b1.MCPRouteSpec{
					ParentRefs: []gwapiv1.ParentReference{{Name: "gw"}},
					BackendRefs: []aigv1b1.MCPRouteBackendRef{{
						BackendObjectReference: gwapiv1.BackendObjectReference{Name: "svc"},
					}},
					SecurityPolicy: tc.policy,
				},
			}
			require.NoError(t, fakeClient.Create(t.Context(), route))

			key := types.NamespacedName{Namespace: "default", Name: "route"}
			result, err := c.Reconcile(t.Context(), reconcile.Request{NamespacedName: key})
			require.NoError(t, err)

			var (
				securityPolicy    egv1a1.SecurityPolicy
				trafficPolicy     egv1a1.BackendTrafficPolicy
				protectedResource egv1a1.HTTPRouteFilter
				authServer        egv1a1.HTTPRouteFilter
			)
			for name, obj := range map[string]client.Object{
				internalapi.MCPGeneratedResourceCommonPrefix + "route": &securityPolicy,
				oauthProtectedResourceMetadataName("route"):            &trafficPolicy,
				oauthAuthServerMetadataFilterName("route"):             &authServer,
			} {
				require.NoError(t, fakeClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: name}, obj))
			}
			require.NoError(t, fakeClient.Get(t.Context(), client.ObjectKey{
				Namespace: "default", Name: oauthProtectedResourceMetadataName("route"),
			}, &protectedResource))
			require.NoError(t, fakeClient.Get(t.Context(), key, route))
			require.Len(t, route.Status.Conditions, 1)

			rendered, err := yaml.Marshal(map[string]any{
				"securityPolicy":              securityPolicy.Spec,
				"backendTrafficPolicy":        trafficPolicy.Spec,
				"protectedResourceMetadata":   protectedResource.Spec,
				"authorizationServerMetadata": authServer.Spec,
				"status":                      route.Status.Conditions[0].Type + ": " + route.Status.Conditions[0].Message,
				"requeueAfter":                fmt.Sprint(result.RequeueAfter),
			})
			require.NoError(t, err)
			// The test server listens on an arbitrary port.
			actual := strings.ReplaceAll(string(rendered), serverURL.Host, "auth.example.com")

			golden := filepath.Join("testdata", "mcp_route_oauth", tc.name+".yaml")
			if os.Getenv("UPDATE_GOLDEN") != "" {
				require.NoError(t, os.MkdirAll(filepath.Dir(golden), 0o755))
				require.NoError(t, os.WriteFile(golden, []byte(goldenHeader+actual), 0o600))
			}
			expected, err := os.ReadFile(golden)
			require.NoError(t, err)
			require.YAMLEq(t, string(expected), actual)
		})
	}
}

const goldenHeader = `# Copyright Envoy AI Gateway Authors
# SPDX-License-Identifier: Apache-2.0
# The full text of the Apache license is available in the LICENSE file at
# the root of the repo.

`
