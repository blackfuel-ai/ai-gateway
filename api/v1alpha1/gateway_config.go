// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package v1alpha1

import (
	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GatewayConfig provides configuration for the AI Gateway external processor
// container that is deployed alongside the Gateway.
//
// A GatewayConfig is referenced by a Gateway via the annotation
// "aigateway.envoyproxy.io/gateway-config". The GatewayConfig must be in the
// same namespace as the Gateway that references it.
//
// This allows gateway-level configuration of the external processor, including
// environment variables (e.g., for tracing configuration) and resource requirements.
//
// Multiple Gateways can reference the same GatewayConfig to share configuration.
//
// Environment Variable Precedence:
// When merging environment variables, the following precedence applies (highest to lowest):
//  1. GatewayConfig.Spec.ExtProc.Kubernetes.Env (this resource)
//  2. Global controller flags (extProcExtraEnvVars)
//
// If the same environment variable name exists in both sources, the GatewayConfig
// value takes precedence.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=gwconfig
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[-1:].type`
// +kubebuilder:deprecatedversion:warning="aigateway.envoyproxy.io/v1alpha1 is deprecated; use aigateway.envoyproxy.io/v1beta1 instead"
type GatewayConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec defines the configuration for the external processor.
	Spec GatewayConfigSpec `json:"spec,omitempty"`
	// Status defines the status of the GatewayConfig.
	Status GatewayConfigStatus `json:"status,omitempty"`
}

// GatewayConfigList contains a list of GatewayConfig.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type GatewayConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayConfig `json:"items"`
}

// GatewayConfigSpec defines the configuration for the AI Gateway.
type GatewayConfigSpec struct {
	// ExtProc defines the configuration for the external processor container.
	//
	// +optional
	ExtProc *GatewayConfigExtProc `json:"extProc,omitempty"`

	// GlobalLLMRequestCosts defines default LLM request costs that apply to all
	// routes referencing this GatewayConfig. These costs can be overridden on a
	// per-route basis via AIGatewayRoute.Spec.LLMRequestCosts.
	//
	// When a request matches a route, the cost calculation proceeds as follows:
	//  1. If the route defines LLMRequestCosts with a matching metadataKey, use that.
	//  2. Otherwise, fall back to the global cost with that metadataKey (if defined here).
	//  3. If neither exists, the cost is not calculated for that metadataKey.
	//
	// This allows you to define common cost formulas once at the gateway level
	// (e.g., billing_charges = input_tokens + output_tokens) and only override
	// them in specific routes when needed (e.g., premium routes with different pricing).
	//
	// +optional
	// +listType=map
	// +listMapKey=metadataKey
	GlobalLLMRequestCosts []LLMRequestCost `json:"globalLLMRequestCosts,omitempty"`

	// ForwardProxy routes all upstream AI/LLM traffic from Gateways referencing this
	// GatewayConfig through an HTTP CONNECT forward proxy. This is intended for data planes
	// in locked-down networks where direct egress to providers is not permitted and all
	// outbound traffic must traverse a proxy.
	//
	// +optional
	ForwardProxy *GatewayConfigForwardProxy `json:"forwardProxy,omitempty"`
	// EmitErrorMetadata enables emission of Envoy dynamic metadata describing upstream
	// LLM error responses (non-2xx). When enabled, the AI Gateway filter sets
	// "llm_error_type" and "llm_error_code" (falling back to the HTTP status code when the
	// provider reports no code), along with "backend_name", "route_name", and
	// "model_name_override", under the "io.envoy.ai_gateway" metadata namespace.
	// These can be referenced in access logs via
	// %DYNAMIC_METADATA(io.envoy.ai_gateway:llm_error_type)%.
	//
	// This applies only to errors returned by the upstream LLM provider. Errors generated
	// before backend selection (e.g. an unknown model) do not carry this metadata.
	//
	// Defaults to false.
	//
	// +optional
	EmitErrorMetadata bool `json:"emitErrorMetadata,omitempty"`

	// UsageEstimates estimates the token usage of each request when it is admitted,
	// from the responses completed during the last completed period (see
	// UsageEstimatePeriod) for requests carrying the same value of a request header
	// (for example the API key identity stamped by an external authorization service)
	// and the same model, and emits each estimate as Envoy dynamic metadata under the
	// "io.envoy.ai_gateway" namespace.
	//
	// Estimates change no routing, cost or quota decision. They can be referenced in
	// access logs, for example %DYNAMIC_METADATA(io.envoy.ai_gateway:estimated_input_token)%.
	// To learn from streamed responses, a streaming OpenAI-compatible request gets
	// stream_options.include_usage set, as with LLMRequestCosts, so its client receives
	// the final usage chunk.
	//
	// A metadataKey must not equal an LLMRequestCost metadataKey, global, per route
	// or added by a QuotaPolicy. On such a collision the controller stops updating
	// the gateway's filter configuration, which keeps serving the last valid one,
	// until the collision is removed.
	//
	// +optional
	// +listType=map
	// +listMapKey=metadataKey
	// +kubebuilder:validation:MaxItems=16
	UsageEstimates []UsageEstimate `json:"usageEstimates,omitempty"`

	// UsageEstimatePeriod is the length of the periods UsageEstimates accumulates
	// completed requests over. Periods are aligned on the clock, and a request is
	// estimated from the last completed one. A longer period gathers more responses
	// per estimate and leaves fewer clients without one, but follows a change in a
	// client's requests more slowly. Changing it starts every estimate over.
	//
	// Defaults to 60s.
	//
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('5s') && duration(self) <= duration('10m')",message="usageEstimatePeriod must be between 5s and 10m"
	UsageEstimatePeriod *gwapiv1.Duration `json:"usageEstimatePeriod,omitempty"`
}

// UsageEstimate estimates one value of a request's token usage when the request
// is admitted, before any upstream has answered.
//
// Completed requests are accumulated per ByHeader value and model over fixed
// periods of GatewayConfigSpec.UsageEstimatePeriod, aligned on the clock. A
// request is estimated from the successful responses of the last completed
// period:
//
//   - input tokens: the request body size times the input tokens per body byte
//     observed in those responses;
//   - cached input tokens: those input tokens times the cache rate of those
//     responses, the mean of the share of each response's input tokens that
//     were cached.
//
// The CEL expression is evaluated on that estimated usage and on the measured
// ratios of that period, and its result is stored under MetadataKey. Nothing is
// emitted when the last completed period holds no successful response.
//
// Each gateway replica estimates from the responses it served itself.
type UsageEstimate struct {
	// MetadataKey is the key of the dynamic metadata storing the estimate.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	MetadataKey string `json:"metadataKey"`
	// CEL is the CEL expression computing the estimate. It accepts the variables
	// of LLMRequestCost.CEL and the measured ratios, and must return an int, a uint
	// or a double that is finite and not negative. It is evaluated on the
	// estimated usage of the request:
	//
	//	* model: the model name extracted from the request content.
	//	* input_tokens: the estimated number of input tokens.
	//	* cached_input_tokens: the estimated number of cached read input tokens,
	//	  input_tokens times cache_rate.
	//	* total_tokens: equal to input_tokens.
	//	* output_tokens, reasoning_tokens and cache_creation_input_tokens: 0.
	//	* backend and route_name: empty, as no route is selected at admission.
	//	* input_tokens_per_byte: the input tokens of the successful responses of the
	//	  period divided by the size of their request bodies, a double.
	//	* cache_rate: the mean, over the successful responses of the period, of the
	//	  share of each response's input tokens that were cached, a double between
	//	  0 and 1.
	//
	// For example, "input_tokens > cached_input_tokens ? input_tokens - cached_input_tokens : uint(0)"
	// estimates the input tokens that are not served from the prompt cache, and
	// "cache_rate" emits the measured cache rate. CEL does not convert between
	// integers and doubles implicitly: write "double(input_tokens) * cache_rate".
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	CEL string `json:"cel"`
	// ByHeader names the request header whose value groups the responses the
	// estimate is drawn from. Requests without the header get no estimate.
	//
	// The header should be set by the gateway, for example by an external
	// authorization service, rather than by clients. Each distinct value with a
	// successful response is kept in memory while it has successful responses,
	// and for one to two periods, plus up to 30 seconds, after its last one.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ByHeader string `json:"byHeader"`
	// EmitMetric also records the estimate in metrics: a counter of the requests
	// that got an estimate or none, and a histogram of the estimate divided by
	// the same CEL expression evaluated on the actual usage of the response. The
	// actual value keeps the model, backend and route_name of the estimate, so the
	// ratio measures the usage estimation alone. The header value is never a
	// metric attribute.
	//
	// Defaults to false.
	//
	// +optional
	EmitMetric bool `json:"emitMetric,omitempty"`
}

// GatewayConfigExtProc holds runtime-specific configuration for the external processor.
type GatewayConfigExtProc struct {
	// Kubernetes defines the configuration for running the external processor as a Kubernetes container.
	//
	// +optional
	Kubernetes *egv1a1.KubernetesContainerSpec `json:"kubernetes,omitempty"`
}

// GatewayConfigForwardProxy configures an HTTP CONNECT forward proxy for upstream egress.
type GatewayConfigForwardProxy struct {
	// Address is the "host:port" of the HTTP CONNECT proxy. The host may be a hostname or an
	// IP address; the port is required. Upstream connections are tunnelled through this proxy
	// via Envoy's http_11_proxy transport socket, preserving the upstream TLS session.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`
}

// GatewayConfigStatus defines the observed state of GatewayConfig.
type GatewayConfigStatus struct {
	// Conditions describe the current conditions of the GatewayConfig.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
