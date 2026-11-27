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

package serveworker_test

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
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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

// ownedMode is the worker helper's mode for a supervised worker whose descriptors are exactly the ones the test
// passes: the helper shifts nothing, so every descriptor number the worker uses comes from its environment.
const ownedMode = "owned"

// ownedGeneration is the generation of every handoff the tests prepare.
const ownedGeneration = "owned-fixture-generation"

// ownedToken is the control token written into each case's state directory.
const ownedToken = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// readyPattern matches the worker's TCP readiness line.
var readyPattern = regexp.MustCompile(`^model-gateway shim listening on 127\.0\.0\.1:([0-9]+)(?:\s|$)`)

// lineBuffer collects what a child writes and can say how many lines it holds.
type lineBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lineBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lineBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// handoff is what a supervisor gives one supervised worker, prepared by the test in the supervisor's place. The
// listener and the drain channel come from a real workerlink reservation, so that the port refuses connections only
// once both copies of the listener are closed, as it does under the supervisor.
//
// The child's descriptor for files[i] is firstChildFD+i: the fixture runner puts its private directory at 3, and the
// helper consumes it. A nil entry is closed in the child.
type handoff struct {
	env     gatewaytest.Environment
	home    string
	files   []*os.File
	reportR *os.File
	liveW   *os.File
	reg     *workerlink.Registry
	res     *workerlink.Reservation
	drain   net.Conn
	reader  *bufio.Reader
	port    uint16
}

// notOpenFD is a descriptor number that no worker child has open: the runtime and the helper open only a few
// descriptors beyond the ones the test passes, and those take the lowest free numbers.
const notOpenFD = "250"

// firstChildFD is the child's descriptor number of the first entry of ExtraFiles that the test passes.
const firstChildFD = 4

// Positions of the default handoff's descriptors in files.
const (
	reportSlot = iota
	liveSlot
	listenerSlot
	drainSlot
)

// fd is the child's descriptor number of files[slot], as the environment spells it.
func fd(slot int) string { return strconv.Itoa(firstChildFD + slot) }

// newHandoff reserves a listener and a drain channel for one worker generation and prepares the environment of a
// valid supervised handoff: the report pipe, the liveness pipe, the listener and the drain channel, each announced
// by its own variable.
func newHandoff(t *testing.T) *handoff {
	t.Helper()
	// The nested test name alone can exceed the Unix socket pathname limit.
	home, err := os.MkdirTemp("", "mgw-own-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
	})
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	state := filepath.Join(env["CLAUDE_CONFIG_DIR"], "model-gateway")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(ownedToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env["CODEX_GATEWAY_SOCKET_PATH"] = filepath.Join(home, "configured.sock")
	var cfg net.ListenConfig
	var dialer net.Dialer
	reg := workerlink.NewRegistry(cfg.Listen, dialer.DialContext)
	res, err := reg.Reserve(t.Context(), ownedGeneration, 0)
	if err != nil {
		t.Fatalf("reserve the worker's listener: %v", err)
	}
	t.Cleanup(func() { _ = res.End() })
	listener, err := res.File()
	if err != nil {
		t.Fatalf("take the worker's copy of its listener: %v", err)
	}
	drain, child, err := res.DrainChannel()
	if err != nil {
		_ = listener.Close()
		t.Fatalf("take the worker's drain channel: %v", err)
	}
	reportR, reportW := pipe(t)
	liveR, liveW := pipe(t)
	h := &handoff{
		env: env, home: home, files: []*os.File{reportW, liveR, listener, child}, reportR: reportR, liveW: liveW,
		reg: reg, res: res, drain: drain, reader: bufio.NewReader(drain), port: res.Port(),
	}
	t.Cleanup(func() {
		for _, f := range h.files {
			if f != nil {
				_ = f.Close()
			}
		}
	})
	env["MODEL_GATEWAY_CONTROL_FDS"] = fd(reportSlot) + "," + fd(liveSlot)
	env[workerlink.ListenerFDEnv] = fd(listenerSlot)
	env[workerlink.DrainFDEnv] = fd(drainSlot)
	env["MODEL_GATEWAY_WORKER_GENERATION"] = ownedGeneration
	env["CODEX_GATEWAY_WORKER_PORT"] = strconv.Itoa(int(h.port))
	return h
}

// add appends a file the child inherits and returns its child descriptor number.
func (h *handoff) add(f *os.File) string {
	h.files = append(h.files, f)
	return fd(len(h.files) - 1)
}

// fixtureLifetime is the longest the fixture runner lets a worker child live; it kills the child then.
const fixtureLifetime = 30 * time.Second

// postCloseRoom is the part of a child's lifetime kept for what a case checks after the child's listener closed: its
// in-flight upload completes, the worker returns cleanly and its diagnostic is read.
const postCloseRoom = 4 * time.Second

// launched is one running worker child.
type launched struct {
	// startedAt is taken just before the runner starts the child, so the runner kills it no earlier than
	// startedAt + fixtureLifetime.
	startedAt time.Time
	cmd       *exec.Cmd
	stdout    *bufio.Scanner
	output    []string
	stderr    *lineBuffer
	done      chan struct{}
	runErr    error
	// drained is closed once the rest of stdout after the readiness line was read; returned and drainErr are its
	// result. The child writes stdout into an unbuffered pipe, so it is read continuously until the child exits.
	drained  chan struct{}
	returned bool
	drainErr error
}

// launch starts the worker helper in mode with h's descriptors and environment. It closes the parent's copies of the
// child's descriptors once the child is running or has failed to start, as the supervisor does after Start.
func (h *handoff) launch(t *testing.T, mode string) *launched {
	t.Helper()
	cmd, err := h.env.HelperCommand(t, "worker", mode)
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = h.files
	l := &launched{cmd: cmd, stderr: new(lineBuffer), done: make(chan struct{}), drained: make(chan struct{})}
	cmd.Stderr = l.stderr
	stdout, w := io.Pipe()
	cmd.Stdout = w
	l.stdout = bufio.NewScanner(stdout)
	// The runner's deadline starts when it starts the child, after this moment, so lifetime bounds derived from
	// startedAt end no later than the runner's kill.
	l.startedAt = time.Now()
	go func() { defer close(l.done); l.runErr = h.env.RunFixtureWithin(cmd, fixtureLifetime); _ = w.Close() }()
	// HelperCommand uses t.Context(), which is canceled before cleanups run.
	t.Cleanup(func() { _ = stdout.Close(); _ = h.liveW.Close(); <-l.done; <-l.drained })
	return l
}

// closeTransfer closes the parent's copies of what the child inherited.
func (h *handoff) closeTransfer() {
	for i, f := range h.files {
		if f != nil {
			_ = f.Close()
			h.files[i] = nil
		}
	}
}

// awaitReady reads the child's stdout up to its readiness line and returns the port it names, or "" when the child
// ended its output without one.
func (l *launched) awaitReady(t *testing.T) string {
	t.Helper()
	defer l.drain()
	for l.stdout.Scan() {
		line := l.stdout.Text()
		l.output = append(l.output, line)
		if m := readyPattern.FindStringSubmatch(line); len(m) > 0 {
			return m[1]
		}
	}
	if err := l.stdout.Err(); err != nil {
		t.Errorf("read startup output: %v", err)
	}
	return ""
}

// drain reads the rest of the child's stdout in the background until it ends, keeping every line in l.output: the
// helper prints the worker's own failure there, also after readiness. l.output may be read once finish returned.
func (l *launched) drain() {
	go func() {
		defer close(l.drained)
		for l.stdout.Scan() {
			line := l.stdout.Text()
			l.output = append(l.output, line)
			if line == "worker returned cleanly" {
				l.returned = true
			}
		}
		l.drainErr = l.stdout.Err()
	}()
}

// lastClose is the latest moment the child's listener may close with room left for the checks after the close.
func (l *launched) lastClose() time.Time { return l.startedAt.Add(fixtureLifetime - postCloseRoom) }

// requireHeadroom fails the case as the test's own infrastructure failure when need, counted from now, does not end
// by lastClose: a correct worker could then be killed by the runner before the case has judged it.
func (l *launched) requireHeadroom(t *testing.T, need time.Duration, after string) {
	t.Helper()
	if left := time.Until(l.lastClose()); left < need {
		t.Fatalf("test infrastructure: %s left %v of the fixture's %v lifetime before the post-close checks, and a correct worker needs %v; the setup used too much of the fixture's lifetime, nothing about the worker is judged; not a product failure", after, left, fixtureLifetime, need)
	}
}

// boundAfter returns anchor + need, the moment by which a correct worker's listener closes, after checking that this
// very value leaves room for the checks after the close; otherwise the case fails as the test's own infrastructure
// failure.
func (l *launched) boundAfter(t *testing.T, anchor time.Time, need time.Duration, after string) time.Time {
	t.Helper()
	bound := anchor.Add(need)
	if last := l.lastClose(); bound.After(last) {
		t.Fatalf("test infrastructure: the bound %v after %s ends %v after the latest close the fixture's %v lifetime leaves room for; nothing about the worker is judged; not a product failure", need, after, bound.Sub(last), fixtureLifetime)
	}
	return bound
}

// finish reads the rest of the child's stdout and waits for it to exit. It reports whether the helper printed its
// clean-return line.
func (l *launched) finish(t *testing.T) bool {
	t.Helper()
	<-l.drained
	if l.drainErr != nil {
		t.Errorf("read shutdown output: %v", l.drainErr)
	}
	<-l.done
	return l.returned
}

// exitCode is the child's exit status, or -1 when it did not exit with one.
func (l *launched) exitCode() int {
	if l.runErr == nil {
		return 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](l.runErr); ok {
		return exit.ExitCode()
	}
	return -1
}

// reportBytes reads what the child wrote on its report pipe, after the parent's write end was closed.
func (h *handoff) reportBytes(t *testing.T) []byte {
	t.Helper()
	if err := h.reportR.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(io.LimitReader(h.reportR, 257))
	if err != nil {
		t.Fatalf("the report pipe did not reach end of file: %v", err)
	}
	return data
}

// readDrain decodes the next message the worker wrote on the drain channel, within limit.
func (h *handoff) readDrain(t *testing.T, limit time.Duration) (workerlink.DrainMessage, error) {
	t.Helper()
	if err := h.drain.SetReadDeadline(time.Now().Add(limit)); err != nil {
		t.Fatal(err)
	}
	return workerlink.DecodeDrain(h.reader)
}

// get sends one GET to the worker's TCP port and returns the status.
func get(ctx context.Context, port uint16, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(int(port))+path, nil)
	if err != nil {
		return 0, err
	}
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, err
}

// TestWorkerModesOwnedListener carries every case of the worker mode table with the supervised worker's listener
// received from its supervisor. Standalone modes are unchanged: the worker binds its own listener. A supervised worker
// serves on the listener it inherited, at the reserved port, and reports that port; every report and liveness
// descriptor failure keeps its own diagnostic while the listener and drain descriptors are valid.
func TestWorkerModesOwnedListener(t *testing.T) {
	tests := map[string]struct{ mode, wantError string }{
		"success: standalone ignores closed stdin":                 {"standalone", ""},
		"success: supervised reports port then follows parent EOF": {"supervised", ""},
		"success: fixture EOF stops standalone worker":             {"fixture", ""},
		"success: NODE_TEST_CONTEXT alone enables fixture EOF":     {"node-fixture", ""},
		"success: outside fixture guard the channel closes unread": {"unguarded", ""},
		"success: missing token created for drain":                 {"new-token", ""},
		"success: configured socket serves health":                 {"socket", ""},
		"success: occupied socket path does not prevent TCP":       {"plain-file", ""},
		"success: configured port needs no report pipe":            {"fixed-port", ""},
		"error: missing liveness descriptor":                       {"missing", "descriptor"},
		"error: missing report descriptor":                         {"no-report", "descriptor"},
		"error: malformed descriptor setting":                      {"malformed", "descriptor"},
		"error: stdin is not the report descriptor":                {"stdin-report", "descriptor"},
		"error: stdin is not the liveness descriptor":              {"stdin-live", "descriptor"},
		"error: supervised generation missing":                     {"no-generation", "generation"},
		"error: malformed token file":                              {"bad-token", "token"},
	}
	supervisedFamily := map[string]bool{"supervised": true, "missing": true, "no-report": true, "malformed": true, "stdin-report": true, "stdin-live": true, "no-generation": true}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHandoff(t)
			token := ownedToken
			tokenPath := filepath.Join(h.env["CLAUDE_CONFIG_DIR"], "model-gateway", "control-token")
			mode := tt.mode
			if supervisedFamily[tt.mode] {
				// The supervised family gets the full valid handoff; each error case breaks one report or liveness
				// detail of it.
				mode = ownedMode
				switch tt.mode {
				// A missing descriptor is a number no open file has in the child. Every slot stays occupied across exec:
				// a free low number could be taken by the runtime's own poller, which macOS reports as a FIFO.
				case "missing":
					h.env["MODEL_GATEWAY_CONTROL_FDS"] = fd(reportSlot) + "," + notOpenFD
				case "no-report":
					h.env["MODEL_GATEWAY_CONTROL_FDS"] = notOpenFD + "," + fd(liveSlot)
				case "malformed":
					h.env["MODEL_GATEWAY_CONTROL_FDS"] = "not-a-descriptor"
				case "stdin-report":
					h.env["MODEL_GATEWAY_CONTROL_FDS"] = "0," + fd(liveSlot)
				case "stdin-live":
					h.env["MODEL_GATEWAY_CONTROL_FDS"] = fd(reportSlot) + ",0"
				case "no-generation":
					delete(h.env, "MODEL_GATEWAY_WORKER_GENERATION")
				}
			} else {
				// A standalone worker gets no supervisor handoff at all.
				for _, key := range []string{"MODEL_GATEWAY_CONTROL_FDS", workerlink.ListenerFDEnv, workerlink.DrainFDEnv, "MODEL_GATEWAY_WORKER_GENERATION", "CODEX_GATEWAY_WORKER_PORT"} {
					delete(h.env, key)
				}
				h.closeTransfer()
				h.files = nil
				if tt.mode == "bad-token" {
					if err := os.WriteFile(tokenPath, []byte("not-hex\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if tt.mode == "new-token" {
					if err := os.Remove(tokenPath); err != nil {
						t.Fatal(err)
					}
				}
				socketPath := h.env["CODEX_GATEWAY_SOCKET_PATH"]
				if tt.mode == "plain-file" {
					if err := os.WriteFile(socketPath, []byte("untouched"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if tt.mode == "fixed-port" {
					// A listener the test keeps until the port is handed over: the number is free only for that moment.
					ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					port := ln.Addr().(*net.TCPAddr).Port
					if err := ln.Close(); err != nil {
						t.Fatal(err)
					}
					if port == 80 || port == 18764 || port == 18765 {
						t.Fatal("ephemeral range includes protected port")
					}
					h.env["CODEX_GATEWAY_WORKER_PORT"] = strconv.Itoa(port)
				}
				if tt.mode == "fixture" || tt.mode == "unguarded" || tt.mode == "node-fixture" {
					if tt.mode == "node-fixture" {
						h.env["NODE_TEST_CONTEXT"] = "child-v8"
					}
					liveR, liveW := pipe(t)
					h.liveW = liveW
					h.env["NODE_CHANNEL_FD"] = h.add(liveR)
				}
			}
			l := h.launch(t, mode)
			port := l.awaitReady(t)
			h.closeTransfer()
			if tt.wantError != "" {
				if port != "" {
					t.Fatalf("served despite invalid startup configuration: %s", port)
				}
				l.finish(t)
				text := strings.ToLower(strings.Join(l.output, "\n") + "\n" + l.stderr.String())
				specific := strings.Contains(text, tt.wantError) || (tt.wantError == "descriptor" && (strings.Contains(text, "control_fds") || strings.Contains(text, "bad file")))
				if l.exitCode() != 1 || !specific {
					t.Fatalf("want %s startup failure (exit 1), got %v: %s", tt.wantError, l.runErr, text)
				}
				return
			}
			if port == "" {
				l.finish(t)
				t.Fatalf("no listening line: %v; %s\n%s", l.runErr, strings.Join(l.output, "\n"), l.stderr)
			}
			n, err := strconv.Atoi(port)
			if err != nil || n <= 0 || n > 65535 {
				t.Fatalf("invalid listening port %q", port)
			}
			if tt.mode == "fixed-port" && port != h.env["CODEX_GATEWAY_WORKER_PORT"] {
				t.Fatalf("worker ignored configured port: got %s want %s", port, h.env["CODEX_GATEWAY_WORKER_PORT"])
			}
			if tt.mode == "supervised" && n != int(h.port) {
				t.Fatalf("supervised worker listens on port %d; want the inherited listener's port %d", n, h.port)
			}
			if tt.mode == "new-token" {
				info, err := os.Stat(tokenPath)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode() != 0o600 {
					t.Fatalf("generated token mode %v, want 0600", info.Mode())
				}
				data, err := os.ReadFile(tokenPath)
				if err != nil {
					t.Fatal(err)
				}
				if !regexp.MustCompile(`^[0-9a-f]{64}\n$`).Match(data) {
					t.Fatal("generated token has wrong format")
				}
				token = strings.TrimSuffix(string(data), "\n")
			}
			addr := "http://127.0.0.1:" + port
			if tt.mode == "supervised" {
				data := h.reportBytes(t)
				if len(data) > 256 || bytes.Count(data, []byte("\n")) != 1 || !bytes.HasSuffix(data, []byte("\n")) {
					t.Fatalf("bad report framing: %q", data)
				}
				var got map[string]any
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				want := map[string]any{"type": "listening", "port": float64(h.port), "generation": ownedGeneration}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Fatal(diff)
				}
			}
			if tt.mode == "unguarded" {
				// The child's copy is the only reader left: the parent's copy was closed after the start.
				if _, err := h.liveW.Write([]byte("ignored")); !errors.Is(err, unix.EPIPE) {
					t.Fatalf("unused fixture descriptor remains open: %v", err)
				}
			}
			client := &http.Client{Transport: &http.Transport{}, Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			req, err := http.NewRequestWithContext(t.Context(), "GET", addr+"/healthz", nil)
			if err != nil {
				t.Fatal(err)
			}
			healthClient := client
			socketPath := h.env["CODEX_GATEWAY_SOCKET_PATH"]
			if tt.mode == "socket" {
				healthClient = &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				}}}
				defer healthClient.CloseIdleConnections()
			}
			resp, err := healthClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var health struct {
				OK      bool   `json:"ok"`
				Version string `json:"version"`
			}
			err = json.UnmarshalRead(resp.Body, &health)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || !health.OK {
				t.Fatalf("health status=%d ok=%t err=%v", resp.StatusCode, health.OK, err)
			}
			if health.Version != "fixture-build" {
				t.Fatalf("health version %q, want fixture-build", health.Version)
			}
			if tt.mode == "supervised" || tt.mode == "fixture" || tt.mode == "node-fixture" {
				if err := h.liveW.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				drain, err := http.NewRequestWithContext(t.Context(), "POST", addr+"/drain", strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				drain.Header.Set("Content-Type", "application/json")
				drain.Header.Set("X-Model-Gateway-Control-Token", token)
				if tt.mode == "socket" {
					unixDrain := drain.Clone(t.Context())
					unixDrain.Body, err = drain.GetBody()
					if err != nil {
						t.Fatal(err)
					}
					denied, err := healthClient.Do(unixDrain)
					if err != nil {
						t.Fatal(err)
					}
					_ = denied.Body.Close()
					if denied.StatusCode != http.StatusForbidden {
						t.Fatalf("Unix drain status %d, want 403", denied.StatusCode)
					}
				}
				resp, err := client.Do(drain)
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != 202 {
					t.Fatalf("drain status=%d err=%v", resp.StatusCode, err)
				}
			}
			returned := l.finish(t)
			if tt.mode == "plain-file" {
				got, err := os.ReadFile(socketPath)
				if err != nil || string(got) != "untouched" {
					t.Fatalf("worker replaced the occupied socket path: %q, %v", got, err)
				}
				if !strings.Contains(l.stderr.String(), socketPath) {
					t.Errorf("no bind diagnostic for occupied socket path: %s", l.stderr)
				}
			}
			if l.runErr != nil || !returned {
				t.Fatalf("clean return=%t, child status=%v: %s", returned, l.runErr, l.stderr)
			}
			if tt.mode == "supervised" {
				// The parent went away: the worker stops at once and writes no drain notice.
				msg, err := h.readDrain(t, 5*time.Second)
				if !errors.Is(err, io.EOF) {
					t.Errorf("drain channel after the parent's end of file: message %+v, error %v; want end of file with no notice", msg, err)
				}
			}
		})
	}
}

// TestSupervisedWorkerRefusesBadHandoff checks that a supervised worker whose listener or drain descriptor is missing,
// malformed, aliased or of the wrong kind exits with status 1 and a diagnostic that identifies the descriptor's role
// and the cause, and never falls back to binding a listener by port: it prints no readiness line and writes nothing
// on its report pipe. Whatever else the handoff carries is valid, so the diagnostic is about the broken descriptor.
// The diagnostic's wording is not pinned: it must name the role (listener, drain channel, report or liveness) and
// say what is wrong in words of one of the listed kinds.
func TestSupervisedWorkerRefusesBadHandoff(t *testing.T) {
	// Roles, as words a diagnostic about that descriptor contains.
	var (
		listenerRole = []string{"listener"}
		drainRole    = []string{"drain"}
		controlRole  = []string{"report", "liveness", "control"}
	)
	// Causes, as words one of which a diagnostic of that kind contains.
	var (
		missing   = []string{"missing", "not set", "needs", "required", "absent", "without"}
		malformed = []string{"malformed", "invalid", "not a", "number", "parse", "syntax"}
		standard  = []string{"standard", "stdin", "stdout", "stderr", "malformed", "invalid"}
		notOpen   = []string{"bad file", "not open", "ebadf", "closed", "unusable", "invalid"}
		aliased   = []string{"same", "alias", "both", "duplicate", "twice", "endpoint"}
		wrongKind = []string{"not a", "kind", "type", "socket", "stream", "listen", "connect", "family", "unix", "tcp", "address", "port", "unusable", "invalid"}
	)
	join := func(sets ...[]string) []string {
		var out []string
		for _, s := range sets {
			out = append(out, s...)
		}
		return out
	}
	type breakFunc func(t *testing.T, h *handoff)
	// replace puts f where slot was, closing what was there.
	replace := func(h *handoff, slot int, f *os.File) {
		_ = h.files[slot].Close()
		h.files[slot] = f
	}
	tests := map[string]struct {
		breakIt breakFunc
		roles   []string
		causes  []string
	}{
		"error: listener setting missing": {func(t *testing.T, h *handoff) {
			delete(h.env, workerlink.ListenerFDEnv)
			// A free port, so that a worker that fell back to binding by port could bind it and would print its line.
			h.env["CODEX_GATEWAY_WORKER_PORT"] = strconv.Itoa(freeTCPPort(t))
		}, listenerRole, missing},
		"error: drain setting missing": {func(_ *testing.T, h *handoff) {
			delete(h.env, workerlink.DrainFDEnv)
		}, drainRole, missing},
		"error: listener setting empty": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.ListenerFDEnv] = ""
		}, listenerRole, join(malformed, missing)},
		"error: listener setting not a number": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.ListenerFDEnv] = "listener"
		}, listenerRole, malformed},
		"error: listener setting with a leading zero": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.ListenerFDEnv] = "0" + fd(listenerSlot)
		}, listenerRole, malformed},
		"error: listener setting with a sign": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.ListenerFDEnv] = "+" + fd(listenerSlot)
		}, listenerRole, malformed},
		"error: listener setting negative": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.ListenerFDEnv] = "-1"
		}, listenerRole, malformed},
		"error: listener setting names a standard stream": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.ListenerFDEnv] = "2"
		}, listenerRole, standard},
		"error: listener setting names a descriptor that is not open": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.ListenerFDEnv] = notOpenFD
		}, listenerRole, notOpen},
		"error: drain setting not a number": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.DrainFDEnv] = "7.0"
		}, drainRole, malformed},
		"error: drain setting names a standard stream": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.DrainFDEnv] = "0"
		}, drainRole, standard},
		"error: drain setting names a descriptor that is not open": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.DrainFDEnv] = notOpenFD
		}, drainRole, notOpen},
		"error: listener and drain settings name the same descriptor": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.DrainFDEnv] = fd(listenerSlot)
		}, join(listenerRole, drainRole), join(aliased, wrongKind)},
		"error: listener setting names the report descriptor": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.ListenerFDEnv] = fd(reportSlot)
		}, join(listenerRole, controlRole), join(aliased, wrongKind)},
		"error: drain setting names the liveness descriptor": {func(_ *testing.T, h *handoff) {
			h.env[workerlink.DrainFDEnv] = fd(liveSlot)
		}, join(drainRole, controlRole), join(aliased, wrongKind)},
		"error: report setting names the listener descriptor": {func(_ *testing.T, h *handoff) {
			h.env["MODEL_GATEWAY_CONTROL_FDS"] = fd(listenerSlot) + "," + fd(liveSlot)
		}, join(listenerRole, controlRole), join(aliased, wrongKind)},
		"error: liveness descriptor is another number for the drain socket": {func(t *testing.T, h *handoff) {
			replace(h, liveSlot, dupFile(t, h.files[drainSlot]))
		}, join(drainRole, controlRole), aliased},
		"error: report descriptor is another number for the drain socket": {func(t *testing.T, h *handoff) {
			replace(h, reportSlot, dupFile(t, h.files[drainSlot]))
		}, join(drainRole, controlRole), aliased},
		"error: liveness descriptor is another number for the listener socket": {func(t *testing.T, h *handoff) {
			replace(h, liveSlot, dupFile(t, h.files[listenerSlot]))
		}, join(listenerRole, controlRole), join(aliased, wrongKind)},
		"error: report and liveness descriptors are one socket under two numbers": {func(t *testing.T, h *handoff) {
			// One end of a connected socket pair is readable and writable, so each number passes its own access check.
			end := datagramPairEnd(t)
			replace(h, reportSlot, dupFile(t, end))
			replace(h, liveSlot, end)
		}, controlRole, aliased},
		"error: listener descriptor is a pipe": {func(t *testing.T, h *handoff) {
			r, _ := pipe(t)
			replace(h, listenerSlot, dupFile(t, r))
		}, listenerRole, wrongKind},
		"error: listener descriptor is a regular file": {func(t *testing.T, h *handoff) {
			f, err := os.CreateTemp(h.home, "plain-")
			if err != nil {
				t.Fatal(err)
			}
			replace(h, listenerSlot, f)
		}, listenerRole, wrongKind},
		"error: listener descriptor is a connected TCP stream": {func(t *testing.T, h *handoff) {
			replace(h, listenerSlot, connectedTCP(t))
		}, listenerRole, wrongKind},
		"error: listener descriptor is a UDP socket on the expected port": {func(t *testing.T, h *handoff) {
			var cfg net.ListenConfig
			pc, err := cfg.ListenPacket(t.Context(), "udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(h.port))))
			if err != nil {
				t.Fatalf("bind a UDP socket on the reserved port: %v", err)
			}
			t.Cleanup(func() { _ = pc.Close() })
			f, err := pc.(*net.UDPConn).File()
			if err != nil {
				t.Fatal(err)
			}
			replace(h, listenerSlot, f)
		}, listenerRole, wrongKind},
		"error: listener descriptor listens on another port than the configured one": {func(t *testing.T, h *handoff) {
			replace(h, listenerSlot, tcpListenerFile(t))
		}, listenerRole, wrongKind},
		"error: listener descriptor is a Unix stream listener": {func(t *testing.T, h *handoff) {
			var cfg net.ListenConfig
			ln, err := cfg.Listen(t.Context(), "unix", filepath.Join(h.home, "l.sock"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			f, err := ln.(*net.UnixListener).File()
			if err != nil {
				t.Fatal(err)
			}
			replace(h, listenerSlot, f)
		}, listenerRole, wrongKind},
		"error: drain descriptor is a pipe": {func(t *testing.T, h *handoff) {
			r, _ := pipe(t)
			replace(h, drainSlot, dupFile(t, r))
		}, drainRole, wrongKind},
		"error: drain descriptor is a connected TCP stream": {func(t *testing.T, h *handoff) {
			replace(h, drainSlot, connectedTCP(t))
		}, drainRole, wrongKind},
		"error: drain descriptor is an unconnected Unix stream socket": {func(t *testing.T, h *handoff) {
			replace(h, drainSlot, rawSocketFile(t, unix.AF_UNIX, unix.SOCK_STREAM, "unconnected unix stream"))
		}, drainRole, wrongKind},
		"error: drain descriptor is a Unix datagram socket of a connected pair": {func(t *testing.T, h *handoff) {
			replace(h, drainSlot, datagramPairEnd(t))
		}, drainRole, wrongKind},
		"error: drain descriptor is a Unix stream listener": {func(t *testing.T, h *handoff) {
			var cfg net.ListenConfig
			ln, err := cfg.Listen(t.Context(), "unix", filepath.Join(h.home, "d.sock"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			f, err := ln.(*net.UnixListener).File()
			if err != nil {
				t.Fatal(err)
			}
			replace(h, drainSlot, f)
		}, drainRole, wrongKind},
		"error: drain descriptor is the TCP listener under another number": {func(t *testing.T, h *handoff) {
			replace(h, drainSlot, dupFile(t, h.files[listenerSlot]))
		}, drainRole, join(wrongKind, aliased)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHandoff(t)
			tc.breakIt(t, h)
			l := h.launch(t, ownedMode)
			port := l.awaitReady(t)
			h.closeTransfer()
			// A worker that served anyway keeps running until its parent goes away.
			_ = h.liveW.Close()
			l.finish(t)
			if l.exitCode() != 1 {
				t.Fatalf("exit status %d (%v); want 1 for a broken handoff: %s", l.exitCode(), l.runErr, l.stderr)
			}
			if port != "" {
				t.Errorf("the worker printed its readiness line on port %s despite a broken handoff", port)
			}
			if data := h.reportBytes(t); len(data) != 0 {
				t.Errorf("the worker reported %q despite a broken handoff", data)
			}
			text := strings.Join(l.output, "\n") + "\n" + l.stderr.String()
			// The old fallback, binding the configured port, fails with the bind's own error when the port is held; that
			// is not a refusal of the broken descriptor.
			if lower := strings.ToLower(text); strings.Contains(lower, "address already in use") || strings.Contains(lower, "eaddrinuse") {
				t.Errorf("the worker tried to bind the port itself instead of refusing the broken handoff: %s", text)
			}
			checkDiagnostic(t, text, tc.roles, tc.causes)
		})
	}
}

// checkDiagnostic fails the test unless one line of text, lowercased, contains one of roles and, apart from where that
// role word stands, one of causes: a role word is never its own cause, and role and cause must be in the same line.
func checkDiagnostic(t *testing.T, text string, roles, causes []string) {
	t.Helper()
	if !diagnosed(text, roles, causes) {
		t.Errorf("diagnostic %q has no line that names a role among %q together with a cause among %q", text, roles, causes)
	}
}

// diagnosed reports whether one line of text names one of roles and, outside that role word, one of causes.
func diagnosed(text string, roles, causes []string) bool {
	for line := range strings.Lines(strings.ToLower(text)) {
		for _, role := range roles {
			before, after, ok := strings.Cut(line, role)
			if !ok {
				continue
			}
			rest := before + strings.Repeat(" ", len(role)) + after
			for _, cause := range causes {
				if strings.Contains(rest, cause) {
					return true
				}
			}
		}
	}
	return false
}

// TestSupervisedWorkerRefusesUnlistenedSocket checks a listener descriptor that is a TCP socket bound to exactly the
// expected loopback address and port but not listening, in an otherwise valid handoff. Linux refuses it when the
// worker imports it: exit 1, a diagnostic about the listener, no readiness line and no report. macOS cannot tell a
// listening socket at import, so the import succeeds there and the worker's first accept fails: exit 1 is required
// and nothing more is pinned about the order of the report.
func TestSupervisedWorkerRefusesUnlistenedSocket(t *testing.T) {
	h := newHandoff(t)
	socket := boundNotListening(t, 0)
	sa, err := unix.Getsockname(int(socket.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	port := sa.(*unix.SockaddrInet4).Port
	_ = h.files[listenerSlot].Close()
	h.files[listenerSlot] = socket
	h.env["CODEX_GATEWAY_WORKER_PORT"] = strconv.Itoa(port)
	l := h.launch(t, ownedMode)
	ready := l.awaitReady(t)
	h.closeTransfer()
	// The liveness pipe stays open: its end of file would stop the worker with status 0 before its failed accept is
	// harvested. The worker's own failure ends it.
	l.finish(t)
	if l.exitCode() != 1 {
		t.Fatalf("exit status %d (%v); want 1 for an unlistened listener descriptor: %s", l.exitCode(), l.runErr, l.stderr)
	}
	text := strings.Join(l.output, "\n") + "\n" + l.stderr.String()
	// A worker that fell back to binding the configured port by itself fails with the bind's error; that is not this
	// refusal.
	if lower := strings.ToLower(text); strings.Contains(lower, "address already in use") || strings.Contains(lower, "eaddrinuse") {
		t.Fatalf("the worker failed by binding the port itself, not by refusing the unlistened descriptor: %s", text)
	}
	if runtime.GOOS != "linux" {
		// macOS cannot tell a listening socket at import; the failure is the first accept's.
		checkDiagnostic(t, text, []string{"accept"}, []string{"invalid", "not listening", "einval", "argument"})
		return
	}
	if ready != "" {
		t.Errorf("the worker printed its readiness line on port %s with an unlistened listener", ready)
	}
	if data := h.reportBytes(t); len(data) != 0 {
		t.Errorf("the worker reported %q with an unlistened listener", data)
	}
	checkDiagnostic(t, text, []string{"listener"}, []string{"not listening", "not a listening", "kind", "invalid", "unusable"})
}

// freeTCPPort returns a loopback port number that was free a moment ago.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// TestStandaloneWorkerIgnoresHandoffSettings checks that the listener and drain settings alone do not make a worker
// supervised: without MODEL_GATEWAY_CONTROL_FDS it binds its own listener, does not use the descriptors those settings
// name, and stops on a drain as a standalone worker does.
func TestStandaloneWorkerIgnoresHandoffSettings(t *testing.T) {
	h := newHandoff(t)
	delete(h.env, "MODEL_GATEWAY_CONTROL_FDS")
	delete(h.env, "MODEL_GATEWAY_WORKER_GENERATION")
	delete(h.env, "CODEX_GATEWAY_WORKER_PORT")
	l := h.launch(t, ownedMode)
	port := l.awaitReady(t)
	h.closeTransfer()
	if port == "" {
		l.finish(t)
		t.Fatalf("no listening line: %v; %s\n%s", l.runErr, strings.Join(l.output, "\n"), l.stderr)
	}
	if port == strconv.Itoa(int(h.port)) {
		t.Errorf("a standalone worker serves on the port of the listener a stray setting named")
	}
	status, err := get(t.Context(), mustPort(t, port), "/healthz")
	if err != nil || status != http.StatusOK {
		t.Fatalf("standalone health: %d, %v", status, err)
	}
	if code, _, err := postDrain(t.Context(), "127.0.0.1:"+port, ownedToken); err != nil || code != http.StatusAccepted {
		t.Fatalf("standalone drain: %d, %v", code, err)
	}
	if !l.finish(t) || l.runErr != nil {
		t.Fatalf("standalone worker did not return cleanly: %v: %s", l.runErr, l.stderr)
	}
	// The drain channel the setting named was not used: no notice reached the reservation.
	if msg, err := h.readDrain(t, 2*time.Second); !errors.Is(err, io.EOF) {
		t.Errorf("drain channel of a standalone worker: message %+v, error %v; want end of file with no notice", msg, err)
	}
}

func mustPort(t *testing.T, s string) uint16 {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 65535 {
		t.Fatalf("invalid port %q", s)
	}
	return uint16(n)
}

// drainDiagnostics returns the stderr lines that are about the drain exchange, over the worker's whole run.
func drainDiagnostics(stderr string) []string {
	var lines []string
	for line := range strings.Lines(stderr) {
		if strings.Contains(strings.ToLower(line), "drain") {
			lines = append(lines, line)
		}
	}
	return lines
}

// checkDrainDiagnostic checks the one diagnostic of an exchange that got no valid answer: exactly one line about the
// drain channel, of at most 512 bytes, that names the role and the cause.
func checkDrainDiagnostic(t *testing.T, stderr string, causes []string) {
	t.Helper()
	lines := drainDiagnostics(stderr)
	if len(lines) != 1 {
		t.Errorf("diagnostics about the drain exchange: %q; want exactly one", lines)
		return
	}
	if len(lines[0]) > 512 {
		t.Errorf("drain diagnostic of %d bytes; want at most 512", len(lines[0]))
	}
	checkDiagnostic(t, lines[0], []string{"drain"}, causes)
}

// stalledUpload starts one request whose upload is paused until release, and returns once the worker has read its
// head and asked for the body. Its result arrives on the returned channel.
func stalledUpload(t *testing.T, ctx context.Context, public string, release <-chan struct{}) <-chan uploadResult {
	t.Helper()
	const invalidBody = `{"model":"claude-grok-test\ud800"}`
	started := make(chan struct{})
	result := make(chan uploadResult, 1)
	go func() {
		tr := &http.Transport{ExpectContinueTimeout: 10 * time.Second, DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		trace := &httptrace.ClientTrace{Got100Continue: func() { close(started) }}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, "http://"+public+"/v1/messages", nil)
		if err != nil {
			result <- uploadResult{err: err}
			return
		}
		req.Body = io.NopCloser(pausedReader{Reader: strings.NewReader(invalidBody), release: release, ctx: ctx})
		req.ContentLength = int64(len(invalidBody))
		req.Header.Set("Expect", "100-continue")
		resp, err := tr.RoundTrip(req)
		if err != nil {
			result <- uploadResult{err: err}
			return
		}
		_, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		result <- uploadResult{resp.StatusCode, err}
	}()
	select {
	case <-started:
	case r := <-result:
		t.Fatalf("the stalled upload ended before the worker asked for its body: %d, %v", r.status, r.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return result
}

// uploadResult is the outcome of a stalled upload.
type uploadResult struct {
	status int
	err    error
}

// closeInfrastructure is the reason a closure sample decides nothing about the worker, or "" when it decides.
type closeInfrastructure string

// observeClose dials the worker's port until it refuses and judges the close against limit. A refusal observed by the
// limit means the listener closed in time. A dial that started after the limit and was accepted proves the listener
// still open after it: that fails the case as the worker's failure. A first refusal observed only after the limit
// brackets the close between the last accepted dial, which started before the limit, and that refusal, so it decides
// nothing and is returned as the test's own infrastructure failure. The worker exiting while its in-flight request is
// still stalled fails the case. With watchdog set, limit is only the fixture's liveness cutoff of a case that cannot
// see when the exchange began: a listener still open after it fails the case as undecided, not as the worker's
// failure.
func observeClose(t *testing.T, ctx context.Context, public string, l *launched, limit time.Time, watchdog bool) closeInfrastructure {
	t.Helper()
	var dialer net.Dialer
	for {
		started := time.Now()
		conn, err := dialer.DialContext(ctx, "tcp4", public)
		if err != nil {
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("dial the worker's port: %v; want the connection refused", err)
			}
			if refusedAt := time.Now(); refusedAt.After(limit) {
				return closeInfrastructure(fmt.Sprintf("the first refusal was observed %v after the limit, by a dial started %v after it; the close lies between the last accepted dial and that refusal", refusedAt.Sub(limit), started.Sub(limit)))
			}
			return ""
		}
		_ = conn.Close()
		if started.After(limit) && watchdog {
			t.Fatalf("test infrastructure: undecided: the worker's port was still open at the fixture's liveness cutoff %v (a connection dialled %v after it was accepted); this case cannot observe when the drain exchange began, so this is no proof of a budget violation, and the exact budget is decided by TestDrainExchangeSharesOneBudget", limit, started.Sub(limit))
		}
		if started.After(limit) {
			t.Fatalf("the worker's port accepted a connection dialled %v after the limit %v", started.Sub(limit), limit)
		}
		select {
		case <-l.done:
			t.Fatalf("the worker exited before its in-flight request finished: %v: %s", l.runErr, l.stderr)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// awaitRefused is observeClose with no retry: a sample that decides nothing is recorded as the test's own
// infrastructure failure, and the case goes on, since the port was found closed and the checks that follow do not
// depend on when.
func awaitRefused(t *testing.T, ctx context.Context, public string, l *launched, limit time.Time, watchdog bool) {
	t.Helper()
	if reason := observeClose(t, ctx, public, l, limit, watchdog); reason != "" {
		t.Errorf("test infrastructure: %s; not a product failure", reason)
	}
}

// TestSupervisedWorkerDrainHandshake checks the worker's side of a drain answered by its supervisor. A drain sent to
// the worker directly makes it announce the drain once for its generation while it keeps accepting; a second drain
// joins the first; only the supervisor's matching answer ends admission, after which the worker closes its listener
// and finishes its in-flight work once, with no diagnostic about the exchange. The parent is a real workerlink
// reservation, so the port refuses connections only once both copies of the listener are closed. The loss of its
// parent before any drain stops it at once, with no notice.
func TestSupervisedWorkerDrainHandshake(t *testing.T) {
	tests := map[string]struct {
		parentLoss bool
	}{
		"success: a direct drain is announced once, answered, and ends with the in-flight work served": {},
		"success: parent loss stops the worker at once without a notice":                               {parentLoss: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHandoff(t)
			l := h.launch(t, ownedMode)
			port := l.awaitReady(t)
			h.closeTransfer()
			if port != strconv.Itoa(int(h.port)) {
				l.finish(t)
				t.Fatalf("readiness on port %q; want the inherited listener's port %d; status %v: %s\n%s", port, h.port, l.runErr, strings.Join(l.output, "\n"), l.stderr)
			}
			if data := h.reportBytes(t); len(data) == 0 {
				t.Fatal("the worker did not report its listener")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			public := "127.0.0.1:" + port
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			inflight := stalledUpload(t, ctx, public, release)

			if tc.parentLoss {
				lossAt := time.Now()
				if err := h.liveW.Close(); err != nil {
					t.Fatal(err)
				}
				msg, err := h.readDrain(t, 10*time.Second)
				if !errors.Is(err, io.EOF) {
					t.Errorf("drain channel after the parent's end of file: message %+v, error %v; want end of file with no notice", msg, err)
				}
				l.finish(t)
				// The exchange's budget is five seconds; a worker that waited for an answer would exit after it.
				if elapsed := time.Since(lossAt); elapsed >= 4*time.Second {
					t.Errorf("the worker took %v to stop after its parent's end of file; want an immediate stop", elapsed)
				}
				if l.runErr != nil {
					t.Errorf("worker status after its parent's end of file: %v: %s", l.runErr, l.stderr)
				}
				return
			}

			if code, active, err := postDrain(ctx, public, ownedToken); err != nil || code != http.StatusAccepted || active != 1 {
				t.Fatalf("direct drain: status %d, active %d, %v; want 202 with the in-flight request counted", code, active, err)
			}
			msg, err := h.readDrain(t, 5*time.Second)
			if err != nil {
				t.Fatalf("read the worker's drain notice: %v", err)
			}
			if diff := gocmp.Diff(workerlink.DrainMessage{Type: workerlink.Draining, Generation: ownedGeneration}, msg); diff != "" {
				t.Errorf("drain notice (-want +got):\n%s", diff)
			}
			// A second drain joins the first: it is accepted and announces nothing more.
			if code, _, err := postDrain(ctx, public, ownedToken); err != nil || code != http.StatusAccepted {
				t.Errorf("second direct drain: status %d, %v; want 202", code, err)
			}
			// Until the answer the worker keeps accepting.
			if status, err := get(ctx, h.port, "/healthz"); err != nil || status != http.StatusOK {
				t.Errorf("request after the notice, before the answer: %d, %v; want 200", status, err)
			}
			h.res.Retire()
			if err := h.res.AcknowledgeDrain(ctx); err != nil {
				t.Fatalf("answer the drain notice: %v", err)
			}
			awaitRefused(t, ctx, public, l, time.Now().Add(5*time.Second), false)
			select {
			case r := <-inflight:
				t.Fatalf("the in-flight request ended before its upload was released: %d, %v", r.status, r.err)
			default:
			}
			unblock()
			if r := <-inflight; r.err != nil || r.status != http.StatusBadRequest {
				t.Errorf("in-flight request: %d, %v; want its own answer, 400", r.status, r.err)
			}
			if !l.finish(t) || l.runErr != nil {
				t.Errorf("worker did not return cleanly after the drain: %v: %s", l.runErr, l.stderr)
			}
			// One notice in total: the second drain announced nothing.
			if msg, err := h.readDrain(t, 2*time.Second); !errors.Is(err, io.EOF) {
				t.Errorf("drain channel after the first notice: message %+v, error %v; want no second notice", msg, err)
			}
			if lines := drainDiagnostics(l.stderr.String()); len(lines) != 0 {
				t.Errorf("diagnostics about an answered drain: %q; want none", lines)
			}
		})
	}
}

// manualHandoff replaces h's listener and drain channel with ones the test holds itself: a listener whose parent copy
// the test closes when it chooses, and a drain channel whose parent end stays open, so that nothing but the worker
// ends the exchange. With fill set, the channel's direction from the worker to its parent is full before the worker
// starts, so the worker's notice cannot be written.
func manualHandoff(t *testing.T, h *handoff, fill bool) (ln net.Listener, parent net.Conn) {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	file, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	_ = h.files[listenerSlot].Close()
	h.files[listenerSlot] = file
	h.env["CODEX_GATEWAY_WORKER_PORT"] = strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	syscall.ForkLock.RLock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fds[0])
		unix.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "drain parent")
	parent, err = net.FileConn(parentFile)
	_ = parentFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Close() })
	if fill {
		if err := unix.SetNonblock(fds[1], true); err != nil {
			t.Fatal(err)
		}
		// Large writes first, then single bytes: the channel is full only when even one byte cannot be sent.
		for _, size := range []int{4096, 1} {
			junk := make([]byte, size)
			for {
				if _, err := unix.Write(fds[1], junk); err != nil {
					if !errors.Is(err, unix.EAGAIN) {
						t.Fatalf("fill the drain channel: %v", err)
					}
					break
				}
			}
		}
		if err := unix.SetNonblock(fds[1], false); err != nil {
			t.Fatal(err)
		}
	}
	_ = h.files[drainSlot].Close()
	h.files[drainSlot] = os.NewFile(uintptr(fds[1]), "drain child")
	return ln, parent
}

// TestSupervisedWorkerDrainWithoutAnswer checks that a worker whose drain notice gets no valid answer still ends
// admission within the exchange's budget of five seconds and writes exactly one bounded diagnostic about it. The
// drain channel's parent end stays open throughout, so nothing but the budget ends the wait: silence, an answer for
// another generation, a draining line in place of the answer, and a notice whose write cannot complete because the
// channel is full. The parent closes its own copy of the listener once the drain began, so the port refuses as soon
// as the worker closed its copy, while an in-flight request keeps the worker alive; that request then finishes once.
// That the write of the notice and the wait for the answer share one budget is decided in process, by
// TestDrainExchangeSharesOneBudget.
func TestSupervisedWorkerDrainWithoutAnswer(t *testing.T) {
	const budget = 5 * time.Second
	// readWatchdog bounds the test's own reads of the drain channel; it is a hang guard, not the budget under test.
	const readWatchdog = 10 * time.Second
	// closureMargin is the time the listener may stay open past the budget, counted from the moment the test received
	// the worker's notice or sent its answer: the exchange began before either, so a worker within its budget closes
	// before that bound.
	const closureMargin = 3 * time.Second
	// Causes of each branch, as descriptions of a failure one of which the diagnostic contains. A wrong answer may be
	// refused at once or ignored until the budget ends, so its cases also accept the descriptions of a timeout. The
	// parent's end stays open throughout, so no branch here can end in an end of file; a failed write can still be
	// reported with the system's own error text.
	var (
		timedOut     = []string{"timed out", "timeout", "deadline exceeded", "no answer", "unanswered", "not answered", "without an answer"}
		otherGen     = slices.Concat([]string{"wrong generation", "another generation", "other generation", "generation mismatch", "unexpected generation"}, timedOut)
		notAnAnswer  = slices.Concat([]string{"unexpected", "not an answer", "invalid", "wrong type", "unknown type"}, timedOut)
		writeBlocked = []string{
			"deadline exceeded", "timeout", "timed out", "could not write", "cannot write", "can't write",
			"write failed", "failed to write", "not written", "could not send", "cannot send", "failed to send", "not sent",
			"broken pipe", "connection reset",
		}
	)
	tests := map[string]struct {
		// answer is the line the parent writes after the notice; "" writes none.
		answer string
		// blocked fills the channel before the worker starts; the parent then reads nothing.
		blocked bool
		// causes are the words of the diagnostic's cause for the branch the case exercises.
		causes []string
	}{
		"error: without an answer the worker closes its listener within the budget and logs one line": {causes: timedOut},
		"error: an answer for another generation is no answer": {
			answer: `{"type":"retired","generation":"another-generation"}` + "\n",
			causes: otherGen,
		},
		"error: a draining line is no answer": {
			answer: `{"type":"draining","generation":"` + ownedGeneration + `"}` + "\n",
			causes: notAnAnswer,
		},
		"error: a notice that cannot be written ends within the same budget": {blocked: true, causes: writeBlocked},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHandoff(t)
			parentLn, parent := manualHandoff(t, h, tc.blocked)
			l := h.launch(t, ownedMode)
			port := l.awaitReady(t)
			h.closeTransfer()
			want := strconv.Itoa(parentLn.Addr().(*net.TCPAddr).Port)
			if port != want {
				l.finish(t)
				t.Fatalf("readiness on port %q; want the inherited listener's port %s; status %v: %s\n%s", port, want, l.runErr, strings.Join(l.output, "\n"), l.stderr)
			}
			if data := h.reportBytes(t); len(data) == 0 {
				t.Fatal("the worker did not report its listener")
			}
			// Nothing the case does with the worker outlives the child.
			ctx, cancel := context.WithDeadline(t.Context(), l.startedAt.Add(fixtureLifetime))
			defer cancel()
			public := "127.0.0.1:" + port
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			inflight := stalledUpload(t, ctx, public, release)
			// A correct worker needs the budget and the margin from the drain's start, then the room for the checks after
			// the close.
			l.requireHeadroom(t, budget+closureMargin, "the setup before the drain request")
			if code, active, err := postDrain(ctx, public, ownedToken); err != nil || code != http.StatusAccepted || active != 1 {
				t.Fatalf("direct drain: status %d, active %d, %v; want 202 with the in-flight request counted", code, active, err)
			}
			// limit is when the listener must be closed. A never-read notice gives the test no moment of the exchange, so
			// its bound is only the child's remaining lifetime; the exact five seconds of that row is
			// TestDrainExchangeSharesOneBudget's matter. Otherwise it is the budget and its margin after the last moment
			// of the exchange the test saw.
			limit := l.lastClose()
			if !tc.blocked {
				if err := parent.SetReadDeadline(time.Now().Add(readWatchdog)); err != nil {
					t.Fatal(err)
				}
				msg, err := workerlink.DecodeDrain(bufio.NewReader(parent))
				if err != nil {
					t.Fatalf("read the worker's drain notice: %v", err)
				}
				limit = l.boundAfter(t, time.Now(), budget+closureMargin, "the notice's receipt")
				if diff := gocmp.Diff(workerlink.DrainMessage{Type: workerlink.Draining, Generation: ownedGeneration}, msg); diff != "" {
					t.Errorf("drain notice (-want +got):\n%s", diff)
				}
				if tc.answer != "" {
					if _, err := io.WriteString(parent, tc.answer); err != nil {
						// The worker had already given up and closed its end: the test did not deliver the answer, and
						// the worker's handling of it is not judged. The rest of the case still checks a worker that ended
						// its exchange without an answer.
						t.Errorf("test infrastructure: the wrong answer %q was never delivered: %v; not a product failure", tc.answer, err)
					} else {
						limit = l.boundAfter(t, time.Now(), budget+closureMargin, "the answer's write")
					}
				}
			}
			if err := parentLn.Close(); err != nil {
				t.Fatal(err)
			}
			// The margin is for a loaded runner with many worker processes in parallel; a worker without the budget never
			// closes its listener and still fails at the limit.
			// Only the never-read row's limit is a liveness cutoff: it has no observed moment of the exchange.
			awaitRefused(t, ctx, public, l, limit, tc.blocked)
			select {
			case r := <-inflight:
				t.Fatalf("the in-flight request ended before its upload was released: %d, %v", r.status, r.err)
			default:
			}
			unblock()
			if r := <-inflight; r.err != nil || r.status != http.StatusBadRequest {
				t.Errorf("in-flight request: %d, %v; want its own answer, 400", r.status, r.err)
			}
			if !l.finish(t) || l.runErr != nil {
				t.Errorf("worker did not return cleanly after the unanswered drain: %v: %s", l.runErr, l.stderr)
			}
			checkDrainDiagnostic(t, l.stderr.String(), tc.causes)
		})
	}
}

// dupFile returns a close-on-exec duplicate of f that the child inherits as a separate descriptor number. It reaches
// the descriptor through SyscallConn, because Fd would put the shared open file description into blocking mode.
func dupFile(t *testing.T, f *os.File) *os.File {
	t.Helper()
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
	return os.NewFile(uintptr(n), "duplicate "+f.Name())
}

// connectedTCP returns the file of one end of an established loopback TCP connection.
func connectedTCP(t *testing.T) *os.File {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	f, err := conn.(*net.TCPConn).File()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// tcpListenerFile returns the file of a loopback TCP listener at a port of its own.
func tcpListenerFile(t *testing.T) *os.File {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// rawSocketFile returns a new close-on-exec socket of the given family and type that is neither bound nor connected.
func rawSocketFile(t *testing.T, family, kind int, name string) *os.File {
	t.Helper()
	syscall.ForkLock.RLock()
	n, err := unix.Socket(family, kind, 0)
	if err == nil {
		unix.CloseOnExec(n)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(n), name)
}

// boundNotListening returns a TCP socket bound to 127.0.0.1:port that never listens.
func boundNotListening(t *testing.T, port uint16) *os.File {
	t.Helper()
	f := rawSocketFile(t, unix.AF_INET, unix.SOCK_STREAM, "bound tcp")
	if err := unix.Bind(int(f.Fd()), &unix.SockaddrInet4{Port: int(port), Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = f.Close()
		t.Fatalf("bind an unlistened socket to port %d: %v", port, err)
	}
	return f
}

// datagramPairEnd returns one end of a connected Unix datagram socket pair; the test keeps the other.
func datagramPairEnd(t *testing.T) *os.File {
	t.Helper()
	syscall.ForkLock.RLock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err == nil {
		unix.CloseOnExec(fds[0])
		unix.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	other := os.NewFile(uintptr(fds[1]), "datagram peer")
	t.Cleanup(func() { _ = other.Close() })
	return os.NewFile(uintptr(fds[0]), "datagram end")
}
