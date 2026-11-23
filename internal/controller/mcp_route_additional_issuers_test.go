// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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

	aigv1b1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
	internaltesting "github.com/envoyproxy/ai-gateway/internal/testing"
)

const reconciledMessage = "MCP Gateway Route reconciled successfully"

// testIssuer is an authorization server whose metadata document can be taken down.
type testIssuer struct {
	url  string
	down atomic.Bool
	// jwksURI overrides the published jwks_uri when not empty.
	jwksURI string
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	i := &testIssuer{}
	mux := http.NewServeMux()
	mux.HandleFunc(oauthWellKnownAuthorizationServerMetadataPath, func(w http.ResponseWriter, _ *http.Request) {
		if i.down.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		jwksURI := i.url + "/certs"
		if i.jwksURI != "" {
			jwksURI = i.jwksURI
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 i.url,
			"authorization_endpoint": i.url + "/auth",
			"token_endpoint":         i.url + "/token",
			"jwks_uri":               jwksURI,
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	i.url = server.URL
	return i
}

// oauthRouteEnv reconciles one MCPRoute and reads back what was generated for it.
type oauthRouteEnv struct {
	t      *testing.T
	client client.Client
	c      *MCPRouteController
}

func newOAuthRouteEnv(t *testing.T, oauth *aigv1b1.MCPRouteOAuth, extraObjs ...client.Object) *oauthRouteEnv {
	t.Helper()
	fakeClient := requireNewFakeClientWithIndexesForMCP(t)
	eventCh := internaltesting.NewControllerEventChan[*gwapiv1.Gateway]()
	e := &oauthRouteEnv{
		t: t, client: fakeClient,
		c: NewMCPRouteController(fakeClient, fakekube.NewClientset(), logr.Discard(), eventCh.Ch),
	}
	require.NoError(t, fakeClient.Create(t.Context(), &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
	}))
	for _, obj := range extraObjs {
		require.NoError(t, fakeClient.Create(t.Context(), obj))
	}
	require.NoError(t, fakeClient.Create(t.Context(), &aigv1b1.MCPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "default"},
		Spec: aigv1b1.MCPRouteSpec{
			ParentRefs: []gwapiv1.ParentReference{{Name: "gw"}},
			BackendRefs: []aigv1b1.MCPRouteBackendRef{{
				BackendObjectReference: gwapiv1.BackendObjectReference{Name: "svc"},
			}},
			SecurityPolicy: &aigv1b1.MCPRouteSecurityPolicy{OAuth: oauth},
		},
	}))
	return e
}

func (e *oauthRouteEnv) route() *aigv1b1.MCPRoute {
	e.t.Helper()
	var route aigv1b1.MCPRoute
	require.NoError(e.t, e.client.Get(e.t.Context(), types.NamespacedName{Namespace: "default", Name: "route"}, &route))
	return &route
}

func (e *oauthRouteEnv) updateOAuth(oauth *aigv1b1.MCPRouteOAuth) {
	e.t.Helper()
	route := e.route()
	route.Spec.SecurityPolicy.OAuth = oauth
	require.NoError(e.t, e.client.Update(e.t.Context(), route))
}

// reconcile returns the reconcile result and the message of the Accepted condition.
func (e *oauthRouteEnv) reconcile() (reconcile.Result, string) {
	e.t.Helper()
	result, err := e.c.Reconcile(e.t.Context(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "route"},
	})
	require.NoError(e.t, err)
	conditions := e.route().Status.Conditions
	require.Len(e.t, conditions, 1)
	require.Equal(e.t, aigv1b1.ConditionTypeAccepted, conditions[0].Type)
	return result, conditions[0].Message
}

func (e *oauthRouteEnv) providers() []egv1a1.JWTProvider {
	e.t.Helper()
	var sp egv1a1.SecurityPolicy
	require.NoError(e.t, e.client.Get(e.t.Context(), client.ObjectKey{
		Namespace: "default", Name: internalapi.MCPGeneratedResourceCommonPrefix + "route",
	}, &sp))
	require.NotNil(e.t, sp.Spec.JWT)
	// A token of no accepted issuer must never pass, so neither relaxation may be set.
	require.Nil(e.t, sp.Spec.JWT.Optional)
	require.Nil(e.t, sp.Spec.JWT.FailOpen)
	return sp.Spec.JWT.Providers
}

func (e *oauthRouteEnv) metadata() (protectedResource, authServer string) {
	e.t.Helper()
	get := func(name string) string {
		var filter egv1a1.HTTPRouteFilter
		require.NoError(e.t, e.client.Get(e.t.Context(), client.ObjectKey{Namespace: "default", Name: name}, &filter))
		return *filter.Spec.DirectResponse.Body.Inline
	}
	return get(oauthProtectedResourceMetadataName("route")), get(oauthAuthServerMetadataFilterName("route"))
}

func testOAuth(issuer string, additional ...aigv1b1.MCPRouteOAuthIssuer) *aigv1b1.MCPRouteOAuth {
	return &aigv1b1.MCPRouteOAuth{
		Issuer:            issuer,
		AdditionalIssuers: additional,
		Audiences:         []string{"aud-1", "aud-2"},
		ProtectedResourceMetadata: aigv1b1.ProtectedResourceMetadata{
			Resource:        "https://api.example.com/mcp",
			ScopesSupported: []string{"read"},
		},
		ClaimToHeaders: []egv1a1.ClaimToHeader{{Claim: "email", Header: "x-user-email"}},
	}
}

func TestMCPRouteController_AdditionalIssuers(t *testing.T) {
	wantClaimToHeaders := []egv1a1.ClaimToHeader{
		{Claim: "sub", Header: internalapi.MCPSubjectHeader},
		{Claim: "email", Header: "x-user-email"},
	}
	provider := func(name, issuer, jwksURI string) egv1a1.JWTProvider {
		return egv1a1.JWTProvider{
			Name: name, Issuer: issuer, Audiences: []string{"aud-1", "aud-2"},
			RemoteJWKS:     &egv1a1.RemoteJWKS{URI: jwksURI},
			ClaimToHeaders: wantClaimToHeaders,
		}
	}

	t.Run("each issuer gets its own keys and the same audiences and claim headers", func(t *testing.T) {
		primary, discovered := newTestIssuer(t), newTestIssuer(t)
		e := newOAuthRouteEnv(t, testOAuth(primary.url,
			aigv1b1.MCPRouteOAuthIssuer{Issuer: discovered.url},
			aigv1b1.MCPRouteOAuthIssuer{Issuer: "https://explicit.example.com", JWKSURI: ptr.To("http://keys.example.com/certs")},
		))
		result, message := e.reconcile()
		require.Zero(t, result)
		require.Equal(t, reconciledMessage, message)
		require.Equal(t, []egv1a1.JWTProvider{
			provider("mcp-jwt-provider", primary.url, primary.url+"/certs"),
			provider("mcp-jwt-provider-1", discovered.url, discovered.url+"/certs"),
			provider("mcp-jwt-provider-2", "https://explicit.example.com", "http://keys.example.com/certs"),
		}, e.providers())
	})

	t.Run("only issuer feeds the published metadata", func(t *testing.T) {
		primary, other := newTestIssuer(t), newTestIssuer(t)
		single := newOAuthRouteEnv(t, testOAuth(primary.url))
		single.reconcile()
		several := newOAuthRouteEnv(t, testOAuth(primary.url, aigv1b1.MCPRouteOAuthIssuer{Issuer: other.url}))
		several.reconcile()

		wantProtectedResource, wantAuthServer := single.metadata()
		protectedResource, authServer := several.metadata()
		require.Equal(t, wantProtectedResource, protectedResource)
		require.Equal(t, wantAuthServer, authServer)
		require.NotContains(t, protectedResource+authServer, other.url)
		require.Equal(t, single.providers()[0], several.providers()[0])
	})

	t.Run("an empty list renders what no list renders", func(t *testing.T) {
		primary := newTestIssuer(t)
		oauth := testOAuth(primary.url)
		none := newOAuthRouteEnv(t, oauth.DeepCopy())
		none.reconcile()
		oauth.AdditionalIssuers = []aigv1b1.MCPRouteOAuthIssuer{}
		empty := newOAuthRouteEnv(t, oauth)
		result, message := empty.reconcile()
		require.Zero(t, result)
		require.Equal(t, reconciledMessage, message)
		require.Equal(t, none.providers(), empty.providers())
	})

	t.Run("an issuer without keys is left out and retried", func(t *testing.T) {
		primary, down, up := newTestIssuer(t), newTestIssuer(t), newTestIssuer(t)
		down.down.Store(true)
		oauth := testOAuth(primary.url,
			aigv1b1.MCPRouteOAuthIssuer{Issuer: down.url},
			aigv1b1.MCPRouteOAuthIssuer{Issuer: up.url},
		)
		// The keys of issuer must never be used for another one.
		oauth.JWKS = &aigv1b1.JWKS{RemoteJWKS: &egv1a1.RemoteJWKS{URI: "http://keys.example.com/primary"}}
		e := newOAuthRouteEnv(t, oauth)

		result, message := e.reconcile()
		require.Equal(t, additionalIssuerRetryInterval, result.RequeueAfter)
		require.Equal(t, reconciledMessage+"; JWKS URI not found for additional issuers: "+down.url+" (tokens refused)", message)
		wantPrimary := provider("mcp-jwt-provider", primary.url, "http://keys.example.com/primary")
		// The name of an issuer follows its position in the list, whatever the state of the others.
		wantUp := provider("mcp-jwt-provider-2", up.url, up.url+"/certs")
		require.Equal(t, []egv1a1.JWTProvider{wantPrimary, wantUp}, e.providers())

		down.down.Store(false)
		result, message = e.reconcile()
		require.Zero(t, result)
		require.Equal(t, reconciledMessage, message)
		require.Equal(t, []egv1a1.JWTProvider{
			wantPrimary, provider("mcp-jwt-provider-1", down.url, down.url+"/certs"), wantUp,
		}, e.providers())
	})

	t.Run("an unreachable issuer keeps its known JWKS URI", func(t *testing.T) {
		primary, other := newTestIssuer(t), newTestIssuer(t)
		e := newOAuthRouteEnv(t, testOAuth(primary.url, aigv1b1.MCPRouteOAuthIssuer{Issuer: other.url}))
		e.reconcile()
		before := e.providers()

		other.down.Store(true)
		for range 2 {
			result, message := e.reconcile()
			require.Equal(t, additionalIssuerRetryInterval, result.RequeueAfter)
			require.Equal(t, reconciledMessage+"; JWKS URI not found for additional issuers: "+other.url+" (last known JWKS URI kept)", message)
			require.Equal(t, before, e.providers())
		}

		// Once removed from the list, it is refused and its keys are forgotten.
		e.updateOAuth(testOAuth(primary.url))
		result, _ := e.reconcile()
		require.Zero(t, result)
		require.Equal(t, before[:1], e.providers())
		e.updateOAuth(testOAuth(primary.url, aigv1b1.MCPRouteOAuthIssuer{Issuer: other.url}))
		_, message := e.reconcile()
		require.Contains(t, message, other.url+" (tokens refused)")
		require.Equal(t, before[:1], e.providers())
	})

	t.Run("moving from one host to another", func(t *testing.T) {
		oldHost, newHost := newTestIssuer(t), newTestIssuer(t)
		oldProvider := provider("mcp-jwt-provider", oldHost.url, oldHost.url+"/certs")
		newProvider := provider("mcp-jwt-provider-1", newHost.url, newHost.url+"/certs")

		e := newOAuthRouteEnv(t, testOAuth(oldHost.url))
		e.reconcile()
		require.Equal(t, []egv1a1.JWTProvider{oldProvider}, e.providers())

		// Accept the tokens of both.
		e.updateOAuth(testOAuth(oldHost.url, aigv1b1.MCPRouteOAuthIssuer{Issuer: newHost.url}))
		e.reconcile()
		require.Equal(t, []egv1a1.JWTProvider{oldProvider, newProvider}, e.providers())
		protectedResource, _ := e.metadata()
		require.Contains(t, protectedResource, oldHost.url)

		// Switch, with the metadata of the old host already gone: its tokens must still pass.
		oldHost.down.Store(true)
		e.updateOAuth(testOAuth(newHost.url, aigv1b1.MCPRouteOAuthIssuer{Issuer: oldHost.url}))
		e.reconcile()
		newProvider.Name, oldProvider.Name = oldProvider.Name, newProvider.Name
		require.Equal(t, []egv1a1.JWTProvider{newProvider, oldProvider}, e.providers())
		protectedResource, _ = e.metadata()
		require.Contains(t, protectedResource, newHost.url)
		require.NotContains(t, protectedResource, oldHost.url)

		// Stop accepting the old one.
		e.updateOAuth(testOAuth(newHost.url))
		e.reconcile()
		require.Equal(t, []egv1a1.JWTProvider{newProvider}, e.providers())
	})

	t.Run("local keys of a former issuer are not carried over", func(t *testing.T) {
		oldHost, newHost := newTestIssuer(t), newTestIssuer(t)
		oauth := testOAuth(oldHost.url)
		oauth.JWKS = &aigv1b1.JWKS{LocalJWKS: &egv1a1.LocalJWKS{
			Type: ptr.To(egv1a1.LocalJWKSTypeInline), Inline: ptr.To(`{"keys":[]}`),
		}}
		e := newOAuthRouteEnv(t, oauth)
		e.reconcile()

		oldHost.down.Store(true)
		e.updateOAuth(testOAuth(newHost.url, aigv1b1.MCPRouteOAuthIssuer{Issuer: oldHost.url}))
		_, message := e.reconcile()
		require.Contains(t, message, oldHost.url+" (tokens refused)")
		require.Equal(t, []egv1a1.JWTProvider{
			provider("mcp-jwt-provider", newHost.url, newHost.url+"/certs"),
		}, e.providers())
	})

	// A URI Envoy Gateway or the API server rejects would take the whole SecurityPolicy down.
	for name, uri := range map[string]string{
		"relative":          "/certs",
		"not http":          "ftp://keys.example.com/certs",
		"no host":           "https:///certs",
		"port out of range": "https://keys.example.com:70000/certs",
		"not a URI":         "https://keys.example.com/%zz",
		"too long":          "https://keys.example.com/" + strings.Repeat("a", 253),
	} {
		t.Run("a JWKS URI that cannot be used is left out: "+name, func(t *testing.T) {
			primary, other := newTestIssuer(t), newTestIssuer(t)
			other.jwksURI = uri
			e := newOAuthRouteEnv(t, testOAuth(primary.url,
				aigv1b1.MCPRouteOAuthIssuer{Issuer: other.url},
				aigv1b1.MCPRouteOAuthIssuer{Issuer: "https://explicit.example.com", JWKSURI: &uri},
			))
			_, message := e.reconcile()
			require.Equal(t, reconciledMessage+"; JWKS URI not found for additional issuers: "+
				other.url+" (tokens refused), https://explicit.example.com (tokens refused)", message)
			require.Equal(t, []egv1a1.JWTProvider{
				provider("mcp-jwt-provider", primary.url, primary.url+"/certs"),
			}, e.providers())
		})
	}

	t.Run("HTTPS keys are fetched through a backend with TLS settings", func(t *testing.T) {
		primary, other := newTestIssuer(t), newTestIssuer(t)
		other.jwksURI = "https://keys.example.com/certs"
		u, err := url.Parse(other.jwksURI)
		require.NoError(t, err)
		e := newOAuthRouteEnv(t, testOAuth(primary.url,
			aigv1b1.MCPRouteOAuthIssuer{Issuer: other.url},
			aigv1b1.MCPRouteOAuthIssuer{Issuer: "https://explicit.example.com", JWKSURI: ptr.To("https://keys.example.com/explicit")},
		), &gwapiv1.BackendTLSPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "keys", Namespace: "default"},
			Spec: gwapiv1.BackendTLSPolicySpec{
				Validation: gwapiv1.BackendTLSPolicyValidation{Hostname: gwapiv1.PreciseHostname(u.Hostname())},
				TargetRefs: []gwapiv1.LocalPolicyTargetReferenceWithSectionName{{
					LocalPolicyTargetReference: gwapiv1.LocalPolicyTargetReference{
						Group: "gateway.envoyproxy.io", Kind: "Backend", Name: "keys-backend",
					},
				}},
			},
		})
		e.reconcile()
		providers := e.providers()
		require.Len(t, providers, 3)
		for _, p := range providers[1:] {
			require.Len(t, p.RemoteJWKS.BackendRefs, 1)
			require.Equal(t, gwapiv1.ObjectName("keys-backend"), p.RemoteJWKS.BackendRefs[0].Name)
		}
	})

	t.Run("a failure of issuer itself still fails the route", func(t *testing.T) {
		primary, other := newTestIssuer(t), newTestIssuer(t)
		primary.down.Store(true)
		e := newOAuthRouteEnv(t, testOAuth(primary.url, aigv1b1.MCPRouteOAuthIssuer{Issuer: other.url}))
		_, err := e.c.Reconcile(t.Context(), reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "route"},
		})
		require.ErrorContains(t, err, "failed to auto-discover JWKS URI")
		require.Equal(t, aigv1b1.ConditionTypeNotAccepted, e.route().Status.Conditions[0].Type)
	})
}
