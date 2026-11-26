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
	"sync"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
)

// Options supplies startup dependencies without requiring process-global lookups.
type Options struct {
	Lookup         config.LookupEnv
	Home, Cwd      func() (string, error)
	Version        string
	Stdout, Stderr io.Writer
}

const (
	controlFDsKey   = "MODEL_GATEWAY_CONTROL_FDS"
	generationKey   = "MODEL_GATEWAY_WORKER_GENERATION"
	nodeChannelKey  = "NODE_CHANNEL_FD"
	sandboxKey      = "MODEL_GATEWAY_TEST_SANDBOX"
	maxReportLength = 256
	// headersTimeout and keepAliveTimeout are the source's server settings (lib/request-worker.js:2676-2682).
	headersTimeout   = 120 * time.Second
	keepAliveTimeout = 75 * time.Second
)

// Run serves until drain, parent disconnect, or cancellation, returning a startup or serving error.
//
// A supervised worker (MODEL_GATEWAY_CONTROL_FDS set) writes one listening report to the first descriptor, closes it,
// and stops when the second descriptor reaches end of file. A standalone worker reads no descriptor; under the test
// guard it stops when NODE_CHANNEL_FD reaches end of file, and otherwise it closes that descriptor unread. Both print
// the TCP readiness line on Stdout after every listener is bound; it names the Anthropic upstream by its origin only.
// A drain closes every listener at once and lets in-flight requests finish, unless the parent, the fixture channel or
// cancellation ends the worker meanwhile; those stops close every connection at once.
func Run(ctx context.Context, options Options) error {
	lookup := options.Lookup
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	stdout, stderr := writerOr(options.Stdout), writerOr(options.Stderr)

	control, err := supervisedDescriptors(lookup)
	if err != nil {
		return err
	}
	if control != nil {
		defer control.close()
	}
	var channel *os.File
	if control == nil {
		channel, err = fixtureChannel(lookup)
		if err != nil {
			return err
		}
	}
	if channel != nil {
		defer func() { _ = channel.Close() }()
	}

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

	port, err := workerPort(lookup)
	if err != nil {
		return err
	}
	tcp, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("model-gateway: worker could not listen on 127.0.0.1:%d: %w", port, err)
	}
	listeners := []net.Listener{tcp}
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()
	if socket, err := resolver.SocketPath(); err != nil {
		_, _ = fmt.Fprintf(stderr, "model-gateway: could not resolve ANTHROPIC_UNIX_SOCKET: %v\n", err)
	} else if ln, err := worker.ListenUnix(socket); err != nil {
		_, _ = fmt.Fprintf(stderr, "model-gateway: could not bind ANTHROPIC_UNIX_SOCKET %s: %s\n", socket, errorCode(err))
	} else {
		listeners = append(listeners, ln)
		_, _ = fmt.Fprintf(stdout, "model-gateway shim listening on ANTHROPIC_UNIX_SOCKET %s\n", socket)
	}

	stop := newStopper()
	handler, err := worker.NewHandler(worker.Options{
		Version:      options.Version,
		ControlToken: token,
		Lookup:       lookup,
		BeginDrain:   func() { stop.signal(stopDrain) },
		Diagnostics:  stderr,
	})
	if err != nil {
		return err
	}
	servers := make([]*http.Server, len(listeners))
	served := make(chan error, len(listeners))
	for i, ln := range listeners {
		// The general OPTIONS handler would answer "OPTIONS *" itself, past the handler's checks.
		servers[i] = &http.Server{Handler: handler, ReadHeaderTimeout: headersTimeout, IdleTimeout: keepAliveTimeout, DisableGeneralOptionsHandler: true}
		go func() { served <- servers[i].Serve(ln) }()
	}

	bound := tcp.Addr().(*net.TCPAddr).Port
	if control != nil {
		if err := control.report(bound); err != nil {
			stop.signal(stopNow)
			shutdown(servers, served, stop, 0)
			return err
		}
	}
	_, _ = fmt.Fprintf(stdout, "model-gateway shim listening on 127.0.0.1:%d (proxy :%s, anthropic %s)\n", bound, jsNumber(config.ProxyPort.Get(lookup)), upstreamOrigin(config.Upstream.Get(lookup)))

	// Cancellation, the parent's end of file and the fixture's end of file stop the worker at once, also while it drains.
	stopOnCancel := context.AfterFunc(ctx, func() { stop.signal(stopNow) })
	defer stopOnCancel()
	var watchers sync.WaitGroup
	switch {
	case control != nil:
		watchers.Go(func() { awaitEOF(control.live); stop.signal(stopNow) })
	case channel != nil:
		watchers.Go(func() { awaitEOF(channel); stop.signal(stopNow) })
	}
	var serveErr error
	received := 0
	select {
	case serveErr = <-served:
		received++
		stop.signal(stopNow)
	case <-stop.done:
	}
	shutdown(servers, served, stop, received)
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
	mu      sync.Mutex
	which   stopMode
}

func newStopper() *stopper { return &stopper{done: make(chan struct{}), now: make(chan struct{})} }

func (s *stopper) signal(mode stopMode) {
	s.once.Do(func() {
		s.mu.Lock()
		s.which = mode
		s.mu.Unlock()
		close(s.done)
	})
	if mode == stopNow {
		s.nowOnce.Do(func() { close(s.now) })
	}
}

func (s *stopper) mode() stopMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.which
}

// shutdown stops every server at the same time. A drain closes all listeners together and lets in-flight requests
// finish (lib/request-worker.js:1235-1258) until an immediate stop arrives: the parent's or the fixture's end of file,
// or cancellation. That closes every connection, as the source's exit on disconnect does (lib/request-worker.js:1210).
// Any other stop closes the connections at once.
// received is the number of Serve results the caller has already taken from served.
func shutdown(servers []*http.Server, served chan error, stop *stopper, received int) {
	if stop.mode() == stopDrain {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var draining sync.WaitGroup
		for _, srv := range servers {
			draining.Go(func() { _ = srv.Shutdown(ctx) })
		}
		drained := make(chan struct{})
		go func() { draining.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-stop.now:
			cancel()
			<-drained
		}
	}
	for _, srv := range servers {
		_ = srv.Close()
	}
	// Every Serve call has returned once its server is closed; collect them so no goroutine outlives Run.
	for range len(servers) - received {
		<-served
	}
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

// awaitEOF reads until end of file or a read error. Data the other side writes is ignored.
func awaitEOF(r io.Reader) {
	buf := make([]byte, 512)
	for {
		if _, err := r.Read(buf); err != nil {
			return
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
