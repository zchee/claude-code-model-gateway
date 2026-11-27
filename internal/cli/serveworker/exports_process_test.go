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
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
)

const (
	// proxyReply is a proxy's complete JSON answer to a message request, with token usage, so its
	// delivery produces a usage record.
	proxyReply = `{"id":"msg_proxy","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":12,"output_tokens":4}}`
	// catalogPage is a complete one-page Anthropic catalog.
	catalogPage = `{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}],"has_more":false}`
	// messageBody is a message request the proxy listing in codexListing routes.
	messageBody = `{"model":"claude-gpt-6.1-sol[1m]","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
)

// messagesProxy serves the proxy listing in codexListing and answers every message request with
// proxyReply, after held returns when held is set.
func messagesProxy(listing http.HandlerFunc, held func()) *http.ServeMux {
	mux := http.NewServeMux()
	if listing == nil {
		listing = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, codexListing)
		}
	}
	mux.HandleFunc("GET /v1/models", listing)
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if held != nil {
			held()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, proxyReply)
	})
	return mux
}

// sendMessage sends messageBody to the worker and reads the whole answer. It reports the status, or
// the error that kept the answer from arriving within the failure bound.
func sendMessage(ctx context.Context, public string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, failureBound)
	defer cancel()
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+public+"/v1/messages", strings.NewReader(messageBody))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return 0, err
	}
	return resp.StatusCode, nil
}

// mustDeliver sends messageBody once the proxy catalog is in place and requires a successful answer,
// whose delivery queues the request's usage record and its trace span.
func mustDeliver(t *testing.T, public string) {
	t.Helper()
	pollHealth(t, public, "the proxy catalog", func(doc map[string]any) bool { return doc["catalog"] == "proxy" })
	status, err := sendMessage(t.Context(), public)
	if err != nil || status != http.StatusOK {
		t.Fatalf("message request: status %d, error %v; want 200", status, err)
	}
}

// listWithCredential reads the worker's /v1/models with an Anthropic credential, which makes a cold
// catalog refresh and waits for it.
func listWithCredential(t *testing.T, public string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), failureBound)
	defer cancel()
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+public+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "fixture-catalog-key")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("the listing request waiting for the catalog refresh got no answer: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// firstEncoding keeps the Accept-Encoding values of the first request it records.
type firstEncoding struct {
	once   sync.Once
	seen   *signal
	values []string
}

func (f *firstEncoding) record(r *http.Request) {
	f.once.Do(func() {
		f.values = r.Header.Values("Accept-Encoding")
		f.seen.fire()
	})
}

// installInertProxyBinary puts a proxy binary in place, which makes /healthz run the readiness check.
// It is not executable, so the check's auth status command fails at once.
func installInertProxyBinary(t *testing.T, state string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(state, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "bin", "claude-code-proxy"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestWorkerOwnedRequestsAskForNoEncoding decides that the requests the running worker makes on its
// own transports carry no Accept-Encoding, as the source's requests carry none: the proxy listing, an
// Anthropic catalog page, the readiness check's proxy request, a usage export and a trace export.
// The clients of these tests send Accept-Encoding to the worker, so none of it is passed on either.
func TestWorkerOwnedRequestsAskForNoEncoding(t *testing.T) {
	tests := map[string]struct{ request string }{
		"success: the proxy listing request asks for no encoding":           {request: "listing"},
		"success: an Anthropic catalog page request asks for no encoding":   {request: "catalog"},
		"success: the readiness check's proxy request asks for no encoding": {request: "readiness"},
		"success: a usage export asks for no encoding":                      {request: "usage"},
		"success: a trace export asks for no encoding":                      {request: "trace"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, state := sandbox(t)
			first := &firstEncoding{seen: newSignal()}
			var armed atomic.Bool
			listed := newSignal()
			env["CODEX_GATEWAY_PROXY_PORT"] = portOf(t, env.StartUpstream(t, messagesProxy(func(w http.ResponseWriter, r *http.Request) {
				if tt.request == "listing" || tt.request == "readiness" && armed.Load() {
					first.record(r)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, codexListing)
				listed.fire()
			}, nil)))
			collector := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				first.record(r)
				w.WriteHeader(http.StatusNoContent)
			})
			switch tt.request {
			case "catalog":
				env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					first.record(r)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, catalogPage)
				}))
			case "readiness":
				installInertProxyBinary(t, state)
				// The periodic refresh is held off, so the only listing request after the startup refresh
				// is the readiness check's.
				env["CODEX_GATEWAY_MODELS_REFRESH_MS"] = "Infinity"
			case "usage":
				env["CODEX_GATEWAY_USAGE_ENDPOINT"] = env.StartUpstream(t, collector) + "/v1/logs"
			case "trace":
				env["CODEX_GATEWAY_TELEMETRY_ENDPOINT"] = env.StartUpstream(t, collector) + "/v1/traces"
				env["CLAUDE_CODE_PROPAGATE_TRACEPARENT"] = "1"
			}
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)

			switch tt.request {
			case "catalog":
				listWithCredential(t, w.public)
			case "readiness":
				listed.wait(t, "the startup refresh to read the proxy listing")
				armed.Store(true)
				// The first /healthz starts the shared readiness check.
				_ = getJSON(t, "http://"+w.public+"/healthz")
			case "usage", "trace":
				mustDeliver(t, w.public)
			}
			first.seen.wait(t, "the worker's "+tt.request+" request")
			if diff := gocmp.Diff([]string(nil), first.values); diff != "" {
				t.Errorf("Accept-Encoding of the worker's %s request (-want +got):\n%s", tt.request, diff)
			}
		})
	}
}

// gzipped compresses s.
func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(zw, s); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeBody answers with body, or with its gzip compression marked as such when compressed is not nil.
func writeBody(w http.ResponseWriter, body string, compressed []byte) {
	w.Header().Set("Content-Type", "application/json")
	if compressed == nil {
		_, _ = io.WriteString(w, body)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
	_, _ = w.Write(compressed)
}

// listedIDs reports which of ids name a row of the worker's /v1/models.
func listedIDs(t *testing.T, public string, ids ...string) map[string]bool {
	t.Helper()
	doc := getJSON(t, "http://"+public+"/v1/models")
	rows, _ := doc["data"].([]any)
	found := make(map[string]bool, len(ids))
	for _, id := range ids {
		found[id] = false
		for _, row := range rows {
			m, _ := row.(map[string]any)
			if rowID, _ := m["id"].(string); strings.Contains(rowID, id) {
				found[id] = true
			}
		}
	}
	return found
}

// TestWorkerOwnedRequestsReadCompressedBytesAsSent decides that the worker reads an answer it did not
// ask to be compressed as the bytes that were sent, as the source does: a gzip-compressed catalog
// page is not JSON, and a gzip-compressed proxy listing lists nothing, so the listing that follows it
// is the one in effect. The uncompressed rows show that only the compression makes the difference.
func TestWorkerOwnedRequestsReadCompressedBytesAsSent(t *testing.T) {
	const compressedID, plainID = "gpt-fixture-compressed", "gpt-fixture-plain"
	listingOf := func(id string) string {
		return `{"data":[{"id":"` + id + `","display_name":"` + id + ` (codex)"}]}`
	}
	tests := map[string]struct {
		listing, compressed bool
		wantCatalogError    any
		wantListed          map[string]bool
	}{
		"success: an Anthropic catalog page sent uncompressed is read": {},
		"error: an Anthropic catalog page sent gzip-compressed without being asked is read as sent, which is not JSON": {
			compressed: true, wantCatalogError: "invalid catalog JSON",
		},
		"success: a proxy listing sent uncompressed is read": {
			listing: true, wantListed: map[string]bool{compressedID: true, plainID: false},
		},
		"error: a proxy listing sent gzip-compressed without being asked is read as sent, which lists nothing": {
			listing: true, compressed: true, wantListed: map[string]bool{compressedID: false, plainID: true},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, state := sandbox(t)
			body := catalogPage
			if tt.listing {
				body = listingOf(compressedID)
			}
			var compressed []byte
			if tt.compressed {
				compressed = gzipped(t, body)
			}
			if tt.listing {
				// The first listing request gets the listing under test; a later one, the retry after an
				// empty listing, gets a different listing that is never compressed.
				var answered atomic.Int64
				env["CODEX_GATEWAY_PROXY_PORT"] = portOf(t, env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/models" {
						http.NotFound(w, r)
						return
					}
					if answered.Add(1) == 1 {
						writeBody(w, body, compressed)
						return
					}
					writeBody(w, listingOf(plainID), nil)
				})))
			} else {
				env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					writeBody(w, body, compressed)
				}))
			}
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)

			if tt.listing {
				pollHealth(t, w.public, "a proxy catalog", func(doc map[string]any) bool { return doc["catalog"] == "proxy" })
				if diff := gocmp.Diff(tt.wantListed, listedIDs(t, w.public, compressedID, plainID)); diff != "" {
					t.Errorf("listing rows in the worker's /v1/models (-want +got):\n%s", diff)
				}
				return
			}
			listWithCredential(t, w.public)
			if diff := gocmp.Diff(tt.wantCatalogError, catalogError(t, state)); diff != "" {
				t.Errorf("recorded catalog failure (-want +got):\n%s", diff)
			}
		})
	}
}

// TestStopEndsPendingExportHandshakes decides what a stop does to an export whose connection to an
// HTTPS collector never finishes its TLS handshake: Run returns, no goroutine of the worker remains,
// and the collector sees its accepted connection closed by the worker. The stop comes as soon as the
// collector accepted the connection, while the handshake is pending, and the leak check runs before
// anything waits on the collector, so neither the export deadline nor the establishment bound has to
// pass for the check to see the export's dial ended.
func TestStopEndsPendingExportHandshakes(t *testing.T) {
	tests := map[string]struct{ trace bool }{
		"success: a stop ends the TLS handshake of a pending usage export": {},
		"success: a stop ends the TLS handshake of a pending trace export": {trace: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, _ := sandbox(t)
			endpoint := startSilentTLS(t, env)
			defer endpoint.close()
			if tt.trace {
				env["CODEX_GATEWAY_TELEMETRY_ENDPOINT"] = "https://" + endpoint.addr() + "/v1/traces"
				env["CLAUDE_CODE_PROPAGATE_TRACEPARENT"] = "1"
			} else {
				env["CODEX_GATEWAY_USAGE_ENDPOINT"] = "https://" + endpoint.addr() + "/v1/logs"
			}
			env["CODEX_GATEWAY_PROXY_PORT"] = portOf(t, env.StartUpstream(t, messagesProxy(nil, nil)))

			// Goroutines that exist before the worker starts, the fixtures' included, are not the worker's.
			ignore := goleak.IgnoreCurrent()
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)
			mustDeliver(t, w.public)
			select {
			case <-endpoint.accepted:
			case <-time.After(failureBound):
				t.Fatal("the export never connected to the TLS collector")
			}

			w.cancel()
			if !w.returned() {
				t.Fatal("the stopped worker did not return")
			}
			if w.err != nil {
				t.Errorf("Run: %v", w.err)
			}
			goleak.VerifyNone(t, ignore)
			for i, c := range endpoint.held.snapshot() {
				closedByPeer(t, c, "connection "+strconv.Itoa(i)+" to the TLS collector")
			}
		})
	}
}

// TestDrainDeliversUsageOfAFinishingRequest follows a request that is still waiting for its provider
// when a drain begins and finishes during the drain: its client gets the answer, Run returns, and the
// collector has received the request's usage record by then, since Run joins the exporters. The record
// is queued before the drain ends the service, so this row does not show that the export's
// connection outlives the service; TestExportsOutliveTheService decides that on the composed worker.
func TestDrainDeliversUsageOfAFinishingRequest(t *testing.T) {
	tests := map[string]struct{}{
		"success: a request that finishes during a drain has its usage record delivered": {},
	}
	for name := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, _ := sandbox(t)
			entered, release := newSignal(), newSignal()
			// Deferred, so it runs before the worker's cleanup waits for Run.
			defer release.fire()
			env["CODEX_GATEWAY_PROXY_PORT"] = portOf(t, env.StartUpstream(t, messagesProxy(nil, func() {
				entered.fire()
				<-release.ch
			})))
			var mu sync.Mutex
			var paths []string
			env["CODEX_GATEWAY_USAGE_ENDPOINT"] = env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			})) + "/v1/logs"
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)
			pollHealth(t, w.public, "the proxy catalog", func(doc map[string]any) bool { return doc["catalog"] == "proxy" })

			type answer struct {
				status int
				err    error
			}
			answered := make(chan answer, 1)
			go func() {
				status, err := sendMessage(t.Context(), w.public)
				answered <- answer{status, err}
			}()
			entered.wait(t, "the provider to hold the message request")
			status, active, err := postDrain(t.Context(), w.public, lifecycleToken)
			if err != nil || status != http.StatusAccepted || active != 1 {
				t.Fatalf("drain status=%d, active=%d, err=%v; want 202 with the held request active", status, active, err)
			}
			release.fire()
			if got := <-answered; got.err != nil || got.status != http.StatusOK {
				t.Fatalf("the request that finished during the drain: status %d, error %v; want 200", got.status, got.err)
			}
			if !w.returned() {
				t.Fatal("the drained worker did not return")
			}
			if w.err != nil {
				t.Errorf("Run: %v", w.err)
			}
			mu.Lock()
			defer mu.Unlock()
			if diff := gocmp.Diff([]string{"/v1/logs"}, paths); diff != "" {
				t.Errorf("usage exports the collector received once Run returned (-want +got):\n%s", diff)
			}
		})
	}
}
