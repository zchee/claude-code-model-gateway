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

// Package serveworker owns the serve-worker command's listener and descriptor lifecycle.
package serveworker

import (
	"context"
	"crypto/rand"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// Options supplies startup dependencies without requiring process-global lookups.
type Options struct {
	Lookup         config.LookupEnv
	Home, Cwd      func() (string, error)
	Version        string
	Stdout, Stderr io.Writer
	// Exit ends the process at once with a status. A supervised worker calls it, with status 0 as the source does on
	// its parent's disconnect (lib/request-worker.js:1208-1210), when its parent goes away before the worker admitted
	// anything, so that a startup step still blocked in a system call cannot keep it alive. nil leaves Run to return
	// once that step ends.
	Exit func(int)
}

const (
	controlFDsKey   = "MODEL_GATEWAY_CONTROL_FDS"
	generationKey   = "MODEL_GATEWAY_WORKER_GENERATION"
	nodeChannelKey  = "NODE_CHANNEL_FD"
	sandboxKey      = "MODEL_GATEWAY_TEST_SANDBOX"
	maxReportLength = 256
	// maxDrainDiagnostic bounds the one line a drain exchange without a valid answer prints.
	maxDrainDiagnostic = 512
	// headersTimeout and keepAliveTimeout are the source's server settings (lib/request-worker.js:2676-2682).
	headersTimeout   = 120 * time.Second
	keepAliveTimeout = 75 * time.Second
)

// Run serves until drain, parent disconnect, or cancellation, returning a startup or serving error.
//
// A supervised worker (MODEL_GATEWAY_CONTROL_FDS set) serves on the TCP listener its supervisor reserved and handed
// over, writes one listening report to the report descriptor, closes it, and stops when the liveness descriptor
// reaches end of file. A standalone worker binds its own listener and reads no descriptor; under the test guard it
// stops when NODE_CHANNEL_FD reaches end of file, and otherwise it closes that descriptor unread. Both print the TCP
// readiness line on Stdout after every listener is bound; it names the Anthropic upstream by its origin only.
//
// A standalone worker's drain closes every listener at once and lets in-flight requests finish. A supervised worker
// first announces its drain on the drain channel while it keeps accepting, and waits for its supervisor's answer
// within one budget; once the supervisor has retired the generation, the worker takes the connections already queued
// on its listener, serves them, closes the listener and lets in-flight requests finish. Without a valid answer it
// closes its listener all the same and prints one diagnostic. The parent's end of file, the fixture channel's end of
// file or cancellation end the worker meanwhile, also during the exchange; those stops close every connection at once.
func Run(ctx context.Context, options Options) error {
	lookup := options.Lookup
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	// Request handlers and background catalog work write to the same streams as Run itself. One lock
	// covers both streams, so lines never interleave when the caller passes the same sink twice.
	var output sync.Mutex
	stdout, stderr := &syncWriter{mu: &output, w: writerOr(options.Stdout)}, &syncWriter{mu: &output, w: writerOr(options.Stderr)}

	control, err := supervisedDescriptors(lookup)
	if err != nil {
		return err
	}
	var channel *os.File
	if control == nil {
		channel, err = fixtureChannel(lookup)
		if err != nil {
			return err
		}
	}
	// The parent's end of file, the fixture channel's end of file and cancellation stop the worker at once from here
	// on, before any step that can block and before any listener admits a connection: every later step runs under
	// runCtx, which the immediate stop ends, and serving begins only while no immediate stop was signalled. Closing the
	// descriptors ends a watcher that is still reading them; the watchers are joined on every return.
	stop := newStopper()
	admission := &admissionGate{exit: options.Exit}
	var watchers sync.WaitGroup
	defer func() {
		if control != nil {
			// The worker closes the liveness descriptor itself now: from here on its end is no parent loss.
			admission.disarm()
			control.close()
		}
		if channel != nil {
			_ = channel.Close()
		}
		watchers.Wait()
	}()
	switch {
	case control != nil:
		watchers.Go(func() {
			// Only the peer's end of file is the parent's loss; the worker's own close of the descriptor is not. The gate
			// decides before the stop is signalled, so that Run, once it sees the stop, finds the decision taken.
			if peer := awaitEOF(control.live); peer {
				admission.parentGone()
				stop.signal(stopNow)
			}
		})
	case channel != nil:
		watchers.Go(func() { awaitEOF(channel); stop.signal(stopNow) })
	}
	stopOnCancel := context.AfterFunc(ctx, func() { stop.signal(stopNow) })
	defer stopOnCancel()
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	stopRunOnStop := context.AfterFunc(stop.nowCtx, cancelRun)
	defer stopRunOnStop()
	ctx = runCtx

	resolver := paths.New(lookup, options.Home, options.Cwd)
	state, err := resolver.StateDir()
	if err != nil {
		return fmt.Errorf("model-gateway: could not resolve the state directory for the control token: %w", err)
	}
	if !filepath.IsAbs(state) || filepath.Clean(state) != state {
		// The token path is derived from this spelling; a spelling the kernel resolves differently is refused.
		return fmt.Errorf("model-gateway: the gateway state directory %q must be a clean absolute path", state)
	}
	token, err := controlauth.EnsureToken(filepath.Join(state, "control-token"), rand.Reader)
	if err != nil {
		return fmt.Errorf("model-gateway: could not load the local control token: %w", err)
	}

	workerOptions, life, err := buildWorker(workerSetup{
		ctx:        ctx,
		lookup:     lookup,
		resolver:   resolver,
		state:      state,
		cwd:        options.Cwd,
		version:    options.Version,
		token:      token,
		stdout:     stdout,
		stderr:     stderr,
		beginDrain: func() { stop.signal(stopDrain) },
	})
	if err != nil {
		return err
	}
	// The lifetime ends exactly once: below after serving, or here on an early return.
	defer life.stop(stop)

	var tcp net.Listener
	if control != nil {
		// The listener the supervisor reserved; there is no listener of the worker's own to fall back to.
		tcp = control.listener
	} else {
		port, err := workerPort(lookup)
		if err != nil {
			return err
		}
		if tcp, err = (&net.ListenConfig{}).Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err != nil {
			return fmt.Errorf("model-gateway: worker could not listen on 127.0.0.1:%d: %w", port, err)
		}
	}
	gated := gateListener(ctx, tcp)
	accepts := gated
	var swept *sweepListener
	if control != nil {
		swept = newSweepListener(gated)
		accepts = swept
	}
	listeners := []net.Listener{accepts}
	// unixReady is the socket path whose readiness line is printed once the worker crossed its admission boundary.
	unixReady := ""
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()
	// The optional Unix socket is bound before the admission boundary, but nothing about it is printed and nothing is
	// accepted on it before the boundary; a bind that waits is ended by the parent-loss exit.
	var unixNote string
	if socket, err := resolver.SocketPath(); err != nil {
		unixNote = fmt.Sprintf("model-gateway: could not resolve ANTHROPIC_UNIX_SOCKET: %v\n", err)
	} else if ln, err := worker.ListenUnix(socket); err != nil {
		unixNote = fmt.Sprintf("model-gateway: could not bind ANTHROPIC_UNIX_SOCKET %s: %s\n", socket, errorCode(err))
	} else {
		// Registered with the gate, so that the parent-loss exit removes the bound pathname before it ends the process.
		admission.register(ln)
		listeners = append(listeners, ln)
		unixReady = socket
	}

	handler, err := worker.NewHandler(workerOptions)
	if err != nil {
		return err
	}
	// The admission boundary: nothing before it served, reported or announced readiness. A parent whose end the
	// supervised worker observes now, or observed earlier, wins, and the worker returns without admitting anything. The
	// gate decides first, also when an immediate stop was signalled, so that no return leaves it open to a hard exit.
	if !admission.cross(control) || stop.immediate() {
		return nil
	}
	if unixNote != "" {
		_, _ = io.WriteString(stderr, unixNote)
	}
	life.start()
	life.serve(listeners, refuseAfterStop(stop, handler))

	bound := tcp.Addr().(*net.TCPAddr).Port
	if control != nil {
		if err := control.report(bound); err != nil {
			stop.signal(stopNow)
			life.stop(stop)
			// A write that fails while the liveness pipe shows the parent's end is the parent-loss stop, with no
			// readiness line and a clean return; with the parent alive it is an error, a closed report reader included.
			if parentEnded(control.live) {
				return nil
			}
			return err
		}
	}
	// Both readiness lines follow the report: a worker whose report failed prints neither.
	if unixReady != "" {
		_, _ = fmt.Fprintf(stdout, "model-gateway shim listening on ANTHROPIC_UNIX_SOCKET %s\n", unixReady)
	}
	_, _ = fmt.Fprintf(stdout, "model-gateway shim listening on 127.0.0.1:%d (proxy :%s, anthropic %s)\n", bound, jsNumber(config.ProxyPort.Get(lookup)), upstreamOrigin(config.Upstream.Get(lookup)))

	var serveErr error
	select {
	case serveErr = <-life.served:
		stop.signal(stopNow)
	case <-stop.done:
	}
	if stop.mode() == stopDrain && control != nil {
		drainSupervised(stop, control, gated, swept, stderr)
		life.release(listeners, stop.now)
	}
	life.stop(stop)
	// Closing the descriptors ends a watcher that is still reading them.
	if control != nil {
		control.close()
	}
	if channel != nil {
		_ = channel.Close()
	}
	watchers.Wait()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// drainSupervised runs a supervised worker's side of its drain while its servers still accept: the exchange with the
// supervisor, then, once the supervisor has answered, the final sweep of the listener's queue. Parent loss or
// cancellation ends the exchange at once and skips the rest, with no diagnostic: the worker then stops at once. An
// exchange that fails otherwise is reported in one bounded line, and there is no sweep. The caller then releases the
// listeners; the worker closes only its own copy of the listener, which the supervisor's still shares.
func drainSupervised(stop *stopper, control *controlDescriptors, gated net.Listener, swept *sweepListener, stderr io.Writer) {
	err := drainExchange(stop.nowCtx, control.drain, workerlink.Generation(control.generation))
	switch {
	case stop.nowCtx.Err() != nil:
	case err != nil:
		_, _ = io.WriteString(stderr, boundedLine("model-gateway: the drain exchange with the supervisor failed, closing the listener without its answer: "+err.Error(), maxDrainDiagnostic))
	default:
		announceSweep(gated)
		if err := swept.sweep(stop.now); err != nil {
			_, _ = fmt.Fprintf(stderr, "model-gateway: the final accept sweep of the listener stopped early: %v\n", err)
		}
	}
}

// boundedLine returns text as one line of at most limit bytes, its line feed included.
func boundedLine(text string, limit int) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " ")
	if len(text) > limit-1 {
		text = strings.ToValidUTF8(text[:limit-1], "")
	}
	return text + "\n"
}

// acceptGateKey is the private context key of the function gateListener applies.
type acceptGateKey struct{}

// gateListener returns the TCP listener the worker accepts on: ln, or what the function ctx carries under
// acceptGateKey makes of it. Only the package's tests put such a function into Run's context, to decide when the
// worker's ordinary accept loop may take a connection. The worker uses the returned listener for every accept, its
// final sweep included, and calls announceSweep with it once its supervisor answered the drain, as the sweep begins.
func gateListener(ctx context.Context, ln net.Listener) net.Listener {
	if gate, ok := ctx.Value(acceptGateKey{}).(func(net.Listener) net.Listener); ok && gate != nil {
		return gate(ln)
	}
	return ln
}

// announceSweep tells a listener that wants to know that the worker's final sweep of a drain begins: the ordinary
// accept loop takes no more connections, and the sweep accepts what is already queued. Any other listener is left
// alone.
func announceSweep(ln net.Listener) {
	if l, ok := ln.(interface{ SweepStarting() }); ok {
		l.SweepStarting()
	}
}

func writerOr(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

type stopMode int

const (
	stopNow stopMode = iota + 1
	stopDrain
)

// stopper records the first stop and its kind. An immediate stop that follows a drain still ends the drain.
type stopper struct {
	once    sync.Once
	done    chan struct{} // Closed by the first stop of either kind.
	nowOnce sync.Once
	now     chan struct{} // Closed by any immediate stop.
	// nowCtx ends with now, for the operations of a drain that an immediate stop interrupts.
	nowCtx    context.Context
	nowCancel context.CancelFunc
	mu        sync.Mutex
	which     stopMode
}

func newStopper() *stopper {
	ctx, cancel := context.WithCancel(context.Background())
	return &stopper{done: make(chan struct{}), now: make(chan struct{}), nowCtx: ctx, nowCancel: cancel}
}

func (s *stopper) signal(mode stopMode) {
	s.once.Do(func() {
		s.mu.Lock()
		s.which = mode
		s.mu.Unlock()
		close(s.done)
	})
	if mode == stopNow {
		s.nowOnce.Do(func() {
			close(s.now)
			s.nowCancel()
		})
	}
}

// admissionGate orders the supervised worker's admission boundary against the loss of its parent. It takes one
// decision, under its lock, with three outcomes: the boundary is crossed (the ordinary paths apply from then on); the
// watcher observed the parent's end first (the hard exit: it closes the Unix listeners the worker registered with the
// gate, which removes their pathnames, and ends the process through exit, since nothing was admitted and a startup step
// may still be blocked); or the boundary itself observed the end (Run returns and its deferred closes remove the
// pathnames; the watcher then exits nothing). Once the worker closes the liveness descriptor itself, the hard exit is
// disarmed.
type admissionGate struct {
	exit     func(int)
	mu       sync.Mutex
	decision gateDecision
	disarmed bool
	bound    []net.Listener
}

// gateDecision is the admission gate's one decision.
type gateDecision int

const (
	gateOpen gateDecision = iota
	gateCrossed
	gateLostByWatcher
	gateRefused
)

// register records a listener the hard exit closes; one registered after the hard exit was decided is closed at once.
// The bind itself happens outside the gate's lock.
func (g *admissionGate) register(ln net.Listener) {
	g.mu.Lock()
	late := g.decision == gateLostByWatcher
	if !late {
		g.bound = append(g.bound, ln)
	}
	g.mu.Unlock()
	if late {
		_ = ln.Close()
	}
}

// disarm ends the hard exit: the worker is about to close the liveness descriptor itself.
func (g *admissionGate) disarm() {
	g.mu.Lock()
	g.disarmed = true
	g.mu.Unlock()
}

// parentGone records that the liveness watcher observed the peer's end of file. Before any decision it decides the
// hard exit and carries it out; after one it does nothing.
func (g *admissionGate) parentGone() {
	g.mu.Lock()
	exit := g.decision == gateOpen && !g.disarmed && g.exit != nil
	if g.decision == gateOpen {
		g.decision = gateLostByWatcher
	}
	bound := g.bound
	if exit {
		g.bound = nil
	}
	g.mu.Unlock()
	if !exit {
		return
	}
	for _, ln := range bound {
		_ = ln.Close()
	}
	g.exit(0)
}

// cross decides the boundary: crossed unless the watcher decided first or a synchronous check of the liveness
// descriptor shows the parent's end now, in which case the boundary refuses and Run returns. A worker that is not
// supervised always crosses.
func (g *admissionGate) cross(control *controlDescriptors) bool {
	if control == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.decision != gateOpen:
		return false
	case parentEnded(control.live):
		g.decision = gateRefused
		return false
	}
	g.decision = gateCrossed
	return true
}

// refuseAfterStop aborts every request whose handling would begin after an immediate stop was signalled, so that a
// worker whose parent went away admits nothing more while its servers are being closed.
func refuseAfterStop(stop *stopper, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stop.immediate() {
			panic(http.ErrAbortHandler)
		}
		handler.ServeHTTP(w, r)
	})
}

// immediate reports whether an immediate stop was signalled.
func (s *stopper) immediate() bool { return s.nowCtx.Err() != nil }

func (s *stopper) mode() stopMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.which
}

func workerPort(lookup config.LookupEnv) (int, error) {
	port := config.WorkerPort(lookup)
	if math.IsNaN(port) || port < 0 || port > 65535 || port != math.Trunc(port) {
		return 0, errors.New("model-gateway: CODEX_GATEWAY_WORKER_PORT is not a valid port")
	}
	return int(port), nil
}

// jsNumber formats a port setting as JavaScript's String(Number(value)) does for the readiness line.
func jsNumber(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// upstreamOrigin renders the configured Anthropic upstream for the readiness line as its origin only. Userinfo, path
// and query can carry credentials, and the line is copied to the supervisor's log.
func upstreamOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "(unparsable URL)"
	}
	return u.Scheme + "://" + u.Host
}

// awaitEOF reads until end of file or a read error and reports whether it was the peer's end of file. Data the other
// side writes is ignored.
func awaitEOF(r io.Reader) bool {
	buf := make([]byte, 512)
	for {
		if _, err := r.Read(buf); err != nil {
			return errors.Is(err, io.EOF)
		}
	}
}

func reportLine(port int, generation string) ([]byte, error) {
	line, err := json.Marshal(struct {
		Type       string `json:"type"`
		Port       int    `json:"port"`
		Generation string `json:"generation"`
	}{"listening", port, generation})
	if err != nil {
		return nil, err
	}
	line = append(line, '\n')
	if len(line) > maxReportLength {
		return nil, errors.New("model-gateway: MODEL_GATEWAY_WORKER_GENERATION is too long for the listening report")
	}
	return line, nil
}

// guarded reports whether the test guard is active: either marker is present, whatever its value.
func guarded(lookup config.LookupEnv) bool {
	_, sandbox := lookup(sandboxKey)
	_, runner := lookup("NODE_TEST_CONTEXT")
	return sandbox || runner
}
