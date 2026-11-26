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
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveshim"
	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

const fixtureVersion = "1.2.3"

func TestMain(m *testing.M) {
	venue.Require()
	if slices.Equal(os.Args[1:], []string{"serve-worker"}) && os.Getenv("MODEL_GATEWAY_TEST_SANDBOX") != "" {
		if err := fixtureWorker(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	goleak.VerifyTestMain(m)
}

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{"supervisor": func(t testing.TB, _ []string) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := serveshim.Run(t.Context(), os.Environ(), exe, fixtureVersion, os.Stdout, os.Stderr); err != nil {
			t.Fatal(err)
		}
	}})
}

// The real supervisor execs this test executable with its normal worker argv.
// Only the worker side of the descriptor protocol is supplied by the fixture.
func fixtureWorker() error {
	gen := os.Getenv("MODEL_GATEWAY_WORKER_GENERATION")
	if _, leaked := os.LookupEnv("NODE_CHANNEL_FD"); leaked || gen == "" || os.Getenv("MODEL_GATEWAY_CONTROL_FDS") != "3,4" {
		return errors.New("invalid worker descriptor environment")
	}
	report, live := os.NewFile(3, "port report"), os.NewFile(4, "parent liveness")
	if report == nil || live == nil {
		return errors.New("missing worker descriptors")
	}
	defer func() { _ = live.Close() }()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
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
	return os.WriteFile(filepath.Join(os.Getenv("MODEL_GATEWAY_TEST_SANDBOX"), "worker-eof"), []byte(strconv.Itoa(os.Getpid())), 0o600)
}

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
			env := gatewaytest.NewEnv(home, nil, nil, map[string]string{"MODEL_GATEWAY_STATE_DIR": state, "NODE_CHANNEL_FD": "4", "MODEL_GATEWAY_STARTUP_GENERATION": "fixture-generation"})
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
			defer func() { _ = send.Close(); <-finished; <-drained }()
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
			checkRecord(t, state, "guardian", guardian, port)
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
			for {
				if _, err := os.Stat(filepath.Join(state, "shim.pid.json")); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatal("worker record was not published")
				case <-time.After(10 * time.Millisecond):
				}
			}
			checkRecord(t, state, "shim", int(child), int(childPort))
			workerLog, err := os.ReadFile(filepath.Join(state, "logs", "shim.log"))
			if err != nil || !strings.Contains(string(workerLog), "model-gateway shim listening on 127.0.0.1:"+strconv.Itoa(int(childPort))+"\n") {
				t.Fatalf("worker announcement missing from shim.log: %v", err)
			}
			if tc.signal != nil {
				err = cmd.Process.Signal(tc.signal)
			} else {
				_, err = io.WriteString(send, "{\"type\":\"fixture-shutdown\"}\n")
			}
			if err != nil {
				t.Fatal(err)
			}
			<-finished
			if result != nil {
				t.Fatalf("stop: %v; stdout=%s stderr=%s", result, &stdout, &stderr)
			}
			eof, err := os.ReadFile(filepath.Join(home, "worker-eof"))
			if err != nil || string(eof) != strconv.Itoa(int(child)) {
				t.Fatalf("worker did not see liveness EOF: %q, %v", eof, err)
			}
			checkLifecycle(t, state, guardian, int(child), tc.event)
		})
	}
}

func checkRecord(t *testing.T, state, name string, pid, port int) {
	t.Helper()
	mirror, err := os.ReadFile(filepath.Join(state, name+".pid"))
	if err != nil || string(mirror) != strconv.Itoa(pid) {
		t.Fatalf("%s PID mirror: %q (%v)", name, mirror, err)
	}
	raw, err := os.ReadFile(filepath.Join(state, name+".pid.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		PID, Port                                                int
		StateRoot, Role, Generation, Version, Command, StartedAt string
		Image                                                    struct{ Path string }
	}
	if err = json.Unmarshal(raw, &rec, json.MatchCaseInsensitiveNames(true)); err != nil {
		t.Fatal(err)
	}
	if rec.PID != pid || rec.StateRoot != state || rec.Role == "" || rec.Generation == "" || rec.Version != fixtureVersion || rec.StartedAt == "" || rec.Command == "" || rec.Image.Path == "" || (port != 0 && rec.Port != port) {
		t.Fatalf("%s record missing process identity: %+v", name, rec)
	}
}

func checkLifecycle(t *testing.T, state string, parent, child int, signal string) {
	t.Helper()
	files := lifecycle.Files{StateDir: state}
	var got []string
	want := []string{"supervisor-started", "worker-started", "supervisor-stop-requested", "worker-stop-requested", "worker-exit", "supervisor-exit"}
	for _, raw := range files.Records() {
		var rec struct {
			Event, Component, Signal string
			PID                      int
			Child                    struct {
				Component string
				PID       int
			}
			ExitCode *int
		}
		if err := json.Unmarshal(raw, &rec, json.MatchCaseInsensitiveNames(true)); err != nil {
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
