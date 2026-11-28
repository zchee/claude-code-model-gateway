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

// Package relay forwards a buffered public request to the current worker generation and relays its answer.
package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// BodyLimit is the largest request body, inclusive, the relay buffers for the worker (lib/commands.js:2302).
const BodyLimit = 64 << 20

// OversizeGrace bounds how long the relay keeps reading an oversized upload after its 413 head was flushed
// (lib/commands.js:2289).
const OversizeGrace = 250 * time.Millisecond

// Lost describes a request a worker generation took and never answered.
type Lost struct {
	// Generation is the worker generation that took the request.
	Generation workerlink.Generation
	// WorkerPID is the process identifier published for that generation, or zero when it is not known.
	WorkerPID int
	// ErrorType is the sanitized type of the failure, never its message.
	ErrorType string
}

// Options supplies the relay's worker source, its lifecycle report and the supervisor's identity.
type Options struct {
	// Workers gives connection leases to the current worker generation.
	Workers workerlink.Source
	// Lost receives one report for each request a worker took and never answered.
	Lost func(Lost)
	// Wait pauses between two delivery attempts for the given time. It returns the context's error when the request's
	// context ends first. Any other error is the supervisor's stop signal (the source's isStopped,
	// lib/commands.js:2359): the relay makes no further attempt and answers as when its attempts have run out. A stop
	// that first arrives during the wait after a failed attempt counts as a stop already in effect at that failure, so
	// the answer can differ from the source's timing by at most one wait interval. Wait is required: New refuses
	// options without it.
	Wait func(context.Context, time.Duration) error
	// Version is the supervisor version added to the worker's health document.
	Version string
	// SupervisorPID is the supervisor process identifier added to the worker's health document.
	SupervisorPID int
}

// New returns the handler that relays requests to the worker generations of o.Workers. It refuses options without a
// worker source or without Wait, since neither has a default; a nil Lost drops the reports.
func New(o Options) (http.Handler, error) {
	if o.Workers == nil {
		return nil, errors.New("relay: the options name no worker source")
	}
	if o.Wait == nil {
		return nil, errors.New("relay: the options name no wait between delivery attempts")
	}
	lost := o.Lost
	if lost == nil {
		lost = func(Lost) {}
	}
	return &handler{workers: o.Workers, lost: lost, wait: o.Wait, version: o.Version, pid: o.SupervisorPID}, nil
}

// OneGeneration marks ctx so that, once a lease was handed over for the request, the request is never offered to another lease.
func OneGeneration(ctx context.Context) context.Context {
	panic("not implemented")
}

// The source's fixed answers (lib/commands.js:2279-2282,2432-2440), byte for byte as JSON.stringify writes them.
const (
	undeliveredBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway could not deliver this request to the shim worker; retry it shortly"}}`
	lostBody        = `{"type":"error","error":{"type":"api_error","message":"model-gateway lost the shim worker connection after this request reached the model; that answer cannot be recovered, and retrying automatically would pay for a second inference, so send it again yourself if you still want one"}}`
	unreadableBody  = `{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway could not read the request body"}}`
)

// handler is the relay New returns. It keeps no state between requests: every request reads its own body, takes its
// own leases and closes them before ServeHTTP ends.
type handler struct {
	workers workerlink.Source
	lost    func(Lost)
	wait    func(context.Context, time.Duration) error
	version string
	pid     int
}

// ServeHTTP buffers the request body, delivers it to a worker generation and relays the worker's answer. Every
// goroutine it starts has ended when it returns or panics, and every lease it took is closed. It ends only by
// returning or by http.ErrAbortHandler, which breaks the client connection once a relayed answer cannot be completed.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := requestTarget(r)
	if !wireSafe(r.Method) || !wireSafe(target) {
		// The public listener refuses such a request line before dispatch; this guards any other caller, because
		// the method and target are written into the worker's request line unchanged.
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	body, ok := readUpload(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if ctx.Err() != nil {
		// A complete upload whose client has gone is not delivered (lib/commands.js:2311-2314).
		return
	}
	ex, fail := h.deliver(ctx, r, target, body)
	if ex == nil {
		h.answerFailure(ctx, w, fail)
		return
	}
	defer ex.finish()
	h.relayAnswer(ctx, w, r.Method, target, ex.resp)
}

// answerFailure answers a request no worker answered: the retryable 503 when no generation holds it, and the 503 that
// forbids an automatic resend, with one Lost report, when a generation took it and may have spent it upstream
// (lib/commands.js:2420-2441). A departed client gets nothing and causes no report; the context is consulted again
// after the report, so that a client that left while it was made gets nothing either.
func (h *handler) answerFailure(ctx context.Context, w http.ResponseWriter, f failure) {
	if f.departed || ctx.Err() != nil {
		return
	}
	body := undeliveredBody
	if t := f.taker; t != nil {
		h.lost(Lost{Generation: t.generation, WorkerPID: t.pid, ErrorType: t.errorType})
		if ctx.Err() != nil {
			return
		}
		w.Header().Set("X-Should-Retry", "false")
		body = lostBody
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusServiceUnavailable)
	// A client that left while the head was committed gets nothing further.
	abortIfDeparted(ctx)
	_, _ = io.WriteString(w, body)
}

// requestTarget returns the request target as the client sent it.
func requestTarget(r *http.Request) string {
	if r.RequestURI != "" {
		return r.RequestURI
	}
	return r.URL.RequestURI()
}

// wireSafe reports whether s can be written into a request line unchanged.
func wireSafe(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if b := s[i]; b <= ' ' || b == 0x7f {
			return false
		}
	}
	return true
}
