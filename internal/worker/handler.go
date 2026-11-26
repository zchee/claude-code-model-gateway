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

package worker

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// Bodies the worker writes itself, byte for byte as the source's JSON.stringify produces them.
const (
	restartingBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway shim is restarting; retry this request shortly"}}`
	noBranchBody   = `{"type":"error","error":{"type":"api_error","message":"model-gateway: no handler is configured for this route"}}`
)

// refusalMessages holds the fixed answer for each classifier refusal. A parser diagnostic never reaches the client.
var refusalMessages = map[string]string{
	route.RefusalNestingDepth:   "model-gateway: request exceeds the supported JSON nesting depth",
	route.RefusalIllFormedModel: "model-gateway: model identifier contains an unpaired surrogate",
}

const defaultRefusalMessage = "model-gateway: request refused"

type handler struct {
	options    Options
	fallback   route.Snapshot
	compaction compactionHealth
	usage      usageHealth

	draining atomic.Bool
	active   atomic.Int64

	servedMu sync.Mutex
	served   []servedCount

	diagnosticsMu sync.Mutex
}

type servedCount struct {
	name  string
	count int64
}

type compactionHealth struct {
	StreamGuard      bool    `json:"streamGuard"`
	Retries          float64 `json:"retries"`
	MaxBufferedBytes float64 `json:"maxBufferedBytes"`
}

type usageHealth struct {
	Enabled              bool    `json:"enabled"`
	Endpoint             *string `json:"endpoint"`
	MaxResponseBytes     float64 `json:"maxResponseBytes"`
	SettingsLevelBaseURL bool    `json:"settingsLevelBaseUrl"`
}

func newHandler(options Options) *handler {
	h := &handler{
		options:  options,
		fallback: fallbackSnapshot(options.Lookup),
		compaction: compactionHealth{
			StreamGuard:      config.CompactGuard.Get(options.Lookup),
			Retries:          config.CompactRetries.Get(options.Lookup),
			MaxBufferedBytes: config.CompactMax.Get(options.Lookup),
		},
		usage: usageHealth{MaxResponseBytes: config.UsageMaxResponse.Get(options.Lookup)},
		// The source's counters start with these four providers; others are added in admission order.
		served: []servedCount{{name: "models"}, {name: "codex"}, {name: "grok"}, {name: "anthropic"}},
	}
	if endpoint := config.UsageURL(options.Lookup); endpoint != nil {
		s := endpoint.String()
		h.usage.Enabled, h.usage.Endpoint = true, &s
	}
	return h
}

func (h *handler) snapshot() route.Snapshot {
	if h.options.Routes != nil {
		if s, ok := h.options.Routes.Load(); ok {
			return s
		}
	}
	return h.fallback
}

func requestTarget(r *http.Request) string {
	if r.RequestURI != "" {
		return r.RequestURI
	}
	return r.URL.RequestURI()
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	snap := h.snapshot()
	target := requestTarget(r)
	// Local endpoints do not depend on the body, so they are classified before any of it is read.
	local := snap.Classify(r.Method, target, nil)
	if local.Branch == "drain" {
		h.drain(w, r)
		return
	}
	if h.draining.Load() && local.Branch != "health" {
		writeJSON(w, http.StatusServiceUnavailable, []byte(restartingBody))
		return
	}
	if local.Branch == "health" {
		h.health(w)
		return
	}
	untrack := h.track()
	defer untrack()
	switch local.Branch {
	case "models":
		h.models(w, r, snap)
	case "head":
		w.WriteHeader(http.StatusOK)
	default:
		h.route(w, r, snap, target, untrack)
	}
}

// track counts a request in activeRequests until the returned function runs; later calls do nothing.
func (h *handler) track() func() {
	h.active.Add(1)
	return sync.OnceFunc(func() { h.active.Add(-1) })
}

func localPorts(r *http.Request) []int {
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr); ok {
		return []int{addr.Port}
	}
	return nil
}

func (h *handler) drain(w http.ResponseWriter, r *http.Request) {
	// Only the listener's own TCP port authorizes a drain; a Unix socket or an unknown listener has none.
	if !controlauth.Authenticated(r, h.options.ControlToken, localPorts(r), false) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	h.draining.Store(true)
	body, err := json.Marshal(struct {
		OK             bool  `json:"ok"`
		Draining       bool  `json:"draining"`
		ActiveRequests int64 `json:"activeRequests"`
	}{true, true, h.active.Load()})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if h.options.BeginDrain != nil {
		// The drain callback stops the listeners and waits for active handlers, this one included.
		go h.options.BeginDrain()
	}
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *handler) servedJSON() jsontext.Value {
	h.servedMu.Lock()
	defer h.servedMu.Unlock()
	out := []byte{'{'}
	for i, c := range h.served {
		if i > 0 {
			out = append(out, ',')
		}
		out, _ = jsontext.AppendQuote(out, c.name)
		out = fmt.Appendf(out, ":%d", c.count)
	}
	return append(out, '}')
}

func (h *handler) count(name string) {
	h.servedMu.Lock()
	defer h.servedMu.Unlock()
	for i := range h.served {
		if h.served[i].name == name {
			h.served[i].count++
			return
		}
	}
	h.served = append(h.served, servedCount{name: name, count: 1})
}

// checkingReadiness is the summary the source reports before its first shared check (lib/request-worker.js:116-129).
func checkingReadiness() readiness.CodexReadiness {
	return readiness.CodexReadiness{
		State:   "checking",
		Message: "Codex readiness is still being checked (proxy /v1/models and codex auth status); ask /healthz again in a few seconds.",
		Stale:   true,
	}
}

func (h *handler) health(w http.ResponseWriter) {
	source, models := h.catalogHealth()
	codex := checkingReadiness()
	if h.options.CodexReadiness != nil {
		codex = h.options.CodexReadiness()
	}
	body, err := json.Marshal(struct {
		OK             bool                     `json:"ok"`
		Version        string                   `json:"version"`
		Models         int                      `json:"models"`
		Catalog        string                   `json:"catalog"`
		Served         jsontext.Value           `json:"served"`
		Draining       bool                     `json:"draining"`
		ActiveRequests int64                    `json:"activeRequests"`
		Compaction     compactionHealth         `json:"compaction"`
		Usage          usageHealth              `json:"usage"`
		CodexReadiness readiness.CodexReadiness `json:"codexReadiness"`
	}{true, h.options.Version, models, source, h.servedJSON(), h.draining.Load(), h.active.Load(), h.compaction, h.usage, codex})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *handler) catalogHealth() (string, int) {
	if h.options.HealthCatalog != nil {
		return h.options.HealthCatalog()
	}
	source, rows := h.snapshot().Catalog()
	return source, countRows(rows)
}

func countRows(rows jsontext.Value) int {
	var elements []jsontext.Value
	if len(rows) == 0 || json.Unmarshal(rows, &elements) != nil {
		return 0
	}
	return len(elements)
}

func (h *handler) models(w http.ResponseWriter, r *http.Request, snap route.Snapshot) {
	h.count("models")
	if h.options.Models != nil {
		body, err := h.options.Models(r.Context(), r.Header.Clone())
		if err == nil {
			writeJSON(w, http.StatusOK, body)
			return
		}
		if r.Context().Err() != nil {
			return
		}
	}
	_, rows := snap.Catalog()
	if len(rows) == 0 {
		rows = jsontext.Value("[]")
	}
	body := append([]byte(`{"data":`), rows...)
	writeJSON(w, http.StatusOK, append(body, `,"has_more":false}`...))
}

func (h *handler) branch(name string) Forward {
	switch name {
	case "proxy":
		return h.options.Proxy
	case "grok":
		return h.options.Grok
	default:
		return h.options.Anthropic
	}
}

// servedKey selects the counter a branch's admission increments (lib/request-worker.js:2551,2620,2649).
func servedKey(d route.Decision) string {
	switch d.Branch {
	case "proxy":
		if d.Provider != "" {
			return d.Provider
		}
		return "codex"
	case "grok":
		return "grok"
	default:
		return "anthropic"
	}
}

func (h *handler) diagnose(format string, args ...any) {
	if h.options.Diagnostics == nil {
		return
	}
	h.diagnosticsMu.Lock()
	defer h.diagnosticsMu.Unlock()
	_, _ = fmt.Fprintf(h.options.Diagnostics, format, args...)
}

// terminal delivers one terminal result for a request, either from the branch or from the client's departure.
type terminal struct {
	finalize func(outcome.Result)
	once     sync.Once

	mu       sync.Mutex
	admitted *outcome.Route
}

func (t *terminal) deliver(result outcome.Result) {
	t.once.Do(func() {
		if t.finalize != nil {
			t.finalize(result)
		}
	})
}

func (t *terminal) admittedRoute() outcome.Route {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.admitted == nil {
		return outcome.Route{}
	}
	return *t.admitted
}

// route reads the body and hands the request to its branch. The client's departure is accounted by a callback on the
// request context: it delivers the cancelled terminal result and leaves activeRequests at once, without waiting for
// the branch or its provider. The callback is stopped, or waited for, before route returns.
func (h *handler) route(w http.ResponseWriter, r *http.Request, snap route.Snapshot, target string, untrack func()) {
	t := &terminal{}
	if h.options.BeforeBody != nil {
		t.finalize = h.options.BeforeBody(r)
	}
	departed := make(chan struct{})
	stop := context.AfterFunc(r.Context(), func() {
		defer close(departed)
		t.deliver(outcome.Result{Route: t.admittedRoute(), Cancelled: true})
		untrack()
	})
	// settle reports whether the departure callback ran; once it returns, the callback is finished or never runs.
	settle := sync.OnceValue(func() bool {
		if stop() {
			return false
		}
		<-departed
		return true
	})
	defer func() {
		if !settle() {
			// Only reached when a branch panicked before returning its facts.
			t.deliver(outcome.Result{Route: t.admittedRoute()})
		}
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		// A body that cannot be read completely means the client is gone; nothing is answered.
		if !settle() {
			t.deliver(outcome.Result{Cancelled: true})
			untrack()
		}
		panic(http.ErrAbortHandler)
	}

	d := snap.Classify(r.Method, target, body)
	if d.Branch == "refused" {
		h.refuse(w, d.Reason)
		if !settle() {
			t.deliver(outcome.Result{Status: http.StatusBadRequest})
		}
		return
	}
	forward := h.branch(d.Branch)
	if forward == nil {
		writeJSON(w, http.StatusBadGateway, []byte(noBranchBody))
		if !settle() {
			t.deliver(outcome.Result{Status: http.StatusBadGateway})
		}
		return
	}
	result, err := forward(w, r, snap, d, body, func(selected outcome.Route, audit *outcome.Audit) {
		h.admit(r, d, t, selected, audit)
	})
	if result.Audit != nil && h.options.LogRoute != nil {
		// A log-only decision, such as an unbound dispatch, is recorded once and never counted.
		h.options.LogRoute(r, *result.Audit)
	}
	if !settle() {
		t.deliver(result)
	}
	if err != nil {
		if !errors.Is(err, http.ErrAbortHandler) {
			h.diagnose("model-gateway: %s request ended with an error after status %d; closing the connection\n", d.Branch, result.Status)
		}
		// net/http aborts the response without writing a second header.
		panic(http.ErrAbortHandler)
	}
}

// admit runs the branch's admission callback at most once per request: it counts the request and logs its accepted
// audit before the branch can still refuse it (lib/request-worker.js:2551-2568,2620-2634,2648-2650).
func (h *handler) admit(r *http.Request, d route.Decision, t *terminal, selected outcome.Route, audit *outcome.Audit) {
	t.mu.Lock()
	first := t.admitted == nil
	if first {
		t.admitted = &selected
	}
	t.mu.Unlock()
	if !first {
		return
	}
	h.count(servedKey(d))
	if audit != nil && h.options.LogRoute != nil {
		h.options.LogRoute(r, *audit)
	}
	if d.Branch == "anthropic" && h.options.RefreshAnthropic != nil {
		h.options.RefreshAnthropic(r.Context(), r.Header.Clone())
	}
}

func (h *handler) refuse(w http.ResponseWriter, reason string) {
	message, ok := refusalMessages[reason]
	if !ok {
		message = defaultRefusalMessage
	}
	body, err := json.Marshal(struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}{Type: "error", Error: struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}{"invalid_request_error", message}})
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusBadRequest, body)
	h.diagnose("model-gateway: refused a request locally: reason=%s status=%d\n", safeReason(reason), http.StatusBadRequest)
}

// safeReason keeps the diagnostic to the classifier's fixed vocabulary.
func safeReason(reason string) string {
	if _, ok := refusalMessages[reason]; ok {
		return reason
	}
	return "unknown"
}
