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
	"sync/atomic"
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
	// SettingsLevelBaseURL is reported by /healthz as usage.settingsLevelBaseUrl: whether the gateway's
	// base URL was wired, through a settings file or the environment, when the worker started.
	SettingsLevelBaseURL bool
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

// ListenUnix binds a Unix listener and holds its pathname reclamation lock until Close.
//
// A worker that died without closing leaves its socket file behind, and binding over it fails with EADDRINUSE although
// nothing answers there. Only a refused connection to an actual socket proves that no process owns the path
// (lib/request-worker.js:1180-1206). A bind creates the socket file before listen makes it accept connections, and a
// connection in between is refused exactly as by a stale socket; a live owner whose accept queue is full is refused on
// some systems as well. Every starter therefore holds an advisory lock on the sibling file path+".lock" from before its
// first bind until its listener is closed, and probes, unlinks and rebinds only while holding it, so no starter can
// take another's half-bound or busy socket for a stale one. A path that changed identity between the checks is not
// reclaimed. Any other bind failure is returned unchanged, after the lock is released. Four further rules hold:
//
//   - On Unix a starter that finds the lock held, by another starter or by a listener not yet closed, does not wait,
//     probe, bind or unlink: it returns an error whose first system error number is EADDRINUSE, as a bind onto a live
//     socket does. A starter that cannot take the lock for any other reason does not bind at all and returns the lock
//     failure, which names the lock file: a socket bound without the lock could itself be taken for stale by a starter
//     that holds it. Only a platform with no lock at all binds the path once, unlocked, and never reclaims it.
//   - The returned listener's Close closes the socket, which removes its file, and only then releases the lock, once,
//     however often it is called; no Close returns before the release has finished. The lock file itself is left in
//     place, so that every starter locks the same file.
//   - When the probe after a refused bind finds the path gone, because its previous owner closed in between, the path
//     is bound once more under the lock without unlinking anything.
//   - On Linux and Android, the systems this program supports that have an abstract namespace, a name starting with
//     '@' or a NUL byte is an abstract address and has no file to reclaim: it is bound once, with no lock file, and
//     nothing is ever unlinked. Elsewhere this code handles such a name as a path.
func ListenUnix(path string) (net.Listener, error) {
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	unlock, locked, err := lockReclaim(path)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", addr)
	if !locked {
		if err != nil {
			return nil, err
		}
		return ln, nil
	}
	if err != nil && errors.Is(err, syscall.EADDRINUSE) && reclaimPath(path) {
		ln, err = net.ListenUnix("unix", addr)
	}
	if err != nil {
		unlock()
		return nil, err
	}
	return lockedUnixListener(ln, unlock), nil
}

// lockedUnixListener returns ln, the Unix listener ListenUnix bound, as a listener that holds the pathname reclamation
// lock until it is closed. The first Close closes ln, which removes the socket file ln bound, and only then calls
// unlock. ln is closed and unlock runs exactly once, however many times and from however many goroutines Close is
// called. Every Close returns the result of that one close of ln, and none returns before unlock has returned. Accept
// and Addr are those of ln.
func lockedUnixListener(ln net.Listener, unlock func()) net.Listener {
	return &lockedListener{Listener: ln, unlock: unlock, released: make(chan struct{})}
}

// lockedListener is the listener lockedUnixListener returns. Accept and Addr are promoted from the embedded listener.
type lockedListener struct {
	net.Listener
	unlock func()
	// closing is set by the one Close call that closes the listener and releases the lock.
	closing atomic.Bool
	// released is closed once that call has stored err and unlock has returned. Waiting on it holds no lock, so a
	// second Close never contends for a mutex that is held across the close or the release.
	released chan struct{}
	// err is the result of the one close of the embedded listener. It is written before released is closed and read
	// only after, so the channel close orders the two.
	err error
}

// Close closes the listener and then releases its lock, the first time it is called, and returns the result of that
// close every time, once the release has finished.
func (l *lockedListener) Close() error {
	if l.closing.CompareAndSwap(false, true) {
		l.release()
	}
	<-l.released
	return l.err
}

// release closes the embedded listener, which removes its socket file, before it unlocks, so that a starter that gets
// the lock never finds this listener's socket. The deferred close of released wakes waiting Close calls even if the
// close or the unlock panics.
func (l *lockedListener) release() {
	defer close(l.released)
	l.err = l.Listener.Close()
	l.unlock()
}

// staleProbeTimeout bounds the connection attempt that decides whether a socket file still has an owner.
const staleProbeTimeout = 2 * time.Second

// reclaimPath reports whether path may be bound again: it is missing, or it was a socket that refused a connection and
// was unlinked while still the same file. A path found missing at any point of the probe is not unlinked.
func reclaimPath(path string) bool {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || before.Mode()&os.ModeSocket == 0 {
		return false
	}
	conn, err := (&net.Dialer{Timeout: staleProbeTimeout}).Dial("unix", path)
	if err == nil {
		_ = conn.Close()
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}
	after, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || !os.SameFile(before, after) {
		return false
	}
	return os.Remove(path) == nil
}
