// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	anthropicschema "github.com/envoyproxy/ai-gateway/internal/apischema/anthropic"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/endpointspec"
	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/llmcostcel"
	"github.com/envoyproxy/ai-gateway/internal/metrics"
	"github.com/envoyproxy/ai-gateway/internal/tracing/tracingapi"
	"github.com/envoyproxy/ai-gateway/internal/usageestimate"
)

type recordedUsageEstimateRequest struct {
	metadataKey, model string
	outcome            metrics.UsageEstimateOutcome
}

type recordedUsageEstimateRatio struct {
	metadataKey, model string
	ratio              float64
}

// mockUsageEstimateMetrics implements [metrics.UsageEstimateMetrics].
type mockUsageEstimateMetrics struct {
	mu       sync.Mutex
	requests []recordedUsageEstimateRequest
	ratios   []recordedUsageEstimateRatio
}

func (m *mockUsageEstimateMetrics) RecordRequest(_ context.Context, metadataKey, originalModel string, outcome metrics.UsageEstimateOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, recordedUsageEstimateRequest{metadataKey, originalModel, outcome})
}

func (m *mockUsageEstimateMetrics) RecordRatio(_ context.Context, metadataKey, originalModel string, ratio float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ratios = append(m.ratios, recordedUsageEstimateRatio{metadataKey, originalModel, ratio})
}

const (
	usageEstimateTestModel  = "some-model"
	usageEstimateTestHeader = "x-client-id"
	usageEstimateTestPeriod = time.Minute
)

func newUsageEstimateTestConfig(t *testing.T, estimates ...filterapi.UsageEstimate) *filterapi.RuntimeConfig {
	cfg := &filterapi.RuntimeConfig{UsageEstimatePeriod: usageEstimateTestPeriod}
	for i := range estimates {
		prog, err := llmcostcel.NewEstimateProgram(estimates[i].CEL)
		require.NoError(t, err)
		re := filterapi.RuntimeUsageEstimate{UsageEstimate: &estimates[i], CELProg: prog}
		if estimates[i].EmitHeader {
			re.Header = filterapi.UsageEstimateHeader(estimates[i].MetadataKey)
		}
		cfg.UsageEstimates = append(cfg.UsageEstimates, re)
	}
	return cfg
}

var (
	testEstimateInput = filterapi.UsageEstimate{
		MetadataKey: "estimated_input_token", CEL: "input_tokens", ByHeader: usageEstimateTestHeader, EmitMetric: true,
	}
	testEstimateFresh = filterapi.UsageEstimate{
		MetadataKey: "estimated_fresh_input_token",
		CEL:         "input_tokens > cached_input_tokens ? input_tokens - cached_input_tokens : uint(0)",
		ByHeader:    usageEstimateTestHeader,
	}
	testEstimateCacheRate = filterapi.UsageEstimate{
		MetadataKey: "estimated_cache_rate", CEL: "cache_rate", ByHeader: usageEstimateTestHeader, EmitMetric: true,
	}
	testEstimateInputTokensPerByte = filterapi.UsageEstimate{
		MetadataKey: "estimated_input_tokens_per_byte", CEL: "input_tokens_per_byte", ByHeader: usageEstimateTestHeader,
	}
)

func newUsageEstimateTestRouter(config *filterapi.RuntimeConfig, ue *usageEstimates, headers map[string]string) *chatCompletionProcessorRouterFilter {
	h := map[string]string{":path": "/v1/chat/completions"}
	for k, v := range headers {
		h[k] = v
	}
	rp := &chatCompletionProcessorRouterFilter{
		config:         config,
		requestHeaders: h,
		logger:         slog.Default(),
		tracer:         tracingapi.NoopTracer[openai.ChatCompletionRequest, openai.ChatCompletionResponse, openai.ChatCompletionResponseChunk]{},
	}
	rp.setUsageEstimates(ue)
	return rp
}

// admit runs the router request body phase.
func admit(t *testing.T, rp *chatCompletionProcessorRouterFilter, stream bool) *extprocv3.ProcessingResponse {
	resp, err := rp.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: bodyFromModel(t, usageEstimateTestModel, stream, nil)})
	require.NoError(t, err)
	return resp
}

// setBackend attaches an upstream leg to the request, a mirror one when mirror is set.
func setBackend(t *testing.T, rp *chatCompletionProcessorRouterFilter, mirror bool) *chatCompletionProcessorUpstreamFilter {
	u := &chatCompletionProcessorUpstreamFilter{requestHeaders: map[string]string{":path": "/v1/chat/completions"}, metrics: &mockMetrics{}}
	require.NoError(t, u.SetBackend(t.Context(),
		&filterapi.RuntimeBackend{Backend: &filterapi.Backend{Name: "primary", Schema: filterapi.VersionedAPISchema{Name: filterapi.APISchemaOpenAI}, IsMirror: mirror}}, "route", rp))
	return u
}

// dispatch runs the upstream request headers phase of u, which sends the request
// upstream unless the auth handler h or the translator error translateErr stops it.
func dispatch(t *testing.T, u *chatCompletionProcessorUpstreamFilter, h filterapi.BackendAuthHandler, translateErr error) (*extprocv3.ProcessingResponse, error) {
	u.translator = &mockTranslator{
		t: t, expRequestBody: u.parent.originalRequestBody,
		expForceRequestBodyMutation: u.onRetry() || u.parent.forceBodyMutation, retErr: translateErr,
	}
	u.handler = h
	u.logger = slog.Default()
	return u.ProcessRequestHeaders(t.Context(), nil)
}

// admitAndDispatch admits a non-streaming request and sends it to a primary upstream leg.
func admitAndDispatch(t *testing.T, rp *chatCompletionProcessorRouterFilter) (*extprocv3.ProcessingResponse, *chatCompletionProcessorUpstreamFilter) {
	resp := admit(t, rp, false)
	u := setBackend(t, rp, false)
	_, err := dispatch(t, u, &mockBackendAuthHandler{}, nil)
	require.NoError(t, err)
	return resp, u
}

// completeWithUsage drives a successful end-of-stream response through the router
// and returns the router's response.
func completeWithUsage(t *testing.T, rp *chatCompletionProcessorRouterFilter, u *chatCompletionProcessorUpstreamFilter, input, cached uint32) *extprocv3.ProcessingResponse {
	inBody := &extprocv3.HttpBody{Body: []byte("some-body"), EndOfStream: true}
	mt := &mockTranslator{t: t, expResponseBody: inBody}
	mt.retUsedToken.SetInputTokens(input)
	mt.retUsedToken.SetCachedInputTokens(cached)
	mt.retUsedToken.SetTotalTokens(input)
	u.translator = mt
	u.responseHeaders = map[string]string{":status": "200"}
	resp, err := rp.ProcessResponseBody(t.Context(), inBody)
	require.NoError(t, err)
	return resp
}

func usageEstimateFields(t *testing.T, resp *extprocv3.ProcessingResponse) map[string]*structpb.Value {
	t.Helper()
	require.NotNil(t, resp.DynamicMetadata)
	return resp.DynamicMetadata.Fields[internalapi.AIGatewayFilterMetadataNamespace].GetStructValue().Fields
}

// usageEstimateTestClock is a settable clock for the usage estimate store.
type usageEstimateTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *usageEstimateTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// nextPeriod moves the clock to the next usage estimate period, which exposes
// the outcomes recorded so far.
func (c *usageEstimateTestClock) nextPeriod() {
	c.advance(usageEstimateTestPeriod)
}

func (c *usageEstimateTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestUsageEstimates() (*usageEstimates, *mockUsageEstimateMetrics, *usageEstimateTestClock) {
	m := &mockUsageEstimateMetrics{}
	clock := &usageEstimateTestClock{now: time.Unix(1_700_000_000, 0).Truncate(usageEstimateTestPeriod)}
	return &usageEstimates{store: usageestimate.NewStore(clock.Now), metrics: m}, m, clock
}

func TestUsageEstimate_ColdThenEstimated(t *testing.T) {
	ue, m, clock := newTestUsageEstimates()
	cfg := newUsageEstimateTestConfig(t, testEstimateInput, testEstimateFresh, testEstimateCacheRate, testEstimateInputTokensPerByte)
	headers := map[string]string{usageEstimateTestHeader: "key-a"}

	// A cold key emits nothing.
	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	resp, u := admitAndDispatch(t, rp)
	require.Nil(t, resp.DynamicMetadata)
	completeWithUsage(t, rp, u, 100, 40)
	clock.nextPeriod()

	// The same key and body size is estimated from the first response.
	rp = newUsageEstimateTestRouter(cfg, ue, headers)
	resp, u = admitAndDispatch(t, rp)
	fields := usageEstimateFields(t, resp)
	require.Len(t, fields, 4)
	require.Equal(t, 100.0, fields["estimated_input_token"].GetNumberValue())
	require.Equal(t, 60.0, fields["estimated_fresh_input_token"].GetNumberValue())
	// The measured ratios of the period: 100 input tokens over the body size, 40
	// of them cached.
	bodyBytes := len(bodyFromModel(t, usageEstimateTestModel, false, nil))
	require.Equal(t, 100/float64(bodyBytes), fields["estimated_input_tokens_per_byte"].GetNumberValue())
	require.Equal(t, 0.4, fields["estimated_cache_rate"].GetNumberValue())
	completeWithUsage(t, rp, u, 200, 40)
	clock.nextPeriod()

	// Only the item with EmitMetric records metrics.
	require.Equal(t, []recordedUsageEstimateRequest{
		{"estimated_input_token", usageEstimateTestModel, metrics.UsageEstimateOutcomeCold},
		{"estimated_cache_rate", usageEstimateTestModel, metrics.UsageEstimateOutcomeCold},
		{"estimated_input_token", usageEstimateTestModel, metrics.UsageEstimateOutcomeEstimated},
		{"estimated_cache_rate", usageEstimateTestModel, metrics.UsageEstimateOutcomeEstimated},
	}, m.requests)
	// The estimate (100) against the actual input tokens (200), and the estimated
	// cache rate (0.4) against the actual one (40 of 200 input tokens).
	require.Equal(t, []recordedUsageEstimateRatio{
		{"estimated_input_token", usageEstimateTestModel, 0.5},
		{"estimated_cache_rate", usageEstimateTestModel, 2},
	}, m.ratios)

	// The second response is the only one of its period.
	st := ue.store.Stats(usageestimate.Key{Header: usageEstimateTestHeader, Value: "key-a", Model: usageEstimateTestModel}, usageEstimateTestPeriod, 1)
	require.Equal(t, 200/float64(bodyBytes), st.InputTokensPerByte)
	require.Equal(t, 0.2, st.CacheRate)
}

func TestUsageEstimate_ConfiguredPeriod(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	cfg := newUsageEstimateTestConfig(t, testEstimateInput)
	headers := map[string]string{usageEstimateTestHeader: "key-a"}

	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	_, u := admitAndDispatch(t, rp)
	completeWithUsage(t, rp, u, 100, 0)

	// A quarter of the configured period later, the period is not over yet.
	clock.advance(usageEstimateTestPeriod / 4)
	resp := admit(t, newUsageEstimateTestRouter(cfg, ue, headers), false)
	require.Nil(t, resp.DynamicMetadata)

	// Once it is over, the response is exposed.
	clock.advance(usageEstimateTestPeriod * 3 / 4)
	resp = admit(t, newUsageEstimateTestRouter(cfg, ue, headers), false)
	require.Equal(t, 100.0, usageEstimateFields(t, resp)["estimated_input_token"].GetNumberValue())
}

func TestUsageEstimate_RatioUsesAdmissionInputs(t *testing.T) {
	ue, m, clock := newTestUsageEstimates()
	// The expression depends on the inputs that are only known after routing: the
	// ratio must evaluate it on the same model, backend and route as the estimate.
	cfg := newUsageEstimateTestConfig(t, filterapi.UsageEstimate{
		MetadataKey: "estimated_input_token",
		CEL:         `model == "` + usageEstimateTestModel + `" && backend == "" && route_name == "" ? input_tokens : input_tokens * uint(2)`,
		ByHeader:    usageEstimateTestHeader,
		EmitMetric:  true,
	})
	headers := map[string]string{usageEstimateTestHeader: "key-a"}
	run := func() {
		rp := newUsageEstimateTestRouter(cfg, ue, headers)
		admit(t, rp, false)
		u := &chatCompletionProcessorUpstreamFilter{requestHeaders: map[string]string{":path": "/v1/chat/completions"}, metrics: &mockMetrics{}}
		require.NoError(t, u.SetBackend(t.Context(), &filterapi.RuntimeBackend{Backend: &filterapi.Backend{
			Name: "primary", Schema: filterapi.VersionedAPISchema{Name: filterapi.APISchemaOpenAI}, ModelNameOverride: "overridden-model",
		}}, "route", rp))
		_, err := dispatch(t, u, &mockBackendAuthHandler{}, nil)
		require.NoError(t, err)
		completeWithUsage(t, rp, u, 100, 0)
		clock.nextPeriod()
	}
	run()
	run()
	// The estimate (100) against the same expression on the actual usage (100).
	require.Equal(t, []recordedUsageEstimateRatio{{"estimated_input_token", usageEstimateTestModel, 1}}, m.ratios)
}

func TestUsageEstimate_NoHeader(t *testing.T) {
	ue, m, _ := newTestUsageEstimates()
	rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, nil)
	resp, u := admitAndDispatch(t, rp)
	require.Nil(t, resp.DynamicMetadata)
	completeWithUsage(t, rp, u, 100, 0)
	require.Empty(t, m.requests)
	require.Empty(t, m.ratios)
}

func TestUsageEstimate_NotConfigured(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	rp := newUsageEstimateTestRouter(&filterapi.RuntimeConfig{}, ue, map[string]string{usageEstimateTestHeader: "key-a"})
	resp, u := admitAndDispatch(t, rp)
	require.Nil(t, resp.DynamicMetadata)
	completeWithUsage(t, rp, u, 100, 0)
	clock.nextPeriod()
	require.Equal(t, usageestimate.Stats{}, ue.store.Stats(usageestimate.Key{Header: usageEstimateTestHeader, Value: "key-a", Model: usageEstimateTestModel}, usageEstimateTestPeriod, 1))
}

func TestUsageEstimate_CELErrorSkipsOnlyThatKey(t *testing.T) {
	ue, m, clock := newTestUsageEstimates()
	failing := filterapi.UsageEstimate{
		MetadataKey: "failing", CEL: "input_tokens > uint(0) ? input_tokens - uint(1000000) : uint(0)", ByHeader: usageEstimateTestHeader,
		EmitMetric: true,
	}
	cfg := newUsageEstimateTestConfig(t, testEstimateInput, failing)
	headers := map[string]string{usageEstimateTestHeader: "key-a"}

	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	_, u := admitAndDispatch(t, rp)
	completeWithUsage(t, rp, u, 100, 0)
	clock.nextPeriod()

	rp = newUsageEstimateTestRouter(cfg, ue, headers)
	resp, _ := admitAndDispatch(t, rp)
	fields := usageEstimateFields(t, resp)
	require.Equal(t, 100.0, fields["estimated_input_token"].GetNumberValue())
	require.NotContains(t, fields, "failing")
	require.Len(t, fields, 1)
	// A failed evaluation is not reported as a cold key.
	require.Equal(t, recordedUsageEstimateRequest{"failing", usageEstimateTestModel, metrics.UsageEstimateOutcomeError}, m.requests[len(m.requests)-1])
}

func TestUsageEstimate_NoUsageRecordsNothing(t *testing.T) {
	key := usageestimate.Key{Header: usageEstimateTestHeader, Value: "key-a", Model: usageEstimateTestModel}
	headers := map[string]string{usageEstimateTestHeader: "key-a"}

	t.Run("error status", func(t *testing.T) {
		ue, _, clock := newTestUsageEstimates()
		rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, headers)
		_, u := admitAndDispatch(t, rp)
		inBody := &extprocv3.HttpBody{Body: []byte("error"), EndOfStream: true}
		u.translator = &mockTranslator{t: t, expResponseBody: inBody}
		u.responseHeaders = map[string]string{":status": "500"}
		_, err := rp.ProcessResponseBody(t.Context(), inBody)
		require.NoError(t, err)
		clock.nextPeriod()
		require.Equal(t, usageestimate.Stats{}, ue.store.Stats(key, usageEstimateTestPeriod, 1))
	})

	t.Run("response without usage", func(t *testing.T) {
		ue, _, clock := newTestUsageEstimates()
		rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, headers)
		_, u := admitAndDispatch(t, rp)
		inBody := &extprocv3.HttpBody{Body: []byte("some-body"), EndOfStream: true}
		u.translator = &mockTranslator{t: t, expResponseBody: inBody}
		u.responseHeaders = map[string]string{":status": "200"}
		_, err := rp.ProcessResponseBody(t.Context(), inBody)
		require.NoError(t, err)
		clock.nextPeriod()
		require.Equal(t, usageestimate.Stats{}, ue.store.Stats(key, usageEstimateTestPeriod, 1))
	})

	t.Run("stream aborted before its end", func(t *testing.T) {
		ue, _, clock := newTestUsageEstimates()
		rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, headers)
		admit(t, rp, true)
		u := setBackend(t, rp, false)
		_, err := dispatch(t, u, &mockBackendAuthHandler{}, nil)
		require.NoError(t, err)
		mt := &mockTranslator{t: t}
		mt.retUsedToken.SetInputTokens(100)
		u.translator = mt
		u.responseHeaders = map[string]string{":status": "200"}
		_, err = rp.ProcessResponseBody(t.Context(), &extprocv3.HttpBody{Body: []byte("data: {}\n\n")})
		require.NoError(t, err)
		clock.nextPeriod()
		require.Equal(t, usageestimate.Stats{}, ue.store.Stats(key, usageEstimateTestPeriod, 1))
	})
}

func TestUsageEstimate_RetryThenSuccess(t *testing.T) {
	key := usageestimate.Key{Header: usageEstimateTestHeader, Value: "key-a", Model: usageEstimateTestModel}
	ue, _, clock := newTestUsageEstimates()
	rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, map[string]string{usageEstimateTestHeader: "key-a"})
	admit(t, rp, false)
	// The first leg fails upstream; Envoy retries without the router seeing its response.
	_, err := dispatch(t, setBackend(t, rp, false), &mockBackendAuthHandler{}, nil)
	require.NoError(t, err)
	u := setBackend(t, rp, false)
	_, err = dispatch(t, u, &mockBackendAuthHandler{}, nil)
	require.NoError(t, err)
	completeWithUsage(t, rp, u, 100, 0)
	clock.nextPeriod()
	st := ue.store.Stats(key, usageEstimateTestPeriod, 1)
	require.Equal(t, 100/float64(len(bodyFromModel(t, usageEstimateTestModel, false, nil))), st.InputTokensPerByte)
}

func TestUsageEstimate_StreamSuccess(t *testing.T) {
	key := usageestimate.Key{Header: usageEstimateTestHeader, Value: "key-a", Model: usageEstimateTestModel}
	ue, _, clock := newTestUsageEstimates()
	rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, map[string]string{usageEstimateTestHeader: "key-a"})
	admit(t, rp, true)
	u := setBackend(t, rp, false)
	_, err := dispatch(t, u, &mockBackendAuthHandler{}, nil)
	require.NoError(t, err)
	mt := &mockTranslator{t: t}
	mt.retUsedToken.SetInputTokens(100)
	u.translator = mt
	u.responseHeaders = map[string]string{":status": "200"}
	// The usage is recorded once, at the end of the stream.
	for _, eos := range []bool{false, false, true} {
		_, err = rp.ProcessResponseBody(t.Context(), &extprocv3.HttpBody{Body: []byte("data: {}\n\n"), EndOfStream: eos})
		require.NoError(t, err)
	}
	clock.nextPeriod()
	require.True(t, ue.store.Stats(key, usageEstimateTestPeriod, 1).Estimated)
}

func TestUsageEstimate_ForcesStreamUsage(t *testing.T) {
	ue, _, _ := newTestUsageEstimates()
	rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, nil)
	_, err := rp.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: bodyFromModel(t, usageEstimateTestModel, true, nil)})
	require.NoError(t, err)
	require.True(t, rp.forceBodyMutation)
	require.Contains(t, string(rp.originalRequestBodyRaw), `"include_usage":true`)
}

// usageEstimateRecordingProcessor records the usage estimate calls of the server.
type usageEstimateRecordingProcessor struct {
	passThroughProcessor
	set *usageEstimates
}

func (p *usageEstimateRecordingProcessor) setUsageEstimates(ue *usageEstimates) { p.set = ue }

func TestNewServer_UsageEstimatesOff(t *testing.T) {
	s, err := NewServer(slog.Default(), false)
	require.NoError(t, err)
	// Without WithUsageEstimates, no store is created that nothing would sweep.
	require.Nil(t, s.usageEstimates)
}

func TestServer_Process_UsageEstimates(t *testing.T) {
	s, _ := requireNewServerWithMockProcessor(t)
	ue, _, _ := newTestUsageEstimates()
	WithUsageEstimates(ue.store, ue.metrics)(s)
	p := &usageEstimateRecordingProcessor{}
	s.Register("/v1/chat/completions", func(*filterapi.RuntimeConfig, map[string]string, *slog.Logger, bool, bool) (Processor, error) {
		return p, nil
	})
	stream := &queuedProcessingStream{
		mockExternalProcessingStream: mockExternalProcessingStream{t: t, ctx: t.Context()},
		reqs: []*extprocv3.ProcessingRequest{{Request: &extprocv3.ProcessingRequest_RequestHeaders{RequestHeaders: &extprocv3.HttpHeaders{
			Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{{Key: ":path", Value: "/v1/chat/completions"}}},
		}}}},
	}
	require.NoError(t, s.Process(stream))
	require.Same(t, ue.store, p.set.store)
}

// usageEstimateHeaderMutation returns the request header mutation of the admission response.
func usageEstimateHeaderMutation(t *testing.T, resp *extprocv3.ProcessingResponse) *extprocv3.HeaderMutation {
	t.Helper()
	m := resp.GetRequestBody().GetResponse().GetHeaderMutation()
	require.NotNil(t, m)
	return m
}

// usageEstimateSetHeaders returns the usage estimate headers set by the mutation.
func usageEstimateSetHeaders(t *testing.T, m *extprocv3.HeaderMutation) map[string]string {
	set := make(map[string]string)
	for _, h := range m.SetHeaders {
		if strings.HasPrefix(h.Header.Key, internalapi.UsageEstimateHeaderPrefix) {
			require.Equal(t, corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD, h.AppendAction)
			set[h.Header.Key] = string(h.Header.RawValue)
		}
	}
	return set
}

func TestUsageEstimate_EmitHeader(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	input := testEstimateInput
	input.EmitHeader = true
	cacheRate := testEstimateCacheRate
	cacheRate.EmitHeader = true
	cfg := newUsageEstimateTestConfig(t, input, testEstimateFresh, cacheRate)
	const (
		inputHeader     = "x-ai-eg-usage-estimate-estimated-input-token"
		cacheRateHeader = "x-ai-eg-usage-estimate-estimated-cache-rate"
	)
	// The client sends a value of its own for one of the estimate headers.
	headers := map[string]string{usageEstimateTestHeader: "key-a", inputHeader: "1"}

	// A cold key removes every estimate header the request carries.
	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	resp, u := admitAndDispatch(t, rp)
	m := usageEstimateHeaderMutation(t, resp)
	require.Empty(t, usageEstimateSetHeaders(t, m))
	require.ElementsMatch(t, []string{inputHeader, cacheRateHeader}, m.RemoveHeaders)
	require.NotContains(t, rp.requestHeaders, inputHeader)
	completeWithUsage(t, rp, u, 100, 40)
	clock.nextPeriod()

	// An estimated key overwrites the client's value, and only the items with
	// EmitHeader get a header.
	rp = newUsageEstimateTestRouter(cfg, ue, headers)
	resp, _ = admitAndDispatch(t, rp)
	m = usageEstimateHeaderMutation(t, resp)
	require.Equal(t, map[string]string{inputHeader: "100", cacheRateHeader: "0.4"}, usageEstimateSetHeaders(t, m))
	require.Empty(t, m.RemoveHeaders)
	require.Equal(t, "100", rp.requestHeaders[inputHeader])
	require.Equal(t, "0.4", rp.requestHeaders[cacheRateHeader])
	// The dynamic metadata is emitted as without EmitHeader.
	require.Len(t, usageEstimateFields(t, resp), 3)
}

func TestUsageEstimate_EmitHeaderRemovedWithoutEstimate(t *testing.T) {
	const inputHeader = "x-ai-eg-usage-estimate-estimated-input-token"
	input := testEstimateInput
	input.EmitHeader = true

	t.Run("no byHeader value", func(t *testing.T) {
		ue, _, _ := newTestUsageEstimates()
		rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, input), ue, map[string]string{inputHeader: "1"})
		resp, _ := admitAndDispatch(t, rp)
		m := usageEstimateHeaderMutation(t, resp)
		require.Empty(t, usageEstimateSetHeaders(t, m))
		require.Equal(t, []string{inputHeader}, m.RemoveHeaders)
		require.NotContains(t, rp.requestHeaders, inputHeader)
	})

	t.Run("usage estimates not enabled on the server", func(t *testing.T) {
		rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, input), nil,
			map[string]string{usageEstimateTestHeader: "key-a", inputHeader: "1"})
		resp, _ := admitAndDispatch(t, rp)
		m := usageEstimateHeaderMutation(t, resp)
		require.Empty(t, usageEstimateSetHeaders(t, m))
		require.Equal(t, []string{inputHeader}, m.RemoveHeaders)
	})

	t.Run("CEL error", func(t *testing.T) {
		ue, _, clock := newTestUsageEstimates()
		failing := filterapi.UsageEstimate{
			MetadataKey: "failing", CEL: "input_tokens > uint(0) ? input_tokens - uint(1000000) : uint(0)",
			ByHeader: usageEstimateTestHeader, EmitHeader: true,
		}
		cfg := newUsageEstimateTestConfig(t, input, failing)
		headers := map[string]string{usageEstimateTestHeader: "key-a"}
		rp := newUsageEstimateTestRouter(cfg, ue, headers)
		_, u := admitAndDispatch(t, rp)
		completeWithUsage(t, rp, u, 100, 0)
		clock.nextPeriod()

		rp = newUsageEstimateTestRouter(cfg, ue, headers)
		resp, _ := admitAndDispatch(t, rp)
		m := usageEstimateHeaderMutation(t, resp)
		require.Equal(t, map[string]string{inputHeader: "100"}, usageEstimateSetHeaders(t, m))
		require.Equal(t, []string{"x-ai-eg-usage-estimate-failing"}, m.RemoveHeaders)
	})
}

func TestUsageEstimate_NoEmitHeaderLeavesHeadersAlone(t *testing.T) {
	ue, _, _ := newTestUsageEstimates()
	const inputHeader = "x-ai-eg-usage-estimate-estimated-input-token"
	rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue,
		map[string]string{usageEstimateTestHeader: "key-a"})
	resp, _ := admitAndDispatch(t, rp)
	m := usageEstimateHeaderMutation(t, resp)
	require.Empty(t, usageEstimateSetHeaders(t, m))
	require.Empty(t, m.RemoveHeaders)
	require.NotContains(t, rp.requestHeaders, inputHeader)
}

// A token-counting endpoint consumes no model usage: its requests are neither
// estimated nor reserved, and their usage is not recorded.
func TestUsageEstimate_TokenCountingEndpoint(t *testing.T) {
	ue, m, clock := newTestUsageEstimates()
	cfg := newUsageEstimateTestConfig(t, testEstimateInput)
	cfg.AdmissionReserves = []filterapi.AdmissionReserve{{MetadataKey: "quota_reserve_estimated_input_token_100", ReleaseMetadataKey: "quota_release_estimated_input_token_100", UsageEstimate: testEstimateInput.MetadataKey, Percent: 100}}
	headers := map[string]string{usageEstimateTestHeader: "key-a"}
	key := usageestimate.Key{Header: usageEstimateTestHeader, Value: "key-a", Model: usageEstimateTestModel}

	// The same key is warm for the endpoints that estimate.
	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	_, u := admitAndDispatch(t, rp)
	completeWithUsage(t, rp, u, 100, 0)
	clock.nextPeriod()
	requests := len(m.requests)

	ct := &routerProcessor[anthropicschema.CountTokensRequest, anthropicschema.CountTokensResponse, struct{}, endpointspec.MessagesCountTokensEndpointSpec]{
		config:         cfg,
		requestHeaders: map[string]string{":path": "/v1/messages/count_tokens", usageEstimateTestHeader: "key-a"},
		logger:         slog.Default(),
		tracer:         tracingapi.NoopTracer[anthropicschema.CountTokensRequest, anthropicschema.CountTokensResponse, struct{}]{},
	}
	ct.setUsageEstimates(ue)
	resp, err := ct.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: []byte(`{"model":"` + usageEstimateTestModel + `","messages":[]}`)})
	require.NoError(t, err)
	require.Nil(t, resp.DynamicMetadata)
	require.Len(t, m.requests, requests)

	var usage metrics.TokenUsage
	usage.SetInputTokens(5000)
	ct.recordUsageEstimateSuccess(t.Context(), &usage)
	clock.nextPeriod()
	require.Equal(t, usageestimate.Stats{}, ue.store.Stats(key, usageEstimateTestPeriod, 1))
}
