// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package dataplanemcp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/json"
)

// A backend that pings its client sessions (mark3labs/mcp-go's heartbeat, the
// go-sdk's KeepAlive) sends `ping` requests with numeric IDs on the standalone
// GET stream. The client answers each with a response echoing the ID it
// received, and the gateway routes that response back to the backend.

const backendKeepAlive = time.Second

// TestMCPBackendPingRoundTrip drives the backend ping over raw HTTP: it reads
// the backend's ping off the GET stream and answers it the way any compliant
// client does, echoing the ID exactly.
func TestMCPBackendPingRoundTrip(t *testing.T) {
	m := requireNewMCPEnvWithBackendKeepAlive(t, nil, backendKeepAlive, false, 1200*time.Second, defaultMCPPath)

	sessionID := rawInitialize(t, m.baseURL)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ping := rawAwaitBackendPing(ctx, t, m.baseURL, sessionID)
	t.Logf("backend ping as delivered to the client: %s", ping)

	var id any
	require.NoError(t, json.Unmarshal(ping["id"], &id))
	reply, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
	require.NoError(t, err)
	status, body := rawPost(t, m.baseURL, sessionID, reply)
	require.Equalf(t, http.StatusAccepted, status,
		"gateway refused the client's reply to the backend ping (id %s): %s", ping["id"], body)
}

// TestMCPBackendPingOpencode connects the opencode CLI to the gateway while
// the backend pings, and records every message opencode posts. It is skipped
// when opencode is not on PATH.
func TestMCPBackendPingOpencode(t *testing.T) {
	opencode, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode not on PATH")
	}
	m := requireNewMCPEnvWithBackendKeepAlive(t, nil, backendKeepAlive, false, 1200*time.Second, defaultMCPPath)
	rec := startRecordingProxy(t, m.baseURL)

	home := t.TempDir()
	cfg := filepath.Join(home, "opencode.json")
	require.NoError(t, os.WriteFile(cfg, fmt.Appendf(nil,
		`{"$schema":"https://opencode.ai/config.json","mcp":{"gw":{"type":"remote","url":%q,"oauth":false}}}`,
		rec.url), 0o600))
	project := filepath.Join(home, "project")
	require.NoError(t, os.Mkdir(project, 0o700))
	env := append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"OPENCODE_CONFIG="+cfg,
	)
	run := func(args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, opencode, args...)
		cmd.Dir, cmd.Env = project, env
		out, err := cmd.CombinedOutput()
		t.Logf("opencode %s: err=%v\n%s", strings.Join(args, " "), err, out)
	}
	// opencode serves CLI commands from a background service on a fixed port;
	// a private port keeps this run off any opencode the developer is running.
	run("service", "set", "port", fmt.Sprint(freePort(t)))
	t.Cleanup(func() { run("service", "stop") })
	run("mcp", "list")

	require.Eventually(t, func() bool { return rec.backendPingReplies() >= 1 }, 30*time.Second, 200*time.Millisecond,
		"opencode never answered a backend ping; posts seen:\n%s", rec.dump())
	for _, p := range rec.posts() {
		require.Lessf(t, p.status, 400, "gateway refused an opencode message:\n%s", rec.dump())
	}
}

func rawHeaders(req *http.Request, sessionID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
}

func rawPost(t *testing.T, baseURL, sessionID string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL, bytes.NewReader(body))
	require.NoError(t, err)
	rawHeaders(req, sessionID)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func rawInitialize(t *testing.T, baseURL string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL, strings.NewReader(
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`))
	require.NoError(t, err)
	rawHeaders(req, "")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sessionID := resp.Header.Get("Mcp-Session-Id")
	require.NotEmpty(t, sessionID)
	status, body := rawPost(t, baseURL, sessionID, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	require.Equalf(t, http.StatusAccepted, status, "notifications/initialized: %s", body)
	return sessionID
}

// rawAwaitBackendPing opens the GET stream and returns the first ping that
// did not originate from the gateway's own heartbeat.
func rawAwaitBackendPing(ctx context.Context, t *testing.T, baseURL, sessionID string) map[string]json.RawMessage {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	require.NoError(t, err)
	rawHeaders(req, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, 2, resp.StatusCode/100, "GET stream status %d", resp.StatusCode)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var msg map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &msg) != nil || string(msg["method"]) != `"ping"` {
			continue
		}
		if strings.HasPrefix(string(msg["id"]), `"aigw-server-to-client-ping`) {
			continue
		}
		return msg
	}
	require.FailNow(t, "GET stream ended without a backend ping", "scan error: %v", sc.Err())
	return nil
}

type recordedPost struct {
	status    int
	req, resp string
}

type recordingProxy struct {
	url string
	mu  sync.Mutex
	log []recordedPost
}

// startRecordingProxy fronts the gateway and records each POST with its status.
func startRecordingProxy(t *testing.T, gatewayURL string) *recordingProxy {
	t.Helper()
	target, err := url.Parse(gatewayURL)
	require.NoError(t, err)
	rp := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: target.Scheme, Host: target.Host})
	rp.FlushInterval = -1
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &recordingProxy{url: fmt.Sprintf("http://%s%s", l.Addr(), target.Path)}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			rp.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		rp.ServeHTTP(sw, r)
		p.mu.Lock()
		p.log = append(p.log, recordedPost{status: sw.status, req: string(body), resp: sw.errBody.String()})
		p.mu.Unlock()
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return p
}

func (p *recordingProxy) posts() []recordedPost {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedPost(nil), p.log...)
}

// backendPingReplies counts posted responses that answer a ping the gateway
// did not originate.
func (p *recordingProxy) backendPingReplies() int {
	n := 0
	for _, post := range p.posts() {
		var msg map[string]json.RawMessage
		if json.Unmarshal([]byte(post.req), &msg) != nil || msg["method"] != nil || msg["id"] == nil {
			continue
		}
		if !strings.HasPrefix(string(msg["id"]), `"aigw-`) {
			n++
		}
	}
	return n
}

func (p *recordingProxy) dump() string {
	var b strings.Builder
	for _, post := range p.posts() {
		fmt.Fprintf(&b, "  POST -> %d %s %s\n", post.status, post.req, post.resp)
	}
	return b.String()
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	errBody bytes.Buffer
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status >= 400 {
		s.errBody.Write(b)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
