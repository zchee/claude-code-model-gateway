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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveworker"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{"worker": func(t testing.TB, args []string) {
		mode := args[0]
		// DispatchHelper has consumed its private directory at fd 3. Shift the inherited pipe ends into the worker's slots.
		if mode == "supervised" || mode == "missing" || mode == "no-generation" || mode == "stdin-live" {
			if err := unix.Dup2(4, 3); err != nil {
				t.Fatal(err)
			}
			if err := unix.Close(4); err != nil {
				t.Fatal(err)
			}
			if mode == "supervised" || mode == "no-generation" {
				if err := unix.Dup2(5, 4); err != nil {
					t.Fatal(err)
				}
				if err := unix.Close(5); err != nil {
					t.Fatal(err)
				}
			}
		}
		lookup := os.LookupEnv
		if mode == "unguarded" || mode == "node-fixture" {
			lookup = func(key string) (string, bool) {
				if key == "MODEL_GATEWAY_TEST_SANDBOX" || (key == "NODE_TEST_CONTEXT" && mode == "unguarded") {
					return "", false
				}
				return os.LookupEnv(key)
			}
		}
		opts := serveworker.Options{Lookup: lookup, Home: os.UserHomeDir, Cwd: os.Getwd, Version: "fixture-build", Stdout: os.Stdout, Stderr: os.Stderr}
		if err := serveworker.Run(t.Context(), opts); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintln(os.Stdout, "worker returned cleanly")
	}, authStatusHelper: authStatusOutput})
}

func pipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return r, w
}

func TestWorkerModes(t *testing.T) {
	const fixtureToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
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
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// The nested test name alone can exceed the Unix socket pathname limit.
			home, err := os.MkdirTemp("", "mgw-")
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
			token := fixtureToken
			tokenPath := filepath.Join(state, "control-token")
			if tt.mode == "bad-token" {
				token = "not-hex"
			}
			if tt.mode != "new-token" {
				if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			socketPath := filepath.Join(home, "configured.sock")
			env["CODEX_GATEWAY_SOCKET_PATH"] = socketPath
			if tt.mode == "plain-file" {
				if err := os.WriteFile(socketPath, []byte("untouched"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.mode == "fixed-port" {
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
				env["CODEX_GATEWAY_WORKER_PORT"] = strconv.Itoa(port)
			}
			cmd, err := env.HelperCommand(t, "worker", tt.mode)
			if err != nil {
				t.Fatal(err)
			}
			reportR, reportW := pipe(t)
			liveR, liveW := pipe(t)
			switch tt.mode {
			case "supervised", "missing", "no-generation", "stdin-live":
				env["MODEL_GATEWAY_CONTROL_FDS"] = "3,4"
				if tt.mode != "no-generation" {
					env["MODEL_GATEWAY_WORKER_GENERATION"] = "fixture-gen"
				}
				cmd.ExtraFiles = []*os.File{reportW}
				if tt.mode == "supervised" || tt.mode == "no-generation" {
					cmd.ExtraFiles = append(cmd.ExtraFiles, liveR)
				}
				if tt.mode == "stdin-live" {
					env["MODEL_GATEWAY_CONTROL_FDS"] = "3,0"
				}
			case "no-report", "stdin-report":
				// DispatchHelper closes fd 3; the inherited liveness reader stays at fd 4.
				cmd.ExtraFiles = []*os.File{liveR}
				env["MODEL_GATEWAY_CONTROL_FDS"] = "3,4"
				env["MODEL_GATEWAY_WORKER_GENERATION"] = "fixture-gen"
				if tt.mode == "stdin-report" {
					env["MODEL_GATEWAY_CONTROL_FDS"] = "0,4"
				}
			case "fixture", "unguarded", "node-fixture":
				if tt.mode == "node-fixture" {
					env["NODE_TEST_CONTEXT"] = "child-v8"
				}
				env["NODE_CHANNEL_FD"] = "4"
				cmd.ExtraFiles = []*os.File{liveR}
			case "malformed":
				env["MODEL_GATEWAY_WORKER_GENERATION"] = "fixture-gen"
				env["MODEL_GATEWAY_CONTROL_FDS"] = "not-a-descriptor"
			}
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			stdout, w := io.Pipe()
			cmd.Stdout = w
			done := make(chan struct{})
			var runErr error
			go func() { defer close(done); runErr = env.RunFixtureWithin(cmd, 10*time.Second); _ = w.Close() }()
			// HelperCommand uses t.Context(), which is canceled before cleanups run.
			t.Cleanup(func() { _ = stdout.Close(); _ = liveW.Close(); <-done })
			scanner := bufio.NewScanner(stdout)
			pattern := regexp.MustCompile(`^model-gateway shim listening on 127\.0\.0\.1:([0-9]+)(?:\s|$)`)
			port := ""
			var output []string
			for scanner.Scan() {
				line := scanner.Text()
				output = append(output, line)
				if m := pattern.FindStringSubmatch(line); len(m) > 0 {
					port = m[1]
					break
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatalf("read startup output: %v", err)
			}
			if tt.wantError != "" {
				if port != "" {
					t.Fatalf("served despite invalid startup configuration: %s", port)
				}
				<-done
				text := strings.ToLower(strings.Join(output, "\n") + diagnostics.String())
				specific := strings.Contains(text, tt.wantError) || (tt.wantError == "descriptor" && (strings.Contains(text, "control_fds") || strings.Contains(text, "bad file")))
				exit, ok := errors.AsType[*exec.ExitError](runErr)
				if !ok || exit.ExitCode() != 1 || !specific {
					t.Fatalf("want %s startup failure (exit 1), got %v: %s", tt.wantError, runErr, text)
				}
				return
			}
			if port == "" {
				<-done
				t.Fatalf("no listening line: %v; %s\n%s", runErr, strings.Join(output, "\n"), &diagnostics)
			}
			n, err := strconv.Atoi(port)
			if err != nil || n <= 0 || n > 65535 {
				t.Fatalf("invalid listening port %q", port)
			}
			if tt.mode == "fixed-port" && port != env["CODEX_GATEWAY_WORKER_PORT"] {
				t.Fatalf("worker ignored configured port: got %s want %s", port, env["CODEX_GATEWAY_WORKER_PORT"])
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
				if err := reportW.Close(); err != nil {
					t.Fatal(err)
				}
				if err := reportR.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(io.LimitReader(reportR, 257))
				if err != nil {
					t.Fatalf("report was not closed: %v", err)
				}
				if len(data) > 256 || bytes.Count(data, []byte("\n")) != 1 || !bytes.HasSuffix(data, []byte("\n")) {
					t.Fatalf("bad report framing: %q", data)
				}
				var got map[string]any
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				want := map[string]any{"type": "listening", "port": float64(n), "generation": "fixture-gen"}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Fatal(diff)
				}
			}
			if tt.mode == "unguarded" {
				if err := liveR.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := liveW.Write([]byte("ignored")); !errors.Is(err, unix.EPIPE) {
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
				if err := liveW.Close(); err != nil {
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
			returned := false
			for scanner.Scan() {
				if scanner.Text() == "worker returned cleanly" {
					returned = true
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatalf("read shutdown output: %v", err)
			}
			<-done
			if tt.mode == "plain-file" {
				got, err := os.ReadFile(socketPath)
				if err != nil || string(got) != "untouched" {
					t.Fatalf("worker replaced the occupied socket path: %q, %v", got, err)
				}
				if !strings.Contains(diagnostics.String(), socketPath) {
					t.Errorf("no bind diagnostic for occupied socket path: %s", &diagnostics)
				}
			}
			if runErr != nil || !returned {
				t.Fatalf("clean return=%t, child status=%v: %s", returned, runErr, &diagnostics)
			}
		})
	}
}
