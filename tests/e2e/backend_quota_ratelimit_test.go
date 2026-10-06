// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/tests/internal/e2elib"
	"github.com/envoyproxy/ai-gateway/tests/internal/testupstreamlib"
)

// Test_Examples_BackendQuotaRateLimit tests the backend-level quota rate limiting
// using the QuotaPolicy CRD. This verifies that the upstream rate limit filter
// enforces per-model token quotas on requests to AIServiceBackends.
func Test_Examples_BackendQuotaRateLimit(t *testing.T) {
	// Apply Redis manifest (shared with token rate limit tests).
	require.NoError(t, e2elib.KubectlApplyManifest(t.Context(), "../../examples/token_ratelimit/redis.yaml"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = e2elib.KubectlDeleteManifest(ctx, "../../examples/token_ratelimit/redis.yaml")
	})

	// Wait for the redis pod to be ready so that the rate limit service can connect.
	e2elib.RequireWaitForPodReady(t, "redis-system", "app=redis")

	require.NoError(t, e2elib.KubectlApplyManifest(t.Context(), "testdata/backend_quota_ratelimit.yaml"))
	t.Cleanup(func() {
		// Bounded: a QuotaPolicy finalizer that a starved controller has not
		// removed yet would otherwise block this delete until the go test
		// timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = e2elib.KubectlDeleteManifest(ctx, "testdata/backend_quota_ratelimit.yaml")
	})

	const egSelector = "gateway.envoyproxy.io/owning-gateway-name=envoy-ai-gateway-quota-ratelimit"
	e2elib.RequireWaitForGatewayPodReady(t, egSelector)

	// Wait for the AI Gateway rate limit service to be ready.
	e2elib.RequireWaitForPodReady(t, e2elib.EnvoyGatewayNamespace, "app=envoy-ai-gateway-ratelimit")

	// Flush any existing quota keys in Redis to start with a clean state.
	flushQuotaKeys(t)

	// makeRequest sends a chat completion request via the test upstream with the
	// specified total_tokens in the fake response and asserts the expected status code.
	makeRequest := func(modelName string, totalTokens int, expectedStatus int, headers ...http.Header) {
		fwd := e2elib.RequireNewHTTPPortForwarder(t, e2elib.EnvoyGatewayNamespace, egSelector, e2elib.EnvoyGatewayDefaultServicePort)
		defer fwd.Kill()

		requestBody := fmt.Sprintf(`{"messages":[{"role":"user","content":"Say this is a test"}],"model":"%s"}`, modelName)
		fakeResponseBody := fmt.Sprintf(
			`{"choices":[{"message":{"content":"This is a test.","role":"assistant"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":%d}}`,
			totalTokens,
		)

		newRequest := func() *http.Request {
			req, err := http.NewRequest(http.MethodPut, fwd.Address()+"/v1/chat/completions", strings.NewReader(requestBody))
			require.NoError(t, err)
			req.Header.Set(testupstreamlib.ResponseBodyHeaderKey, base64.StdEncoding.EncodeToString([]byte(fakeResponseBody)))
			req.Header.Set(testupstreamlib.ExpectedPathHeaderKey, base64.StdEncoding.EncodeToString([]byte("/v1/chat/completions")))
			req.Header.Set("Host", "openai.com")
			for _, h := range headers {
				for k, vals := range h {
					for _, v := range vals {
						req.Header.Set(k, v)
					}
				}
			}
			return req
		}

		// A bounded client: without a timeout, one hung request on a starved
		// CI runner stalls the whole suite until the go test timeout.
		client := &http.Client{Timeout: 30 * time.Second}

		// Retry 404s: right after the manifests are applied, the route may not
		// be programmed in the proxy yet. The 404 fallback carries no quota
		// rate limits, so retried requests do not consume any counter and the
		// exact Redis assertions below stay valid.
		var resp *http.Response
		require.Eventually(t, func() bool {
			r, doErr := client.Do(newRequest()) //nolint:bodyclose // closed below or on the retry path.
			if doErr != nil {
				t.Logf("request failed, retrying: %v", doErr)
				return false
			}
			if expectedStatus != http.StatusNotFound && r.StatusCode == http.StatusNotFound {
				_ = r.Body.Close()
				return false
			}
			resp = r
			return true
		}, 30*time.Second, 500*time.Millisecond, "request kept failing or returning 404")
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		require.Equal(t, expectedStatus, resp.StatusCode, "unexpected status code, body: %s", string(body))
	}

	// Test per-model quota enforcement by verifying the quota counter in Redis.
	// The QuotaPolicy sets a quota of 10 total tokens per hour for "quota-test-model".
	t.Run("per-model quota", func(t *testing.T) {
		makeRequest("quota-test-model", 20, http.StatusOK)
		requireQuotaUsage(t, "quota-test-model", 21)
		makeRequest("quota-test-model", 5, http.StatusTooManyRequests)
		requireQuotaUsage(t, "quota-test-model", 22)
	})

	// Test the per-request limit override for "quota-dynamic-model": the static
	// fallback is 5 total tokens per hour, and the x-test-quota-limit header
	// supplies the limit at request time. The override and the static fallback
	// share the same Redis counter.
	t.Run("dynamic limit override", func(t *testing.T) {
		limitHeader := func(v string) http.Header { return http.Header{"x-test-quota-limit": []string{v}} }

		// Exhaust the static fallback limit (5): first request passes, second is rejected.
		makeRequest("quota-dynamic-model", 20, http.StatusOK)
		requireQuotaUsage(t, "quota-dynamic-model", 21)
		makeRequest("quota-dynamic-model", 5, http.StatusTooManyRequests)
		requireQuotaUsage(t, "quota-dynamic-model", 22)

		// The header raises the limit past the current counter: allowed again,
		// and the burndown lands on the same counter.
		makeRequest("quota-dynamic-model", 5, http.StatusOK, limitHeader("1000"))
		requireQuotaUsage(t, "quota-dynamic-model", 28)

		// A zero override blocks the request outright.
		makeRequest("quota-dynamic-model", 5, http.StatusTooManyRequests, limitHeader("0"))

		// A malformed override falls back to the static limit (5), which is exhausted.
		makeRequest("quota-dynamic-model", 5, http.StatusTooManyRequests, limitHeader("not-a-number"))
	})

	// Per-org token burndown: the quota Lua filter copies each Distinct
	// selector header's value into dynamic metadata, and the stream-done
	// charge keys the per-org descriptor from it. "quota-org-model" gives each
	// distinct x-org-id a 100-token/1h bucket: a single 200-token response
	// exhausts it, so the next request from the same org is rejected while a
	// different org is unaffected.
	t.Run("per-org token burndown", func(t *testing.T) {
		orgHeader := http.Header{"x-org-id": []string{"org-burndown-a"}}

		makeRequest("quota-org-model", 200, http.StatusOK, orgHeader)

		// The stream-done charge lands asynchronously after the response.
		require.Eventually(t, func() bool {
			fwd := e2elib.RequireNewHTTPPortForwarder(t, e2elib.EnvoyGatewayNamespace, egSelector, e2elib.EnvoyGatewayDefaultServicePort)
			defer fwd.Kill()
			req := newChatRequest(t, fwd.Address(), "quota-org-model", 5, orgHeader)
			client := &http.Client{Timeout: 30 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Logf("request failed, retrying: %v", err)
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			return resp.StatusCode == http.StatusTooManyRequests
		}, 30*time.Second, time.Second, "per-org bucket was not exhausted by the token charge")

		// A different org's bucket is untouched.
		makeRequest("quota-org-model", 5, http.StatusOK, http.Header{"x-org-id": []string{"org-burndown-b"}})
	})

	// sendWithUsage sends a chat completion request for model whose fake
	// response, with status, reports promptTokens input tokens of which
	// cachedTokens were served from the prefix cache.
	sendWithUsage := func(model string, promptTokens, cachedTokens, status int, headers http.Header) {
		fwd := e2elib.RequireNewHTTPPortForwarder(t, e2elib.EnvoyGatewayNamespace, egSelector, e2elib.EnvoyGatewayDefaultServicePort)
		defer fwd.Kill()
		httpClient := &http.Client{Timeout: 30 * time.Second}
		var resp *http.Response
		require.Eventually(t, func() bool {
			req := newChatRequestWithUsage(t, fwd.Address(), model, promptTokens, cachedTokens, headers)
			if status != http.StatusOK {
				req.Header.Set(testupstreamlib.ResponseStatusKey, strconv.Itoa(status))
			}
			r, err := httpClient.Do(req) //nolint:bodyclose // closed below or on the retry path.
			if err != nil {
				t.Logf("request failed, retrying: %v", err)
				return false
			}
			if r.StatusCode == http.StatusNotFound {
				_ = r.Body.Close()
				return false
			}
			resp = r
			return true
		}, 30*time.Second, 500*time.Millisecond, "request kept failing or returning 404")
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, status, resp.StatusCode, "body: %s", string(body))
	}

	// Admission reserve on "quota-reserve-model": its bucket counts fresh input
	// tokens (input minus cached), and a request is charged, when admitted, the
	// fresh input its x-test-client's responses of the last completed 5s usage
	// estimate period predict (percent 100). When its stream ends, the reserve
	// is released and a successful response charges its actual fresh input, so
	// every request ends at its actual fresh input tokens plus the +1 the
	// request-time entry charges, whatever the reserve.
	t.Run("admission reserve", func(t *testing.T) {
		client := http.Header{"x-test-client": []string{"client-reserve-a"}}
		send := func(promptTokens, cachedTokens, status int, headers http.Header) {
			sendWithUsage("quota-reserve-model", promptTokens, cachedTokens, status, headers)
		}

		// A client with no previous response reserves nothing: 1,000 - 800 =
		// 200 fresh.
		send(1000, 800, http.StatusOK, client)
		requireQuotaUsage(t, "quota-reserve-model", 201)

		// The estimate draws on completed periods only.
		waitForNextUsageEstimatePeriod(t)

		// The same request again: the reserve is the predicted 200, released
		// once the actual 200 is charged.
		send(1000, 800, http.StatusOK, client)
		requireQuotaUsage(t, "quota-reserve-model", 402)

		// A response fully served from cache costs nothing: the reserve is
		// released and nothing is charged.
		send(1000, 1000, http.StatusOK, client)
		requireQuotaUsage(t, "quota-reserve-model", 403)

		// A reserve above the actual cost: only the actual 100 stays charged.
		send(1000, 900, http.StatusOK, client)
		requireQuotaUsage(t, "quota-reserve-model", 504)

		// An upstream error charges no tokens: the reserve is released.
		send(1000, 800, http.StatusInternalServerError, client)
		requireQuotaUsage(t, "quota-reserve-model", 505)

		// Another client has no history of its own and reserves nothing.
		send(1000, 800, http.StatusOK, http.Header{"x-test-client": []string{"client-reserve-b"}})
		requireQuotaUsage(t, "quota-reserve-model", 706)
	})

	// Admission reserve on "quota-release-model", whose QuotaPolicy targets two
	// backends of which only the first serves the route: the reserve is charged
	// to both and released from both, so the serving backend ends at the actual
	// fresh input tokens plus the request-time +1, and the other at the
	// request-time +1 alone.
	t.Run("admission reserve released from every target", func(t *testing.T) {
		const (
			model   = "quota-release-model"
			serving = "default/envoy-ai-gateway-quota-ratelimit-testupstream"
			other   = "default/envoy-ai-gateway-quota-ratelimit-testupstream-b"
		)
		client := http.Header{"x-test-client": []string{"client-release-a"}}

		sendWithUsage(model, 1000, 800, http.StatusOK, client)
		requireBackendQuotaUsage(t, serving, model, 201)
		requireBackendQuotaUsage(t, other, model, 1)

		waitForNextUsageEstimatePeriod(t)

		// The reserve is the predicted 200, over the actual 100.
		sendWithUsage(model, 1000, 900, http.StatusOK, client)
		requireBackendQuotaUsage(t, serving, model, 302)
		requireBackendQuotaUsage(t, other, model, 2)
	})

	// A QuotaPolicy change on a route Envoy Gateway has already translated
	// reaches the Envoy route config: "quota-late-reserve-model" is served under
	// a policy with no admission reserve, then the policy gains one, and the
	// route must carry that reserve's charge entry without any other change.
	t.Run("admission reserve added to an already-translated route", func(t *testing.T) {
		const (
			model  = "quota-late-reserve-model"
			policy = "envoy-ai-gateway-quota-ratelimit-late-reserve-policy"
			// Percent 50 keeps this reserve's metadata key apart from the
			// percent-100 reserves the other models' routes already carry.
			reserveKey = "quota_reserve_reserve_fresh_input_token_50"
		)

		sendWithUsage(model, 1000, 800, http.StatusOK, http.Header{"x-test-client": []string{"client-late-a"}})
		requireQuotaUsage(t, model, 201)

		routeConfigs := newEnvoyRouteConfigReader(t, egSelector)
		require.NotContains(t, routeConfigs(), reserveKey)

		patch := `[{"op":"add","path":"/spec/perModelQuotas/0/quota/defaultBucket/admissionReserve",` +
			`"value":{"usageEstimate":"reserve_fresh_input_token","percent":50}}]`
		require.NoError(t, e2elib.Kubectl(t.Context(), "patch", "quotapolicy", policy,
			"-n", "default", "--type=json", "-p", patch).Run())

		require.Eventually(t, func() bool {
			return strings.Contains(routeConfigs(), reserveKey)
		}, 2*time.Minute, time.Second, "the Envoy route config never gained the %s charge entry", reserveKey)
	})
}

// newEnvoyRouteConfigReader port-forwards to the admin port of the Envoy pod
// matching selector, which listens on the pod's loopback only, and returns a
// function reading its dynamic route configs from config_dump.
func newEnvoyRouteConfigReader(t *testing.T, selector string) func() string {
	t.Helper()
	getPod := e2elib.Kubectl(t.Context(), "get", "pod", "-n", e2elib.EnvoyGatewayNamespace,
		"-l", selector, "-o", "jsonpath={.items[0].metadata.name}")
	getPod.Stdout = nil
	pod, err := getPod.Output()
	require.NoError(t, err)
	require.NotEmpty(t, pod)

	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	localPort := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	// t.Context() is canceled before cleanups run, which kills the port-forward.
	cmd := e2elib.Kubectl(t.Context(), "port-forward", "-n", e2elib.EnvoyGatewayNamespace,
		"pod/"+string(pod), fmt.Sprintf("%d:19000", localPort))
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Wait() })

	url := fmt.Sprintf("http://127.0.0.1:%d/config_dump?resource=dynamic_route_configs", localPort)
	httpClient := &http.Client{Timeout: 10 * time.Second}
	return func() string {
		var body []byte
		require.Eventually(t, func() bool {
			resp, getErr := httpClient.Get(url)
			if getErr != nil {
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return false
			}
			body, getErr = io.ReadAll(resp.Body)
			return getErr == nil
		}, 30*time.Second, 500*time.Millisecond, "Envoy admin config_dump was not reachable")
		return string(body)
	}
}

// newChatRequest builds one chat completion request against the test upstream
// with the given total_tokens in the fake response.
func newChatRequest(t *testing.T, addr, modelName string, totalTokens int, headers ...http.Header) *http.Request {
	t.Helper()
	requestBody := fmt.Sprintf(`{"messages":[{"role":"user","content":"Say this is a test"}],"model":"%s"}`, modelName)
	fakeResponseBody := fmt.Sprintf(
		`{"choices":[{"message":{"content":"This is a test.","role":"assistant"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":%d}}`,
		totalTokens,
	)
	req, err := http.NewRequest(http.MethodPut, addr+"/v1/chat/completions", strings.NewReader(requestBody))
	require.NoError(t, err)
	req.Header.Set(testupstreamlib.ResponseBodyHeaderKey, base64.StdEncoding.EncodeToString([]byte(fakeResponseBody)))
	req.Header.Set(testupstreamlib.ExpectedPathHeaderKey, base64.StdEncoding.EncodeToString([]byte("/v1/chat/completions")))
	req.Header.Set("Host", "openai.com")
	for _, h := range headers {
		for k, vals := range h {
			for _, v := range vals {
				req.Header.Set(k, v)
			}
		}
	}
	return req
}

// redisExec runs a redis-cli command on the Redis pod and returns the output.
func redisExec(t *testing.T, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{
		"exec", "-n", "redis-system",
		"deploy/redis", "--",
		"redis-cli",
	}, args...)
	cmd := exec.CommandContext(t.Context(), "kubectl", cmdArgs...)
	out, err := cmd.Output()
	require.NoError(t, err, "redis-cli %v failed", args)
	return strings.TrimSpace(string(out))
}

// flushQuotaKeys deletes all quota-related keys from Redis.
func flushQuotaKeys(t *testing.T) {
	t.Helper()
	keys := redisExec(t, "KEYS", "ai-gateway-quota_*")
	if keys == "" {
		return
	}
	for _, key := range strings.Split(keys, "\n") {
		key = strings.TrimSpace(key)
		if key != "" {
			redisExec(t, "DEL", key)
		}
	}
}

// getQuotaUsage retrieves the current quota counter value from Redis for the given model.
// The key pattern is: ai-gateway-quota_backend_name_{backend}_model_name_override_{model}_{timestamp}
func getQuotaUsage(t *testing.T, modelName string) (int, bool) {
	t.Helper()
	pattern := fmt.Sprintf("ai-gateway-quota_backend_name_*_model_name_override_%s_*", modelName)
	keys := redisExec(t, "KEYS", pattern)
	if keys == "" {
		return 0, false
	}
	// Use the first matching key (there should be exactly one per model per time window).
	key := strings.Split(keys, "\n")[0]
	key = strings.TrimSpace(key)
	val := redisExec(t, "GET", key)
	if val == "" {
		return 0, false
	}
	n, err := strconv.Atoi(val)
	require.NoError(t, err, "failed to parse quota counter value: %q", val)
	return n, true
}

// requireBackendQuotaUsage polls Redis until the quota counter of the given
// backend ("namespace/name") and model reaches the expected value.
func requireBackendQuotaUsage(t *testing.T, backend, modelName string, expected int) {
	t.Helper()
	pattern := fmt.Sprintf("ai-gateway-quota_backend_name_%s_model_name_override_%s_*", backend, modelName)
	require.Eventually(t, func() bool {
		keys := redisExec(t, "KEYS", pattern)
		if keys == "" {
			return false
		}
		val := redisExec(t, "GET", strings.TrimSpace(strings.Split(keys, "\n")[0]))
		n, err := strconv.Atoi(val)
		return err == nil && n == expected
	}, 30*time.Second, 500*time.Millisecond,
		"quota counter of backend %q for model %q did not reach expected value %d", backend, modelName, expected)
}

// requireQuotaUsage polls Redis until the quota counter for the given model reaches
// the expected value. The stream-done rate limit entry updates Redis asynchronously
// after the response, so polling is necessary.
func requireQuotaUsage(t *testing.T, modelName string, expected int) {
	t.Helper()
	require.Eventually(t, func() bool {
		usage, ok := getQuotaUsage(t, modelName)
		return ok && usage == expected
	}, 30*time.Second, 500*time.Millisecond,
		"quota counter for model %q did not reach expected value %d", modelName, expected)
}

// usageEstimatePeriod is the usageEstimatePeriod of the GatewayConfig in
// testdata/backend_quota_ratelimit.yaml.
const usageEstimatePeriod = 5 * time.Second

// waitForNextUsageEstimatePeriod waits until a second into the next usage estimate
// period, which completes the period of the responses received so far. Periods are
// aligned on the clock, which the test and the kind cluster share.
func waitForNextUsageEstimatePeriod(t *testing.T) {
	t.Helper()
	now := time.Now()
	time.Sleep(now.Truncate(usageEstimatePeriod).Add(usageEstimatePeriod + time.Second).Sub(now))
}

// newChatRequestWithUsage builds one chat completion request against the test
// upstream whose fake response reports promptTokens input tokens, of which
// cachedTokens were served from the prefix cache.
func newChatRequestWithUsage(t *testing.T, addr, modelName string, promptTokens, cachedTokens int, headers ...http.Header) *http.Request {
	t.Helper()
	requestBody := fmt.Sprintf(`{"messages":[{"role":"user","content":"Say this is a test"}],"model":"%s"}`, modelName)
	fakeResponseBody := fmt.Sprintf(
		`{"choices":[{"message":{"content":"This is a test.","role":"assistant"}}],"usage":{"prompt_tokens":%d,"completion_tokens":1,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`,
		promptTokens, promptTokens+1, cachedTokens,
	)
	req, err := http.NewRequest(http.MethodPut, addr+"/v1/chat/completions", strings.NewReader(requestBody))
	require.NoError(t, err)
	req.Header.Set(testupstreamlib.ResponseBodyHeaderKey, base64.StdEncoding.EncodeToString([]byte(fakeResponseBody)))
	req.Header.Set(testupstreamlib.ExpectedPathHeaderKey, base64.StdEncoding.EncodeToString([]byte("/v1/chat/completions")))
	req.Header.Set("Host", "openai.com")
	for _, h := range headers {
		for k, vals := range h {
			for _, v := range vals {
				req.Header.Set(k, v)
			}
		}
	}
	return req
}
