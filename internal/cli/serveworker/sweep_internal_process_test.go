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

//go:build process && (linux || darwin)

package serveworker

import (
	"bufio"
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

const sweepToken = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// gatedListener lets the worker's ordinary accept loop take a connection only while the gate is open. Once the worker
// announced its final sweep, accepts pass again; after Close, a waiting or later accept fails at once and is not a
// sweep. Its deadline and its raw connection are the underlying listener's, so a sweep that accepts through the raw
// descriptor until EAGAIN works as well as one that accepts under a deadline. An accept parked at the closed gate
// honours a deadline set meanwhile and then fails as a timeout, so a worker that deadlines and joins its ordinary
// accept before it announces the sweep does not wait for the gate. An announcement made before the test allowed it
// is recorded as early.
type gatedListener struct {
	net.Listener
	mu       sync.Mutex
	open     bool
	sweeping bool
	closed   bool
	// allowed says that the test has begun to answer the drain notice; early records an announcement before that.
	allowed  bool
	early    bool
	deadline time.Time
	wake     chan struct{}
	parked   chan struct{}
}

func newGatedListener(ln net.Listener) *gatedListener {
	return &gatedListener{Listener: ln, open: true, wake: make(chan struct{}), parked: make(chan struct{}, 1)}
}

func (g *gatedListener) Accept() (net.Conn, error) {
	for {
		g.mu.Lock()
		switch {
		case g.closed:
			g.mu.Unlock()
			return nil, net.ErrClosed
		case g.open || g.sweeping:
			g.mu.Unlock()
			return g.Listener.Accept()
		}
		wake, deadline := g.wake, g.deadline
		g.mu.Unlock()
		select {
		case g.parked <- struct{}{}:
		default:
		}
		if deadline.IsZero() {
			<-wake
			continue
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
			return nil, &net.OpError{Op: "accept", Net: g.Addr().Network(), Addr: g.Addr(), Err: os.ErrDeadlineExceeded}
		}
	}
}

// wakeLocked lets every parked accept look at the gate again.
func (g *gatedListener) wakeLocked() {
	close(g.wake)
	g.wake = make(chan struct{})
}

// allowSweep records that the test is answering the drain notice: an announcement from now on is not early.
func (g *gatedListener) allowSweep() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allowed = true
}

// sweptEarly reports whether the worker announced its sweep before the test began to answer its drain notice.
func (g *gatedListener) sweptEarly() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.early
}

// SweepStarting is the worker's announcement that its final sweep begins.
func (g *gatedListener) SweepStarting() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.sweeping {
		g.sweeping = true
		g.early = !g.allowed
		g.wakeLocked()
	}
}

func (g *gatedListener) sweepStarted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sweeping
}

func (g *gatedListener) closeGate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open = false
}

func (g *gatedListener) SetDeadline(t time.Time) error {
	d, ok := g.Listener.(interface{ SetDeadline(time.Time) error })
	if !ok {
		return errors.New("the gated listener's listener has no deadline")
	}
	g.mu.Lock()
	g.deadline = t
	g.wakeLocked()
	g.mu.Unlock()
	return d.SetDeadline(t)
}

func (g *gatedListener) SyscallConn() (syscall.RawConn, error) {
	sc, ok := g.Listener.(syscall.Conn)
	if !ok {
		return nil, errors.New("the gated listener's listener has no raw connection")
	}
	return sc.SyscallConn()
}

func (g *gatedListener) Close() error {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		g.wakeLocked()
	}
	g.mu.Unlock()
	return g.Listener.Close()
}

// TestAnnounceSweepLeavesOtherListenersAlone checks that the sweep announcement reaches only a listener that asks for
// it: an ordinary listener keeps accepting.
func TestAnnounceSweepLeavesOtherListenersAlone(t *testing.T) {
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	announceSweep(ln)
	gate := newGatedListener(ln)
	announceSweep(gate)
	tests := map[string]struct {
		got, want bool
	}{
		"success: a gated listener learns that the sweep began": {gate.sweepStarted(), true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tc.want, tc.got); diff != "" {
				t.Errorf("sweep announced (-want +got):\n%s", diff)
			}
		})
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("an ordinary listener stopped accepting after the announcement: %v", err)
	}
	_ = conn.Close()
}

// rawDescriptor returns a close-on-exec duplicate of f as a bare descriptor number that the test hands over to Run,
// and closes f. It reaches the descriptor through SyscallConn, because Fd would put the shared open file description
// into blocking mode.
func rawDescriptor(t testing.TB, f *os.File) int {
	t.Helper()
	defer func() { _ = f.Close() }()
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	n := -1
	var dupErr error
	if err := raw.Control(func(fd uintptr) { n, dupErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 3) }); err != nil {
		t.Fatal(err)
	}
	if dupErr != nil {
		t.Fatal(dupErr)
	}
	return n
}

// openDescriptors lists the descriptor numbers open in this process below 65,536 or its descriptor limit, whichever
// is lower, without opening one to read them. A number whose inspection fails for any reason other than EBADF counts
// as open. The cap suffices while fewer than 65,536 descriptors are open, as a new descriptor takes the lowest free
// number.
func openDescriptors() []int {
	limit := 1 << 16
	var rl unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NOFILE, &rl) == nil && rl.Cur < uint64(limit) {
		limit = int(rl.Cur)
	}
	var open []int
	for n := range limit {
		if _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); err == nil || !errors.Is(err, unix.EBADF) {
			open = append(open, n)
		}
	}
	return open
}

// inProcessHandoff prepares a supervised handoff whose descriptors are bare numbers of this process, for Run started
// here. The reservation is a real workerlink reservation.
type inProcessHandoff struct {
	env        gatewaytest.Environment
	home       string
	res        *workerlink.Reservation
	drain      net.Conn
	reportR    *os.File
	liveW      *os.File
	handedOver []int
	port       uint16
}

func newInProcessHandoff(t testing.TB) *inProcessHandoff {
	t.Helper()
	home, err := os.MkdirTemp("", "mgw-sweep-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	state := filepath.Join(env["CLAUDE_CONFIG_DIR"], "model-gateway")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(sweepToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env["CODEX_GATEWAY_SOCKET_PATH"] = filepath.Join(home, "s.sock")
	var cfg net.ListenConfig
	var dialer net.Dialer
	reg := workerlink.NewRegistry(cfg.Listen, dialer.DialContext)
	res, err := reg.Reserve(t.Context(), "sweep-generation", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.End() })
	listener, err := res.File()
	if err != nil {
		t.Fatal(err)
	}
	drain, child, err := res.DrainChannel()
	if err != nil {
		t.Fatal(err)
	}
	reportR, reportW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	liveR, liveW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = reportR.Close(), liveW.Close() })
	h := &inProcessHandoff{env: env, home: home, res: res, drain: drain, reportR: reportR, liveW: liveW, port: res.Port()}
	report, live, ln, dr := rawDescriptor(t, reportW), rawDescriptor(t, liveR), rawDescriptor(t, listener), rawDescriptor(t, child)
	h.handedOver = []int{report, live, ln, dr}
	env["MODEL_GATEWAY_CONTROL_FDS"] = strconv.Itoa(report) + "," + strconv.Itoa(live)
	env[workerlink.ListenerFDEnv] = strconv.Itoa(ln)
	env[workerlink.DrainFDEnv] = strconv.Itoa(dr)
	env["MODEL_GATEWAY_WORKER_GENERATION"] = "sweep-generation"
	env["CODEX_GATEWAY_WORKER_PORT"] = strconv.Itoa(int(h.port))
	return h
}

// run starts Run in this process with ctx and returns its readiness port channel and its result channel.
func (h *inProcessHandoff) run(ctx context.Context) (<-chan string, <-chan error) {
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Lookup: func(k string) (string, bool) { v, ok := h.env[k]; return v, ok },
			Home:   func() (string, error) { return h.home, nil },
			Cwd:    func() (string, error) { return h.home, nil },
			Stdout: writerFunc(func(p []byte) (int, error) {
				if line, ok := strings.CutPrefix(string(p), "model-gateway shim listening on 127.0.0.1:"); ok {
					port, _, _ := strings.Cut(strings.TrimSpace(line), " ")
					select {
					case ready <- port:
					default:
					}
				}
				return len(p), nil
			}),
			Stderr: io.Discard,
		})
	}()
	return ready, done
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// queuedRequest dials address and writes a complete health request on the new connection without reading.
func queuedRequest(t *testing.T, address string) net.Conn {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp4", address)
	if err != nil {
		t.Fatalf("dial %s: %v", address, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: "+address+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	return conn
}

// TestDrainFinalSweepServesQueuedConnections checks the worker's final sweep after its supervisor answered a drain:
// connections that wait in the listener's queue while the ordinary accept loop takes none are accepted by the sweep
// and served by the worker's own handler, and the sweep does not wait once the queue is empty, so the listener closes
// promptly after the answer. The accept loop is held at a gate the test controls; a sweep is what accepts after the
// gate's deadline was set.
func TestDrainFinalSweepServesQueuedConnections(t *testing.T) {
	h := newInProcessHandoff(t)
	var gate *gatedListener
	ctx := context.WithValue(t.Context(), acceptGateKey{}, func(ln net.Listener) net.Listener {
		gate = newGatedListener(ln)
		return gate
	})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ready, done := h.run(ctx)
	defer func() { _ = h.liveW.Close() }()
	var port string
	select {
	case port = <-ready:
	case err := <-done:
		t.Fatalf("the worker stopped before it was ready: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if port != strconv.Itoa(int(h.port)) || gate == nil {
		t.Fatalf("the worker serves on port %s through the gate %t; want the inherited port %d through the gate", port, gate != nil, h.port)
	}
	public := "127.0.0.1:" + port
	// A kept-alive connection, accepted while the gate is open, carries the drain request later.
	keep := &http.Transport{}
	defer keep.CloseIdleConnections()
	health, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+public+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := keep.RoundTrip(health)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	gate.closeGate()
	// An accept already in progress takes the next connection; this one is served at once or waits in the queue.
	queued := []net.Conn{queuedRequest(t, public)}
	select {
	case <-gate.parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker's accept loop never came back to the gate")
	}
	queued = append(queued, queuedRequest(t, public), queuedRequest(t, public))

	drain, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+public+"/drain", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	drain.Header.Set("Content-Type", "application/json")
	drain.Header.Set("X-Model-Gateway-Control-Token", sweepToken)
	resp, err = keep.RoundTrip(drain)
	if err != nil {
		t.Fatalf("drain on the kept-alive connection: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("drain status %d; want 202", resp.StatusCode)
	}
	keep.CloseIdleConnections()
	if err := h.drain.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	msg, err := workerlink.DecodeDrain(bufio.NewReader(h.drain))
	if err != nil || msg != (workerlink.DrainMessage{Type: workerlink.Draining, Generation: "sweep-generation"}) {
		t.Fatalf("drain notice %+v, %v; want draining for sweep-generation", msg, err)
	}
	// The worker still admits connections until its notice is answered, so its final sweep cannot have begun before
	// this point, where the test starts to answer; an announcement from here on is not early.
	gate.allowSweep()
	if gate.sweptEarly() {
		t.Error("the worker announced its final sweep before its drain notice was answered")
	}
	h.res.Retire()
	if err := h.res.AcknowledgeDrain(ctx); err != nil {
		t.Fatalf("answer the drain notice: %v", err)
	}
	answeredAt := time.Now()
	for i, conn := range queued {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Errorf("queued connection %d: %v; want it served", i, err)
			continue
		}
		var body struct {
			OK bool `json:"ok"`
		}
		err = json.UnmarshalRead(resp.Body, &body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil || !body.OK {
			t.Errorf("queued connection %d: status %d, health ok %t, %v; want the worker's own health answer", i, resp.StatusCode, body.OK, err)
		}
	}
	if !gate.sweepStarted() {
		t.Error("the queued connections were served without the worker announcing its final sweep")
	}
	// With the queue empty the sweep ends at once and the listener closes.
	var dialer net.Dialer
	for {
		conn, err := dialer.DialContext(ctx, "tcp4", public)
		if err != nil {
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("dial after the sweep: %v", err)
			}
			break
		}
		_ = conn.Close()
		if time.Since(answeredAt) > 2*time.Second {
			t.Fatalf("the worker's port still accepted connections %v after the answer with an empty queue", time.Since(answeredAt))
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the worker returned %v after the drain; want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the worker did not return after the drain")
	}
}

// UnwindCensusHelper is the fixture helper that takes the census of TestSupervisedImportUnwindsOnLaterFailure in a
// process of its own. The package's TestGatewayFixtureHelper dispatches it to UnwindCensus.
const UnwindCensusHelper = "unwind-census"

// unwindCensusClean is the line the census helper prints when its census found nothing left open.
const unwindCensusClean = "unwind census clean"

// UnwindCensus is the census helper's part of TestSupervisedImportUnwindsOnLaterFailure, for the external test
// package's fixture dispatch.
func UnwindCensus(t testing.TB) { unwindCensus(t) }

// TestSupervisedImportUnwindsOnLaterFailure checks that a worker whose drain descriptor is refused after its listener
// was imported releases the imported listener and consumes every descriptor the handoff named, so that a failed
// start leaves none of them open in the process and no descriptor of its own either.
//
// The census runs in a fixture helper process that does nothing else, so no other test can open a descriptor that
// lands on a number the census watches, and its exit reclaims whatever the worker left open: the census never closes
// a number it cannot prove it owns.
func TestSupervisedImportUnwindsOnLaterFailure(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	cmd, err := env.HelperCommand(t, UnwindCensusHelper)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = env.RunFixtureWithin(cmd, 30*time.Second)
	if err != nil || !strings.Contains(out.String(), unwindCensusClean) {
		t.Errorf("the census helper ended with %v and did not report a clean census:\n%s", err, out.String())
	}
}

// unwindCensus takes the census in the helper process.
func unwindCensus(t testing.TB) {
	h := newInProcessHandoff(t)
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pipeW.Close() })
	// The drain setting names a pipe: the listener's import succeeds and the drain's is refused.
	badDrain := rawDescriptor(t, pipeR)
	drainNumber, _ := strconv.Atoi(h.env[workerlink.DrainFDEnv])
	_ = unix.Close(drainNumber)
	h.env[workerlink.DrainFDEnv] = strconv.Itoa(badDrain)
	h.handedOver[3] = badDrain
	before := openDescriptors()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, done := h.run(ctx)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the worker started with a drain descriptor that is a pipe")
		}
	case <-ctx.Done():
		t.Fatal("the worker did not refuse a drain descriptor that is a pipe")
	}
	after := openDescriptors()
	var want []int
	for _, n := range before {
		if !slices.Contains(h.handedOver, n) {
			want = append(want, n)
		}
	}
	var leaked, kept []int
	for _, n := range after {
		if !slices.Contains(before, n) {
			leaked = append(leaked, n)
		}
	}
	for _, n := range h.handedOver {
		if slices.Contains(after, n) {
			kept = append(kept, n)
		}
	}
	// In this process nothing but the worker opens descriptors while it runs, so a handed-over number that is open
	// again refers to something the worker left open, whether the original or a reuse of its number.
	if len(leaked) != 0 || len(kept) != 0 {
		t.Errorf("after the refused start: descriptors opened and left open %v, handed-over descriptor numbers open %v; want none of either (open before minus handed over: %d descriptors)", leaked, kept, len(want))
		return
	}
	_, _ = fmt.Println(unwindCensusClean)
}
