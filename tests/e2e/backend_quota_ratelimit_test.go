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
