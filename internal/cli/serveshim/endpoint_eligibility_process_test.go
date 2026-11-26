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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveshim"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// eligibilityControlKey names the Unix socket over which a test drives the worker child it is waiting for. It is set
// only in the environment of a worker the supervisor under test starts.
const eligibilityControlKey = "ELIGIBILITY_FIXTURE_CONTROL"

// eligibilityChild is the worker the supervisor under test starts through the launcher script, which runs this test
// binary again. It binds, reports and exits only when the test tells it to, so the test decides when an endpoint
// becomes eligible.
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
	// A worker stops when its supervisor goes away.
	go func() {
		_, _ = io.Copy(io.Discard, live)
		os.Exit(0)
	}()
	var ln net.Listener
	port := 0
	commands := bufio.NewScanner(conn)
	for commands.Scan() {
		switch commands.Text() {
		case "listen":
			var cfg net.ListenConfig
			if ln, err = cfg.Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(configured))); err != nil {
				send("error %v", err)
				continue
			}
			port = ln.Addr().(*net.TCPAddr).Port
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("X-Child-Pid", strconv.Itoa(os.Getpid()))
				w.WriteHeader(http.StatusOK)
			})}
			go func() { _ = srv.Serve(ln) }()
			send("listening %d", port)
		case "report":
			line, err := json.Marshal(map[string]any{"type": "listening", "port": port, "generation": generation})
			if err == nil {
				_, err = report.Write(append(line, '\n'))
			}
			if err = errors.Join(err, report.Close()); err != nil {
				send("error %v", err)
				continue
			}
			send("reported")
		case "exit":
			if ln != nil {
				_ = ln.Close()
			}
			send("closed")
			os.Exit(0)
		}
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
	done     chan struct{}
	result   error
	stderr   *lockedBuffer
	children chan *fixtureChild
}

// startShim runs serveshim.Run with the given port settings. The control token is a fixture written by the test.
func startShim(t *testing.T, env map[string]string) *shimUnderTest {
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
		_ = pr.Close()
	})
	go func() {
		defer close(s.done)
		s.result = serveshim.Run(ctx, serveshim.Options{
			Environment: entries, Home: func() (string, error) { return home, nil }, Executable: launcher,
			Version: fixtureVersion, Stdout: pw, Stderr: s.stderr,
		})
		_ = pw.Close()
	}()
	t.Cleanup(func() {
		cancel()
		<-s.done
		watched.Wait()
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
		s.public = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	case <-s.done:
		t.Fatalf("supervisor stopped before it was ready: %v; stderr: %s", s.result, s.stderr)
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
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
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

// freePort returns a loopback port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	f := startForeign(t, 0)
	port := f.port
	_ = f.ln.Close()
	f.wg.Wait()
	return port
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
			if diff := gocmp.Diff(retryable, s.probe(t, marker)); diff != "" {
				t.Errorf("request while no worker has reported (-want +got):\n%s", diff)
			}
			got, conns := foreign.traffic(s.token, marker)
			if diff := gocmp.Diff(foreignTraffic{}, got); diff != "" {
				t.Errorf("traffic on the configured port held by another program (%d connections) (-want +got):\n%s", conns, diff)
			}
		}},
		"error: configured port the child bound receives nothing before the child reports": {func(t *testing.T) {
			port := freePort(t)
			s := startShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port)})
			s.awaitReady(t)
			child := s.nextChild(t)
			child.command(t, "listen", "listening "+strconv.Itoa(port))
			if diff := gocmp.Diff(retryable, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request while the child listens but has not reported (-want +got):\n%s", diff)
			}
		}},
		"success: configured port is relayed to once the child reports it": {func(t *testing.T) {
			port := freePort(t)
			s := startShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port)})
			s.awaitReady(t)
			child := s.nextChild(t)
			child.command(t, "listen", "listening "+strconv.Itoa(port))
			child.command(t, "report", "reported")
			if diff := gocmp.Diff(probeResult{Status: http.StatusOK, ChildPID: child.pid}, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request after the child reported (-want +got):\n%s", diff)
			}
		}},
		"success: request before the first report waits and gets the retryable 503": {func(t *testing.T) {
			s := startShim(t, nil)
			s.awaitReady(t)
			s.nextChild(t)
			if diff := gocmp.Diff(retryable, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request before any report (-want +got):\n%s", diff)
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
