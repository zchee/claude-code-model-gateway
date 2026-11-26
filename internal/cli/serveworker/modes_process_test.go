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
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
		if mode == "supervised" || mode == "missing" {
			if err := unix.Dup2(4, 3); err != nil {
				t.Fatal(err)
			}
			if err := unix.Close(4); err != nil {
				t.Fatal(err)
			}
			if mode == "supervised" {
				if err := unix.Dup2(5, 4); err != nil {
					t.Fatal(err)
				}
				if err := unix.Close(5); err != nil {
					t.Fatal(err)
				}
			}
		}
		lookup := os.LookupEnv
		if mode == "unguarded" {
			lookup = func(key string) (string, bool) {
				if key == "MODEL_GATEWAY_TEST_SANDBOX" || key == "NODE_TEST_CONTEXT" {
					return "", false
				}
				return os.LookupEnv(key)
			}
		}
		if err := serveworker.Run(t.Context(), lookup, os.Stdout, os.Stderr); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintln(os.Stdout, "worker returned cleanly")
	}})
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
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tests := map[string]struct{ mode string }{"success: standalone ignores closed stdin": {"standalone"}, "success: supervised reports port then follows parent EOF": {"supervised"}, "success: fixture EOF stops standalone worker": {"fixture"}, "success: outside fixture guard the channel closes unread": {"unguarded"}, "error: missing liveness descriptor": {"missing"}, "error: malformed descriptor setting": {"malformed"}}
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
			if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(token+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd, err := env.HelperCommand(t, "worker", tt.mode)
			if err != nil {
				t.Fatal(err)
			}
			reportR, reportW := pipe(t)
			liveR, liveW := pipe(t)
			switch tt.mode {
			case "supervised", "missing":
				env["MODEL_GATEWAY_CONTROL_FDS"] = "3,4"
				env["MODEL_GATEWAY_WORKER_GENERATION"] = "fixture-gen"
				cmd.ExtraFiles = []*os.File{reportW}
				if tt.mode == "supervised" {
					cmd.ExtraFiles = append(cmd.ExtraFiles, liveR)
				}
			case "fixture", "unguarded":
				env["NODE_CHANNEL_FD"] = "4"
				cmd.ExtraFiles = []*os.File{liveR}
			case "malformed":
				env["MODEL_GATEWAY_CONTROL_FDS"] = "not-a-descriptor"
			}
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			stdout, w := io.Pipe()
			cmd.Stdout = w
			done := make(chan struct{})
			var runErr error
			go func() { defer close(done); runErr = env.RunFixtureWithin(cmd, 10*time.Second); _ = w.Close() }()
			defer func() { _ = stdout.Close(); _ = liveW.Close(); <-done }()
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
			if tt.mode == "missing" || tt.mode == "malformed" {
				if port != "" {
					t.Fatalf("served despite invalid descriptors: %s", port)
				}
				<-done
				text := strings.ToLower(strings.Join(output, "\n") + diagnostics.String())
				if runErr == nil || (!strings.Contains(text, "descriptor") && !strings.Contains(text, "control_fds") && !strings.Contains(text, "bad file")) {
					t.Fatalf("want descriptor-specific startup failure, got %v: %s", runErr, text)
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
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var health struct {
				OK bool `json:"ok"`
			}
			err = json.UnmarshalRead(resp.Body, &health)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || !health.OK {
				t.Fatalf("health status=%d ok=%t err=%v", resp.StatusCode, health.OK, err)
			}
			if tt.mode == "supervised" || tt.mode == "fixture" {
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
			if runErr != nil || !returned {
				t.Fatalf("clean return=%t, child status=%v: %s", returned, runErr, &diagnostics)
			}
		})
	}
}
