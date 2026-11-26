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

package serveworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
	"github.com/zchee/claude-code-model-gateway/internal/telemetry"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/wiring"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// syncWriter serializes whole Write calls of the worker's request handlers and background work onto
// one output stream. Writers built over the same mutex never interleave, even when they share a sink.
type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

const (
	// compatBaseURL is the compatibility base URL the gateway recognizes as its own (lib/runtime.js:24-26).
	compatBaseURL = "http://api.anthropic.com"
	// shellOnlyMessage is the source's startup warning for a base URL wired in the shell only
	// (lib/request-worker.js:1218-1220).
	shellOnlyMessage = "model-gateway: ANTHROPIC_BASE_URL is shell-only; wire it through local .claude/settings.local.json or global settings so background sessions stay metered\n"
	// policyLinePrefix starts the advertised sentry policy lines, which the source prints on stdout
	// (lib/request-worker.js:1450-1458); its other catalog diagnostics go to stderr (:1532).
	policyLinePrefix = "model-gateway: sentry policy "
	// statusWaitDelay bounds how long an auth status command's output pipes are drained after its
	// deadline killed it, so a child that keeps them open cannot outlive the check.
	statusWaitDelay = time.Second
	// maxStatusOutput bounds each output stream of an auth status command: Node's execFile default
	// maxBuffer, which the source's call keeps.
	maxStatusOutput = 1 << 20
	// establishTimeout bounds the dial and TLS handshake of a catalog, listing or readiness connection.
	// No request on those transports waits longer than a catalog page, and with keep-alives off a
	// connection that arrives after its request gave up serves no other request.
	establishTimeout = modelcache.AnthropicPageTimeout
)

// lifetime owns everything of one worker that outlives a single call: the servers and their Serve
// loops, every request handler, the service context, the background catalog and readiness work and
// their connections, the Grok credential refresh, the upstream client and the exporters. stop is the
// only place it ends.
type lifetime struct {
	ctx    context.Context
	cancel context.CancelFunc

	cache     *modelcache.Cache
	readiness *readiness.SharedCheck
	grok      *grok.Branch
	upstream  *upstream.Client
	auth      *http.Transport
	dials     *dials
	usage     *usage.Emitter
	telemetry *telemetry.Recorder

	servers []*http.Server
	serving sync.WaitGroup
	served  chan error // one result per Serve loop; buffered, so no loop waits for a reader

	handlers sync.WaitGroup // every request handler that was admitted
	mu       sync.Mutex
	closed   bool          // no handler is admitted any more; guarded by mu
	live     int           // admitted handlers whose client has not left; guarded by mu
	changed  chan struct{} // signalled whenever live drops

	stopOnce sync.Once
}

// start begins the catalog refreshes under the service context.
func (l *lifetime) start() { l.cache.Start(l.ctx) }

// serve starts one server per listener, all sharing handler.
func (l *lifetime) serve(listeners []net.Listener, handler http.Handler) {
	tracked := l.track(handler)
	l.served = make(chan error, len(listeners))
	for _, ln := range listeners {
		// The general OPTIONS handler would answer "OPTIONS *" itself, past the handler's checks.
		srv := &http.Server{Handler: tracked, ReadHeaderTimeout: headersTimeout, IdleTimeout: keepAliveTimeout, DisableGeneralOptionsHandler: true}
		l.servers = append(l.servers, srv)
		l.serving.Go(func() { l.served <- srv.Serve(ln) })
	}
}

// track wraps handler so that every request handler is joined at shutdown and counted as live until
// it returns or its client leaves, whichever comes first. A request that arrives after admission
// closed is aborted without reaching handler.
func (l *lifetime) track(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.enter() {
			panic(http.ErrAbortHandler)
		}
		defer l.handlers.Done()
		leave := sync.OnceFunc(l.leave)
		stopLeave := context.AfterFunc(r.Context(), leave)
		defer func() {
			stopLeave()
			// Waits for a departure callback that is already running.
			leave()
		}()
		handler.ServeHTTP(w, r)
	})
}

func (l *lifetime) enter() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.live++
	l.handlers.Add(1)
	return true
}

func (l *lifetime) leave() {
	l.mu.Lock()
	l.live--
	l.mu.Unlock()
	select {
	case l.changed <- struct{}{}:
	default:
	}
}

// awaitLive returns once no admitted handler has a client that is still there, or when now closes.
func (l *lifetime) awaitLive(now <-chan struct{}) {
	for {
		l.mu.Lock()
		live := l.live
		l.mu.Unlock()
		if live == 0 {
			return
		}
		select {
		case <-l.changed:
		case <-now:
			return
		}
	}
}

// stop ends the worker, once, in this order:
//  1. stop admitting connections: every listener closes, and a drain also closes idle connections;
//  2. a drain lets the responses whose client is still there finish; an immediate stop, also one
//     that arrives during the drain, skips the rest of the wait;
//  3. cancel the service context: upstream exchanges, relays, the compaction guard, a Grok credential
//     wait and every background refresh end, so the handler of a client that already left returns;
//  4. a drain lets net/http finish the connections that are left; then every connection is closed
//     and every Serve loop is joined;
//  5. close admission and join every request handler, so none still submits work below;
//  6. join the Grok credential refresh: a token exchange already sent finishes under its own
//     deadline and stores its result, a discovery has ended with the service;
//  7. close the upstream client and the credential transport's idle connections, and end and join
//     the connection establishment of the catalog and readiness transports;
//  8. join the readiness check, the model cache, the usage exporter and the trace exporter.
//
// A nil stopper stops at once. Nothing of the worker runs after stop returns.
func (l *lifetime) stop(stop *stopper) {
	l.stopOnce.Do(func() {
		if stop != nil && stop.mode() == stopDrain {
			l.drain(stop.now)
		} else {
			l.cancel()
		}
		for _, srv := range l.servers {
			_ = srv.Close()
		}
		l.serving.Wait()
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		l.handlers.Wait()
		if l.grok != nil {
			l.grok.Wait()
		}
		_ = l.upstream.Close()
		l.auth.CloseIdleConnections()
		l.dials.close()
		if l.readiness != nil {
			l.readiness.Wait()
		}
		if l.cache != nil {
			l.cache.Wait()
		}
		if l.usage != nil {
			l.usage.Wait()
		}
		if l.telemetry != nil {
			l.telemetry.Wait()
		}
	})
}

// dials owns the transports of the worker's catalog, listing and readiness requests. Their
// connections are bound to its context, which ends with the service: no dial starts afterwards, a dial
// or TLS handshake in progress is cancelled and every connection closes, the connection of a response
// that switched protocols included. net/http carries on a dial after the request that started it
// ended, so close also waits for every dial and handshake still running; what net/http does with the
// result once the dial function returned is not part of that wait. Unlike the package transports these
// replace, establishment is also bounded by establishTimeout.
type dials struct {
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool // no dial starts any more; guarded by mu
	running sync.WaitGroup
}

func newDials(parent context.Context) *dials {
	ctx, cancel := context.WithCancel(parent)
	return &dials{ctx: ctx, cancel: cancel}
}

// transport returns a new transport of the upstream policy bound to d. Like the package transports
// it replaces, it never consults the environment's proxy settings, keeps no idle connection and asks
// for a gzip body that net/http decodes. It never returns a response that switched protocols.
func (d *dials) transport() http.RoundTripper {
	t := upstream.NewLifetimeTransport(upstream.Options{}, d.ctx)
	t.DisableKeepAlives = true
	t.DisableCompression = false
	t.DialContext = d.track(t.DialContext)
	t.DialTLSContext = d.track(t.DialTLSContext)
	return refusingSwitch{t}
}

// refusingSwitch fails a request whose response switched protocols, as the upstream client does,
// after closing that connection. The readiness check and the local provider probes read a body
// whatever its status, and no deadline of theirs would end the read of a switched connection.
type refusingSwitch struct{ *http.Transport }

func (t refusingSwitch) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.Transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if err := upstream.RefuseSwitch(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// track counts every call of dial until it returns and bounds it by establishTimeout. Once close
// began, no call starts.
func (d *dials) track(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return nil, net.ErrClosed
		}
		d.running.Add(1)
		d.mu.Unlock()
		defer d.running.Done()
		ctx, cancel := context.WithTimeout(ctx, establishTimeout)
		defer cancel()
		return dial(ctx, network, addr)
	}
}

// close ends d's context and returns once no dial or handshake of its transports is running.
func (d *dials) close() {
	d.cancel()
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.running.Wait()
}

// drain closes every listener, waits for the live responses (lib/request-worker.js:1235-1258), cancels
// the service and lets net/http finish the remaining connections, until now closes: the parent's or the
// fixture's end of file, or cancellation, which closes every connection as the source's exit on
// disconnect does (lib/request-worker.js:1210). net/http writes the end of a response after its handler
// returns, so connections are not closed the moment the last live handler returns.
func (l *lifetime) drain(now <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var draining sync.WaitGroup
	for _, srv := range l.servers {
		draining.Go(func() { _ = srv.Shutdown(ctx) })
	}
	drained := make(chan struct{})
	go func() { draining.Wait(); close(drained) }()
	l.awaitLive(now)
	l.cancel()
	select {
	case <-drained:
	case <-now:
		cancel()
		<-drained
	}
}

// workerSetup carries what the collaborators are built from.
type workerSetup struct {
	ctx            context.Context
	lookup         config.LookupEnv
	resolver       paths.Resolver
	state          string
	cwd            func() (string, error)
	version, token string
	stdout, stderr io.Writer
	beginDrain     func()
}

// buildWorker constructs the worker's collaborators and composes its handler options. It starts no
// background work; the returned lifetime's cache is started by the caller once it serves.
//
// The context window settings are read here and an unusable file stops the worker, as the source
// fails to load (lib/runtime.js:307-320).
func buildWorker(s workerSetup) (worker.Options, *lifetime, error) {
	windows, err := policy.ReadWindows(filepath.Join(s.state, "context-window.json"), s.lookup)
	if err != nil {
		return worker.Options{}, nil, fmt.Errorf("model-gateway: the context window settings are unusable: %w", err)
	}
	now := time.Now
	var project string
	if s.cwd != nil {
		if dir, err := s.cwd(); err == nil && filepath.IsAbs(dir) {
			project = dir
		}
	}
	startup := project
	if startup == "" {
		startup = s.state
	}
	proxyBase := "http://127.0.0.1:" + jsNumber(config.ProxyPort.Get(s.lookup))
	defaultBase := "http://127.0.0.1:" + jsNumber(config.PublicPort.Get(s.lookup))
	anthropicBase := config.Upstream.Get(s.lookup)
	sentryEnabled := config.Sentry.Get(s.lookup)

	serviceCtx, cancel := context.WithCancel(s.ctx)
	client := upstream.NewClient(upstream.Options{})
	// Credential discovery and refresh never share the inference client's pool or lifetime.
	authTransport := upstream.NewTransport(upstream.Options{})
	// The catalog, listing and readiness requests get transports of their own, bound to the service.
	owned := newDials(serviceCtx)
	life := &lifetime{ctx: serviceCtx, cancel: cancel, upstream: client, auth: authTransport, dials: owned, changed: make(chan struct{}, 1)}
	fail := func(err error) (worker.Options, *lifetime, error) {
		life.stop(nil)
		return worker.Options{}, nil, err
	}

	routes, state := &route.Store{}, &sentry.State{}
	grokHome, grokErr := s.resolver.GrokHome()
	var grokIDs func() []string
	if grokErr == nil {
		grokIDs = compose.NewGrokModelIDs(filepath.Join(grokHome, "models_cache.json"))
	}
	// The effective base URL depends on the project the worker was started in; without one the worker
	// cannot be shown to be wired, and the discovery cache is left alone.
	var effectiveBaseURL func() (string, error)
	settingsLevel := false
	if project != "" {
		settings := wiring.Store{Paths: s.resolver, Project: project, Lookup: s.lookup}
		effectiveBaseURL = func() (string, error) {
			r, err := settings.EffectiveBaseURL()
			if err != nil || r.Winner == nil {
				return "", err
			}
			return r.Winner.Value, nil
		}
		settingsLevel = startupWiring(settings, s.lookup, defaultBase, s.stderr)
	}
	cache, err := modelcache.New(modelcache.Options{
		Lookup:           s.lookup,
		Paths:            s.resolver,
		Windows:          windows,
		ProxyBaseURL:     proxyBase,
		AnthropicBaseURL: anthropicBase,
		DefaultBaseURL:   defaultBase,
		Client:           &http.Client{Transport: owned.transport()},
		Clock:            now,
		Routes:           routes,
		Sentry:           state,
		EffectiveBaseURL: effectiveBaseURL,
		CurrentPins:      func() map[string]string { return currentPins(s.state) },
		GrokIDs:          grokIDs,
		Logf: func(format string, args ...any) {
			sink := s.stderr
			if strings.HasPrefix(format, policyLinePrefix) {
				sink = s.stdout
			}
			_, _ = fmt.Fprintf(sink, format+"\n", args...)
		},
	})
	if err != nil {
		return fail(err)
	}
	life.cache = cache

	var grokAuth, grokVersion string
	if grokErr == nil {
		grokAuth, grokVersion = filepath.Join(grokHome, "auth.json"), filepath.Join(grokHome, "version.json")
	}
	grokBranch, err := compose.NewGrokBranch(compose.GrokOptions{
		Endpoint:        config.GrokEndpoint.Get(s.lookup),
		VersionFile:     grokVersion,
		AuthFile:        grokAuth,
		InferenceClient: client,
		AuthTransport:   authTransport,
		Sentry:          state,
		SentryEnabled:   sentryEnabled,
		Now:             now,
	})
	if err != nil {
		return fail(err)
	}
	life.grok = grokBranch
	dispatchPath, err := s.resolver.DispatchCachePath()
	if err != nil {
		return fail(fmt.Errorf("model-gateway: could not resolve the dispatch route cache: %w", err))
	}
	prepare, err := proxyreq.New(proxyreq.Options{StartupDir: startup, DispatchPath: dispatchPath, Lookup: s.lookup, Now: now, Windows: windows})
	if err != nil {
		return fail(err)
	}

	projects, _ := s.resolver.TranscriptsDir()
	life.usage = usage.NewEmitter(s.lookup, usage.NewBuilder(usage.Options{ProjectsDir: projects, Now: now}))
	var highWater *usage.HighWater
	if dir, err := s.resolver.RequestBodyDir(); err == nil {
		highWater = usage.NewHighWater(dir, now)
	}
	var logPath string
	if config.RequestLog.Get(s.lookup) {
		logPath, _ = s.resolver.RequestLogPath()
	}
	life.telemetry = telemetry.NewRecorder(telemetry.Options{Lookup: s.lookup, LogPath: logPath, Diagnostics: s.stderr, Now: now})

	// The shared proxy and Codex auth check behind /healthz runs under the service context
	// (lib/request-worker.js:47-61,102-114,1229).
	probe := readiness.ProxyAuthProbe{
		ProxyBinary:    proxyBinary(s.state),
		ProxyModelsURL: proxyBase + "/v1/models",
		Client:         &http.Client{Transport: owned.transport()},
		RunAuthStatus:  runStatus,
	}
	life.readiness = readiness.NewSharedCheck(serviceCtx, func(ctx context.Context) readiness.ProxyAuthResult {
		return readiness.ProbeProxyAndAuth(ctx, probe)
	}, now)

	// The running executable stands in for the stable binary in operator advice until one is installed.
	command, _ := os.Executable()
	options, err := compose.New(compose.Options{
		ServiceContext:       serviceCtx,
		Version:              s.version,
		ControlToken:         s.token,
		Lookup:               s.lookup,
		AnthropicBaseURL:     anthropicBase,
		ProxyBaseURL:         proxyBase,
		CommandPath:          command,
		InstalledVersion:     s.version,
		LogsDir:              filepath.Join(s.state, "logs"),
		Windows:              windows,
		SentryEnabled:        sentryEnabled,
		SentryEnvTrigger:     config.CompactTrigger.Get(s.lookup),
		Routes:               routes,
		ModelCache:           cache,
		Sentry:               state,
		Upstream:             client,
		UpstreamState:        &upstreamstate.Store{Dir: s.state, Now: now, CommandPath: command},
		ProxyReq:             prepare,
		CompactGuard:         &compactguard.Runner{ServiceContext: serviceCtx, Settings: compactguard.LoadSettings(s.lookup), Stderr: s.stderr},
		Grok:                 grokBranch,
		Usage:                life.usage,
		HighWater:            highWater,
		Telemetry:            life.telemetry,
		ReadinessCheck:       life.readiness,
		SettingsLevelBaseURL: settingsLevel,
		Now:                  now,
		BeginDrain:           s.beginDrain,
		Diagnostics:          s.stderr,
	})
	if err != nil {
		return fail(err)
	}
	return options, life, nil
}

// startupWiring is the source's startup read of the settings wiring (lib/request-worker.js:1218-1220):
// it reports whether the gateway's base URL is wired, through a settings file or the environment, and
// warns when the environment names a gateway URL that no wiring backs. The source skips an unreadable
// settings file and goes on; the Go settings reader fails as a whole, and a failed read reports the
// worker as not wired and warns about nothing.
func startupWiring(settings wiring.Store, lookup config.LookupEnv, defaultBase string, stderr io.Writer) bool {
	wired, err := settings.WiredMode()
	if err != nil {
		return false
	}
	if env := config.AnthropicBaseURL.Get(lookup); env != nil && (*env == defaultBase || *env == compatBaseURL) && wired == nil {
		_, _ = io.WriteString(stderr, shellOnlyMessage)
	}
	return wired != nil
}

// proxyBinary is the installed claude-code-proxy binary (lib/runtime.js:18,23).
func proxyBinary(state string) string {
	name := "claude-code-proxy"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(state, "bin", name)
}

// errStatusOutput reports an auth status command that printed more than maxStatusOutput bytes on
// one stream.
var errStatusOutput = errors.New("model-gateway: auth status output exceeded its limit")

// runStatus runs one auth status command until it exits or ctx ends, and returns what it printed.
// Each stream holds at most maxStatusOutput bytes, as execFile's default maxBuffer does for the
// source's call (lib/request-worker.js:49-53). A stream that goes past it stops the command, which is
// killed and reaped, and the run fails with errStatusOutput and no output.
func runStatus(ctx context.Context, binary string, args []string) (stdout, stderr []byte, err error) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(ctx, binary, args...)
	out, errOut := &cappedOutput{overflow: stop}, &cappedOutput{overflow: stop}
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.WaitDelay = statusWaitDelay
	err = cmd.Run()
	// Run has joined the goroutines that copy the two streams, so their state is settled here.
	if out.exceeded || errOut.exceeded {
		return nil, nil, errStatusOutput
	}
	return out.buf.Bytes(), errOut.buf.Bytes(), err
}

// cappedOutput collects one output stream of a command up to maxStatusOutput bytes. The write that
// would go past the limit stores nothing, calls overflow and fails, which ends the copy of the
// stream; every later write fails too. Only one goroutine writes to it.
type cappedOutput struct {
	buf      bytes.Buffer
	overflow func()
	exceeded bool
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	if c.exceeded || c.buf.Len()+len(p) > maxStatusOutput {
		if !c.exceeded {
			c.exceeded = true
			c.overflow()
		}
		return 0, errStatusOutput
	}
	return c.buf.Write(p)
}
