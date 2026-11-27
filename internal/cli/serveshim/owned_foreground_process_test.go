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
	"crypto/sha256"
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
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// ownedRefusedFile is written into the sandbox by an owned fixture worker that did not receive its listener and
// drain channel from the supervisor; it holds the reason.
const ownedRefusedFile = "worker-refused"

// inheritedDescriptor parses one descriptor number a supervisor announced in the environment.
func inheritedDescriptor(name string) (int, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return 0, fmt.Errorf("the supervisor did not set %s", name)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 2 || strconv.Itoa(n) != raw {
		return 0, fmt.Errorf("%s=%q is not a descriptor number", name, raw)
	}
	return n, nil
}

// ownedFixtureWorker is the worker the real supervisor execs as this test executable when ownedWorkerKey is set. It
// supplies only the worker side of the descriptor protocol: it adopts the listener and the drain channel the
// supervisor announced, reports the listener's port, serves health on it, and stops at the liveness pipe's end of
// file. Every descriptor number it uses comes from the environment. A worker that was not given its listener and drain
// channel writes the reason into ownedRefusedFile and waits for its supervisor to go away, so that the supervisor does
// not start it again and again.
func ownedFixtureWorker() error {
	sandbox := os.Getenv("MODEL_GATEWAY_TEST_SANDBOX")
	gen := os.Getenv("MODEL_GATEWAY_WORKER_GENERATION")
	if _, leaked := os.LookupEnv("NODE_CHANNEL_FD"); leaked || gen == "" || os.Getenv("MODEL_GATEWAY_CONTROL_FDS") != "3,4" {
		return errors.New("invalid worker descriptor environment")
	}
	report, live := os.NewFile(3, "port report"), os.NewFile(4, "parent liveness")
	if report == nil || live == nil {
		return errors.New("missing worker descriptors")
	}
	defer func() { _ = live.Close() }()
	refuse := func(reason error) error {
		if err := os.WriteFile(filepath.Join(sandbox, ownedRefusedFile), []byte(reason.Error()), 0o600); err != nil {
			return errors.Join(reason, err)
		}
		_, _ = io.Copy(io.Discard, live)
		return nil
	}
	listenerFD, err := inheritedDescriptor(workerlink.ListenerFDEnv)
	if err != nil {
		return refuse(err)
	}
	drainFD, err := inheritedDescriptor(workerlink.DrainFDEnv)
	if err != nil {
		return refuse(err)
	}
	expected, err := strconv.Atoi(os.Getenv("CODEX_GATEWAY_WORKER_PORT"))
	if err != nil || expected <= 0 || expected > 65535 {
		return refuse(fmt.Errorf("the supervisor published CODEX_GATEWAY_WORKER_PORT=%q, not the reserved port", os.Getenv("CODEX_GATEWAY_WORKER_PORT")))
	}
	ln, err := workerlink.ImportListener(os.NewFile(uintptr(listenerFD), "inherited listener"), uint16(expected))
	if err != nil {
		return refuse(fmt.Errorf("import the inherited listener: %w", err))
	}
	defer func() { _ = ln.Close() }()
	drain, err := workerlink.ImportDrainChannel(os.NewFile(uintptr(drainFD), "inherited drain channel"))
	if err != nil {
		return refuse(fmt.Errorf("import the inherited drain channel: %w", err))
	}
	defer func() { _ = drain.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	line, err := json.Marshal(map[string]any{"type": "listening", "port": port, "generation": gen})
	if err != nil {
		return err
	}
	if _, err = report.Write(append(line, '\n')); err != nil {
		return err
	}
	if err = report.Close(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(os.Stdout, "model-gateway shim listening on 127.0.0.1:%d\n", port); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stderr, workerStderr); err != nil {
		return err
	}
	srv := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"pid": os.Getpid(), "workerField": "preserved", "port": port})
	})}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	var b [1]byte
	n, readErr := live.Read(b[:])
	closeErr := srv.Close()
	serveErr := <-done
	if n != 0 || !errors.Is(readErr, io.EOF) || closeErr != nil || !errors.Is(serveErr, http.ErrServerClosed) {
		return errors.Join(fmt.Errorf("worker shutdown without clean liveness EOF (bytes read: %d)", n), readErr, closeErr, serveErr)
	}
	return os.WriteFile(filepath.Join(sandbox, "worker-eof"), []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// ownedRefusal fails the test when the fixture worker recorded that it did not get its listener and drain channel.
func ownedRefusal(t *testing.T, home string) {
	t.Helper()
	if reason, err := os.ReadFile(filepath.Join(home, ownedRefusedFile)); err == nil {
		t.Fatalf("the worker did not receive its listener from the supervisor: %s", reason)
	}
}

// TestForegroundSupervisor carries every assertion of the foreground supervisor test with the worker's
// listener received from the supervisor: one readiness line, an independent control token per state root, the
// guardian's and the worker's records and their modes, raw Host results, the health rewrite, stdout and log
// separation, a clean liveness end of file, and the lifecycle sequence and its fields. Only the worker listener's
// source changes: the worker serves on the listener it inherited, so its port is the supervisor's reservation, which
// no other program can bind while the worker runs.
func TestForegroundSupervisor(t *testing.T) {
	tokens := make(map[[sha256.Size]byte]bool)
	tests := map[string]struct {
		signal os.Signal
		event  string
	}{"success: terminate": {syscall.SIGTERM, "SIGTERM"}, "success: interrupt": {os.Interrupt, "SIGINT"}, "success: fixture channel": {nil, "fixture-shutdown"}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(home, "state")
			env := gatewaytest.NewEnv(home, nil, nil, map[string]string{"MODEL_GATEWAY_STATE_DIR": state, "NODE_CHANNEL_FD": "4", "MODEL_GATEWAY_STARTUP_GENERATION": "fixture-generation", ownedWorkerKey: "1"})
			cmd, err := env.HelperCommand(t, "supervisor")
			if err != nil {
				t.Fatal(err)
			}
			ctl, send, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ctl.Close() }()
			cmd.ExtraFiles = []*os.File{ctl} // The helper launcher inserts its checked directory before this descriptor.
			pr, pw := io.Pipe()
			var stdout, stderr bytes.Buffer
			cmd.Stdout = io.MultiWriter(&stdout, pw)
			cmd.Stderr = &stderr
			ready := make(chan int, 1)
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				defer func() { _ = pr.Close() }()
				s := bufio.NewScanner(pr)
				for s.Scan() {
					if v, ok := strings.CutPrefix(s.Text(), "model-gateway shim supervisor listening on 127.0.0.1:"); ok {
						if n, e := strconv.Atoi(v); e == nil {
							select {
							case ready <- n:
							default:
							}
						}
					}
				}
				if err := s.Err(); err != nil {
					t.Errorf("read supervisor stdout: %v", err)
				}
			}()
			finished := make(chan struct{})
			var result error
			go func() { result = env.RunFixtureWithin(cmd, 20*time.Second); _ = pw.Close(); close(finished) }()
			defer func() {
				select {
				case <-finished:
				default:
					_, _ = io.WriteString(send, fixtureStop)
				}
				_ = send.Close()
				<-finished
				<-drained
			}()
			var port int
			select {
			case port = <-ready:
			case <-finished:
				t.Fatalf("no readiness announcement: %v; stdout=%s stderr=%s", result, &stdout, &stderr)
			}
			if port <= 0 || port > 65535 || port == 80 || port == 18764 || port == 18765 {
				t.Fatalf("unexpected public port %d", port)
			}
			guardian := cmd.Process.Pid
			ownedCheckRecord(t, state, "guardian", guardian, port)
			// Both the socket and the token are owned by this fixture, never the user's gateway.
			headers, err := controlauth.RequestHeaders(filepath.Join(state, "control-token"))
			if err != nil || headers.Get("X-Model-Gateway-Control-Token") == "" {
				t.Fatalf("fixture did not publish a valid control token: %v", err)
			}
			digest := sha256.Sum256([]byte(headers.Get("X-Model-Gateway-Control-Token")))
			if tokens[digest] {
				t.Fatal("distinct state roots reused the same startup token")
			}
			tokens[digest] = true
			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+"/healthz?probe=1", nil)
			if err != nil {
				t.Fatal(err)
			}
			transport := &http.Transport{DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			var health map[string]any
			for {
				ownedRefusal(t, home)
				resp, err := transport.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode == http.StatusOK {
					err = json.UnmarshalRead(resp.Body, &health)
				}
				_ = resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode == http.StatusOK {
					break
				}
				select {
				case <-ctx.Done():
					ownedRefusal(t, home)
					t.Fatalf("health never ready: last status %d", resp.StatusCode)
				case <-finished:
					t.Fatalf("supervisor exited: %v", result)
				case <-time.After(10 * time.Millisecond):
				}
			}
			child, ok := health["pid"].(float64)
			if !ok || child <= 0 || int(child) == guardian {
				t.Fatal("missing or invalid worker PID in health response")
			}
			delete(health, "pid")
			childPort, ok := health["port"].(float64)
			if !ok || childPort < 1 || childPort > 65535 || int(childPort) == port {
				t.Fatal("worker health has no separate listener port")
			}
			delete(health, "port")
			if diff := gocmp.Diff(map[string]any{"supervisorPid": float64(guardian), "supervisorVersion": fixtureVersion, "proxyRecovery": true, "workerField": "preserved"}, health); diff != "" {
				t.Fatalf("health (-want +got): %s", diff)
			}
			// The worker's port is held by the supervisor's reservation and the worker's copy of it.
			var cfg net.ListenConfig
			if ln, err := cfg.Listen(t.Context(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(childPort)))); err == nil {
				_ = ln.Close()
				t.Errorf("another listener bound the worker's port %d while the worker served", int(childPort))
			}
			// A worker becomes eligible only after its eligible record is published, so the record is complete by the time
			// the first relayed answer arrives. Read both entries without ReadRecord: its mirror repair would mask a
			// missing write.
			mirror, mirrorErr := os.ReadFile(filepath.Join(state, "shim.pid"))
			raw, recordErr := os.ReadFile(filepath.Join(state, "shim.pid.json"))
			var record procinfo.Record
			if recordErr == nil {
				recordErr = json.Unmarshal(raw, &record)
			}
			if mirrorErr != nil || recordErr != nil || string(mirror) != strconv.Itoa(int(child)) || record.PID != int(child) || int(record.Port) != int(childPort) {
				t.Fatalf("worker record when its first relayed answer arrived: mirror %q (%v), record PID %d port %d (%v); want PID %.0f port %.0f", mirror, mirrorErr, record.PID, record.Port, recordErr, child, childPort)
			}
			ownedCheckRecord(t, state, "shim", int(child), int(childPort))
			ownedCheckHostAsSent(t, port)
			if tc.signal != nil {
				err = cmd.Process.Signal(tc.signal)
			} else {
				_, err = io.WriteString(send, fixtureStop)
			}
			if err != nil {
				t.Fatal(err)
			}
			<-finished
			if result != nil {
				t.Fatalf("stop: %v; stdout=%s stderr=%s", result, &stdout, &stderr)
			}
			workerLog, err := os.ReadFile(filepath.Join(state, "logs", "shim.log"))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range []string{"model-gateway shim listening on 127.0.0.1:" + strconv.Itoa(int(childPort)), workerStderr} {
				if !slices.Contains(strings.Split(string(workerLog), "\n"), line) {
					t.Errorf("shim.log missing %q", line)
				}
				if strings.Contains(stdout.String(), line) || strings.Contains(stderr.String(), line) {
					t.Errorf("supervisor streams include worker output %q", line)
				}
			}
			eof, err := os.ReadFile(filepath.Join(home, "worker-eof"))
			if err != nil || string(eof) != strconv.Itoa(int(child)) {
				t.Fatalf("worker did not see liveness EOF: %q, %v", eof, err)
			}
			ownedCheckLifecycle(t, state, guardian, int(child), tc.event)
		})
	}
}

// ownedCheckHostAsSent writes raw HTTP/1.1 to the supervisor's public port. net/http drops the Host header of an
// absolute-form request before any handler runs, so only a listener path that keeps the header as sent refuses these.
// The fixture worker answers 404 off /healthz and 200 on it, so a request that slips past the gate is visible.
func ownedCheckHostAsSent(t *testing.T, port int) {
	t.Helper()
	public := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	type result struct {
		Status    int
		EmptyBody bool
	}
	refused := result{Status: http.StatusForbidden, EmptyBody: true}
	tests := map[string]struct {
		target string
		header []string
		want   result
	}{
		"error: absolute-form relay path with foreign Host": {"http://" + public + "/sample", []string{"Host: outside.invalid"}, refused},
		"error: absolute-form health with foreign Host":     {"http://" + public + "/healthz", []string{"Host: outside.invalid"}, refused},
		"error: Origin lines in mixed case on health":       {"/healthz", []string{"Host: " + public, "oRiGiN: ", "ORIGIN: null"}, refused},
		"success: origin-form health with own Host":         {"/healthz", []string{"Host: " + public}, result{Status: http.StatusOK}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var dialer net.Dialer
			conn, err := dialer.DialContext(t.Context(), "tcp4", public)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var head strings.Builder
			fmt.Fprintf(&head, "GET %s HTTP/1.1\r\n", tc.target)
			for _, line := range tc.header {
				head.WriteString(line)
				head.WriteString("\r\n")
			}
			head.WriteString("Connection: close\r\n\r\n")
			if _, err := io.WriteString(conn, head.String()); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("read response to %q: %v", head.String(), err)
			}
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tc.want, result{Status: resp.StatusCode, EmptyBody: len(b) == 0}); diff != "" {
				t.Errorf("request %q (-want +got):\n%s", head.String(), diff)
			}
		})
	}
}

func ownedCheckRecord(t *testing.T, state, name string, pid, port int) {
	t.Helper()
	mirror, err := os.ReadFile(filepath.Join(state, name+".pid"))
	if err != nil || string(mirror) != strconv.Itoa(pid) {
		t.Fatalf("%s PID mirror: %q (%v)", name, mirror, err)
	}
	rec, err := procinfo.ReadRecord(state, name)
	if err != nil {
		t.Fatalf("%s record: %v", name, err)
	}
	if rec.PID != pid || int(rec.Port) != port || rec.Version != fixtureVersion || (name == "guardian" && rec.Generation != "fixture-generation") {
		t.Errorf("unexpected %s identity: %+v", name, rec)
	}
	for _, suffix := range []string{".pid", ".pid.json"} {
		info, err := os.Lstat(filepath.Join(state, name+suffix))
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() & ^os.FileMode(0o600) != 0 {
			t.Errorf("%s%s has mode %v; want private regular file", name, suffix, info.Mode())
		}
	}
}

func ownedCheckLifecycle(t *testing.T, state string, parent, child int, signal string) {
	t.Helper()
	files := lifecycle.Files{StateDir: state}
	var got []string
	want := []string{"supervisor-started", "worker-started", "supervisor-stop-requested", "worker-stop-requested", "worker-exit", "supervisor-exit"}
	for _, raw := range files.Records() {
		var rec struct {
			Event     string `json:"event"`
			Component string `json:"component"`
			Signal    string `json:"signal"`
			PID       int    `json:"pid"`
			Child     struct {
				Component string `json:"component"`
				PID       int    `json:"pid"`
			} `json:"child"`
			ExitCode *int `json:"exitCode"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(want, rec.Event) {
			continue
		}
		got = append(got, rec.Event)
		if rec.PID != parent || rec.Component != "supervisor" {
			t.Errorf("%s: wrong supervisor identity", rec.Event)
		}
		if strings.HasPrefix(rec.Event, "worker-") && (rec.Child.Component != "worker" || rec.Child.PID != child) {
			t.Errorf("%s: wrong child identity", rec.Event)
		}
		if strings.HasSuffix(rec.Event, "stop-requested") && rec.Signal != signal {
			t.Errorf("%s signal=%q, want %q", rec.Event, rec.Signal, signal)
		}
		if rec.Event == "supervisor-exit" && (rec.ExitCode == nil || *rec.ExitCode != 0) {
			t.Error("orderly supervisor exit lacks zero exit code")
		}
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("lifecycle sequence (-want +got): %s", diff)
	}
}
