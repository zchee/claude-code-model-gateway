// Copyright 2026 The claude-code-model-gateway Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build process && (darwin || linux)

package serveworker_test

import (
	"context"
	json "encoding/json/v2"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// unanswered returns a fixture handler that takes the connection over and never answers properly:
// with switched it writes a protocol switch and keeps the connection open and silent, otherwise it
// closes the connection, as an endpoint that could not be reached. Held connections go to held.
func unanswered(held *heldConns, switched bool, observed *signal) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		if observed != nil {
			defer observed.fire()
		}
		if !switched {
			_ = conn.Close()
			return
		}
		held.add(conn)
		_, _ = io.WriteString(conn, switchReply)
	}
}

// stopReturns stops the worker and fails the test when Run does not return within the failure bound.
func stopReturns(t *testing.T, w *workerRun, what string) {
	t.Helper()
	w.cancel()
	if !w.returned() {
		t.Fatalf("the stopped worker did not return while %s", what)
	}
	if w.err != nil {
		t.Errorf("Run: %v", w.err)
	}
}

// TestGrokCredentialSwitchedResponse decides what a Grok credential refresh makes of an issuer that
// answers its discovery or its token exchange with a protocol switch and then stays silent: the
// refresh fails as it does for an issuer that could not be reached, the client gets the same answer,
// and a worker stopped afterwards returns without waiting for the socket's inactivity limit.
func TestGrokCredentialSwitchedResponse(t *testing.T) {
	tests := map[string]struct {
		// switched names the issuer requests answered with a protocol switch; the others are answered
		// normally, except in the unreachable row, where every request is closed unanswered.
		discoverySwitched, exchangeSwitched, unreachable bool
	}{
		"error: an issuer that closes its connections without answering fails the refresh":              {unreachable: true},
		"error: a discovery answered by a protocol switch fails the refresh as an unreachable one":      {discoverySwitched: true, exchangeSwitched: true},
		"error: a token exchange answered by a protocol switch fails the refresh as an unreachable one": {exchangeSwitched: true},
	}
	want := map[string]any{"status": float64(http.StatusUnauthorized), "type": "authentication_error", "message": "model-gateway: Grok CLI auth refresh failed. Run `grok` and log in again."}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, _ := sandbox(t)
			var held heldConns
			defer held.close()
			mux := http.NewServeMux()
			discovery := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"token_endpoint":"http://`+r.Host+`/oauth/token"}`)
			})
			if tt.unreachable || tt.discoverySwitched {
				discovery = unanswered(&held, tt.discoverySwitched, nil)
			}
			mux.Handle("GET /.well-known/openid-configuration", discovery)
			// The token endpoint is also the fallback a failed discovery uses.
			mux.Handle("POST /oauth/token", unanswered(&held, tt.exchangeSwitched, nil))
			writeExpiredGrokAuth(t, env, env.StartUpstream(t, mux))
			env["CODEX_GATEWAY_GROK_ENDPOINT"] = env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_grok","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			})) + "/v1/responses"
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)

			// The deadline only aborts a wait for an answer that should already have come.
			ctx, cancel := context.WithTimeout(t.Context(), failureBound)
			defer cancel()
			tr := &http.Transport{DisableKeepAlives: true}
			defer tr.CloseIdleConnections()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+w.public+"/v1/messages", strings.NewReader(`{"model":"claude-grok-4.7[1m]","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if resp, err := tr.RoundTrip(req); err != nil {
				t.Errorf("the Grok request got no answer: %v", err)
			} else {
				var doc struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				decodeErr := json.UnmarshalRead(resp.Body, &doc)
				_ = resp.Body.Close()
				got := map[string]any{"status": float64(resp.StatusCode), "type": doc.Error.Type, "message": doc.Error.Message}
				if decodeErr != nil {
					got["decode error"] = decodeErr.Error()
				}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Errorf("answer to a Grok request whose credential refresh failed (-want +got):\n%s", diff)
				}
			}
			stopReturns(t, w, "an issuer held a switched connection open")
		})
	}
}

// catalogError reads the failure the last Anthropic catalog refresh recorded.
func catalogError(t *testing.T, state string) any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(state, "anthropic-models.json"))
	if err != nil {
		t.Fatalf("no Anthropic catalog state was recorded: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("Anthropic catalog state: %v", err)
	}
	return doc["error"]
}

// TestCatalogSwitchedResponse decides what the model catalog makes of an upstream that answers with
// a protocol switch and then stays silent: an Anthropic catalog page fails as a request that could
// not be completed, as for an upstream that closes without answering; a proxy listing fails and its
// connection is closed; and a worker stopped afterwards returns.
func TestCatalogSwitchedResponse(t *testing.T) {
	tests := map[string]struct {
		listing, switched bool
	}{
		"error: an Anthropic catalog page closed without an answer records a failed request":                  {},
		"error: an Anthropic catalog page answered by a protocol switch records a failed request":             {switched: true},
		"error: a proxy listing answered by a protocol switch is a failed listing whose connection is closed": {listing: true, switched: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, state := sandbox(t)
			var held heldConns
			defer held.close()
			listed := newSignal()
			if tt.listing {
				env["CODEX_GATEWAY_PROXY_PORT"] = portOf(t, env.StartUpstream(t, unanswered(&held, true, listed)))
			} else {
				env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = env.StartUpstream(t, unanswered(&held, tt.switched, nil))
			}
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)

			if tt.listing {
				// The startup refresh reads the listing at once.
				listed.wait(t, "the startup refresh to read the proxy listing")
				conns := held.snapshot()
				if len(conns) == 0 {
					t.Fatal("the listing request reached the fixture but no connection was held")
				}
				closedByPeer(t, conns[0], "the switched listing connection")
				stopReturns(t, w, "a listing connection had been switched")
				return
			}
			// A cold catalog is refreshed for a listing request that carries a credential, and the
			// request waits for that refresh, which records its outcome before it ends.
			ctx, cancel := context.WithTimeout(t.Context(), failureBound)
			defer cancel()
			tr := &http.Transport{DisableKeepAlives: true}
			defer tr.CloseIdleConnections()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+w.public+"/v1/models", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Api-Key", "fixture-catalog-key")
			if resp, err := tr.RoundTrip(req); err != nil {
				t.Errorf("the listing request waiting for the catalog refresh got no answer: %v", err)
			} else {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if diff := gocmp.Diff(any("catalog request failed"), catalogError(t, state)); diff != "" {
					t.Errorf("recorded catalog failure (-want +got):\n%s", diff)
				}
			}
			stopReturns(t, w, "a catalog connection had been switched")
		})
	}
}

// TestReadinessSwitchedResponse decides what the shared readiness check makes of a proxy whose
// /v1/models answers with a protocol switch and then stays silent: the check completes with the
// proxy not answering, as for a proxy that closes without answering, and a worker stopped meanwhile
// returns.
func TestReadinessSwitchedResponse(t *testing.T) {
	tests := map[string]struct{ switched bool }{
		"error: a proxy that closes the readiness request without an answer is not answering":       {},
		"error: a proxy that answers the readiness request with a protocol switch is not answering": {switched: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, state := sandbox(t)
			var held heldConns
			defer held.close()
			// The readiness check runs only when the proxy binary exists; this one is not executable,
			// so its auth status command fails at once.
			if err := os.MkdirAll(filepath.Join(state, "bin"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "bin", "claude-code-proxy"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			// The catalog's listing fetch reads the same URL; it is answered until the fixture is armed,
			// and the periodic refresh is held off, so only the readiness check meets the fixture.
			var armed atomic.Bool
			listed := newSignal()
			fixture := unanswered(&held, tt.switched, nil)
			env["CODEX_GATEWAY_MODELS_REFRESH_MS"] = "Infinity"
			env["CODEX_GATEWAY_PROXY_PORT"] = portOf(t, env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if armed.Load() {
					fixture(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, codexListing)
				listed.fire()
			})))
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)
			listed.wait(t, "the startup refresh to read the proxy listing")
			armed.Store(true)

			doc := pollHealth(t, w.public, "a completed readiness check", func(doc map[string]any) bool {
				state, _ := codexReadiness(t, doc)["state"].(string)
				return state != "" && state != "checking"
			})
			checks, _ := codexReadiness(t, doc)["checks"].(map[string]any)
			got := map[string]any{"proxyBinary": checks["proxyBinary"], "proxyModels": checks["proxyModels"]}
			if diff := gocmp.Diff(map[string]any{"proxyBinary": true, "proxyModels": false}, got); diff != "" {
				t.Errorf("readiness checks (-want +got):\n%s", diff)
			}
			stopReturns(t, w, "a readiness connection had been switched")
		})
	}
}
