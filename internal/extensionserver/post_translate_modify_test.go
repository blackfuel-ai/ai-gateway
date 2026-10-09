// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extensionserver

import (
	"bytes"
	"log/slog"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	httpconnectionmanagerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gwaiev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	aigv1b1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
)

func TestInsertAIGatewayExtProcFilter(t *testing.T) {
	tests := []struct {
		name                string
		existingFilters     []*httpconnectionmanagerv3.HttpFilter
		expectedPosition    int
		shouldPanic         bool
		expectedPanicMsg    string
		expectedFilterCount int
	}{
		{
			name:                "insert with only router filter",
			existingFilters:     []*httpconnectionmanagerv3.HttpFilter{{Name: "envoy.filters.http.router"}},
			expectedPosition:    0,
			expectedFilterCount: 2,
		},
		{
			name: "insert before router filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 3,
		},
		{
			name: "insert before extproc filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.ext_proc.existing"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 4,
		},
		{
			name: "insert before multiple extproc filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.ext_proc.existing"},
				{Name: "envoy.filters.http.ext_proc.existing.another"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 5,
		},
		{
			name: "insert before wasm filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.wasm"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 4,
		},
		{
			name: "insert before lua filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.lua"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 4,
		},
		{
			name: "insert before rbac filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.rbac"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 4,
		},
		{
			name: "insert before local_ratelimit filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.local_ratelimit"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 4,
		},
		{
			name: "insert before ratelimit filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.ratelimit"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 4,
		},
		{
			name: "insert before custom_response filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.custom_response"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 4,
		},
		{
			name: "insert before credential_injector filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.credential_injector"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 4,
		},
		{
			name: "insert before compressor filter",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.compressor"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 4,
		},
		{
			name: "insert at end when only early filters present",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.cors"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    2,
			expectedFilterCount: 4,
		},
		{
			name: "insert with multiple filters requiring ordering",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.fault"},
				{Name: "envoy.filters.http.cors"},
				{Name: "envoy.filters.http.ext_proc.other"},
				{Name: "envoy.filters.http.rbac"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    2,
			expectedFilterCount: 6,
		},
		{
			// Mirrors the EKS setup where an api-key ext_proc and a buffer filter are added ahead of AI
			// Gateway. The ext_proc at index 0 matches afterExtProcFilterPrefixes, but the buffer filter
			// must still run first so its larger request buffer limit applies to AI Gateway's BUFFERED
			// extproc. AI Gateway is inserted after the buffer filter (position 2).
			name: "insert after buffer when ext_proc precedes buffer",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.ext_proc.apikey"},
				{Name: "envoy.filters.http.buffer"},
				{Name: "envoy.filters.http.jwt_authn"},
				{Name: "envoy.filters.http.rbac"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    2,
			expectedFilterCount: 6,
		},
		{
			// When the buffer filter already precedes the first ext_proc filter, AI Gateway is inserted
			// right after the buffer filter (position 1), preserving Envoy Gateway's buffer-before-extproc
			// ordering.
			name: "insert after buffer when buffer precedes ext_proc",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.buffer"},
				{Name: "envoy.filters.http.ext_proc.apikey"},
				{Name: "envoy.filters.http.rbac"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    1,
			expectedFilterCount: 5,
		},
		{
			// Regression guard: with no buffer filter present, insertion behavior is unchanged and AI
			// Gateway lands ahead of the first ext_proc filter (position 0).
			name: "no buffer filter leaves ext_proc insertion unchanged",
			existingFilters: []*httpconnectionmanagerv3.HttpFilter{
				{Name: "envoy.filters.http.ext_proc.apikey"},
				{Name: "envoy.filters.http.rbac"},
				{Name: "envoy.filters.http.router"},
			},
			expectedPosition:    0,
			expectedFilterCount: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &httpconnectionmanagerv3.HttpConnectionManager{
				HttpFilters: make([]*httpconnectionmanagerv3.HttpFilter, len(tt.existingFilters)),
			}
			copy(mgr.HttpFilters, tt.existingFilters)

			newFilter := &httpconnectionmanagerv3.HttpFilter{
				Name:       aiGatewayExtProcName,
				ConfigType: &httpconnectionmanagerv3.HttpFilter_TypedConfig{TypedConfig: &anypb.Any{}},
			}

			err := insertAIGatewayExtProcFilter(mgr, newFilter)
			require.NoError(t, err)

			require.Len(t, mgr.HttpFilters, tt.expectedFilterCount)
			require.Equal(t, aiGatewayExtProcName, mgr.HttpFilters[tt.expectedPosition].Name)

			for i, originalFilter := range tt.existingFilters {
				if i < tt.expectedPosition {
					require.Equal(t, originalFilter.Name, mgr.HttpFilters[i].Name, "filter at position %d should be preserved", i)
				} else {
					require.Equal(t, originalFilter.Name, mgr.HttpFilters[i+1].Name, "filter at position %d should be shifted by 1", i)
				}
			}
		})
	}
}

func TestInsertHeaderToMetadataFilter(t *testing.T) {
	hcm := &httpconnectionmanagerv3.HttpConnectionManager{
		HttpFilters: []*httpconnectionmanagerv3.HttpFilter{{Name: wellknown.Router}},
	}
	filter, err := buildHeaderToMetadataFilter(map[string]string{"agent-session-id": "session.id"})
	require.NoError(t, err)
	err = insertHeaderToMetadataFilter(hcm, filter)
	require.NoError(t, err)
	require.Len(t, hcm.HttpFilters, 2)
	require.Equal(t, headerToMetadataFilterName, hcm.HttpFilters[0].Name)
	require.Equal(t, wellknown.Router, hcm.HttpFilters[1].Name)
}

func TestServer_isRouteGeneratedByAIGateway(t *testing.T) {
	emptyStruct, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	structWithEmptyResources, err := structpb.NewStruct(map[string]any{
		"resources": nil,
	})
	require.NoError(t, err)

	withAnnotationsListStruct, err := structpb.NewStruct(map[string]any{
		"resources": []any{
			map[string]any{
				"annotations": map[string]any{},
			},
		},
	})
	require.NoError(t, err)

	withOKAnnotationsListStruct, err := structpb.NewStruct(map[string]any{
		"resources": []any{
			map[string]any{
				"annotations": map[string]any{
					internalapi.AIGatewayGeneratedHTTPRouteAnnotation: "true",
				},
			},
		},
	})
	require.NoError(t, err)

	for _, tt := range []struct {
		name     string
		route    *routev3.Route
		expected bool
	}{
		{
			name:     "no metadata",
			route:    &routev3.Route{},
			expected: false,
		},
		{
			name: "no metadata.Fields",
			route: &routev3.Route{
				Metadata: &corev3.Metadata{},
			},
			expected: false,
		},
		{
			name: "no metadata.Fields 'envoy-ai_gateway'",
			route: &routev3.Route{
				Metadata: &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{}},
			},
			expected: false,
		},
		{
			name: "no resources in metadata.Fields 'envoy-gateway'",
			route: &routev3.Route{
				Metadata: &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
					"envoy-gateway": emptyStruct,
				}},
			},
			expected: false,
		},
		{
			name: "resources do not have annotations",
			route: &routev3.Route{
				Metadata: &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
					"envoy-gateway": structWithEmptyResources,
				}},
			},
			expected: false,
		},
		{
			name: "annotations are empty",
			route: &routev3.Route{
				Metadata: &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
					"envoy-gateway": withAnnotationsListStruct,
				}},
			},
			expected: false,
		},
		{
			name: "annotations are empty",
			route: &routev3.Route{
				Metadata: &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
					"envoy-gateway": withOKAnnotationsListStruct,
				}},
			},
			expected: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{log: zap.New()}
			result := s.isRouteGeneratedByAIGateway(tt.route)
			require.Equal(t, tt.expected, result)
		})
	}
}

func Test_shouldAIGatewayExtProcBeInserted(t *testing.T) {
	tests := []struct {
		name     string
		filters  []*httpconnectionmanagerv3.HttpFilter
		expected bool
	}{
		{
			filters:  []*httpconnectionmanagerv3.HttpFilter{{}},
			expected: true,
		},
		{
			filters:  []*httpconnectionmanagerv3.HttpFilter{{Name: aiGatewayExtProcName}},
			expected: false,
		},
		{
			filters:  []*httpconnectionmanagerv3.HttpFilter{{}, {Name: aiGatewayExtProcName}, {}},
			expected: false,
		},
		{
			filters:  []*httpconnectionmanagerv3.HttpFilter{{}, {}},
			expected: true,
		},
	}

	for _, tt := range tests {
		result := shouldAIGatewayExtProcBeInserted(tt.filters)
		require.Equal(t, tt.expected, result)
	}
}

func TestServer_insertRouterLevelAIGatewayExtProc_setsSchemeHeaderTransformation(t *testing.T) {
	hcm := &httpconnectionmanagerv3.HttpConnectionManager{
		HttpFilters: []*httpconnectionmanagerv3.HttpFilter{{Name: wellknown.Router}},
	}
	listener := &listenerv3.Listener{
		DefaultFilterChain: &listenerv3.FilterChain{
			Filters: []*listenerv3.Filter{
				{
					Name:       wellknown.HTTPConnectionManager,
					ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustToAny(t, hcm)},
				},
			},
		},
	}
	s := &Server{log: zap.New()}
	require.NoError(t, s.insertRouterLevelAIGatewayExtProc(listener))

	updatedHCM, _, err := findHCM(listener.DefaultFilterChain)
	require.NoError(t, err)
	require.True(t, updatedHCM.GetSchemeHeaderTransformation().GetMatchUpstream(),
		"SchemeHeaderTransformation.MatchUpstream must be true so :scheme matches upstream TLS transport")
}

func Test_findListenerRouteConfigs(t *testing.T) {
	newHCM := func(name string) *httpconnectionmanagerv3.HttpConnectionManager {
		return &httpconnectionmanagerv3.HttpConnectionManager{
			RouteSpecifier: &httpconnectionmanagerv3.HttpConnectionManager_Rds{
				Rds: &httpconnectionmanagerv3.Rds{RouteConfigName: name},
			},
		}
	}
	l := &listenerv3.Listener{
		DefaultFilterChain: &listenerv3.FilterChain{
			Filters: []*listenerv3.Filter{
				{
					Name:       wellknown.HTTPConnectionManager,
					ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustToAny(t, newHCM("foo"))},
				},
			},
		},
		FilterChains: []*listenerv3.FilterChain{
			{
				Filters: []*listenerv3.Filter{
					{
						Name:       wellknown.HTTPConnectionManager,
						ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustToAny(t, newHCM("bar"))},
					},
				},
			},
			// Non-HCM filter chain.
			{Filters: []*listenerv3.Filter{}},
		},
	}
	names := findListenerRouteConfigs(l)
	require.ElementsMatch(t, []string{"foo", "bar"}, names)
}

// extProcFilterConfig returns the upstream ai-gateway ext_proc configuration of the cluster.
func extProcFilterConfig(t *testing.T, cluster *clusterv3.Cluster) *extprocv3.ExternalProcessor {
	t.Helper()
	po := &httpv3.HttpProtocolOptions{}
	require.NoError(t, cluster.TypedExtensionProtocolOptions["envoy.extensions.upstreams.http.v3.HttpProtocolOptions"].UnmarshalTo(po))
	for _, f := range po.HttpFilters {
		if f.Name == aiGatewayExtProcName {
			cfg := &extprocv3.ExternalProcessor{}
			require.NoError(t, f.GetTypedConfig().UnmarshalTo(cfg))
			return cfg
		}
	}
	t.Fatal("upstream ext_proc filter not found")
	return nil
}

// resourceMetadata builds the metadata Envoy Gateway stamps onto a virtual host, naming the
// Gateway API objects it came from.
func resourceMetadata(resources ...map[string]string) *corev3.Metadata {
	values := make([]*structpb.Value, 0, len(resources))
	for _, resource := range resources {
		fields := make(map[string]*structpb.Value, len(resource))
		for k, v := range resource {
			fields[k] = structpb.NewStringValue(v)
		}
		values = append(values, structpb.NewStructValue(&structpb.Struct{Fields: fields}))
	}
	return &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
		"envoy-gateway": {Fields: map[string]*structpb.Value{
			"resources": structpb.NewListValue(&structpb.ListValue{Values: values}),
		}},
	}}
}

// snapshotOf builds the route configurations of a snapshot owned by the named Gateways, all in
// namespace "ns", each on its own virtual host as Envoy Gateway produces them.
func snapshotOf(gatewayNames ...string) []*routev3.RouteConfiguration {
	vhosts := make([]*routev3.VirtualHost, 0, len(gatewayNames))
	for _, name := range gatewayNames {
		vhosts = append(vhosts, &routev3.VirtualHost{
			Name:     name + "/example_com",
			Metadata: resourceMetadata(map[string]string{"kind": "Gateway", "namespace": "ns", "name": name}),
		})
	}
	return []*routev3.RouteConfiguration{{Name: "route-config", VirtualHosts: vhosts}}
}

func newMetadataForwardingServer(t *testing.T) (*Server, client.Client) {
	t.Helper()
	c := newFakeClient()
	newGateway := func(name, configName string) *gwapiv1.Gateway {
		gw := &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Spec:       gwapiv1.GatewaySpec{GatewayClassName: "eg"},
		}
		if configName != "" {
			gw.Annotations = map[string]string{gatewayConfigAnnotationKey: configName}
		}
		return gw
	}
	newGatewayConfig := func(name string, namespaces ...string) *aigv1b1.GatewayConfig {
		return &aigv1b1.GatewayConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Spec: aigv1b1.GatewayConfigSpec{
				ExtProc: &aigv1b1.GatewayConfigExtProc{MetadataForwardingNamespaces: namespaces},
			},
		}
	}
	newRoute := func(name string, parents ...string) *aigv1b1.AIGatewayRoute {
		route := &aigv1b1.AIGatewayRoute{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Spec: aigv1b1.AIGatewayRouteSpec{
				Rules: []aigv1b1.AIGatewayRouteRule{
					{BackendRefs: []aigv1b1.AIGatewayRouteRuleBackendRef{
						{Name: "some-backend"},
						{Name: "plain-backend"},
					}},
				},
			},
		}
		for _, parent := range parents {
			route.Spec.ParentRefs = append(route.Spec.ParentRefs, gwapiv1.ParentReference{Name: gwapiv1.ObjectName(parent)})
		}
		return route
	}
	for _, obj := range []client.Object{
		newGateway("eg-gateway", "gwconfig"),
		newGatewayConfig("gwconfig", "envoy.filters.http.ext_authz"),
		newGateway("other-gateway", "other-gwconfig"),
		newGatewayConfig("other-gwconfig", "other.ns", "envoy.filters.http.ext_authz"),
		newGateway("plain-gateway", ""),
		newRoute("myroute", "eg-gateway"),
		newRoute("plainroute", "plain-gateway"),
		// Attached to a Gateway that declares and one that does not.
		newRoute("sharedroute", "eg-gateway", "plain-gateway"),
	} {
		require.NoError(t, c.Create(t.Context(), obj))
	}
	s, err := New(c, logr.Discard(), udsPath, false, nil, nil, "envoy-ai-gateway-ratelimit.envoy-gateway-system", 5, false, false)
	require.NoError(t, err)
	return s, c
}

// The declaration is the Gateway's, so it comes from the Gateways owning the snapshot.
func Test_metadataForwardingNamespacesForSnapshot(t *testing.T) {
	s, _ := newMetadataForwardingServer(t)

	t.Run("the snapshot's Gateway decides", func(t *testing.T) {
		got, err := s.metadataForwardingNamespacesForSnapshot(t.Context(), snapshotOf("eg-gateway"))
		require.NoError(t, err)
		require.Equal(t, []string{"envoy.filters.http.ext_authz"}, got)
	})

	t.Run("a Gateway with no GatewayConfig declares nothing", func(t *testing.T) {
		got, err := s.metadataForwardingNamespacesForSnapshot(t.Context(), snapshotOf("plain-gateway"))
		require.NoError(t, err)
		require.Empty(t, got)
	})

	// mergeGateways puts several Gateways behind one Envoy, so their declarations combine.
	t.Run("merged Gateways combine, sorted and deduped", func(t *testing.T) {
		got, err := s.metadataForwardingNamespacesForSnapshot(t.Context(), snapshotOf("eg-gateway", "other-gateway"))
		require.NoError(t, err)
		require.Equal(t, []string{"envoy.filters.http.ext_authz", "other.ns"}, got)
	})

	t.Run("non-Gateway resources are ignored", func(t *testing.T) {
		routes := []*routev3.RouteConfiguration{{VirtualHosts: []*routev3.VirtualHost{{
			Metadata: resourceMetadata(
				map[string]string{"kind": "HTTPRoute", "namespace": "ns", "name": "other-gateway"},
				map[string]string{"kind": "Gateway", "namespace": "ns", "name": "eg-gateway"},
			),
		}}}}
		got, err := s.metadataForwardingNamespacesForSnapshot(t.Context(), routes)
		require.NoError(t, err)
		require.Equal(t, []string{"envoy.filters.http.ext_authz"}, got)
	})

	// Nothing identifying the Gateway means nothing is forwarded, logged rather than silent.
	t.Run("an unidentifiable snapshot forwards nothing and says so", func(t *testing.T) {
		var buf bytes.Buffer
		logged, err := New(newFakeClient(), logr.FromSlogHandler(slog.NewTextHandler(&buf, nil)), udsPath,
			false, nil, nil, "envoy-ai-gateway-ratelimit.envoy-gateway-system", 5, false, false)
		require.NoError(t, err)

		got, err := logged.metadataForwardingNamespacesForSnapshot(t.Context(),
			[]*routev3.RouteConfiguration{{VirtualHosts: []*routev3.VirtualHost{{Name: "example_com"}}}})
		require.NoError(t, err)
		require.Empty(t, got)
		require.Contains(t, buf.String(), "cannot tell which Gateway this xDS snapshot belongs to")
	})
}

func Test_maybeModifyCluster_forwardsDeclaredMetadataNamespaces(t *testing.T) {
	s, _ := newMetadataForwardingServer(t)

	modify := func(t *testing.T, clusterName string, gatewayNames ...string) *extprocv3.ExternalProcessor {
		t.Helper()
		namespaces, err := s.metadataForwardingNamespacesForSnapshot(t.Context(), snapshotOf(gatewayNames...))
		require.NoError(t, err)
		cluster := &clusterv3.Cluster{Name: clusterName}
		require.NoError(t, s.maybeModifyCluster(t.Context(), cluster, namespaces, nil))
		return extProcFilterConfig(t, cluster)
	}

	t.Run("declared namespaces reach every cluster of the route", func(t *testing.T) {
		for _, name := range []string{"httproute/ns/myroute/rule/0", "httproute/ns/myroute/rule/0/backend/1"} {
			cfg := modify(t, name, "eg-gateway")
			require.Equal(t, []string{aigv1b1.AIGatewayFilterMetadataNamespace},
				cfg.MetadataOptions.GetReceivingNamespaces().GetUntyped())
			require.Equal(t, []string{"envoy.filters.http.ext_authz"},
				cfg.MetadataOptions.GetForwardingNamespaces().GetUntyped())
		}
	})

	t.Run("no GatewayConfig means no forwarding namespaces", func(t *testing.T) {
		require.Nil(t, modify(t, "httproute/ns/plainroute/rule/0", "plain-gateway").
			MetadataOptions.GetForwardingNamespaces())
	})

	// The same cluster name reaches every parent's snapshot, so a route must not widen a Gateway.
	t.Run("a route parent does not widen another Gateway", func(t *testing.T) {
		require.Equal(t, []string{"envoy.filters.http.ext_authz"},
			modify(t, "httproute/ns/sharedroute/rule/0", "eg-gateway").
				MetadataOptions.GetForwardingNamespaces().GetUntyped())

		require.Nil(t, modify(t, "httproute/ns/sharedroute/rule/0", "plain-gateway").
			MetadataOptions.GetForwardingNamespaces())
	})
}

// A cluster handed back with our filters on it must end up with the current forwarding
// namespaces and no duplicates.
func Test_maybeModifyCluster_rebuildsOwnFiltersOnExistingChain(t *testing.T) {
	c := newFakeClient()
	require.NoError(t, c.Create(t.Context(), &aigv1b1.AIGatewayRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "myroute", Namespace: "ns"},
		Spec: aigv1b1.AIGatewayRouteSpec{
			ParentRefs: []gwapiv1.ParentReference{{Name: "eg-gateway"}},
			Rules: []aigv1b1.AIGatewayRouteRule{
				{BackendRefs: []aigv1b1.AIGatewayRouteRuleBackendRef{{Name: "some-backend"}}},
			},
		},
	}))
	require.NoError(t, c.Create(t.Context(), &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "eg-gateway",
			Namespace:   "ns",
			Annotations: map[string]string{gatewayConfigAnnotationKey: "gwconfig"},
		},
		Spec: gwapiv1.GatewaySpec{GatewayClassName: "eg"},
	}))
	require.NoError(t, c.Create(t.Context(), &aigv1b1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "gwconfig", Namespace: "ns"},
		Spec: aigv1b1.GatewayConfigSpec{
			ExtProc: &aigv1b1.GatewayConfigExtProc{
				MetadataForwardingNamespaces: []string{"envoy.filters.http.ext_authz"},
			},
		},
	}))
	s, err := New(c, logr.Discard(), udsPath, false, nil, nil, "envoy-ai-gateway-ratelimit.envoy-gateway-system", 5, false, false)
	require.NoError(t, err)

	// A chain as an earlier pass left it: our two filters, no forwarding namespaces.
	stale, err := toAny(&extprocv3.ExternalProcessor{
		MetadataOptions: &extprocv3.MetadataOptions{
			ReceivingNamespaces: &extprocv3.MetadataOptions_MetadataNamespaces{
				Untyped: []string{aigv1b1.AIGatewayFilterMetadataNamespace},
			},
		},
	})
	require.NoError(t, err)
	poAny, err := toAny(&httpv3.HttpProtocolOptions{
		HttpFilters: []*httpconnectionmanagerv3.HttpFilter{
			{Name: aiGatewayExtProcName, ConfigType: &httpconnectionmanagerv3.HttpFilter_TypedConfig{TypedConfig: stale}},
			{Name: aiGatewayHeaderMutationName},
			{Name: "envoy.filters.http.upstream_codec"},
		},
	})
	require.NoError(t, err)
	cluster := &clusterv3.Cluster{
		Name: "httproute/ns/myroute/rule/0",
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": poAny,
		},
	}

	require.NoError(t, s.maybeModifyCluster(t.Context(), cluster, []string{"envoy.filters.http.ext_authz"}, nil))

	cfg := extProcFilterConfig(t, cluster)
	require.Equal(t, []string{"envoy.filters.http.ext_authz"},
		cfg.MetadataOptions.GetForwardingNamespaces().GetUntyped())
	require.Equal(t, []string{aigv1b1.AIGatewayFilterMetadataNamespace},
		cfg.MetadataOptions.GetReceivingNamespaces().GetUntyped())

	po := &httpv3.HttpProtocolOptions{}
	require.NoError(t, cluster.TypedExtensionProtocolOptions["envoy.extensions.upstreams.http.v3.HttpProtocolOptions"].UnmarshalTo(po))
	var names []string
	for _, f := range po.HttpFilters {
		names = append(names, f.GetName())
	}
	require.Equal(t, []string{aiGatewayExtProcName, aiGatewayHeaderMutationName, "envoy.filters.http.upstream_codec"}, names)
}

func Test_maybeModifyCluster_handlesMirrorClusters(t *testing.T) {
	// Envoy Gateway names mirror backend clusters "httproute/<ns>/<name>/rule/<ruleIdx>-mirror-<mirrorIdx>".
	// The extension server must apply the same upstream ExtProc + header-mutation filters
	// to mirror clusters as it does to primary clusters so shadow traffic honors
	// per-backend ModelNameOverride / HeaderMutation / BodyMutation.
	c := newFakeClient()
	require.NoError(t, c.Create(t.Context(), &aigv1b1.AIGatewayRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "myroute", Namespace: "ns"},
		Spec: aigv1b1.AIGatewayRouteSpec{
			Rules: []aigv1b1.AIGatewayRouteRule{
				{
					BackendRefs: []aigv1b1.AIGatewayRouteRuleBackendRef{
						{Name: "primary"},
					},
					Mirrors: []aigv1b1.AIGatewayRouteRuleMirror{
						{
							BackendRef: aigv1b1.AIGatewayRouteRuleBackendRef{
								Name:              "shadow",
								ModelNameOverride: "shadow-model",
							},
						},
					},
				},
			},
		},
	}))

	s, err := New(c, logr.Discard(), udsPath, false, nil, nil, "envoy-ai-gateway-ratelimit.envoy-gateway-system", 5, false, false)
	require.NoError(t, err)

	// Envoy Gateway names mirror clusters with 1-based indexing — the first
	// mirror of rule 0 is "0-mirror-1". The extension server must convert that
	// back to 0-based for slice access into httpRouteRule.Mirrors.
	mirrorCluster := &clusterv3.Cluster{
		Name: "httproute/ns/myroute/rule/0-mirror-1",
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			Endpoints: []*endpointv3.LocalityLbEndpoints{
				{LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Hostname: "shadow.example.com"}},
				}}},
			},
		},
	}
	err = s.maybeModifyCluster(t.Context(), mirrorCluster, nil, nil)
	require.NoError(t, err)

	// Cluster metadata must contain the mirror backend name and the mirror flag
	// (used downstream to suppress LLMRequestCost double-emission).
	require.NotNil(t, mirrorCluster.Metadata)
	internalMD := mirrorCluster.Metadata.FilterMetadata[internalapi.InternalEndpointMetadataNamespace]
	require.NotNil(t, internalMD)
	require.Equal(t,
		internalapi.PerRouteRuleMirrorBackendName("ns", "shadow", "myroute", 0, 0),
		internalMD.Fields[internalapi.InternalMetadataBackendNameKey].GetStringValue())
	require.True(t, internalMD.Fields[internalapi.InternalMetadataMirrorKey].GetBoolValue())

	// Endpoint metadata must mirror the cluster-level metadata.
	epMD := mirrorCluster.LoadAssignment.Endpoints[0].LbEndpoints[0].Metadata
	require.NotNil(t, epMD)
	epInternal := epMD.FilterMetadata[internalapi.InternalEndpointMetadataNamespace]
	require.Equal(t,
		internalapi.PerRouteRuleMirrorBackendName("ns", "shadow", "myroute", 0, 0),
		epInternal.Fields[internalapi.InternalMetadataBackendNameKey].GetStringValue())
	require.True(t, epInternal.Fields[internalapi.InternalMetadataMirrorKey].GetBoolValue())
	// Like primary endpoints, the mirror endpoint carries its upstream host for backend auth (e.g. SigV4).
	require.Equal(t, "shadow.example.com", epInternal.Fields[internalapi.InternalMetadataUpstreamHostKey].GetStringValue())

	// TypedExtensionProtocolOptions must include the upstream ExtProc filter and
	// the header-mutation filter — same chain we install on primary clusters.
	raw, ok := mirrorCluster.TypedExtensionProtocolOptions["envoy.extensions.upstreams.http.v3.HttpProtocolOptions"]
	require.True(t, ok, "mirror cluster must carry HttpProtocolOptions with ExtProc filter")
	var po httpv3.HttpProtocolOptions
	require.NoError(t, raw.UnmarshalTo(&po))
	filterNames := make([]string, 0, len(po.HttpFilters))
	for _, f := range po.HttpFilters {
		filterNames = append(filterNames, f.Name)
	}
	require.Contains(t, filterNames, aiGatewayExtProcName)
	require.Contains(t, filterNames, "envoy.filters.http.header_mutation")
}

func Test_maybeModifyCluster_mirrorIndexIsOneBased(t *testing.T) {
	// Envoy Gateway emits mirror cluster names with 1-based mirror indices
	// (the first mirror of rule 0 is "0-mirror-1", the second is "0-mirror-2").
	// The extension server must convert that suffix back to 0-based for slice
	// access into httpRouteRule.Mirrors. Without the conversion, every single-
	// mirror route silently skips ExtProc injection on the mirror cluster
	// because mirrors[1] is out of range against a length-1 slice — observed
	// in the local-dev shadow-traffic e2e against commit 0975e451 before the
	// fix landed.
	c := newFakeClient()
	require.NoError(t, c.Create(t.Context(), &aigv1b1.AIGatewayRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "myroute", Namespace: "ns"},
		Spec: aigv1b1.AIGatewayRouteSpec{
			Rules: []aigv1b1.AIGatewayRouteRule{
				{
					BackendRefs: []aigv1b1.AIGatewayRouteRuleBackendRef{
						{Name: "primary"},
					},
					Mirrors: []aigv1b1.AIGatewayRouteRuleMirror{
						{BackendRef: aigv1b1.AIGatewayRouteRuleBackendRef{Name: "shadow-a"}},
						{BackendRef: aigv1b1.AIGatewayRouteRuleBackendRef{Name: "shadow-b"}},
					},
				},
			},
		},
	}))

	s, err := New(c, logr.Discard(), udsPath, false, nil, nil, "envoy-ai-gateway-ratelimit.envoy-gateway-system", 5, false, false)
	require.NoError(t, err)

	cases := []struct {
		clusterName        string
		expectInstalled    bool
		expectBackend      string
		expectMirrorIndex0 int // 0-based mirror index after the conversion
	}{
		// "0-mirror-1" → mirrors[0] = shadow-a, ExtProc inserted.
		{"httproute/ns/myroute/rule/0-mirror-1", true, "shadow-a", 0},
		// "0-mirror-2" → mirrors[1] = shadow-b, ExtProc inserted.
		{"httproute/ns/myroute/rule/0-mirror-2", true, "shadow-b", 1},
		// "0-mirror-0" is invalid (mirror indices start at 1) — bail without error.
		{"httproute/ns/myroute/rule/0-mirror-0", false, "", 0},
		// "0-mirror-3" exceeds the slice — bail without error.
		{"httproute/ns/myroute/rule/0-mirror-3", false, "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.clusterName, func(t *testing.T) {
			cluster := &clusterv3.Cluster{
				Name: tc.clusterName,
				LoadAssignment: &endpointv3.ClusterLoadAssignment{
					Endpoints: []*endpointv3.LocalityLbEndpoints{
						{LbEndpoints: []*endpointv3.LbEndpoint{{}}},
					},
				},
			}
			require.NoError(t, s.maybeModifyCluster(t.Context(), cluster, nil, nil))

			_, installed := cluster.TypedExtensionProtocolOptions["envoy.extensions.upstreams.http.v3.HttpProtocolOptions"]
			require.Equal(t, tc.expectInstalled, installed,
				"ExtProc filter chain installation should match expectation for %q", tc.clusterName)

			if tc.expectInstalled {
				internalMD := cluster.Metadata.FilterMetadata[internalapi.InternalEndpointMetadataNamespace]
				require.NotNil(t, internalMD)
				require.Equal(t,
					internalapi.PerRouteRuleMirrorBackendName("ns", tc.expectBackend, "myroute", 0, tc.expectMirrorIndex0),
					internalMD.Fields[internalapi.InternalMetadataBackendNameKey].GetStringValue(),
					"cluster metadata must reference the correct 0-based mirror entry for %q", tc.clusterName)
			}
		})
	}
}

func Test_maybeModifyCluster_rejectsMalformedMirrorClusterName(t *testing.T) {
	// Malformed mirror suffixes (non-numeric indices) should log and bail without
	// returning an error so a bad cluster name doesn't tear down the whole xDS push.
	c := newFakeClient()
	s, err := New(c, logr.Discard(), udsPath, false, nil, nil, "envoy-ai-gateway-ratelimit.envoy-gateway-system", 5, false, false)
	require.NoError(t, err)
	for _, name := range []string{
		"httproute/ns/myroute/rule/abc-mirror-0",
		"httproute/ns/myroute/rule/0-mirror-xyz",
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, s.maybeModifyCluster(t.Context(), &clusterv3.Cluster{Name: name}, nil, nil))
		})
	}
}

// Test_maybeModifyCluster_inferencePoolMirror verifies that a mirror cluster whose
// AIGatewayRoute mirror leg targets an InferencePool is rewritten to ORIGINAL_DST keyed on the
// MIRROR endpoint-picker header, carries the mirror + pool metadata, drops the placeholder
// Service endpoints, and reports the (rule cluster → pool) pair to the collector.
func Test_maybeModifyCluster_inferencePoolMirror(t *testing.T) {
	c := newFakeClient()
	require.NoError(t, c.Create(t.Context(), &aigv1b1.AIGatewayRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "myroute", Namespace: "ns"},
		Spec: aigv1b1.AIGatewayRouteSpec{
			Rules: []aigv1b1.AIGatewayRouteRule{
				{
					BackendRefs: []aigv1b1.AIGatewayRouteRuleBackendRef{{Name: "primary"}},
					Mirrors: []aigv1b1.AIGatewayRouteRuleMirror{
						{
							BackendRef: aigv1b1.AIGatewayRouteRuleBackendRef{
								Name:  "mirror-pool",
								Group: ptr.To("inference.networking.k8s.io"),
								Kind:  ptr.To("InferencePool"),
							},
						},
					},
				},
			},
		},
	}))
	require.NoError(t, c.Create(t.Context(), &gwaiev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mirror-pool", Namespace: "ns",
			Annotations: map[string]string{"aigateway.envoyproxy.io/processing-body-mode": "buffered"},
		},
		Spec: gwaiev1.InferencePoolSpec{
			EndpointPickerRef: &gwaiev1.EndpointPickerRef{
				Name: "mirror-pool-epp",
				Port: ptr.To(gwaiev1.Port{Number: 9002}),
			},
		},
	}))

	s, err := New(c, logr.Discard(), udsPath, false, nil, nil, "envoy-ai-gateway-ratelimit.envoy-gateway-system", 5, false, false)
	require.NoError(t, err)

	mirrorCluster := &clusterv3.Cluster{
		Name: "httproute/ns/myroute/rule/0-mirror-1",
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			Endpoints: []*endpointv3.LocalityLbEndpoints{
				{LbEndpoints: []*endpointv3.LbEndpoint{{}}},
			},
		},
	}
	collected := map[string]*gwaiev1.InferencePool{}
	require.NoError(t, s.maybeModifyCluster(t.Context(), mirrorCluster, nil, collected))

	// ORIGINAL_DST keyed on the mirror endpoint-picker header, placeholder endpoints dropped.
	require.Equal(t, clusterv3.Cluster_ORIGINAL_DST, mirrorCluster.GetType())
	require.Equal(t, clusterv3.Cluster_CLUSTER_PROVIDED, mirrorCluster.LbPolicy)
	lb := mirrorCluster.GetOriginalDstLbConfig()
	require.NotNil(t, lb)
	require.True(t, lb.UseHttpHeader)
	require.Equal(t, internalapi.MirrorEndpointPickerHeaderKey, lb.HttpHeaderName)
	require.Nil(t, mirrorCluster.LoadAssignment)

	// Mirror attribution + pool metadata are both present (the pool metadata is what makes
	// buildClustersForInferencePoolEndpointPickers emit the mirror pool's EPP cluster).
	internalMD := mirrorCluster.Metadata.FilterMetadata[internalapi.InternalEndpointMetadataNamespace]
	require.NotNil(t, internalMD)
	require.Equal(t,
		internalapi.PerRouteRuleMirrorBackendName("ns", "mirror-pool", "myroute", 0, 0),
		internalMD.Fields[internalapi.InternalMetadataBackendNameKey].GetStringValue())
	require.True(t, internalMD.Fields[internalapi.InternalMetadataMirrorKey].GetBoolValue())
	pool := getInferencePoolByMetadata(mirrorCluster.Metadata)
	require.NotNil(t, pool)
	require.Equal(t, "mirror-pool", pool.Name)
	require.Equal(t, "buffered", pool.Annotations["aigateway.envoyproxy.io/processing-body-mode"])

	// The collector maps the rule cluster to the pool for listener/vhost patching.
	require.Len(t, collected, 1)
	require.Equal(t, "mirror-pool", collected["httproute/ns/myroute/rule/0"].Name)

	// The EPP cluster is emitted for the mirror pool, exactly once even when two clusters
	// reference the same pool.
	eppClusters, err := buildClustersForInferencePoolEndpointPickers(
		[]*clusterv3.Cluster{mirrorCluster, mirrorCluster})
	require.NoError(t, err)
	require.Len(t, eppClusters, 1)
	require.Equal(t, clusterNameForInferencePool(pool), eppClusters[0].Name)

	// A mirror pool without an endpoint picker leaves the cluster as Envoy Gateway generated it
	// and is not collected for listener patching.
	var noEPPPool gwaiev1.InferencePool
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: "ns", Name: "mirror-pool"}, &noEPPPool))
	noEPPPool.Spec.EndpointPickerRef = nil
	require.NoError(t, c.Update(t.Context(), &noEPPPool))
	noEPPCluster := &clusterv3.Cluster{
		Name: "httproute/ns/myroute/rule/0-mirror-1",
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			Endpoints: []*endpointv3.LocalityLbEndpoints{
				{LbEndpoints: []*endpointv3.LbEndpoint{{}}},
			},
		},
	}
	collected = map[string]*gwaiev1.InferencePool{}
	require.NoError(t, s.maybeModifyCluster(t.Context(), noEPPCluster, nil, collected))
	require.NotEqual(t, clusterv3.Cluster_ORIGINAL_DST, noEPPCluster.GetType())
	require.NotNil(t, noEPPCluster.LoadAssignment)
	require.Nil(t, getInferencePoolByMetadata(noEPPCluster.Metadata))
	require.Empty(t, collected)
}

// Test_patchListenerAndVirtualHost_mirrorPool verifies the downstream wiring for a mirror pool:
// the listener chain gains [mirror-EPP, mirror endpoint copy filter] BEFORE the primary pool's
// EPP filter (all before the router), and per-route config enables the mirror filters only on
// the mirror rule's route.
func Test_patchListenerAndVirtualHost_mirrorPool(t *testing.T) {
	s, err := New(newFakeClient(), logr.Discard(), udsPath, false, nil, nil, "envoy-ai-gateway-ratelimit.envoy-gateway-system", 5, false, false)
	require.NoError(t, err)

	primaryPool := &gwaiev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "primary-pool", Namespace: "ns"},
		Spec: gwaiev1.InferencePoolSpec{
			EndpointPickerRef: &gwaiev1.EndpointPickerRef{Name: "primary-epp", Port: ptr.To(gwaiev1.Port{Number: 9002})},
		},
	}
	mirrorPool := &gwaiev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "mirror-pool", Namespace: "ns"},
		Spec: gwaiev1.InferencePoolSpec{
			EndpointPickerRef: &gwaiev1.EndpointPickerRef{Name: "mirror-epp", Port: ptr.To(gwaiev1.Port{Number: 9002})},
		},
	}

	routerFilter := &httpconnectionmanagerv3.HttpFilter{Name: "envoy.filters.http.router"}
	hcmIn := &httpconnectionmanagerv3.HttpConnectionManager{
		HttpFilters: []*httpconnectionmanagerv3.HttpFilter{routerFilter},
	}
	hcmAny, err := toAny(hcmIn)
	require.NoError(t, err)
	listener := &listenerv3.Listener{
		Name: "test-listener",
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: hcmAny},
			}},
		}},
	}
	s.patchListenerWithInferencePoolFilters(listener,
		[]*gwaiev1.InferencePool{primaryPool}, []*gwaiev1.InferencePool{mirrorPool})

	hcm, _, err := findHCM(listener.FilterChains[0])
	require.NoError(t, err)
	names := make([]string, 0, len(hcm.HttpFilters))
	for _, f := range hcm.HttpFilters {
		names = append(names, f.Name)
	}
	mirrorEPPName := httpFilterNameForInferencePool(mirrorPool)
	primaryEPPName := httpFilterNameForInferencePool(primaryPool)
	idx := func(n string) int {
		for i, v := range names {
			if v == n {
				return i
			}
		}
		return -1
	}
	require.GreaterOrEqual(t, idx(mirrorEPPName), 0, "mirror EPP filter missing: %v", names)
	require.GreaterOrEqual(t, idx(mirrorEndpointCopyFilterName), 0, "copy filter missing: %v", names)
	require.GreaterOrEqual(t, idx(primaryEPPName), 0, "primary EPP filter missing: %v", names)
	require.Less(t, idx(mirrorEPPName), idx(mirrorEndpointCopyFilterName), "mirror EPP must precede the copy filter")
	require.Less(t, idx(mirrorEndpointCopyFilterName), idx(primaryEPPName), "copy filter must precede the primary EPP")
	require.Equal(t, "envoy.filters.http.router", names[len(names)-1])

	// Failure semantics: the mirror EPP filter is fail-open (its failure drops the shadow
	// clone, never the primary request), the primary EPP filter fail-closed.
	extProcOf := func(i int) *extprocv3.ExternalProcessor {
		ep := &extprocv3.ExternalProcessor{}
		require.NoError(t, hcm.HttpFilters[i].GetTypedConfig().UnmarshalTo(ep))
		return ep
	}
	mirrorEP := extProcOf(idx(mirrorEPPName))
	require.True(t, mirrorEP.FailureModeAllow, "mirror EPP must be fail-open")
	require.True(t, mirrorEP.DisableImmediateResponse, "mirror EPP ImmediateResponse must not reach the caller")
	primaryEP := extProcOf(idx(primaryEPPName))
	require.False(t, primaryEP.FailureModeAllow, "primary EPP must stay fail-closed")
	require.False(t, primaryEP.DisableImmediateResponse)

	// Virtual host per-route config: the mirror rule's route keeps the mirror filters enabled;
	// a plain route gets everything disabled including the copy filter.
	mirrorRuleCluster := "httproute/ns/myroute/rule/0"
	vh := &routev3.VirtualHost{
		Routes: []*routev3.Route{
			{
				Name: "mirror-rule-route",
				Action: &routev3.Route_Route{Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: mirrorRuleCluster},
				}},
			},
			{
				Name: "plain-route",
				Action: &routev3.Route_Route{Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: "httproute/ns/other/rule/0"},
				}},
			},
		},
	}
	mirrorMap := map[string]*gwaiev1.InferencePool{mirrorRuleCluster: mirrorPool}
	require.NoError(t, s.patchVirtualHostWithInferencePool(vh,
		[]*gwaiev1.InferencePool{primaryPool}, []*gwaiev1.InferencePool{mirrorPool}, mirrorMap))

	mirrorRoute := vh.Routes[0]
	plainRoute := vh.Routes[1]
	// Mirror route: mirror EPP enabled (no disable override), primary EPP disabled (route
	// metadata carries no primary pool in this synthetic setup).
	require.NotContains(t, mirrorRoute.TypedPerFilterConfig, mirrorEPPName)
	require.Contains(t, mirrorRoute.TypedPerFilterConfig, primaryEPPName)
	// Plain route: every foreign EPP filter disabled.
	require.Contains(t, plainRoute.TypedPerFilterConfig, mirrorEPPName)
	require.Contains(t, plainRoute.TypedPerFilterConfig, primaryEPPName)
	// The copy filter is never per-route disabled — a generic FilterConfig disable is evaluated
	// against the INITIAL route match, which on this listener precedes the aigateway extproc's
	// model-header injection, so it would strip the filter from every real request stream.
	require.NotContains(t, mirrorRoute.TypedPerFilterConfig, mirrorEndpointCopyFilterName)
	require.NotContains(t, plainRoute.TypedPerFilterConfig, mirrorEndpointCopyFilterName)
}

// Test_patchListenerWithInferencePoolFilters_mirrorPoolWithoutEPP: a mirror pool without an
// endpoint picker gets neither an EPP filter nor the copy filter.
func Test_patchListenerWithInferencePoolFilters_mirrorPoolWithoutEPP(t *testing.T) {
	s, err := New(newFakeClient(), logr.Discard(), udsPath, false, nil, nil, "envoy-ai-gateway-ratelimit.envoy-gateway-system", 5, false, false)
	require.NoError(t, err)

	hcmAny, err := toAny(&httpconnectionmanagerv3.HttpConnectionManager{
		HttpFilters: []*httpconnectionmanagerv3.HttpFilter{{Name: "envoy.filters.http.router"}},
	})
	require.NoError(t, err)
	listener := &listenerv3.Listener{
		Name: "test-listener",
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: hcmAny},
			}},
		}},
	}
	s.patchListenerWithInferencePoolFilters(listener, nil, []*gwaiev1.InferencePool{
		{ObjectMeta: metav1.ObjectMeta{Name: "mirror-pool", Namespace: "ns"}},
	})

	hcm, _, err := findHCM(listener.FilterChains[0])
	require.NoError(t, err)
	require.Len(t, hcm.HttpFilters, 1)
	require.Equal(t, "envoy.filters.http.router", hcm.HttpFilters[0].Name)
}

// Test_buildMirrorEndpointCopyFilter_copiesWithoutRemovingSource: the copy filter strips a
// spoofed mirror header first, then copies the standard endpoint-picker header WITHOUT removing
// it — the standard header must survive for the mirror's deployment-id-pinned rule, whose
// cluster keys ORIGINAL_DST on it with no later EPP to re-set it.
func Test_buildMirrorEndpointCopyFilter_copiesWithoutRemovingSource(t *testing.T) {
	f, err := buildMirrorEndpointCopyFilter()
	require.NoError(t, err)
	hm := &header_mutationv3.HeaderMutation{}
	require.NoError(t, f.GetTypedConfig().UnmarshalTo(hm))
	muts := hm.GetMutations().GetRequestMutations()
	require.Len(t, muts, 2)
	require.Equal(t, internalapi.MirrorEndpointPickerHeaderKey, muts[0].GetRemove(),
		"first mutation must strip a client-supplied mirror header")
	appendMut := muts[1].GetAppend()
	require.NotNil(t, appendMut)
	require.Equal(t, internalapi.MirrorEndpointPickerHeaderKey, appendMut.GetHeader().GetKey())
	for _, m := range muts {
		require.NotEqual(t, internalapi.EndpointPickerHeaderKey, m.GetRemove(),
			"the standard endpoint-picker header must never be removed")
	}
}

// Test_patchListenerWithInferencePoolFilters_sharedPool: a pool referenced both as a mirror leg
// and as a primary backendRef (the mirror's deployment-id-pinned probe rule) yields exactly one
// EPP filter — the fail-open mirror variant. The filter name is keyed by pool identity, so
// without cross-list dedup the chain would carry two same-named filters.
func Test_patchListenerWithInferencePoolFilters_sharedPool(t *testing.T) {
	s, err := New(newFakeClient(), logr.Discard(), udsPath, false, nil, nil, "envoy-ai-gateway-ratelimit.envoy-gateway-system", 5, false, false)
	require.NoError(t, err)

	sharedPool := &gwaiev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-pool", Namespace: "ns"},
		Spec: gwaiev1.InferencePoolSpec{
			EndpointPickerRef: &gwaiev1.EndpointPickerRef{Name: "shared-epp", Port: ptr.To(gwaiev1.Port{Number: 9002})},
		},
	}

	routerFilter := &httpconnectionmanagerv3.HttpFilter{Name: "envoy.filters.http.router"}
	hcmIn := &httpconnectionmanagerv3.HttpConnectionManager{
		HttpFilters: []*httpconnectionmanagerv3.HttpFilter{routerFilter},
	}
	hcmAny, err := toAny(hcmIn)
	require.NoError(t, err)
	listener := &listenerv3.Listener{
		Name: "test-listener",
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: hcmAny},
			}},
		}},
	}
	s.patchListenerWithInferencePoolFilters(listener,
		[]*gwaiev1.InferencePool{sharedPool}, []*gwaiev1.InferencePool{sharedPool})

	hcm, _, err := findHCM(listener.FilterChains[0])
	require.NoError(t, err)
	sharedName := httpFilterNameForInferencePool(sharedPool)
	var matches []*httpconnectionmanagerv3.HttpFilter
	for _, f := range hcm.HttpFilters {
		if f.Name == sharedName {
			matches = append(matches, f)
		}
	}
	require.Len(t, matches, 1, "shared pool must yield exactly one EPP filter, got names: %v",
		func() []string {
			names := make([]string, 0, len(hcm.HttpFilters))
			for _, f := range hcm.HttpFilters {
				names = append(names, f.Name)
			}
			return names
		}())
	ep := &extprocv3.ExternalProcessor{}
	require.NoError(t, matches[0].GetTypedConfig().UnmarshalTo(ep))
	require.True(t, ep.FailureModeAllow, "shared pool filter must be the fail-open mirror variant")
	require.True(t, ep.DisableImmediateResponse)
}
