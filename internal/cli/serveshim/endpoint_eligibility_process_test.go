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

package serveshim_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveshim"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// eligibilityControlKey names the Unix socket over which a test drives the worker child it is waiting for. It is set
// only in the environment of a worker the supervisor under test starts.
const eligibilityControlKey = "ELIGIBILITY_FIXTURE_CONTROL"

// reportDeadline is how long the supervisor waits for its worker's listening report before it stops.
const reportDeadline = 5 * time.Second

// eligibilityChild is the worker the supervisor under test starts through the launcher script, which runs this test
// binary again. It binds, reports and exits only when the test tells it to, so the test decides when an endpoint
// becomes eligible. "listen shared" binds beside the test's reservation of the configured port (see reservePort). It
// counts the requests it receives before and after its report, and tells the test those counts when its supervisor
// goes away.
func eligibilityChild(ctx context.Context, control string) error {
	generation := os.Getenv("MODEL_GATEWAY_WORKER_GENERATION")
	if generation == "" || os.Getenv("MODEL_GATEWAY_CONTROL_FDS") != "3,4" {
		return errors.New("invalid worker descriptor environment")
	}
	report, live := os.NewFile(3, "port report"), os.NewFile(4, "parent liveness")
	if report == nil || live == nil {
		return errors.New("missing worker descriptors")
	}
	configured, err := strconv.Atoi(os.Getenv("CODEX_GATEWAY_WORKER_PORT"))
	if err != nil {
		return err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", control)
	if err != nil {
		return err
	}
	var writes sync.Mutex
	send := func(format string, args ...any) {
		writes.Lock()
		defer writes.Unlock()
		_, _ = fmt.Fprintf(conn, format+"\n", args...)
	}
	send("hello %d", os.Getpid())
	var reported atomic.Bool
	var before, after atomic.Int32
	// A worker stops when its supervisor goes away.
	go func() {
		_, _ = io.Copy(io.Discard, live)
		send("parent gone %d %d", before.Load(), after.Load())
		os.Exit(0)
	}()
	var ln net.Listener
	port := 0
	commands := bufio.NewScanner(conn)
	for commands.Scan() {
		command, arg, _ := strings.Cut(commands.Text(), " ")
		switch command {
		case "listen":
			var cfg net.ListenConfig
			if arg == "shared" {
				cfg.Control = reusePort
			}
			if ln, err = cfg.Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(configured))); err != nil {
				send("error %v", err)
				continue
			}
			port = ln.Addr().(*net.TCPAddr).Port
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if reported.Load() {
					after.Add(1)
				} else {
					before.Add(1)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("X-Child-Pid", strconv.Itoa(os.Getpid()))
				w.WriteHeader(http.StatusOK)
			})}
			go func() { _ = srv.Serve(ln) }()
			send("listening %d", port)
		case "report":
			// "report N" reports port N instead of the bound one.
			reportPort := port
			if arg != "" {
				if reportPort, err = strconv.Atoi(arg); err != nil {
					send("error %v", err)
					continue
				}
			}
			reported.Store(true)
			line, err := json.Marshal(map[string]any{"type": "listening", "port": reportPort, "generation": generation})
			if err == nil {
				_, err = report.Write(append(line, '\n'))
			}
			if err = errors.Join(err, report.Close()); err != nil {
				send("error %v", err)
				continue
			}
			send("reported")
		case "requests":
			send("requests %d %d", before.Load(), after.Load())
		case "exit":
			if ln != nil {
				_ = ln.Close()
			}
			send("closed")
			os.Exit(0)
		}
	}
	if err := commands.Err(); err != nil {
		return fmt.Errorf("read control commands: %w", err)
	}
	// The test closed its control socket.
	os.Exit(0)
	return nil
}

// fixtureChild is one worker process the supervisor started, as seen through its control connection.
type fixtureChild struct {
	pid   int
	conn  net.Conn
	lines *bufio.Scanner
}

// command sends one command and returns the child's reply, which must start with want.
func (c *fixtureChild) command(t *testing.T, cmd, want string) string {
	t.Helper()
	if _, err := io.WriteString(c.conn, cmd+"\n"); err != nil {
		t.Fatalf("command %q: %v", cmd, err)
	}
	if !c.lines.Scan() {
		t.Fatalf("command %q: child closed its control connection: %v", cmd, c.lines.Err())
	}
	reply := c.lines.Text()
	if !strings.HasPrefix(reply, want) {
		t.Fatalf("command %q: reply %q, want prefix %q", cmd, reply, want)
	}
	return reply
}

// exited waits until the child's control connection ends, which happens when the process has exited.
func (c *fixtureChild) exited(t *testing.T) {
	t.Helper()
	for c.lines.Scan() {
		t.Errorf("unexpected line from an exiting child: %q", c.lines.Text())
	}
}

// farewell returns the request counts the child sent when its supervisor went away.
func (c *fixtureChild) farewell(t *testing.T) string {
	t.Helper()
	if !c.lines.Scan() {
		t.Fatalf("child closed its control connection without its request counts: %v", c.lines.Err())
	}
	return c.lines.Text()
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// shimUnderTest is a foreground supervisor running in this process with the fixture child as its worker.
type shimUnderTest struct {
	public   string
	token    string
	ready    chan int
	readyAt  time.Time
	done     chan struct{}
	result   error
	doneAt   time.Time
	stderr   *lockedBuffer
	children chan *fixtureChild
}

// startShim runs serveshim.Run with the given port settings. The control token is a fixture written by the test.
// Each prepare function runs on the state directory before the supervisor starts.
func startShim(t *testing.T, env map[string]string, prepare ...func(state string)) *shimUnderTest {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(raw[:])
	if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range prepare {
		p(state)
	}

	// sun_path is short; the test's temporary directory can exceed it.
	sockDir, err := os.MkdirTemp("", "mg-elig-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(sockDir); err != nil {
			t.Error(err)
		}
	})
	controlPath := filepath.Join(sockDir, "c.sock")
	var lc net.ListenConfig
	control, err := lc.Listen(t.Context(), "unix", controlPath)
	if err != nil {
		t.Fatal(err)
	}
	s := &shimUnderTest{token: token, ready: make(chan int, 1), done: make(chan struct{}), stderr: new(lockedBuffer), children: make(chan *fixtureChild, 8)}
	var accepted sync.WaitGroup
	var connsMu sync.Mutex
	var conns []net.Conn
	accepted.Go(func() {
		for {
			conn, err := control.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, conn)
			connsMu.Unlock()
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			lines := bufio.NewScanner(conn)
			if !lines.Scan() {
				continue
			}
			v, ok := strings.CutPrefix(lines.Text(), "hello ")
			pid, err := strconv.Atoi(v)
			if !ok || err != nil {
				continue
			}
			s.children <- &fixtureChild{pid: pid, conn: conn, lines: lines}
		}
	})

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(exe, '\'') {
		t.Fatalf("test executable path %q cannot be quoted for the launcher script", exe)
	}
	launcher := filepath.Join(home, "worker-launcher")
	script := "#!/bin/sh\nexec '" + exe + "' -test.run='^TestEndpointEligibility$'\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	overrides := map[string]string{"MODEL_GATEWAY_STATE_DIR": state}
	maps.Copy(overrides, env)
	fixture := gatewaytest.NewEnv(home, nil, map[string]string{eligibilityControlKey: controlPath}, overrides)
	entries := make([]string, 0, len(fixture))
	for k, v := range fixture {
		entries = append(entries, k+"="+v)
	}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	var watched sync.WaitGroup
	var watchErr error
	watched.Go(func() {
		lines := bufio.NewScanner(pr)
		for lines.Scan() {
			if v, ok := strings.CutPrefix(lines.Text(), "model-gateway shim supervisor listening on 127.0.0.1:"); ok {
				if n, err := strconv.Atoi(v); err == nil {
					select {
					case s.ready <- n:
					default:
					}
				}
			}
		}
		watchErr = lines.Err()
		_ = pr.Close()
	})
	go func() {
		defer close(s.done)
		s.result = serveshim.Run(ctx, serveshim.Options{
			Environment: entries, Home: func() (string, error) { return home, nil }, Executable: launcher,
			Version: fixtureVersion, Stdout: pw, Stderr: s.stderr,
		})
		s.doneAt = time.Now()
		_ = pw.Close()
	}()
	t.Cleanup(func() {
		cancel()
		<-s.done
		watched.Wait()
		if watchErr != nil {
			t.Errorf("read the supervisor's stdout: %v", watchErr)
		}
		_ = control.Close()
		accepted.Wait()
		connsMu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		connsMu.Unlock()
	})
	return s
}

// awaitReady returns once the supervisor printed its readiness line.
func (s *shimUnderTest) awaitReady(t *testing.T) {
	t.Helper()
	select {
	case port := <-s.ready:
		s.readyAt = time.Now()
		s.public = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	case <-s.done:
		t.Fatalf("supervisor stopped before it was ready: %v; stderr: %s", s.result, s.stderr)
	}
}

// awaitStop waits until the supervisor stops or the deadline passes, and reports whether it stopped.
func (s *shimUnderTest) awaitStop(deadline time.Time) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-s.done:
		return true
	case <-timer.C:
		return false
	}
}

// nextChild returns the next worker process the supervisor started.
func (s *shimUnderTest) nextChild(t *testing.T) *fixtureChild {
	t.Helper()
	select {
	case c := <-s.children:
		return c
	case <-s.done:
		t.Fatalf("supervisor stopped before starting a worker: %v; stderr: %s", s.result, s.stderr)
	case <-t.Context().Done():
		t.Fatal("test ended before the supervisor started a worker")
	}
	return nil
}

// probeResult is what the client sees for one relayed drain request.
type probeResult struct {
	Status   int
	NoRetry  bool
	ChildPID int
}

// probe sends an authenticated drain request carrying a marker in a header and in the body. The supervisor relays
// it to its current worker endpoint, or answers it with 503 when it has none.
func (s *shimUnderTest) probe(t *testing.T, marker string) probeResult {
	t.Helper()
	got, err := s.send(t.Context(), marker)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// send is probe for use outside the test goroutine.
func (s *shimUnderTest) send(ctx context.Context, marker string) (probeResult, error) {
	return s.sendTraced(ctx, marker, nil)
}

// sendTraced is send that calls wrote, when it is not nil, once the whole request has been written to the supervisor.
func (s *shimUnderTest) sendTraced(ctx context.Context, marker string, wrote func()) (probeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if wrote != nil {
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote()
			}
		}})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+s.public+"/drain", strings.NewReader(`{"probe":"`+marker+`"}`))
	if err != nil {
		return probeResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Model-Gateway-Control-Token", s.token)
	req.Header.Set("X-Eligibility-Probe", marker)
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return probeResult{}, fmt.Errorf("probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	pid, _ := strconv.Atoi(resp.Header.Get("X-Child-Pid"))
	return probeResult{Status: resp.StatusCode, NoRetry: resp.Header.Get("X-Should-Retry") == "false", ChildPID: pid}, nil
}

// retryable is the answer for a request that found no eligible worker endpoint.
var retryable = probeResult{Status: http.StatusServiceUnavailable}

// foreignService is another program's loopback listener on a port the supervisor might relay to. It records what
// each connection carried and answers with a status no worker uses.
type foreignService struct {
	port   int
	ln     net.Listener
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  int
	stream []byte
}

func startForeign(t *testing.T, port int) *foreignService {
	t.Helper()
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	var cfg net.ListenConfig
	ln, err := env.Listen(t.Context(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), cfg.Listen)
	if err != nil {
		t.Fatalf("bind the foreign service on port %d: %v", port, err)
	}
	f := &foreignService{port: ln.Addr().(*net.TCPAddr).Port, ln: ln}
	f.wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Go(func() { f.serve(conn) })
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		f.wg.Wait()
	})
	return f
}

func (f *foreignService) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var got []byte
	buf := make([]byte, 4096)
	for !bytes.Contains(got, []byte("\r\n\r\n")) {
		n, err := conn.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	f.mu.Lock()
	f.conns++
	f.stream = append(f.stream, got...)
	f.mu.Unlock()
	_, _ = io.WriteString(conn, "HTTP/1.1 421 Misdirected Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

// foreignTraffic says what reached the foreign service, without the token value itself.
type foreignTraffic struct {
	RequestBytes bool
	Token        bool
	Marker       bool
}

func (f *foreignService) traffic(token, marker string) (foreignTraffic, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return foreignTraffic{
		RequestBytes: len(f.stream) > 0,
		Token:        bytes.Contains(f.stream, []byte(token)),
		Marker:       bytes.Contains(f.stream, []byte(marker)),
	}, f.conns
}

// inFlight is a probe sent from another goroutine; answer delivers its result once, and cancel makes its client leave.
type inFlight struct {
	wrote  chan struct{}
	answer chan probeAnswer
	cancel context.CancelFunc
}

type probeAnswer struct {
	result probeResult
	err    error
}

// startProbe sends a probe from another goroutine and returns once the whole request is written to the supervisor.
func (s *shimUnderTest) startProbe(t *testing.T, marker string) *inFlight {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	p := &inFlight{wrote: make(chan struct{}), answer: make(chan probeAnswer, 1), cancel: cancel}
	t.Cleanup(cancel)
	wrote := sync.OnceFunc(func() { close(p.wrote) })
	go func() {
		got, err := s.sendTraced(ctx, marker, wrote)
		p.answer <- probeAnswer{got, err}
	}()
	select {
	case <-p.wrote:
	case a := <-p.answer:
		t.Fatalf("probe ended before its request was written: %+v, %v", a.result, a.err)
	}
	return p
}

// freePort returns a loopback port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	f := startForeign(t, 0)
	port := f.port
	_ = f.ln.Close()
	f.wg.Wait()
	return port
}

// portReservation holds a loopback port number for a worker that binds it later.
type portReservation struct {
	port    int
	release func()
}

// reservePort reserves a TCP port number on 127.0.0.1 until release is called or the test ends. The reservation is a
// socket bound with SO_REUSEPORT alone that never listens, so no connection reaches it. While it is held it keeps the
// endpoint from the ordinary competing binds: a bind to port 0 (the supervisor's public listener included) or an
// outgoing connection is not handed the number, and a plain bind that names it fails. It covers neither other
// addresses or protocols at that number, nor a socket of the same user that names the endpoint with SO_REUSEPORT set
// itself. A worker told to "listen shared" is such a socket: it binds and listens on the number beside the
// reservation, so the number passes from the test to the worker without a moment in which an ordinary bind could
// take it. A port number that was free a moment ago, as freePort returns, can be taken by any bind between its
// release and the worker's.
//
// A test that hands the number to a worker releases the reservation once the worker listens, before any request can
// be relayed to that worker, so no traffic to the worker's listener overlaps the reservation.
func reservePort(t *testing.T) *portReservation {
	t.Helper()
	// The supervisor under test starts its workers from this process; none may inherit the reservation, so the
	// descriptor is marked close-on-exec before a fork can happen.
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatalf("create the port reservation socket: %v", err)
	}
	r := &portReservation{release: sync.OnceFunc(func() { _ = unix.Close(fd) })}
	t.Cleanup(r.release)
	// SO_REUSEADDR stays unset: Go sets it on every listener, and on Linux two sockets that both set it may share a
	// port that neither listens on, so any listener that names the number could take the port beside the reservation.
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		t.Fatalf("set SO_REUSEPORT on the port reservation: %v", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind the port reservation: %v", err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatalf("read the port reservation's address: %v", err)
	}
	inet4, ok := sa.(*unix.SockaddrInet4)
	if !ok || inet4.Port == 0 {
		t.Fatalf("port reservation bound to %#v; want a loopback port", sa)
	}
	r.port = inet4.Port
	return r
}

// reusePort sets SO_REUSEPORT on a socket before it binds, so that it can bind a port reservePort holds.
func reusePort(_, _ string, c syscall.RawConn) error {
	var optErr error
	if err := c.Control(func(fd uintptr) {
		optErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return optErr
}

// TestPortReservation checks the hand-over the fixed-port cases rely on: while reservePort holds a number, a listener
// that names it without SO_REUSEPORT is refused, and a listener with SO_REUSEPORT, as a worker's "listen shared",
// binds it and receives the connections to it once the reservation is released.
func TestPortReservation(t *testing.T) {
	reserved := reservePort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(reserved.port))
	var plain net.ListenConfig
	ln, err := plain.Listen(t.Context(), "tcp4", addr)
	if err == nil {
		_ = ln.Close()
		t.Fatalf("a listener without SO_REUSEPORT bound the reserved port %d", reserved.port)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("a listener without SO_REUSEPORT on the reserved port %d: %v; want %v", reserved.port, err, syscall.EADDRINUSE)
	}
	shared := net.ListenConfig{Control: reusePort}
	ln, err = shared.Listen(t.Context(), "tcp4", addr)
	if err != nil {
		t.Fatalf("a listener with SO_REUSEPORT could not bind the reserved port %d: %v", reserved.port, err)
	}
	defer func() { _ = ln.Close() }()
	reserved.release()
	// A connection that went to some other socket sharing the number would leave the accept waiting; the bound makes
	// that a failure of this premise instead of a package timeout.
	const bound = 10 * time.Second
	if err := ln.(*net.TCPListener).SetDeadline(time.Now().Add(bound)); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
		accepted <- err
	}()
	dialer := net.Dialer{Timeout: bound}
	conn, err := dialer.DialContext(t.Context(), "tcp4", addr)
	if err != nil {
		t.Fatalf("connect to the listener on the formerly reserved port %d: %v", reserved.port, err)
	}
	_ = conn.Close()
	if err := <-accepted; err != nil {
		t.Errorf("accept on the formerly reserved port %d within %v: %v", reserved.port, bound, err)
	}
}

// otherFreePort returns a free loopback port other than port.
func otherFreePort(t *testing.T, port int) int {
	t.Helper()
	for {
		if other := freePort(t); other != port {
			return other
		}
	}
}

// TestEndpointEligibility checks that the supervisor relays to a worker endpoint only after the current worker
// process itself reported that it is listening there, for its own generation, also when a port is configured. A port
// that is only configured, or that belonged to a worker that has exited, receives no request: another program may
// hold it, and the request carries the client's headers, body and control token. Until an endpoint is eligible,
// requests wait and then receive the retryable 503.
func TestEndpointEligibility(t *testing.T) {
	if control := os.Getenv(eligibilityControlKey); control != "" {
		// This process is a worker the supervisor under test started, not the test itself.
		if err := eligibilityChild(t.Context(), control); err != nil {
			t.Fatal(err)
		}
		return
	}
	tests := map[string]struct {
		run func(t *testing.T)
	}{
		"error: configured port held by another program receives nothing": {func(t *testing.T) {
			foreign := startForeign(t, 0)
			s := startShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(foreign.port)})
			s.awaitReady(t)
			s.nextChild(t)
			marker := rand.Text()
			// The client gives up long before the relay's retries end or the report deadline passes; a relay to the
			// configured port would happen at the first attempt.
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			got, err := s.send(ctx, marker)
			switch {
			case err == nil:
				if diff := gocmp.Diff(retryable, got); diff != "" {
					t.Errorf("request while no worker has reported (-want +got):\n%s", diff)
				}
			case ctx.Err() == nil:
				t.Errorf("request while no worker has reported: %v", err)
			}
			cancel()
			traffic, conns := foreign.traffic(s.token, marker)
			if diff := gocmp.Diff(foreignTraffic{}, traffic); diff != "" {
				t.Errorf("traffic on the configured port held by another program (%d connections) (-want +got):\n%s", conns, diff)
			}
		}},
		"error: configured port the child bound receives nothing before the child reports": {func(t *testing.T) {
			reserved := reservePort(t)
			port := reserved.port
			s := startShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port)})
			s.awaitReady(t)
			child := s.nextChild(t)
			child.command(t, "listen shared", "listening "+strconv.Itoa(port))
			reserved.release()
			probe := s.startProbe(t, rand.Text())
			defer probe.cancel()
			// The child counts each request as arriving before or after its report, so the verdict does not depend on
			// this window; the window only gives a supervisor that relays before the report the chance to do so.
			window := time.NewTimer(300 * time.Millisecond)
			var a probeAnswer
			answered := false
			select {
			case a = <-probe.answer:
				answered = true
			case <-window.C:
			}
			window.Stop()
			child.command(t, "report", "reported")
			if !answered {
				a = <-probe.answer
			}
			if a.err != nil {
				t.Fatal(a.err)
			}
			if diff := gocmp.Diff(probeResult{Status: http.StatusOK, ChildPID: child.pid}, a.result); diff != "" {
				t.Errorf("request sent before the report and answered after it (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("requests 0 1", child.command(t, "requests", "requests ")); diff != "" {
				t.Errorf("requests the child received before and after its report (-want +got):\n%s", diff)
			}
		}},
		"success: configured port is relayed to once the child reports it": {func(t *testing.T) {
			reserved := reservePort(t)
			port := reserved.port
			s := startShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port)})
			s.awaitReady(t)
			child := s.nextChild(t)
			child.command(t, "listen shared", "listening "+strconv.Itoa(port))
			reserved.release()
			child.command(t, "report", "reported")
			if diff := gocmp.Diff(probeResult{Status: http.StatusOK, ChildPID: child.pid}, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request after the child reported (-want +got):\n%s", diff)
			}
		}},
		"success: request before the first report waits and gets the retryable 503": {func(t *testing.T) {
			// Until the first report the supervisor's endpoint is 0, as the other cases show. The public listener is
			// served here with that endpoint directly, so the relay's wait never races the report deadline.
			var cfg net.ListenConfig
			ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var raw [32]byte
			if _, err := rand.Read(raw[:]); err != nil {
				t.Fatal(err)
			}
			s := &shimUnderTest{public: ln.Addr().String(), token: hex.EncodeToString(raw[:])}
			h, err := supervisor.NewHandler(ln.Addr().(*net.TCPAddr).Port, func() int { return 0 }, s.token, fixtureVersion, os.Getpid())
			if err != nil {
				_ = ln.Close()
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			served := make(chan error, 1)
			go func() { served <- supervisor.Serve(ctx, ln, h) }()
			defer func() {
				cancel()
				if err := <-served; err != nil {
					t.Errorf("public listener: %v", err)
				}
				_ = ln.Close()
			}()
			if diff := gocmp.Diff(retryable, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request before any report (-want +got):\n%s", diff)
			}
		}},
		"error: configured port whose child never reports stops the supervisor at the report deadline": {func(t *testing.T) {
			// The configured port stays reserved for the whole case, so the supervisor can never choose it as its public
			// port; the child never binds it.
			port := reservePort(t).port
			s := startShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port)})
			s.awaitReady(t)
			s.nextChild(t)
			// The deadline counts from the worker's start, which follows the readiness line.
			guard := 10 * time.Second
			if !s.awaitStop(s.readyAt.Add(reportDeadline + guard)) {
				t.Fatalf("supervisor still running %v after readiness although its current child never reported", reportDeadline+guard)
			}
			if s.result == nil {
				t.Error("supervisor stopped with exit status 0; want 1 when its child never reported")
			}
			if elapsed := s.doneAt.Sub(s.readyAt); elapsed < reportDeadline-250*time.Millisecond {
				t.Errorf("supervisor stopped %v after readiness, before the report deadline of %v", elapsed, reportDeadline)
			}
		}},
		"error: configured port reported as another port stops the supervisor at once": {func(t *testing.T) {
			// The configured port stays reserved for the whole case, so the supervisor can never choose it as its public
			// port; the child only names it.
			port := reservePort(t).port
			other := otherFreePort(t, port)
			s := startShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port)})
			s.awaitReady(t)
			child := s.nextChild(t)
			reportAt := time.Now()
			child.command(t, "report "+strconv.Itoa(other), "reported")
			// A supervisor that acts on the report stops within moments of it; the bound leaves room for a loaded venue.
			const atOnce = 3 * time.Second
			if !s.awaitStop(reportAt.Add(atOnce)) {
				t.Fatalf("supervisor still running %v after its child reported port %d instead of the configured %d", atOnce, other, port)
			}
			// A supervisor that waits for the report deadline instead stops no earlier than readiness plus the deadline. A
			// supervisor that acts on the report reaches that point only if its child took the whole deadline to start, and
			// then its own deadline would have stopped it before the report.
			if since := s.doneAt.Sub(s.readyAt); since >= reportDeadline-250*time.Millisecond {
				t.Errorf("supervisor stopped %v after readiness and %v after the report: at the report deadline, not at the report", since, s.doneAt.Sub(reportAt))
			}
			if s.result == nil {
				t.Error("supervisor stopped with exit status 0; want 1 when its child reported another port")
			}
		}},
		"error: worker whose pid record cannot be published stops the supervisor and receives nothing": {func(t *testing.T) {
			// A directory where the worker's pid record belongs makes every publication of that record fail.
			s := startShim(t, nil, func(state string) {
				if err := os.Mkdir(filepath.Join(state, "shim.pid.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			})
			s.awaitReady(t)
			child := s.nextChild(t)
			child.command(t, "listen", "listening ")
			probe := s.startProbe(t, rand.Text())
			child.command(t, "report", "reported")
			// The child says goodbye when its supervisor disconnects it; a supervisor that keeps serving the untracked
			// worker never does, and the control connection's deadline ends the wait.
			if diff := gocmp.Diff("parent gone 0 0", child.farewell(t)); diff != "" {
				t.Errorf("requests the untracked worker received before and after its report (-want +got):\n%s", diff)
			}
			// The probe has no worker left to reach; its client leaves rather than wait out the relay's retries.
			probe.cancel()
			if !s.awaitStop(time.Now().Add(10 * time.Second)) {
				t.Fatal("supervisor still running after it disconnected a worker whose pid record cannot be published")
			}
			if s.result == nil {
				t.Error("supervisor stopped with exit status 0; want 1 when the worker's pid record cannot be published")
			}
			// The two publications fail with two distinct diagnostics. The provisional record's failure is a warning, the
			// only line the supervisor prints; the eligible record's failure is the fatal error Run returns, which its
			// caller prints, so it never appears on stderr as well.
			const provisionalWarning = "model-gateway: could not publish the worker pid record: "
			const eligibleFatal = "model-gateway: the shim worker could not be tracked: "
			stderr := s.stderr.String()
			lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
			if len(lines) != 1 || !strings.HasPrefix(lines[0], provisionalWarning) || !strings.HasSuffix(stderr, "\n") {
				t.Errorf("stderr %q; want exactly one line, the provisional record's warning starting with %q", stderr, provisionalWarning)
			}
			if s.result != nil && !strings.HasPrefix(s.result.Error(), eligibleFatal) {
				t.Errorf("supervisor stopped with %q; want the eligible record's fatal error starting with %q", s.result, eligibleFatal)
			}
			if strings.Contains(stderr, eligibleFatal) {
				t.Errorf("stderr %q repeats the fatal error Run returns", stderr)
			}
			if a := <-probe.answer; a.err == nil && a.result != retryable {
				t.Errorf("request in flight while the pid record could not be published: %+v; want the retryable 503 or no answer", a.result)
			}
		}},
		"error: port of a child that exited receives nothing": {func(t *testing.T) {
			s := startShim(t, nil)
			s.awaitReady(t)
			first := s.nextChild(t)
			reply := first.command(t, "listen", "listening ")
			port, err := strconv.Atoi(strings.TrimPrefix(reply, "listening "))
			if err != nil {
				t.Fatal(err)
			}
			first.command(t, "report", "reported")
			if diff := gocmp.Diff(probeResult{Status: http.StatusOK, ChildPID: first.pid}, s.probe(t, rand.Text())); diff != "" {
				t.Fatalf("request to the reporting child (-want +got):\n%s", diff)
			}
			// The child closes its listener and exits; another program takes the port before a replacement reports.
			first.command(t, "exit", "closed")
			foreign := startForeign(t, port)
			first.exited(t)
			marker := rand.Text()
			type answer struct {
				result probeResult
				err    error
			}
			answered := make(chan answer, 1)
			go func() {
				got, err := s.send(t.Context(), marker)
				answered <- answer{got, err}
			}()
			second := s.nextChild(t)
			second.command(t, "listen", "listening ")
			second.command(t, "report", "reported")
			a := <-answered
			if a.err != nil {
				t.Fatal(a.err)
			}
			got := a.result
			if got != retryable {
				if diff := gocmp.Diff(probeResult{Status: http.StatusOK, ChildPID: second.pid}, got); diff != "" {
					t.Errorf("request sent after the child exited; want the replacement's answer or the retryable 503 (-want +got):\n%s", diff)
				}
			}
			traffic, conns := foreign.traffic(s.token, marker)
			if diff := gocmp.Diff(foreignTraffic{}, traffic); diff != "" {
				t.Errorf("traffic on the exited child's port (%d connections) (-want +got):\n%s", conns, diff)
			}
		}},
		"error: worker port equal to the public port is refused at startup": {func(t *testing.T) {
			port := strconv.Itoa(freePort(t))
			s := startShim(t, map[string]string{"CODEX_GATEWAY_PORT": port, "CODEX_GATEWAY_WORKER_PORT": port})
			select {
			case <-s.ready:
				t.Error("supervisor announced readiness with a worker port equal to its public port")
			case <-s.done:
				if s.result == nil {
					t.Error("supervisor exited without an error for a worker port equal to its public port")
				}
			}
		}},
	}
	for name, tc := range tests {
		t.Run(name, tc.run)
	}
}
