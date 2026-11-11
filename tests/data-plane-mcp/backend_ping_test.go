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
//
// The go-sdk KeepAlive closes the backend session at the first ping that gets
// no matching response within half the interval, and pings no more. A ping
// arriving after the client answered the previous one therefore proves the
// backend matched that answer.
//
// TestMCPBackendPingRoundTrip is the regression guard. TestMCPBackendPingOpencode
// checks the same contract with the opencode CLI, the client that surfaced it;
// `make install.opencode` installs the pinned version it runs.

const (
	// backendKeepAlive leaves the go-sdk a 2s ping timeout (interval/2).
	backendKeepAlive = 4 * time.Second
	// pingingBackend is the backend that runs testmcp with KeepAlive set.
	pingingBackend = "default-mcp-backend"
	// backendPingsToAnswer is how many backend pings each test answers: the
	// arrival of every ping after the first proves the previous answer matched.
	backendPingsToAnswer = 3
)

// TestMCPBackendPingRoundTrip drives the backend ping over raw HTTP: it reads
// each backend ping off the GET stream and answers it the way any compliant
// client does, echoing the ID byte for byte.
func TestMCPBackendPingRoundTrip(t *testing.T) {
	m := requireNewMCPEnvWithBackendKeepAlive(t, nil, backendKeepAlive, false, 1200*time.Second, defaultMCPPath)

	sessionID := rawInitialize(t, m.baseURL)

	ctx, cancel := context.WithTimeout(t.Context(), backendPingsToAnswer*backendKeepAlive+30*time.Second)
	defer cancel()
	pings, scanErr := rawBackendPings(ctx, t, m.baseURL, sessionID)

	for i := range backendPingsToAnswer {
		var ping map[string]json.RawMessage
		select {
		case p, ok := <-pings:
			if !ok {
				require.FailNowf(t, "backend ping stream closed",
					"the GET stream ended before ping %d (read error: %v); the backend closes its session when an earlier answer is not matched",
					i+1, <-scanErr)
			}
			ping = p
		case <-ctx.Done():
			require.FailNowf(t, "backend ping missing",
				"ping %d never arrived; the backend closes its session when an earlier answer is not matched", i+1)
		}
		t.Logf("backend ping %d as delivered to the client: id=%s", i+1, ping["id"])

		var id string
		require.NoErrorf(t, json.Unmarshal(ping["id"], &id), "backend ping id %s is not the gateway's rewritten string", ping["id"])
		require.Truef(t, strings.HasSuffix(id, "__i__"+pingingBackend), "backend ping id %q does not route to %s", id, pingingBackend)

		status, body := rawPost(t, m.baseURL, sessionID, fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"result":{}}`, ping["id"]))
		require.Equalf(t, http.StatusAccepted, status,
			"gateway refused the client's reply to backend ping %d (id %s): %s", i+1, ping["id"], body)
	}

	// A numeric reply ID cannot be one the gateway rewrote, so the gateway refuses it before routing.
	status, body := rawPost(t, m.baseURL, sessionID, []byte(`{"jsonrpc":"2.0","id":999,"result":{}}`))
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, body, "invalid response ID type")
	// A rewritten ID naming a backend outside the route has nowhere to go.
	status, body = rawPost(t, m.baseURL, sessionID, []byte(`{"jsonrpc":"2.0","id":"1__i__nonexistent","result":{}}`))
	require.Equal(t, http.StatusNotFound, status)
	require.Contains(t, body, "unknown backend nonexistent")
}

// TestMCPBackendPingOpencode connects the opencode CLI to the gateway while the
// backend pings, and records every message opencode posts. It runs the binary
// named by AIGW_OPENCODE_BIN, else opencode from PATH. Without either it skips,
// unless AIGW_REQUIRE_OPENCODE=1, which CI sets.
func TestMCPBackendPingOpencode(t *testing.T) {
	opencode := requireOpencode(t)
	m := requireNewMCPEnvWithBackendKeepAlive(t, nil, backendKeepAlive, false, 1200*time.Second, defaultMCPPath)
	rec := startRecordingProxy(t, m.baseURL)

	home := t.TempDir()
	cfg := filepath.Join(home, "opencode.json")
	require.NoError(t, os.WriteFile(cfg, fmt.Appendf(nil,
		`{"$schema":"https://opencode.ai/config.json","mcp":{"gw":{"type":"remote","url":%q,"oauth":false}}}`,
		rec.url), 0o600))
	project := filepath.Join(home, "project")
	require.NoError(t, os.Mkdir(project, 0o700))
	env := opencodeEnv(home, cfg)
	run := func(ctx context.Context, args ...string) error {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, opencode, args...)
		cmd.Dir, cmd.Env = project, env
		// The background service opencode starts may hold the output pipes open.
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.CombinedOutput()
		t.Logf("opencode %s: err=%v\n%s", strings.Join(args, " "), err, out)
		return err
	}

	require.NoError(t, run(t.Context(), "--version"))
	// opencode serves CLI commands from a background service on a fixed port;
	// a private port keeps this run off any opencode the developer is running.
	require.NoError(t, run(t.Context(), "service", "set", "port", fmt.Sprint(freePort(t))))
	t.Cleanup(func() { _ = run(context.Background(), "service", "stop") })
	require.NoError(t, run(t.Context(), "mcp", "list"))

	require.Eventuallyf(t, func() bool { return len(rec.backendPingReplies()) >= backendPingsToAnswer },
		backendPingsToAnswer*backendKeepAlive+30*time.Second, 200*time.Millisecond,
		"opencode answered fewer than %d backend pings; posts seen:\n%s", backendPingsToAnswer, rec.dump())
	for _, p := range rec.backendPingReplies() {
		require.Lessf(t, p.status, 400, "gateway refused opencode's reply to a backend ping:\n%s", rec.dump())
	}
}

func requireOpencode(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("AIGW_OPENCODE_BIN"); bin != "" {
		if info, err := os.Stat(bin); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return bin
		}
		if os.Getenv("AIGW_REQUIRE_OPENCODE") == "1" {
			require.FailNowf(t, "pinned opencode missing",
				"AIGW_OPENCODE_BIN=%s does not exist or is not executable; run `make install.opencode`", bin)
		}
		t.Logf("AIGW_OPENCODE_BIN=%s does not exist or is not executable; falling back to PATH", bin)
	}
	if bin, err := exec.LookPath("opencode"); err == nil {
		return bin
	}
	if os.Getenv("AIGW_REQUIRE_OPENCODE") == "1" {
		require.FailNow(t, "opencode not found", "AIGW_REQUIRE_OPENCODE=1 and neither AIGW_OPENCODE_BIN nor PATH has opencode; run `make install.opencode`")
	}
	t.Skip("opencode not installed; run `make install.opencode`")
	return ""
}

// opencodeEnv confines opencode to home: the developer's OPENCODE_* settings
// are dropped and every config, data, state and cache directory points there.
func opencodeEnv(home, cfg string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OPENCODE_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"OPENCODE_CONFIG="+cfg,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	)
}

// rawClient bounds each raw POST, so a wedged gateway fails the test within its
// own budget. The GET stream is bound to the test's context instead.
var rawClient = &http.Client{Timeout: 30 * time.Second}

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
	resp, err := rawClient.Do(req)
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
	resp, err := rawClient.Do(req)
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

// rawBackendPings opens the GET stream and delivers every ping on it that did
// not originate from the gateway's own heartbeat. The ping channel closes with
// the stream, after the error channel has received the scanner's error (nil at
// a clean end of stream).
func rawBackendPings(ctx context.Context, t *testing.T, baseURL, sessionID string) (<-chan map[string]json.RawMessage, <-chan error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	require.NoError(t, err)
	rawHeaders(req, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, 2, resp.StatusCode/100, "GET stream status %d", resp.StatusCode)

	pings := make(chan map[string]json.RawMessage)
	scanErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		defer func() {
			scanErr <- sc.Err()
			close(pings)
		}()
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var msg map[string]json.RawMessage
			if json.Unmarshal([]byte(data), &msg) != nil || string(msg["method"]) != `"ping"` {
				continue
			}
			// IDs the gateway originates (its heartbeat ping and tools-changed requests,
			// mcpproxy's envoyAIGatewayServerToClient*RequestIDPrefix) start with aigw-;
			// rewritten backend IDs never do.
			if strings.HasPrefix(string(msg["id"]), `"aigw-`) {
				continue
			}
			select {
			case pings <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	return pings, scanErr
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

// backendPingReplies returns the posted responses that answer a ping the
// gateway did not originate.
func (p *recordingProxy) backendPingReplies() []recordedPost {
	var replies []recordedPost
	for _, post := range p.posts() {
		var msg map[string]json.RawMessage
		if json.Unmarshal([]byte(post.req), &msg) != nil || msg["method"] != nil || msg["id"] == nil {
			continue
		}
		// Replies to the gateway's own requests carry its aigw- prefixed IDs.
		if !strings.HasPrefix(string(msg["id"]), `"aigw-`) {
			replies = append(replies, post)
		}
	}
	return replies
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
