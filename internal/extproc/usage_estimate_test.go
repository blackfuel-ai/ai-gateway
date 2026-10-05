// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/backendauth"
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
		prog, err := llmcostcel.NewProgram(estimates[i].CEL)
		require.NoError(t, err)
		cfg.UsageEstimates = append(cfg.UsageEstimates, filterapi.RuntimeUsageEstimate{UsageEstimate: &estimates[i], CELProg: prog})
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

// completeWithUsage drives a successful end-of-stream response through the router.
func completeWithUsage(t *testing.T, rp *chatCompletionProcessorRouterFilter, u *chatCompletionProcessorUpstreamFilter, input, cached uint32) {
	inBody := &extprocv3.HttpBody{Body: []byte("some-body"), EndOfStream: true}
	mt := &mockTranslator{t: t, expResponseBody: inBody}
	mt.retUsedToken.SetInputTokens(input)
	mt.retUsedToken.SetCachedInputTokens(cached)
	mt.retUsedToken.SetTotalTokens(input)
	u.translator = mt
	u.responseHeaders = map[string]string{":status": "200"}
	_, err := rp.ProcessResponseBody(t.Context(), inBody)
	require.NoError(t, err)
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
	cfg := newUsageEstimateTestConfig(t, testEstimateInput, testEstimateFresh)
	headers := map[string]string{usageEstimateTestHeader: "key-a"}

	// A cold key emits only the period counts.
	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	resp, u := admitAndDispatch(t, rp)
	fields := usageEstimateFields(t, resp)
	require.Len(t, fields, 4)
	for _, key := range []string{"estimated_input_token", "estimated_fresh_input_token"} {
		require.Equal(t, 0.0, fields[key+"_samples"].GetNumberValue())
		require.Equal(t, 0.0, fields[key+"_failures"].GetNumberValue())
		require.NotContains(t, fields, key)
	}
	completeWithUsage(t, rp, u, 100, 40)
	rp.finishUsageEstimates()
	clock.nextPeriod()

	// The same key and body size is estimated from the first response.
	rp = newUsageEstimateTestRouter(cfg, ue, headers)
	resp, u = admitAndDispatch(t, rp)
	fields = usageEstimateFields(t, resp)
	require.Equal(t, 100.0, fields["estimated_input_token"].GetNumberValue())
	require.Equal(t, 60.0, fields["estimated_fresh_input_token"].GetNumberValue())
	require.Equal(t, 1.0, fields["estimated_input_token_samples"].GetNumberValue())
	require.Equal(t, 0.0, fields["estimated_input_token_failures"].GetNumberValue())
	completeWithUsage(t, rp, u, 200, 40)
	rp.finishUsageEstimates()
	clock.nextPeriod()

	// Only the item with EmitMetric records metrics.
	require.Equal(t, []recordedUsageEstimateRequest{
		{"estimated_input_token", usageEstimateTestModel, metrics.UsageEstimateOutcomeCold},
		{"estimated_input_token", usageEstimateTestModel, metrics.UsageEstimateOutcomeEstimated},
	}, m.requests)
	// The estimate (100) against the actual input tokens (200).
	require.Equal(t, []recordedUsageEstimateRatio{{"estimated_input_token", usageEstimateTestModel, 0.5}}, m.ratios)

	st := ue.store.Stats(usageestimate.Key{Header: usageEstimateTestHeader, Value: "key-a", Model: usageEstimateTestModel}, usageEstimateTestPeriod, 1)
	require.Equal(t, uint32(1), st.Samples)
	require.Equal(t, uint32(0), st.Failures)
}

func TestUsageEstimate_ConfiguredPeriod(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	cfg := newUsageEstimateTestConfig(t, testEstimateInput)
	headers := map[string]string{usageEstimateTestHeader: "key-a"}

	rp := newUsageEstimateTestRouter(cfg, ue, headers)
	_, u := admitAndDispatch(t, rp)
	completeWithUsage(t, rp, u, 100, 0)
	rp.finishUsageEstimates()

	// A quarter of the configured period later, the period is not over yet.
	clock.advance(usageEstimateTestPeriod / 4)
	resp := admit(t, newUsageEstimateTestRouter(cfg, ue, headers), false)
	require.NotContains(t, usageEstimateFields(t, resp), "estimated_input_token")

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
		rp.finishUsageEstimates()
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
	rp.finishUsageEstimates()
	require.Empty(t, m.requests)
	require.Empty(t, m.ratios)
}

func TestUsageEstimate_NotConfigured(t *testing.T) {
	ue, _, clock := newTestUsageEstimates()
	rp := newUsageEstimateTestRouter(&filterapi.RuntimeConfig{}, ue, map[string]string{usageEstimateTestHeader: "key-a"})
	resp, u := admitAndDispatch(t, rp)
	require.Nil(t, resp.DynamicMetadata)
	completeWithUsage(t, rp, u, 100, 0)
	rp.finishUsageEstimates()
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
	require.Equal(t, 1.0, fields["failing_samples"].GetNumberValue())
	// A failed evaluation is not reported as a cold key.
	require.Equal(t, recordedUsageEstimateRequest{"failing", usageEstimateTestModel, metrics.UsageEstimateOutcomeError}, m.requests[len(m.requests)-1])
}

func TestUsageEstimate_Failures(t *testing.T) {
	key := usageestimate.Key{Header: usageEstimateTestHeader, Value: "key-a", Model: usageEstimateTestModel}
	headers := map[string]string{usageEstimateTestHeader: "key-a"}

	t.Run("error status after reaching an upstream", func(t *testing.T) {
		ue, _, clock := newTestUsageEstimates()
		rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, headers)
		_, u := admitAndDispatch(t, rp)
		inBody := &extprocv3.HttpBody{Body: []byte("error"), EndOfStream: true}
		u.translator = &mockTranslator{t: t, expResponseBody: inBody}
		u.responseHeaders = map[string]string{":status": "500"}
		_, err := rp.ProcessResponseBody(t.Context(), inBody)
		require.NoError(t, err)
		rp.finishUsageEstimates()
		rp.finishUsageEstimates() // Recorded once.
		clock.nextPeriod()
		require.Equal(t, usageestimate.Stats{Samples: 1, Failures: 1}, ue.store.Stats(key, usageEstimateTestPeriod, 1))
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
		rp.finishUsageEstimates()
		clock.nextPeriod()
		require.Equal(t, usageestimate.Stats{Samples: 1, Failures: 1}, ue.store.Stats(key, usageEstimateTestPeriod, 1))
	})

	t.Run("aborted before any upstream", func(t *testing.T) {
		ue, _, clock := newTestUsageEstimates()
		rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, headers)
		_, err := rp.ProcessRequestBody(t.Context(), &extprocv3.HttpBody{Body: bodyFromModel(t, usageEstimateTestModel, false, nil)})
		require.NoError(t, err)
		rp.finishUsageEstimates()
		clock.nextPeriod()
		require.Equal(t, usageestimate.Stats{}, ue.store.Stats(key, usageEstimateTestPeriod, 1))
	})

	t.Run("mirror leg only", func(t *testing.T) {
		ue, _, clock := newTestUsageEstimates()
		rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, headers)
		admit(t, rp, false)
		mirror := setBackend(t, rp, true)
		_, err := dispatch(t, mirror, &mockBackendAuthHandler{}, nil)
		require.NoError(t, err)
		rp.finishUsageEstimates()
		clock.nextPeriod()
		require.Equal(t, usageestimate.Stats{}, ue.store.Stats(key, usageEstimateTestPeriod, 1))
	})

	// The gateway answering or failing the request itself is not an outcome of
	// the key: the request never reached an upstream.
	for _, tc := range []struct {
		name         string
		handler      filterapi.BackendAuthHandler
		translateErr error
		expErr       bool
	}{
		{name: "missing upstream credential", handler: &mockBackendAuthHandlerError{err: backendauth.ErrCredentialMissing}},
		{name: "upstream auth error", handler: &mockBackendAuthHandlerError{err: errors.New("authentication failed")}, expErr: true},
		{name: "request rejected by the translator", translateErr: fmt.Errorf("%w: missing required field", internalapi.ErrInvalidRequestBody)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ue, _, clock := newTestUsageEstimates()
			rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, headers)
			admit(t, rp, false)
			u := setBackend(t, rp, false)
			resp, err := dispatch(t, u, tc.handler, tc.translateErr)
			if tc.expErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp.GetImmediateResponse())
			}
			rp.finishUsageEstimates()
			clock.nextPeriod()
			require.Equal(t, usageestimate.Stats{}, ue.store.Stats(key, usageEstimateTestPeriod, 1))
		})
	}

	t.Run("stream aborted after reaching an upstream", func(t *testing.T) {
		ue, _, clock := newTestUsageEstimates()
		rp := newUsageEstimateTestRouter(newUsageEstimateTestConfig(t, testEstimateInput), ue, headers)
		admit(t, rp, true)
		u := setBackend(t, rp, false)
		_, err := dispatch(t, u, &mockBackendAuthHandler{}, nil)
		require.NoError(t, err)
		u.translator = &mockTranslator{t: t}
		u.responseHeaders = map[string]string{":status": "200"}
		_, err = rp.ProcessResponseBody(t.Context(), &extprocv3.HttpBody{Body: []byte("data: {}\n\n")})
		require.NoError(t, err)
		rp.finishUsageEstimates()
		clock.nextPeriod()
		require.Equal(t, usageestimate.Stats{Samples: 1, Failures: 1}, ue.store.Stats(key, usageEstimateTestPeriod, 1))
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
	rp.finishUsageEstimates()
	clock.nextPeriod()
	st := ue.store.Stats(key, usageEstimateTestPeriod, 1)
	require.Equal(t, uint32(1), st.Samples)
	require.Equal(t, uint32(0), st.Failures)
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
	rp.finishUsageEstimates()
	clock.nextPeriod()
	st := ue.store.Stats(key, usageEstimateTestPeriod, 1)
	require.Equal(t, uint32(1), st.Samples)
	require.Equal(t, uint32(0), st.Failures)
	require.True(t, st.Estimated)
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
	set      *usageEstimates
	finished int
}

func (p *usageEstimateRecordingProcessor) setUsageEstimates(ue *usageEstimates) { p.set = ue }
func (p *usageEstimateRecordingProcessor) finishUsageEstimates()                { p.finished++ }

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
	require.Equal(t, 1, p.finished)
}
