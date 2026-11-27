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
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// ownedControlKey names the Unix socket over which a test drives the worker child it is waiting for. It is set only in
// the environment of a worker the supervisor under test starts.
const ownedControlKey = "OWNED_ELIGIBILITY_FIXTURE_CONTROL"

// ownedReportDeadline is how long the supervisor waits for its worker's listening report before it stops.
const ownedReportDeadline = 5 * time.Second

// ownedEligibilityChild is the worker the supervisor under test starts through the launcher script, which runs this
// test binary again. It adopts the listener and the drain channel its supervisor announced and then does only what the
// test tells it to: serve, report, hold a request, announce a drain, close its listener, crash or exit. It counts the
// requests it receives before its report, after it, and after its drain notice was answered, and the drain requests
// it receives, and tells the test those counts when its supervisor goes away. A child that was not given a usable
// listener and drain channel says so and then waits for the test, so that its supervisor does not start it again and
// again.
func ownedEligibilityChild(ctx context.Context, control string) error {
	generation := os.Getenv("MODEL_GATEWAY_WORKER_GENERATION")
	if generation == "" || os.Getenv("MODEL_GATEWAY_CONTROL_FDS") != "3,4" {
		return errors.New("invalid worker descriptor environment")
	}
	report, live := os.NewFile(3, "port report"), os.NewFile(4, "parent liveness")
	if report == nil || live == nil {
		return errors.New("missing worker descriptors")
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
	var before, after, late, drains atomic.Int32
	counts := func() string {
		return fmt.Sprintf("%d %d %d %d", before.Load(), after.Load(), late.Load(), drains.Load())
	}
	// A worker stops when its supervisor goes away.
	go func() {
		_, _ = io.Copy(io.Discard, live)
		send("parent gone %s", counts())
		os.Exit(0)
	}()
	ln, drain, err := adoptHandoff()
	if err != nil {
		send("refused %v", err)
		_, _ = io.Copy(io.Discard, conn)
		os.Exit(0)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	send("inherited %d %s", port, generation)

	token, _ := os.ReadFile(filepath.Join(os.Getenv("MODEL_GATEWAY_STATE_DIR"), "control-token"))
	var reported, answered, holdArmed, drainHeld atomic.Bool
	held, releaseHeld := make(chan struct{}), make(chan struct{})
	drainArrived, drainGo, drainResponded := make(chan string, 1), make(chan struct{}), make(chan struct{})
	accepts := &holdingListener{Listener: ln, held: make(chan struct{}), release: make(chan struct{})}
	answers := bufio.NewReader(drain)
	exchangeDone := make(chan struct{})
	var exchangeOnce sync.Once
	var exchangeResult string
	// exchange announces the drain on the drain channel and waits for the supervisor's answer.
	exchange := func() {
		exchangeOnce.Do(func() {
			defer close(exchangeDone)
			if err := workerlink.EncodeDrain(drain, workerlink.DrainMessage{Type: workerlink.Draining, Generation: workerlink.Generation(generation)}); err != nil {
				exchangeResult = "unanswered " + err.Error()
				return
			}
			_ = drain.SetReadDeadline(time.Now().Add(5 * time.Second))
			msg, err := workerlink.DecodeDrain(answers)
			switch {
			case err != nil:
				exchangeResult = "unanswered " + err.Error()
			case msg != workerlink.DrainMessage{Type: workerlink.Retired, Generation: workerlink.Generation(generation)}:
				exchangeResult = fmt.Sprintf("unanswered wrong answer %+v", msg)
			default:
				answered.Store(true)
				exchangeResult = "answered"
			}
		})
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/drain" {
			drains.Add(1)
			// The drain the supervisor sends must be an authenticated control request for this worker.
			verdict := "valid"
			switch {
			case r.Method != http.MethodPost:
				verdict = "method " + r.Method
			case r.Host != "127.0.0.1:"+strconv.Itoa(port):
				verdict = "host " + r.Host
			case r.Header.Get("X-Model-Gateway-Control-Token") != strings.TrimSpace(string(token)) || len(token) == 0:
				verdict = "token"
			case r.Header.Get("Content-Type") != "application/json":
				verdict = "content type " + r.Header.Get("Content-Type")
			}
			_, _ = io.Copy(io.Discard, r.Body)
			if drainHeld.Load() {
				// The test announces the drain and lets the response go when it chooses.
				drainArrived <- verdict
				<-drainGo
			} else {
				go exchange()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"ok":true,"draining":true}`)
			if drainHeld.Load() {
				close(drainResponded)
			}
			return
		}
		switch {
		case answered.Load():
			late.Add(1)
		case reported.Load():
			after.Add(1)
		default:
			before.Add(1)
		}
		if holdArmed.CompareAndSwap(true, false) {
			close(held)
			<-releaseHeld
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Child-Pid", strconv.Itoa(os.Getpid()))
		w.WriteHeader(http.StatusOK)
	})}
	commands := bufio.NewScanner(conn)
	for commands.Scan() {
		command, arg, _ := strings.Cut(commands.Text(), " ")
		switch command {
		case "serve":
			go func() { _ = srv.Serve(accepts) }()
			send("serving")
		case "report", "report-gen":
			// "report N" reports port N instead of the inherited one; "report-gen G" reports generation G.
			reportPort, reportGen := port, generation
			switch {
			case command == "report-gen":
				reportGen = arg
			case arg != "":
				if reportPort, err = strconv.Atoi(arg); err != nil {
					send("error %v", err)
					continue
				}
			}
			reported.Store(true)
			line, err := json.Marshal(map[string]any{"type": "listening", "port": reportPort, "generation": reportGen})
			if err == nil {
				_, err = report.Write(append(line, '\n'))
			}
			if err = errors.Join(err, report.Close()); err != nil {
				send("error %v", err)
				continue
			}
			send("reported")
		case "requests":
			send("requests %s", counts())
		case "hold":
			holdArmed.Store(true)
			send("armed")
		case "await-held":
			<-held
			send("held")
		case "release":
			close(releaseHeld)
			send("released")
		case "hold-accept":
			accepts.armed.Store(true)
			send("armed")
		case "await-accept":
			<-accepts.held
			send("accept held")
		case "release-accept":
			close(accepts.release)
			send("released")
		case "notice":
			go exchange()
			<-exchangeDone
			send("%s", exchangeResult)
		case "notice-raw":
			// One line exactly as given, with {generation} replaced by this worker's own, without waiting for an answer.
			if _, err := io.WriteString(drain, strings.ReplaceAll(arg, "{generation}", generation)+"\n"); err != nil {
				send("error %v", err)
				continue
			}
			send("sent")
		case "drop-notice":
			// The drain is announced and the channel then closed, so that the supervisor's answer cannot be written
			// while the notice stays readable to it. Shutting only this end's reading direction would not do: on
			// macOS the supervisor's write then succeeds and its data is dropped. The answer is held by the test
			// until this close has returned.
			err := workerlink.EncodeDrain(drain, workerlink.DrainMessage{Type: workerlink.Draining, Generation: workerlink.Generation(generation)})
			if err == nil {
				err = drain.Close()
			}
			if err != nil {
				send("error %v", err)
				continue
			}
			send("dropped")
		case "drain-hold":
			drainHeld.Store(true)
			send("armed")
		case "await-drain":
			send("drain %s", <-drainArrived)
		case "drain-respond":
			close(drainGo)
			<-drainResponded
			send("responded")
		case "drain-result":
			<-exchangeDone
			send("%s", exchangeResult)
		case "close":
			_ = ln.Close()
			send("closed")
		case "crash":
			// No notice and no orderly close: the process ends as a crash does.
			os.Exit(3)
		case "exit":
			_ = ln.Close()
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

// holdingListener holds the next connection it accepts, once armed, until released: the connection is established
// and its request waits unread.
type holdingListener struct {
	net.Listener
	armed   atomic.Bool
	held    chan struct{}
	release chan struct{}
}

func (l *holdingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil && l.armed.CompareAndSwap(true, false) {
		close(l.held)
		<-l.release
	}
	return conn, err
}

// adoptHandoff imports the listener and the drain channel the supervisor announced in the environment.
func adoptHandoff() (net.Listener, net.Conn, error) {
	listenerFD, err := inheritedDescriptor(workerlink.ListenerFDEnv)
	if err != nil {
		return nil, nil, err
	}
	drainFD, err := inheritedDescriptor(workerlink.DrainFDEnv)
	if err != nil {
		return nil, nil, err
	}
	expected, err := strconv.Atoi(os.Getenv("CODEX_GATEWAY_WORKER_PORT"))
	if err != nil || expected <= 0 || expected > 65535 {
		return nil, nil, fmt.Errorf("the supervisor published CODEX_GATEWAY_WORKER_PORT=%q, not the reserved port", os.Getenv("CODEX_GATEWAY_WORKER_PORT"))
	}
	ln, err := workerlink.ImportListener(os.NewFile(uintptr(listenerFD), "inherited listener"), uint16(expected))
	if err != nil {
		return nil, nil, fmt.Errorf("import the inherited listener: %w", err)
	}
	drain, err := workerlink.ImportDrainChannel(os.NewFile(uintptr(drainFD), "inherited drain channel"))
	if err != nil {
		_ = ln.Close()
		return nil, nil, fmt.Errorf("import the inherited drain channel: %w", err)
	}
	return ln, drain, nil
}

// ownedChild is one worker process the supervisor started, as seen through its control connection.
type ownedChild struct {
	pid        int
	port       int
	generation string
	refusal    string
	conn       net.Conn
	lines      *bufio.Scanner
}

// command sends one command and returns the child's reply, which must start with want.
func (c *ownedChild) command(t *testing.T, cmd, want string) string {
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

// tell sends a command that ends the child without a reply.
func (c *ownedChild) tell(t *testing.T, cmd string) {
	t.Helper()
	if _, err := io.WriteString(c.conn, cmd+"\n"); err != nil {
		t.Fatalf("command %q: %v", cmd, err)
	}
}

// exited waits until the child's control connection ends, which happens when the process has exited.
func (c *ownedChild) exited(t *testing.T) {
	t.Helper()
	for c.lines.Scan() {
		t.Errorf("unexpected line from an exiting child: %q", c.lines.Text())
	}
}

// counts returns the child's request counts, also when its supervisor already disconnected it: then the counts are
// the ones it sent in its farewell.
func (c *ownedChild) counts(t *testing.T) string {
	t.Helper()
	// A child that its supervisor already disconnected may have closed the connection after its farewell, so a failed
	// question still leaves the farewell to read.
	_, askErr := io.WriteString(c.conn, "requests\n")
	for c.lines.Scan() {
		line := c.lines.Text()
		if v, ok := strings.CutPrefix(line, "requests "); ok {
			return v
		}
		if v, ok := strings.CutPrefix(line, "parent gone "); ok {
			return v
		}
	}
	t.Fatalf("the child closed its control connection without its counts (asking: %v; reading: %v)", askErr, c.lines.Err())
	return ""
}

// farewell returns the request counts the child sent when its supervisor went away.
func (c *ownedChild) farewell(t *testing.T) string {
	t.Helper()
	if !c.lines.Scan() {
		t.Fatalf("child closed its control connection without its request counts: %v", c.lines.Err())
	}
	return c.lines.Text()
}

// ownedBuffer collects the supervisor's stderr.
type ownedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *ownedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *ownedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ownedShim is a foreground supervisor running in this process with the owned eligibility child as its worker.
type ownedShim struct {
	state     string
	public    string
	token     string
	ready     chan int
	readyAt   time.Time
	startedAt time.Time
	done      chan struct{}
	result    error
	doneAt    time.Time
	stderr    *ownedBuffer
	children  chan *ownedChild
	// watched ends once everything the supervisor printed on stdout was read.
	watched *sync.WaitGroup
	// cancel ends the context Run was started with.
	cancel context.CancelFunc
}

// startOwnedShim runs serveshim.Run with the given port settings and the context wrap returns. The control token is
// a fixture written by the test. Each prepare function runs on the state directory before the supervisor starts.
func startOwnedShim(t *testing.T, env map[string]string, wrap func(context.Context) context.Context, prepare ...func(state string)) *ownedShim {
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
	sockDir, err := os.MkdirTemp("", "mg-own-")
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
	s := &ownedShim{state: state, token: token, ready: make(chan int, 1), done: make(chan struct{}), stderr: new(ownedBuffer), children: make(chan *ownedChild, 8)}
	var accepted sync.WaitGroup
	var connsMu sync.Mutex
	var conns []net.Conn
	// stopping ends a hand-over to the test that nobody waits for any more, as when a crash loop starts more workers
	// than children can hold.
	stopping := make(chan struct{})
	accepted.Go(func() {
		for {
			conn, err := control.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, conn)
			connsMu.Unlock()
			_ = conn.SetDeadline(time.Now().Add(40 * time.Second))
			lines := bufio.NewScanner(conn)
			if !lines.Scan() {
				continue
			}
			v, ok := strings.CutPrefix(lines.Text(), "hello ")
			pid, err := strconv.Atoi(v)
			if !ok || err != nil {
				continue
			}
			c := &ownedChild{pid: pid, conn: conn, lines: lines}
			if lines.Scan() {
				line := lines.Text()
				if v, ok := strings.CutPrefix(line, "inherited "); ok {
					port, gen, _ := strings.Cut(v, " ")
					c.port, _ = strconv.Atoi(port)
					c.generation = gen
				} else {
					c.refusal = line
				}
			} else {
				c.refusal = "the child closed its control connection before it adopted its listener"
			}
			select {
			case s.children <- c:
			case <-stopping:
				return
			}
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
	fixture := gatewaytest.NewEnv(home, nil, map[string]string{ownedControlKey: controlPath}, overrides)
	entries := make([]string, 0, len(fixture))
	for k, v := range fixture {
		entries = append(entries, k+"="+v)
	}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	s.cancel = cancel
	runCtx := ctx
	if wrap != nil {
		runCtx = wrap(ctx)
	}
	var watched sync.WaitGroup
	s.watched = &watched
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
	s.startedAt = time.Now()
	go func() {
		defer close(s.done)
		s.result = serveshim.Run(runCtx, serveshim.Options{
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
		close(stopping)
		_ = control.Close()
		connsMu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		connsMu.Unlock()
		accepted.Wait()
	})
	return s
}

// awaitReady returns once the supervisor printed its readiness line.
func (s *ownedShim) awaitReady(t *testing.T) {
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
func (s *ownedShim) awaitStop(deadline time.Time) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-s.done:
		return true
	case <-timer.C:
		return false
	}
}

// awaitExitRecord returns once the supervisor's lifecycle journal holds the worker-exit record of the worker with pid,
// which the supervisor writes only after it observed that worker's exit and retired it.
func (s *ownedShim) awaitExitRecord(t *testing.T, pid int) {
	t.Helper()
	const bound = 10 * time.Second
	deadline := time.Now().Add(bound)
	for {
		for _, raw := range (lifecycle.Files{StateDir: s.state}).Records() {
			var rec struct {
				Event string `json:"event"`
				Child struct {
					PID int `json:"pid"`
				} `json:"child"`
			}
			if json.Unmarshal(raw, &rec) == nil && rec.Event == "worker-exit" && rec.Child.PID == pid {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the supervisor never recorded the exit of worker %d within %v; stderr: %s", pid, bound, s.stderr)
		}
		select {
		case <-s.done:
			t.Fatalf("supervisor stopped before it recorded the exit of worker %d: %v; stderr: %s", pid, s.result, s.stderr)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// nextChild returns the next worker process the supervisor started, which must have adopted the listener and drain
// channel the supervisor gave it.
func (s *ownedShim) nextChild(t *testing.T) *ownedChild {
	t.Helper()
	select {
	case c := <-s.children:
		if c.refusal != "" {
			t.Fatalf("worker %d did not receive its listener and drain channel from the supervisor: %s", c.pid, c.refusal)
		}
		return c
	case <-s.done:
		t.Fatalf("supervisor stopped before starting a worker: %v; stderr: %s", s.result, s.stderr)
	case <-t.Context().Done():
		t.Fatal("test ended before the supervisor started a worker")
	}
	return nil
}

// ownedProbeResult is what the client sees for one relayed probe request.
type ownedProbeResult struct {
	Status   int
	NoRetry  bool
	ChildPID int
}

// ownedRetryable is the answer for a request that found no eligible worker generation.
var ownedRetryable = ownedProbeResult{Status: http.StatusServiceUnavailable}

// probe sends a request that carries the control token and a marker in a header and in the body. The supervisor
// relays it to its current worker generation, or answers it with 503 when it has none. The path is an ordinary one:
// a drain request would be a public drain.
func (s *ownedShim) probe(t *testing.T, marker string) ownedProbeResult {
	t.Helper()
	got, err := s.send(t.Context(), marker, nil)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// send is probe for use outside the test goroutine; it calls wrote, when it is not nil, once the whole request has
// been written to the supervisor.
func (s *ownedShim) send(ctx context.Context, marker string, wrote func()) (ownedProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if wrote != nil {
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote()
			}
		}})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+s.public+"/v1/messages", strings.NewReader(`{"probe":"`+marker+`"}`))
	if err != nil {
		return ownedProbeResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Model-Gateway-Control-Token", s.token)
	req.Header.Set("X-Eligibility-Probe", marker)
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return ownedProbeResult{}, fmt.Errorf("probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	pid, _ := strconv.Atoi(resp.Header.Get("X-Child-Pid"))
	return ownedProbeResult{Status: resp.StatusCode, NoRetry: resp.Header.Get("X-Should-Retry") == "false", ChildPID: pid}, nil
}

// publicDrain sends the authenticated public drain request to the supervisor and returns its status. It returns on
// every path, so that a caller in another goroutine always gets its result.
func (s *ownedShim) publicDrain(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+s.public+"/drain", strings.NewReader("{}"))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Model-Gateway-Control-Token", s.token)
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return 0, fmt.Errorf("public drain: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// admissionWatch counts the relay's eligibility lookups that the RelayLookup hook reports and learns from the
// StopInhibited hook that the supervisor began to stop. The counters are what the relay did, in the order it did it,
// whenever the test reads them; notify only wakes the test and never blocks the supervisor.
type admissionWatch struct {
	refused, eligible atomic.Int64
	notify            chan struct{}
	stopping          chan struct{}
	stopped           func()
}

func newAdmissionWatch() *admissionWatch {
	w := &admissionWatch{notify: make(chan struct{}, 1), stopping: make(chan struct{})}
	w.stopped = sync.OnceFunc(func() { close(w.stopping) })
	return w
}

// install returns hooks with the watch's RelayLookup and StopInhibited added.
func (w *admissionWatch) install(hooks serveshim.Hooks) serveshim.Hooks {
	hooks.RelayLookup = func(eligible bool) {
		if eligible {
			w.eligible.Add(1)
		} else {
			w.refused.Add(1)
		}
		select {
		case w.notify <- struct{}{}:
		default:
		}
	}
	hooks.StopInhibited = w.stopped
	return hooks
}

// lookups returns how many lookups found no eligible worker and how many found one, so far.
func (w *admissionWatch) lookups() (refused, eligible int64) {
	return w.refused.Load(), w.eligible.Load()
}

// isStopping reports whether the supervisor has begun to stop.
func (w *admissionWatch) isStopping() bool {
	select {
	case <-w.stopping:
		return true
	default:
		return false
	}
}

// admitsNothing sends a probe and requires the relay to look for an eligible worker for it at least twice and to find
// none, unless the supervisor begins to stop first; it then abandons the probe. Only lookups made after the probe
// started count. Whenever the probe ends, the decision rests on the lookups the relay had already made by then, not on
// the order in which the test noticed them: the retryable answer after at least two refused lookups is the relay
// giving up correctly; an answer after fewer, or any lookup that found an eligible worker, fails. A request that
// cannot be sent or that ends otherwise is allowed only once the supervisor has begun to stop.
func (s *ownedShim) admitsNothing(t *testing.T, w *admissionWatch, while string) {
	t.Helper()
	baseRefused, baseEligible := w.lookups()
	ctx, cancel := context.WithCancel(t.Context())
	answer := make(chan ownedAnswer, 1)
	go func() {
		got, err := s.send(ctx, rand.Text(), nil)
		answer <- ownedAnswer{got, err}
	}()
	answered := false
	defer func() {
		cancel()
		if answered {
			return
		}
		select {
		case <-answer:
		case <-time.After(10 * time.Second):
			t.Errorf("the abandoned probe %s did not end within 10s of its cancellation", while)
		}
	}()
	// observed reports the lookups since the probe started and fails the case on an eligible one.
	observed := func() (refused int64, ok bool) {
		refused, eligible := w.lookups()
		if eligible != baseEligible {
			t.Errorf("the relay found an eligible worker %d times %s; want none", eligible-baseEligible, while)
			return refused - baseRefused, false
		}
		return refused - baseRefused, true
	}
	timeout := time.After(10 * time.Second)
	for {
		select {
		case <-w.notify:
			refused, ok := observed()
			if !ok || refused >= 2 {
				return
			}
		case a := <-answer:
			answered = true
			refused, ok := observed()
			switch {
			case !ok:
			case a.err == nil && a.result == ownedRetryable && refused >= 2:
				// The relay looked, found nothing, and gave up with the retryable answer.
			case w.isStopping():
				// The supervisor began to stop: nothing can be admitted any more.
			default:
				t.Errorf("the probe %s ended with %+v (%v) after %d refused lookups while the supervisor was not stopping; want the relay to look at least twice and find nothing eligible", while, a.result, a.err, refused)
			}
			return
		case <-w.stopping:
			_, _ = observed()
			return
		case <-timeout:
			refused, _ := observed()
			t.Errorf("the relay looked for an eligible worker %d times within 10s %s; want at least two", refused, while)
			return
		}
	}
}

// ownedInFlight is a probe sent from another goroutine.
type ownedInFlight struct {
	wrote  chan struct{}
	answer chan ownedAnswer
	cancel context.CancelFunc
}

type ownedAnswer struct {
	result ownedProbeResult
	err    error
}

// startProbe sends a probe from another goroutine and returns once the whole request is written to the supervisor.
func (s *ownedShim) startProbe(t *testing.T, marker string) *ownedInFlight {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	p := &ownedInFlight{wrote: make(chan struct{}), answer: make(chan ownedAnswer, 1), cancel: cancel}
	t.Cleanup(cancel)
	wrote := sync.OnceFunc(func() { close(p.wrote) })
	go func() {
		got, err := s.send(ctx, marker, wrote)
		p.answer <- ownedAnswer{got, err}
	}()
	select {
	case <-p.wrote:
	case a := <-p.answer:
		t.Fatalf("probe ended before its request was written: %+v, %v", a.result, a.err)
	}
	return p
}

// ownedForeign is another program's loopback listener on a port the supervisor might relay to. It records what each
// connection carried and answers with a status no worker uses.
type ownedForeign struct {
	port    int
	ln      net.Listener
	wg      sync.WaitGroup
	mu      sync.Mutex
	streams [][]byte
	// control marks the test's own exchange with the holder, which alone is left out of its counts.
	control string
}

// bindForeign tries to bind another program's listener on port, with SO_REUSEPORT when shared is set.
func bindForeign(t *testing.T, port int, shared bool) (*ownedForeign, error) {
	t.Helper()
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	var cfg net.ListenConfig
	if shared {
		cfg.Control = ownedReusePort
	}
	ln, err := env.Listen(t.Context(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), cfg.Listen)
	if err != nil {
		return nil, err
	}
	f := &ownedForeign{port: ln.Addr().(*net.TCPAddr).Port, ln: ln}
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
	return f, nil
}

// mustBindForeign binds another program's listener on port or fails the test.
func mustBindForeign(t *testing.T, port int, shared bool) *ownedForeign {
	t.Helper()
	f, err := bindForeign(t, port, shared)
	if err != nil {
		t.Fatalf("bind the foreign service on port %d: %v", port, err)
	}
	return f
}

// refuseForeign checks that no other program can bind port now, with or without SO_REUSEPORT.
func refuseForeign(t *testing.T, port int, while string) {
	t.Helper()
	for _, shared := range []bool{false, true} {
		f, err := bindForeign(t, port, shared)
		switch {
		case err == nil:
			f.stop()
			t.Errorf("another program bound port %d (SO_REUSEPORT %t) %s", port, shared, while)
		case !errors.Is(err, syscall.EADDRINUSE):
			t.Errorf("another program's bind of port %d (SO_REUSEPORT %t) %s failed with %v; want %v", port, shared, while, err, syscall.EADDRINUSE)
		}
	}
}

// awaitForeign binds another program's listener on port once the port is free, within bound.
func awaitForeign(t *testing.T, port int, bound time.Duration, after string) *ownedForeign {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		f, err := bindForeign(t, port, false)
		if err == nil {
			return f
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %d still could not be bound %v %s: %v", port, bound, after, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stop closes the foreign listener and waits until every connection it accepted was recorded, so that its counts are
// final.
func (f *ownedForeign) stop() {
	_ = f.ln.Close()
	f.wg.Wait()
}

// talk checks that the foreign service, the port's new independent holder, is reachable and answers. Its request
// carries a marker of its own, which alone the traffic counts leave out.
func (f *ownedForeign) talk(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	f.control = "/holder-control-" + rand.Text()
	control := f.control
	f.mu.Unlock()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port)))
	if err != nil {
		t.Fatalf("connect to the port's new holder: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "GET "+control+" HTTP/1.1\r\nHost: holder\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("the port's new holder answered %v, %v; want its own 421", resp, err)
	}
	_ = resp.Body.Close()
}

func (f *ownedForeign) serve(conn net.Conn) {
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
	f.streams = append(f.streams, got)
	f.mu.Unlock()
	_, _ = io.WriteString(conn, "HTTP/1.1 421 Misdirected Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

// ownedTraffic says what reached the foreign service, without the token value itself. Connections counts every
// connection, also one that carried no byte.
type ownedTraffic struct {
	Connections  int
	RequestBytes bool
	Token        bool
	Marker       bool
}

// traffic says what reached the foreign service from bind onward, leaving out only the test's identified control
// exchange.
func (f *ownedForeign) traffic(token string, markers ...string) ownedTraffic {
	f.mu.Lock()
	defer f.mu.Unlock()
	var got ownedTraffic
	for _, stream := range f.streams {
		if f.control != "" && bytes.Contains(stream, []byte(f.control)) {
			continue
		}
		got.Connections++
		got.RequestBytes = got.RequestBytes || len(stream) > 0
		got.Token = got.Token || bytes.Contains(stream, []byte(token))
		for _, m := range markers {
			got.Marker = got.Marker || bytes.Contains(stream, []byte(m))
		}
	}
	return got
}

// ownedReusePort sets SO_REUSEPORT on a socket before it binds.
func ownedReusePort(_, _ string, c syscall.RawConn) error {
	var optErr error
	if err := c.Control(func(fd uintptr) {
		optErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return optErr
}

// configuredPort returns a loopback port number for CODEX_GATEWAY_WORKER_PORT. The number is free when this returns
// and is bound by the supervisor's own reservation moments later; nothing hands it over without a gap, because the
// supervisor's bind is exclusive and refuses any socket that shares the number. A bind by another program in that gap
// makes the supervisor refuse the port, which fails the case loudly rather than passing it.
func configuredPort(t *testing.T) int {
	t.Helper()
	f := mustBindForeign(t, 0, false)
	port := f.port
	_ = f.ln.Close()
	f.wg.Wait()
	return port
}

// otherPort returns a free loopback port other than port.
func otherPort(t *testing.T, port int) int {
	t.Helper()
	for {
		if other := configuredPort(t); other != port {
			return other
		}
	}
}

// exitHold holds the supervisor after the system reported a worker's exit and before the supervisor acts on it. The
// first exit is held until release; later exits pass.
type exitHold struct {
	held    chan int
	release chan struct{}
	once    sync.Once
	first   atomic.Bool
}

// newExitHold returns a hold whose first exit waits until free is called. The caller registers free as a cleanup after
// the supervisor's own, so that it runs first and a failed case never leaves the supervisor held while it is stopped.
func newExitHold() *exitHold {
	return &exitHold{held: make(chan int, 1), release: make(chan struct{})}
}

// observe is the ExitObserved hook: it holds the first exit until free is called.
func (h *exitHold) observe(pid int) {
	if !h.first.CompareAndSwap(false, true) {
		return
	}
	h.held <- pid
	<-h.release
}

// withHooks returns a context wrap that installs hooks.
func withHooks(hooks serveshim.Hooks) func(context.Context) context.Context {
	return func(ctx context.Context) context.Context { return serveshim.WithHooks(ctx, hooks) }
}

func (h *exitHold) free() { h.once.Do(func() { close(h.release) }) }

// TestEndpointEligibility carries every case of the endpoint eligibility test with the worker's listener
// received from its supervisor, and decides the two windows in which a relay by port number could reach another
// program. The supervisor reserves each worker generation's listener itself, so its port cannot be taken while the
// generation lives, also between the worker's exit and the supervisor observing it; and the supervisor retires a
// generation that announces a drain before it answers, so nothing is delivered to a retired generation and whoever
// binds its port later receives nothing. A generation becomes eligible only after its own listening report for the
// reserved port and its eligible pid record; until then requests wait and then receive the retryable 503. An occupied
// configured port is refused by the exclusive bind of the reservation, without a worker contacting its occupant.
//
// The case of a request that arrives before any generation is eligible is decided against the public handler alone
// in internal/supervisor, TestRelayDeliveryFollowsGeneration, "error: no generation is ever eligible".
func TestEndpointEligibility(t *testing.T) {
	if control := os.Getenv(ownedControlKey); control != "" {
		// This process is a worker the supervisor under test started, not the test itself.
		if err := ownedEligibilityChild(t.Context(), control); err != nil {
			t.Fatal(err)
		}
		return
	}
	// occupied checks a configured port that another program holds: the supervisor's exclusive reservation refuses it
	// at once, with an error that is the bind's own, before the report deadline; no worker process is started and the
	// occupant receives nothing.
	occupied := func(shared bool) func(t *testing.T) {
		return func(t *testing.T) {
			foreign := mustBindForeign(t, 0, shared)
			var starts atomic.Int32
			s := startOwnedShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(foreign.port)}, withHooks(serveshim.Hooks{Starting: func() { starts.Add(1) }}))
			// A supervisor that refuses the port stops within moments; the bound leaves room for a loaded venue and stays
			// below the report deadline, at which a supervisor that launched a worker without the port would stop.
			const atOnce = 3 * time.Second
			if !s.awaitStop(s.startedAt.Add(atOnce)) {
				t.Fatalf("supervisor still running %v after its start although its configured port %d is held by another program", atOnce, foreign.port)
			}
			if !errors.Is(s.result, syscall.EADDRINUSE) {
				t.Errorf("supervisor stopped with %v; want the reservation's bind failure, %v", s.result, syscall.EADDRINUSE)
			}
			if n := starts.Load(); n != 0 {
				t.Errorf("%d worker processes were started although the configured port could not be reserved; want none", n)
			}
			foreign.stop()
			traffic := foreign.traffic(s.token)
			if diff := gocmp.Diff(ownedTraffic{}, traffic); diff != "" {
				t.Errorf("traffic on the configured port held by another program (-want +got):\n%s", diff)
			}
		}
	}
	// servingChild returns the first worker, serving and reported, after one request it answered.
	servingChild := func(t *testing.T, s *ownedShim) *ownedChild {
		t.Helper()
		s.awaitReady(t)
		child := s.nextChild(t)
		child.command(t, "serve", "serving")
		child.command(t, "report", "reported")
		if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: child.pid}, s.probe(t, rand.Text())); diff != "" {
			t.Fatalf("request to the reporting child (-want +got):\n%s", diff)
		}
		return child
	}
	tests := map[string]struct {
		run func(t *testing.T)
	}{
		"error: configured port held by another program receives nothing":                   {occupied(false)},
		"error: configured port held by another program that shares ports receives nothing": {occupied(true)},
		"error: configured port the child bound receives nothing before the child reports": {func(t *testing.T) {
			port := configuredPort(t)
			s := startOwnedShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port)}, nil)
			s.awaitReady(t)
			child := s.nextChild(t)
			if child.port != port {
				t.Fatalf("the child inherited a listener on port %d; want the configured port %d", child.port, port)
			}
			child.command(t, "serve", "serving")
			probe := s.startProbe(t, rand.Text())
			defer probe.cancel()
			// The child counts each request as arriving before or after its report, so the verdict does not depend on
			// this window; the window only gives a supervisor that relays before the report the chance to do so.
			window := time.NewTimer(300 * time.Millisecond)
			var a ownedAnswer
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
			if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: child.pid}, a.result); diff != "" {
				t.Errorf("request sent before the report and answered after it (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("requests 0 1 0 0", child.command(t, "requests", "requests ")); diff != "" {
				t.Errorf("requests the child received before and after its report (-want +got):\n%s", diff)
			}
		}},
		"success: configured port is relayed to once the child reports it": {func(t *testing.T) {
			port := configuredPort(t)
			s := startOwnedShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port)}, nil)
			s.awaitReady(t)
			child := s.nextChild(t)
			if child.port != port {
				t.Fatalf("the child inherited a listener on port %d; want the configured port %d", child.port, port)
			}
			child.command(t, "serve", "serving")
			child.command(t, "report", "reported")
			if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: child.pid}, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request after the child reported (-want +got):\n%s", diff)
			}
		}},
		"error: configured port whose child never reports stops the supervisor at the report deadline": {func(t *testing.T) {
			port := configuredPort(t)
			s := startOwnedShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port)}, nil)
			s.awaitReady(t)
			child := s.nextChild(t)
			// While the generation lives, its port is the supervisor's: no other program can bind it.
			refuseForeign(t, child.port, "while the supervisor waited for its child's report")
			// The deadline counts from the worker's start, which follows the readiness line.
			guard := 10 * time.Second
			if !s.awaitStop(s.readyAt.Add(ownedReportDeadline + guard)) {
				t.Fatalf("supervisor still running %v after readiness although its current child never reported", ownedReportDeadline+guard)
			}
			if s.result == nil {
				t.Error("supervisor stopped with exit status 0; want 1 when its child never reported")
			}
			if elapsed := s.doneAt.Sub(s.readyAt); elapsed < ownedReportDeadline-250*time.Millisecond {
				t.Errorf("supervisor stopped %v after readiness, before the report deadline of %v", elapsed, ownedReportDeadline)
			}
		}},
		"error: configured port reported as another port stops the supervisor at once": {func(t *testing.T) {
			port := configuredPort(t)
			s := startOwnedShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port)}, nil)
			s.awaitReady(t)
			child := s.nextChild(t)
			other := otherPort(t, port)
			reportAt := time.Now()
			child.command(t, "report "+strconv.Itoa(other), "reported")
			ownedStopsAtOnce(t, s, reportAt, other, port)
		}},
		"error: reserved port reported as another port stops the supervisor at once": {func(t *testing.T) {
			s := startOwnedShim(t, nil, nil)
			s.awaitReady(t)
			child := s.nextChild(t)
			other := otherPort(t, child.port)
			reportAt := time.Now()
			child.command(t, "report "+strconv.Itoa(other), "reported")
			ownedStopsAtOnce(t, s, reportAt, other, child.port)
		}},
		"error: a report for another generation makes nothing eligible": {func(t *testing.T) {
			watch := newAdmissionWatch()
			s := startOwnedShim(t, nil, withHooks(watch.install(serveshim.Hooks{})))
			s.awaitReady(t)
			child := s.nextChild(t)
			child.command(t, "serve", "serving")
			child.command(t, "report-gen stale-generation", "reported")
			s.admitsNothing(t, watch, "after a report for another generation")
			if diff := gocmp.Diff("0 0 0 0", child.counts(t)); diff != "" {
				t.Errorf("requests the child received after a report for another generation (-want +got):\n%s", diff)
			}
		}},
		"error: worker whose eligible record is still being published receives nothing": {func(t *testing.T) {
			// The supervisor is held after the eligible record was written and before the worker becomes eligible: a
			// request in that window finds no eligible generation and gets the retryable 503 when its attempts end.
			held, release := make(chan int, 1), make(chan struct{})
			unhold := sync.OnceFunc(func() { close(release) })
			watch := newAdmissionWatch()
			s := startOwnedShim(t, nil, withHooks(watch.install(serveshim.Hooks{Published: func(pid int) {
				select {
				case held <- pid:
				default:
				}
				<-release
			}})))
			t.Cleanup(unhold)
			s.awaitReady(t)
			child := s.nextChild(t)
			child.command(t, "serve", "serving")
			child.command(t, "report", "reported")
			select {
			case <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("the supervisor never reached the publication of its worker's eligible record")
			}
			// Admission is inspected while the publication is held: the relay looks and finds nothing eligible.
			s.admitsNothing(t, watch, "while the eligible publication is held")
			if diff := gocmp.Diff("0 0 0 0", child.counts(t)); diff != "" {
				t.Errorf("requests the child received while its eligible publication was held (-want +got):\n%s", diff)
			}
			unhold()
			if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: child.pid}, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request after the eligible publication completed (-want +got):\n%s", diff)
			}
		}},
		"error: worker whose pid record cannot be published stops the supervisor and receives nothing": {func(t *testing.T) {
			// A directory where the worker's pid record belongs makes every publication of that record fail.
			s := startOwnedShim(t, nil, nil, func(state string) {
				if err := os.Mkdir(filepath.Join(state, "shim.pid.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			})
			s.awaitReady(t)
			child := s.nextChild(t)
			child.command(t, "serve", "serving")
			probe := s.startProbe(t, rand.Text())
			child.command(t, "report", "reported")
			// The child says goodbye when its supervisor disconnects it; a supervisor that keeps serving the untracked
			// worker never does, and the control connection's deadline ends the wait.
			if diff := gocmp.Diff("parent gone 0 0 0 0", child.farewell(t)); diff != "" {
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
			if a := <-probe.answer; a.err == nil && a.result != ownedRetryable {
				t.Errorf("request in flight while the pid record could not be published: %+v; want the retryable 503 or no answer", a.result)
			}
		}},
		"error: port of a child that exited receives nothing": {func(t *testing.T) {
			// Also before the supervisor observed the exit: the supervisor is held between the system's report of the
			// exit and its own action on it, and its reservation keeps the port from every other program meanwhile.
			// The replacement's launch is held too, until another program has taken the exited child's port, so the
			// replacement cannot be given that port by chance.
			hold := newExitHold()
			var launches atomic.Int32
			replacementGo := make(chan struct{})
			letReplacement := sync.OnceFunc(func() { close(replacementGo) })
			s := startOwnedShim(t, nil, withHooks(serveshim.Hooks{ExitObserved: hold.observe, Launching: func() {
				if launches.Add(1) == 2 {
					<-replacementGo
				}
			}}))
			t.Cleanup(hold.free)
			t.Cleanup(letReplacement)
			first := servingChild(t, s)
			port := first.port
			// The child crashes: no drain notice, no orderly close.
			first.tell(t, "crash")
			first.exited(t)
			select {
			case pid := <-hold.held:
				if pid != first.pid {
					t.Fatalf("the supervisor reported the exit of pid %d; want the crashed child %d", pid, first.pid)
				}
			case <-s.done:
				t.Fatalf("supervisor stopped before it saw its child's exit: %v; stderr: %s", s.result, s.stderr)
			case <-time.After(10 * time.Second):
				t.Fatal("the supervisor did not report the crashed child's exit within 10s")
			}
			// Until the supervisor has acted on the exit, it still holds the generation's listener.
			refuseForeign(t, port, "between the child's exit and the supervisor acting on it")
			held := rand.Text()
			inWindow := s.startProbe(t, held)
			// The window only gives a relay that delivers to the dead generation's port the chance to do so; no other
			// program could take that port.
			time.Sleep(300 * time.Millisecond)
			hold.free()
			s.awaitExitRecord(t, first.pid)
			// After the exit was acted on, an independent holder takes the port and is reachable there.
			foreign := awaitForeign(t, port, 5*time.Second, "after the supervisor acted on its worker's exit")
			foreign.talk(t)
			// A request at this barrier, with the holder bound and no replacement yet.
			barrier := rand.Text()
			atBarrier := s.startProbe(t, barrier)
			letReplacement()
			second := s.nextChild(t)
			second.command(t, "serve", "serving")
			second.command(t, "report", "reported")
			for name, p := range map[string]*ownedInFlight{"sent while the exit was held": inWindow, "sent with the port's new holder bound": atBarrier} {
				a := <-p.answer
				if a.err != nil {
					t.Fatal(a.err)
				}
				if a.result != ownedRetryable {
					if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: second.pid}, a.result); diff != "" {
						t.Errorf("request %s; want the replacement's answer or the retryable 503 (-want +got):\n%s", name, diff)
					}
				}
			}
			after := rand.Text()
			if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: second.pid}, s.probe(t, after)); diff != "" {
				t.Errorf("request after the replacement reported (-want +got):\n%s", diff)
			}
			foreign.stop()
			traffic := foreign.traffic(s.token, held, barrier, after)
			if diff := gocmp.Diff(ownedTraffic{}, traffic); diff != "" {
				t.Errorf("traffic on the exited child's port (-want +got):\n%s", diff)
			}
		}},
		"error: after a drain notice the retired child receives nothing and a later holder of its port receives nothing": {func(t *testing.T) {
			s := startOwnedShim(t, nil, nil)
			child := servingChild(t, s)
			port := child.port
			// One request is in flight in the child when it announces its drain.
			child.command(t, "hold", "armed")
			inFlight := s.startProbe(t, rand.Text())
			child.command(t, "await-held", "held")
			// The child announces the drain; the supervisor retires the generation and answers.
			child.command(t, "notice", "answered")
			// The child closes its listener after the answer; the supervisor has released its own copy, so the port is
			// free for whoever binds it next.
			child.command(t, "close", "closed")
			foreign := awaitForeign(t, port, 5*time.Second, "after the drain was answered and the child closed its listener")
			foreign.talk(t)
			late := rand.Text()
			if diff := gocmp.Diff(ownedRetryable, s.probe(t, late)); diff != "" {
				t.Errorf("request after the drain while the retired child still runs (-want +got):\n%s", diff)
			}
			// The in-flight request finishes once, in the retired child.
			child.command(t, "release", "released")
			a := <-inFlight.answer
			if a.err != nil {
				t.Fatal(a.err)
			}
			if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: child.pid}, a.result); diff != "" {
				t.Errorf("request in flight during the drain (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("requests 0 2 0 0", child.command(t, "requests", "requests ")); diff != "" {
				t.Errorf("requests the child received before its report, after it, and after its drain was answered (-want +got):\n%s", diff)
			}
			foreign.stop()
			traffic := foreign.traffic(s.token, late)
			if diff := gocmp.Diff(ownedTraffic{}, traffic); diff != "" {
				t.Errorf("traffic on the retired child's port (-want +got):\n%s", diff)
			}
		}},
		"success: a connection established before the drain notice is served by the retired generation": {func(t *testing.T) {
			s := startOwnedShim(t, nil, nil)
			child := servingChild(t, s)
			// The child accepts the next connection and holds it unread: the lease reached the generation before its
			// drain, so the request is the retired generation's to answer, not an undelivered one to send elsewhere.
			child.command(t, "hold-accept", "armed")
			pinned := s.startProbe(t, rand.Text())
			child.command(t, "await-accept", "accept held")
			child.command(t, "notice", "answered")
			child.command(t, "release-accept", "released")
			a := <-pinned.answer
			if a.err != nil {
				t.Fatal(a.err)
			}
			if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: child.pid}, a.result); diff != "" {
				t.Errorf("request whose connection the child accepted before its drain (-want +got):\n%s", diff)
			}
			// The handed-over request is read after the answer, so the child counts it as late, and it is the only one.
			if diff := gocmp.Diff("requests 0 1 1 0", child.command(t, "requests", "requests ")); diff != "" {
				t.Errorf("requests the child received (-want +got):\n%s", diff)
			}
		}},
		"error: a drain notice before publication keeps a later report from making the worker eligible": {func(t *testing.T) {
			watch := newAdmissionWatch()
			s := startOwnedShim(t, nil, withHooks(watch.install(serveshim.Hooks{})))
			s.awaitReady(t)
			child := s.nextChild(t)
			child.command(t, "serve", "serving")
			child.command(t, "notice", "answered")
			child.command(t, "report", "reported")
			s.admitsNothing(t, watch, "after a drain notice and a later report")
			if diff := gocmp.Diff("0 0 0 0", child.counts(t)); diff != "" {
				t.Errorf("requests the child received after its drain and late report (-want +got):\n%s", diff)
			}
		}},
		"success: after frames that are not its drain notice the generation still serves and its own notice is answered": {func(t *testing.T) {
			// This case does not prove how the supervisor handles invalid, wrong-direction or repeated drain frames, nor
			// that a stale reader cannot retire its successor: nothing observable here orders the supervisor's reading of
			// a frame before the probe that follows it. Those rules are an obligation of the worker manager's own tests.
			// This case only checks that after such frames the generation still serves and its own notice is answered.
			s := startOwnedShim(t, nil, nil)
			child := servingChild(t, s)
			for _, frame := range []string{
				`{"type":"retired","generation":"{generation}"}`,
				`{"type":"draining","generation":"another-generation"}`,
			} {
				child.command(t, "notice-raw "+frame, "sent")
				if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: child.pid}, s.probe(t, rand.Text())); diff != "" {
					t.Errorf("request after the frame %s, which is not this generation's drain (-want +got):\n%s", frame, diff)
				}
			}
			child.command(t, "notice", "answered")
			if diff := gocmp.Diff("requests 0 3 0 0", child.command(t, "requests", "requests ")); diff != "" {
				t.Errorf("requests the child received (-want +got):\n%s", diff)
			}
		}},
		"error: a worker whose drain cannot be acknowledged is stopped and receives nothing more": {func(t *testing.T) {
			// The replacement's launch is held until another program has taken the ended generation's port, so a
			// correct replacement cannot be given that port first. The supervisor's answer to the notice is held until
			// the child has closed its end of the drain channel, so that the answer's write fails on every system.
			var launches atomic.Int32
			replacementGo, ackGo, ackEntered := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
			letReplacement := sync.OnceFunc(func() { close(replacementGo) })
			letAck := sync.OnceFunc(func() { close(ackGo) })
			s := startOwnedShim(t, nil, withHooks(serveshim.Hooks{
				Launching: func() {
					if launches.Add(1) == 2 {
						<-replacementGo
					}
				},
				Acknowledging: func() {
					select {
					case ackEntered <- struct{}{}:
					default:
					}
					<-ackGo
				},
			}))
			t.Cleanup(letReplacement)
			t.Cleanup(letAck)
			child := servingChild(t, s)
			port := child.port
			child.command(t, "drop-notice", "dropped")
			// The supervisor must have reached its answer to the notice; it writes the answer only once released, after
			// the child closed its end.
			select {
			case <-ackEntered:
			case <-time.After(10 * time.Second):
				t.Error("the supervisor did not reach its answer to the worker's drain notice within 10s")
			}
			letAck()
			// The answer cannot be written: the supervisor stops that worker, which says goodbye with its counts.
			if diff := gocmp.Diff("parent gone 0 1 0 0", child.farewell(t)); diff != "" {
				t.Errorf("requests of the worker whose drain could not be acknowledged (-want +got):\n%s", diff)
			}
			child.exited(t)
			// The generation was ended: the supervisor released its copy of the listener, so the port is free, and its
			// new holder answers there.
			foreign := awaitForeign(t, port, 5*time.Second, "after the worker whose drain could not be acknowledged was ended")
			foreign.talk(t)
			letReplacement()
		}},
		"error: after a public drain the retired child receives nothing": {func(t *testing.T) {
			s := startOwnedShim(t, nil, nil)
			child := servingChild(t, s)
			child.command(t, "drain-hold", "armed")
			type drained struct {
				status int
				err    error
			}
			public := make(chan drained, 1)
			go func() {
				status, err := s.publicDrain(t.Context())
				public <- drained{status, err}
			}()
			// The supervisor sends an authenticated drain to the worker on a lease it took before it retired the
			// generation; by the time the worker has it, admission is already withdrawn.
			if diff := gocmp.Diff("drain valid", child.command(t, "await-drain", "drain ")); diff != "" {
				t.Errorf("the drain request the worker received (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(ownedRetryable, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request after the public drain reached the worker, before its notice (-want +got):\n%s", diff)
			}
			// The notice is answered while the worker still withholds the drain's HTTP response.
			child.command(t, "notice", "answered")
			select {
			case d := <-public:
				t.Errorf("the public drain was answered with %d before the worker answered it", d.status)
			default:
			}
			child.command(t, "drain-respond", "responded")
			select {
			case d := <-public:
				if d.err != nil || d.status != http.StatusAccepted {
					t.Errorf("public drain: %d, %v; want 202", d.status, d.err)
				}
			case <-time.After(20 * time.Second):
				t.Error("the public drain got no answer within 20s of the worker's response")
			}
			if diff := gocmp.Diff("requests 0 1 0 1", child.command(t, "requests", "requests ")); diff != "" {
				t.Errorf("requests the child received before its report, after it, after its drain was answered, and drains (-want +got):\n%s", diff)
			}
		}},
		"success: stale handoff settings in the supervisor's environment are replaced": {func(t *testing.T) {
			s := startOwnedShim(t, map[string]string{
				workerlink.ListenerFDEnv: "97", workerlink.DrainFDEnv: "98", "MODEL_GATEWAY_CONTROL_FDS": "7,8",
				"MODEL_GATEWAY_WORKER_GENERATION": "stale-generation",
			}, nil)
			child := servingChild(t, s)
			if child.generation == "" || child.generation == "stale-generation" {
				t.Errorf("the worker was given generation %q; want a fresh one in place of the stale setting", child.generation)
			}
			if diff := gocmp.Diff("requests 0 1 0 0", child.command(t, "requests", "requests ")); diff != "" {
				t.Errorf("requests the child received (-want +got):\n%s", diff)
			}
		}},
		"error: worker port equal to the public port is refused at startup": {func(t *testing.T) {
			port := strconv.Itoa(configuredPort(t))
			s := startOwnedShim(t, map[string]string{"CODEX_GATEWAY_PORT": port, "CODEX_GATEWAY_WORKER_PORT": port}, nil)
			<-s.done
			// Everything the supervisor printed has been read once its stdout watcher ended.
			s.watched.Wait()
			select {
			case <-s.ready:
				t.Error("supervisor announced readiness with a worker port equal to its public port")
			default:
			}
			switch {
			case s.result == nil:
				t.Error("supervisor exited without an error for a worker port equal to its public port")
			case errors.Is(s.result, syscall.EADDRINUSE):
				// Another program took the number in the moment between its release and the supervisor's start; the
				// case then decides nothing about the equal ports and fails rather than passing.
				t.Fatalf("supervisor stopped with %v: the port was taken by another program, not refused as the worker's own", s.result)
			case !strings.Contains(s.result.Error(), "is the public port"):
				t.Errorf("supervisor stopped with %q; want the refusal of a worker port equal to the public port", s.result)
			}
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Each case runs a supervisor of its own; the cases need not wait in turn.
			t.Parallel()
			tc.run(t)
		})
	}
}

// ownedStopsAtOnce checks that the supervisor stopped with the port mismatch soon after its child reported other
// instead of its reserved port, and not by the report deadline. The bound counts from the report, so the child's own
// start-up is not charged against it.
func ownedStopsAtOnce(t *testing.T, s *ownedShim, reportAt time.Time, other, reserved int) {
	t.Helper()
	// A supervisor that acts on the report stops within moments of it; the bound leaves room for a loaded venue.
	const atOnce = 3 * time.Second
	if !s.awaitStop(reportAt.Add(atOnce)) {
		t.Fatalf("supervisor still running %v after its child reported port %d instead of the reserved %d", atOnce, other, reserved)
	}
	if since := s.doneAt.Sub(reportAt); since >= atOnce {
		t.Errorf("supervisor stopped %v after the report; want at once", since)
	}
	switch {
	case s.result == nil:
		t.Error("supervisor stopped with exit status 0; want 1 when its child reported another port")
	case s.result.Error() == ownedTimeoutText:
		t.Errorf("supervisor stopped with the report timeout %q; want the port mismatch", s.result)
	case !strings.Contains(s.result.Error(), strconv.Itoa(other)):
		t.Errorf("supervisor stopped with %q; want a diagnostic of the mismatch that names the reported port %d", s.result, other)
	}
}
