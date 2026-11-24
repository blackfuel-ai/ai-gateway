// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package dataplanemcp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	_ "embed"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/json"
	internaltesting "github.com/envoyproxy/ai-gateway/internal/testing"
	"github.com/envoyproxy/ai-gateway/tests/internal/testmcp"
)

//go:embed oauth_issuers.yaml.tmpl
var oauthIssuersConfig string

const (
	oauthIssuersAddr     = "http://127.0.0.1:1975"
	oauthIssuersAudience = "https://api.example.com/mcp"
	oauthIssuersSubject  = "user-1"
	// oauthIssuersScope is the scope the authorization rule of the route requires to call the echo tool.
	oauthIssuersScope = "echo"
)

// oauthIssuer is an authorization server with its own signing key.
type oauthIssuer struct {
	URL string
	key *rsa.PrivateKey
}

// newOAuthIssuer starts an authorization server. Its metadata is served unless withMetadata is false.
func newOAuthIssuer(t *testing.T, withMetadata bool) *oauthIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	issuer := &oauthIssuer{key: key}
	respond := func(w http.ResponseWriter, body any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	mux := http.NewServeMux()
	if withMetadata {
		mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
			respond(w, map[string]any{
				"issuer":                 issuer.URL,
				"authorization_endpoint": issuer.URL + "/auth",
				"token_endpoint":         issuer.URL + "/token",
				"jwks_uri":               issuer.URL + "/certs",
			})
		})
	}
	mux.HandleFunc("/certs", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig",
			// Every issuer uses the same key ID, so that only the key itself tells them apart.
			"kid": "key",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	issuer.URL = server.URL
	return issuer
}

// token returns a token of the issuer that the route accepts, with some claims replaced. A nil value removes the claim.
func (i *oauthIssuer) token(t *testing.T, replaced jwt.MapClaims) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":   i.URL,
		"aud":   oauthIssuersAudience,
		"sub":   oauthIssuersSubject,
		"scope": oauthIssuersScope,
		"exp":   time.Now().Add(30 * time.Minute).Unix(),
	}
	for name, value := range replaced {
		if value == nil {
			delete(claims, name)
			continue
		}
		claims[name] = value
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "key"
	signed, err := token.SignedString(i.key)
	require.NoError(t, err)
	return signed
}

// bearerTransport adds the token, and a client-supplied claim header that must never reach the backend.
type bearerTransport struct{ token string }

func (b *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("x-user-sub", "forged")
	return http.DefaultTransport.RoundTrip(req)
}

// startOAuthIssuersGateway runs the gateway with a route that accepts several issuers. The controller, Envoy Gateway
// and Envoy all take part, so that the providers are combined as they are in a cluster.
func startOAuthIssuersGateway(t *testing.T, data map[string]any) {
	t.Helper()
	aigwBin, err := internaltesting.BuildGoBinaryOnDemand("AIGW_BIN", "aigw", "./cmd/aigw")
	require.NoError(t, err)
	tmpl, err := template.New("config").Parse(oauthIssuersConfig)
	require.NoError(t, err)
	var config strings.Builder
	require.NoError(t, tmpl.Execute(&config, data))
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(config.String()), 0o600))

	buffers := internaltesting.DumpLogsOnFail(t, "aigw stdout", "aigw stderr")
	cmd := exec.CommandContext(context.Background(), aigwBin, "run", "--debug", configPath)
	cmd.Stdout, cmd.Stderr = buffers[0], buffers[1]
	// Its own process group, so that Envoy is stopped with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	})
}

// TestMCPOAuthAdditionalIssuers checks against a real Envoy what a route with additional issuers accepts and refuses.
func TestMCPOAuthAdditionalIssuers(t *testing.T) {
	internaltesting.ClearTestEnv(t)

	primary := newOAuthIssuer(t, true)
	additional := newOAuthIssuer(t, true)
	// Its metadata is not served, so the controller cannot find its keys.
	withoutMetadata := newOAuthIssuer(t, false)
	// Its keys are configured, but nothing answers there.
	unreachableKeys := newOAuthIssuer(t, false)
	// Not listed by the route.
	stranger := newOAuthIssuer(t, true)

	// The backend refuses any call without the claim header of the verified token.
	t.Setenv("TEST_EXPECTED_CLAIM_HEADERS", "x-user-sub="+oauthIssuersSubject)
	ports := internaltesting.RequireRandomPorts(t, 2)
	server, _ := testmcp.NewServer(&testmcp.Options{Port: ports[0], WriteTimeout: time.Minute, DisableLog: true})
	t.Cleanup(func() { _ = server.Close() })

	startOAuthIssuersGateway(t, map[string]any{
		"Primary":            primary.URL,
		"Additional":         additional.URL,
		"WithoutMetadata":    withoutMetadata.URL,
		"UnreachableKeys":    unreachableKeys.URL,
		"UnreachableJWKSURI": fmt.Sprintf("http://127.0.0.1:%d/certs", ports[1]),
		"Audience":           oauthIssuersAudience,
		"Scope":              oauthIssuersScope,
		"BackendPort":        ports[0],
	})

	// initialize returns the status of an initialize request with the tokens in the header and the query, if any.
	initialize := func(headerToken, queryToken string) (int, error) {
		target := oauthIssuersAddr + "/mcp"
		if queryToken != "" {
			target += "?access_token=" + queryToken
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if headerToken != "" {
			req.Header.Set("Authorization", "Bearer "+headerToken)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}

	internaltesting.RequireEventuallyNoError(t, func() error {
		status, err := initialize(primary.token(t, nil), "")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("unexpected status %d", status)
		}
		return nil
	}, 120*time.Second, time.Second, "the gateway never accepted a token of issuer")

	t.Run("verification", func(t *testing.T) {
		for _, tc := range []struct {
			name                    string
			headerToken, queryToken string
			want                    int
		}{
			{name: "issuer", headerToken: primary.token(t, nil), want: http.StatusOK},
			{name: "additional issuer", headerToken: additional.token(t, nil), want: http.StatusOK},

			{name: "no token", want: http.StatusUnauthorized},
			{name: "not a token", headerToken: "invalid", want: http.StatusUnauthorized},
			{name: "issuer that is not accepted", headerToken: stranger.token(t, nil), want: http.StatusUnauthorized},
			{name: "no issuer, key of issuer", headerToken: primary.token(t, jwt.MapClaims{"iss": nil}), want: http.StatusUnauthorized},
			{name: "no issuer, key of the additional issuer", headerToken: additional.token(t, jwt.MapClaims{"iss": nil}), want: http.StatusUnauthorized},

			// The keys of an issuer only verify its own tokens.
			{name: "issuer, key of the additional issuer", headerToken: additional.token(t, jwt.MapClaims{"iss": primary.URL}), want: http.StatusUnauthorized},
			{name: "additional issuer, key of issuer", headerToken: primary.token(t, jwt.MapClaims{"iss": additional.URL}), want: http.StatusUnauthorized},
			{name: "issuer, key of a stranger", headerToken: stranger.token(t, jwt.MapClaims{"iss": primary.URL}), want: http.StatusUnauthorized},
			{name: "additional issuer, key of a stranger", headerToken: stranger.token(t, jwt.MapClaims{"iss": additional.URL}), want: http.StatusUnauthorized},

			// A token must pass in full for its issuer.
			{name: "issuer, other audience", headerToken: primary.token(t, jwt.MapClaims{"aud": "other"}), want: http.StatusForbidden},
			{name: "additional issuer, other audience", headerToken: additional.token(t, jwt.MapClaims{"aud": "other"}), want: http.StatusForbidden},
			{name: "issuer, no audience", headerToken: primary.token(t, jwt.MapClaims{"aud": nil}), want: http.StatusForbidden},
			{name: "additional issuer, no audience", headerToken: additional.token(t, jwt.MapClaims{"aud": nil}), want: http.StatusForbidden},
			{name: "issuer, expired", headerToken: primary.token(t, jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}), want: http.StatusUnauthorized},
			{name: "additional issuer, expired", headerToken: additional.token(t, jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}), want: http.StatusUnauthorized},

			// Issuers without keys are refused and never fall back to the keys of another one.
			{name: "issuer without metadata", headerToken: withoutMetadata.token(t, nil), want: http.StatusUnauthorized},
			{name: "issuer without metadata, key of issuer", headerToken: primary.token(t, jwt.MapClaims{"iss": withoutMetadata.URL}), want: http.StatusUnauthorized},
			{name: "issuer with unreachable keys", headerToken: unreachableKeys.token(t, nil), want: http.StatusUnauthorized},
			{name: "issuer with unreachable keys, key of issuer", headerToken: primary.token(t, jwt.MapClaims{"iss": unreachableKeys.URL}), want: http.StatusUnauthorized},

			// One provider must accept every token of the request: a valid one does not vouch for another.
			{name: "valid header, stranger in the query", headerToken: primary.token(t, nil), queryToken: stranger.token(t, nil), want: http.StatusUnauthorized},
			{name: "stranger in the header, valid query", headerToken: stranger.token(t, nil), queryToken: primary.token(t, nil), want: http.StatusUnauthorized},
			{name: "stranger in the header, valid query of the additional issuer", headerToken: stranger.token(t, nil), queryToken: additional.token(t, nil), want: http.StatusUnauthorized},
			{name: "valid tokens of two issuers", headerToken: primary.token(t, nil), queryToken: additional.token(t, nil), want: http.StatusUnauthorized},
		} {
			t.Run(tc.name, func(t *testing.T) {
				status, err := initialize(tc.headerToken, tc.queryToken)
				require.NoError(t, err)
				require.Equal(t, tc.want, status)
			})
		}
	})

	connect := func(t *testing.T, transport *bearerTransport) *mcp.ClientSession {
		t.Helper()
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint: oauthIssuersAddr + "/mcp",
			HTTPClient: &http.Client{
				Transport: transport,
				Timeout:   20 * time.Second,
			},
			DisableStandaloneSSE: true,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	echo := func(t *testing.T, session *mcp.ClientSession) error {
		t.Helper()
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "backend__" + testmcp.ToolEcho.Tool.Name, Arguments: testmcp.ToolEchoArgs{Text: "hello"},
		})
		if err != nil {
			return err
		}
		require.False(t, result.IsError)
		return nil
	}

	for name, issuer := range map[string]*oauthIssuer{"issuer": primary, "additional issuer": additional} {
		t.Run("claim headers and authorization rules of a token of "+name, func(t *testing.T) {
			require.NoError(t, echo(t, connect(t, &bearerTransport{token: issuer.token(t, nil)})))

			withoutScope := connect(t, &bearerTransport{token: issuer.token(t, jwt.MapClaims{"scope": "other"})})
			require.ErrorContains(t, echo(t, withoutScope), "Forbidden")
		})
	}

	t.Run("published metadata", func(t *testing.T) {
		for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server/mcp"} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, oauthIssuersAddr+path, nil)
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, string(body), primary.URL)
			for _, other := range []*oauthIssuer{additional, withoutMetadata, unreachableKeys} {
				require.NotContains(t, string(body), other.URL)
			}
		}
	})

	// The subject is not qualified by the issuer, as the API documents.
	t.Run("a session goes on with a token of another issuer for the same subject only", func(t *testing.T) {
		transport := &bearerTransport{token: primary.token(t, nil)}
		session := connect(t, transport)
		require.NoError(t, echo(t, session))
		transport.token = additional.token(t, nil)
		require.NoError(t, echo(t, session))
		transport.token = additional.token(t, jwt.MapClaims{"sub": "user-2"})
		require.Error(t, echo(t, session))
	})
}
