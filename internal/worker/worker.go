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

// Package worker serves the gateway's local endpoints and routes inference requests.
package worker

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// Forward handles a classified request using one catalog generation and the original body bytes.
// An accepting branch calls the last argument once before its sentry check or fallback probe.
// The returned result describes the terminal outcome, even when the response must be aborted.
type Forward func(http.ResponseWriter, *http.Request, route.Snapshot, route.Decision, []byte, func(outcome.Route, *outcome.Audit)) (outcome.Result, error)

// Options supplies the worker's startup settings and its three upstream handlers.
type Options struct {
	Version, ControlToken  string
	Lookup                 config.LookupEnv
	Routes                 *route.Store
	Anthropic, Proxy, Grok Forward
	BeginDrain             func()
	// Diagnostics receives bounded refusal reasons and statuses, not request contents.
	Diagnostics io.Writer
	// BeforeBody starts observation after local endpoints and before the first body read.
	// When supplied, its finalizer receives one terminal result, including cancellation.
	BeforeBody func(*http.Request) func(outcome.Result)
	// Models returns the serialized response for a local catalog request.
	Models func(context.Context, http.Header) (jsontext.Value, error)
	// HealthCatalog returns the source and row count without waiting for a refresh.
	HealthCatalog func() (string, int)
	// RefreshAnthropic starts catalog refresh for an accepted Anthropic request.
	RefreshAnthropic func(context.Context, http.Header)
	// LogRoute records an accepted or dispatch-unbound route audit entry immediately, never at completion.
	// When nil, no route audit entry is written.
	LogRoute func(*http.Request, outcome.Audit)
	// CodexReadiness computes the /healthz codexReadiness summary when non-nil.
	// When nil, /healthz reports the default checking summary.
	CodexReadiness func() readiness.CodexReadiness
}

// NewHandler constructs the handler shared by the TCP and Unix listeners.
//
// The handler answers the drain, health and catalog endpoints and HEAD locally, matched on the raw request target cut
// at '?'. Every other request is buffered, classified against the published route snapshot (or the fallback catalog
// before the first publication) and handed to the branch the classification selects. A classifier refusal is answered
// here with HTTP 400 and never reaches a branch. Served counters count admissions, not completions.
func NewHandler(options Options) (http.Handler, error) {
	if !validToken(options.ControlToken) {
		return nil, errors.New("model-gateway: the worker needs a valid local control token before serving")
	}
	if options.Lookup == nil {
		options.Lookup = func(string) (string, bool) { return "", false }
	}
	return newHandler(options), nil
}

func validToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for i := range len(token) {
		if b := token[i]; (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

// ListenUnix binds path, reclaiming a refused socket but never a live owner or non-socket file.
//
// A worker that died without closing leaves its socket file behind, and binding over it fails with EADDRINUSE although
// nothing answers there. Only a refused connection to an actual socket proves that no process owns the path
// (lib/request-worker.js:1180-1206). A bind creates the socket file before listen makes it accept connections, and a
// connection in between is refused exactly as by a stale socket. Every starter therefore holds an advisory lock on the
// sibling file path+".lock" from its first bind until it listens, and probes, unlinks and rebinds only while holding
// it, so no starter can take another's half-bound socket for a stale one. A path that changed identity between the
// checks is not reclaimed. When the lock cannot be taken the path is bound once and never reclaimed. Any other bind
// failure is returned unchanged.
func ListenUnix(path string) (net.Listener, error) {
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	unlock, lockErr := lockReclaim(path)
	if lockErr == nil {
		defer unlock()
	}
	ln, err := net.ListenUnix("unix", addr)
	if err == nil {
		return ln, nil
	}
	if lockErr != nil {
		return nil, err
	}
	if !errors.Is(err, syscall.EADDRINUSE) || !removeStaleSocket(path) {
		return nil, err
	}
	if ln, err = net.ListenUnix("unix", addr); err != nil {
		return nil, err
	}
	return ln, nil
}

// staleProbeTimeout bounds the connection attempt that decides whether a socket file still has an owner.
const staleProbeTimeout = 2 * time.Second

// removeStaleSocket unlinks path when it is a socket that refuses connections and is still the same file afterwards.
func removeStaleSocket(path string) bool {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSocket == 0 {
		return false
	}
	conn, err := (&net.Dialer{Timeout: staleProbeTimeout}).Dial("unix", path)
	if err == nil {
		_ = conn.Close()
		return false
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		return false
	}
	return os.Remove(path) == nil
}
