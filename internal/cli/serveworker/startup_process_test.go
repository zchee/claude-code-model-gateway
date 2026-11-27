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

package serveworker_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveworker"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// invalidUpstreamError restates the error compose.New returns for an unusable AnthropicBaseURL
// (internal/worker/compose/compose.go, validate); the worker command returns it unwrapped before it listens.
const invalidUpstreamError = "compose: AnthropicBaseURL must be an absolute http or https URL"

type logSink func([]byte) (int, error)

func (f logSink) Write(p []byte) (int, error) { return f(p) }

func TestWorkerStartupBoundaries(t *testing.T) {
	tests := map[string]struct {
		upstream   string
		wantOrigin string
		forbidden  []string
		wantErr    string
		stateKind  string
	}{
		"success: readiness prints only http origin": {
			upstream:   "http://fixture-user:fixture-pass@127.0.0.1:9/fixture-path?fixture-query=yes",
			wantOrigin: "http://127.0.0.1:9",
			forbidden:  []string{"fixture-user", "fixture-pass", "fixture-path", "fixture-query"},
		},
		"success: readiness prints only https origin with port": {
			upstream:   "https://alice:secret-pass@upstream.invalid:8443/v1/custom?api_key=tok#frag",
			wantOrigin: "https://upstream.invalid:8443",
			forbidden:  []string{"alice", "secret-pass", "/v1/custom", "api_key", "tok", "frag"},
		},
		// The worker refuses to start with an upstream that is not an absolute http or https URL: a value url.Parse
		// refuses; an http value it parses with an empty host (the rest is opaque); a scheme-relative one it parses
		// with a host and no scheme; and one with a host and a scheme other than http or https. Each carries its own
		// user, password, host, port, path, query key and query value, and none of them may appear on either stream.
		// The port 28417 lies below the default ephemeral port ranges of Linux and macOS and appears nowhere else.
		// An http value also forbids its scheme prefix with the next character of the value: "http" alone is in the
		// error text, and "http://1" is the start of the fixture's own loopback endpoints.
		"error: worker refuses an upstream that does not parse and never repeats it": {
			upstream:  "http://fixture-user:fixture-pass@[fixture-host:28417/fixture-path?fixture-query=fixture-value",
			wantErr:   invalidUpstreamError,
			forbidden: []string{"http://f", "fixture-user", "fixture-pass", "fixture-host", "28417", "fixture-path", "fixture-query", "fixture-value"},
		},
		"error: worker refuses an http upstream with an empty host and never repeats it": {
			upstream:  "http:fixture-user:fixture-pass@fixture-host:28417/fixture-path?fixture-query=fixture-value",
			wantErr:   invalidUpstreamError,
			forbidden: []string{"http:f", "fixture-user", "fixture-pass", "fixture-host", "28417", "fixture-path", "fixture-query", "fixture-value"},
		},
		"error: worker refuses an upstream with a host and no scheme and never repeats it": {
			upstream:  "//fixture-user:fixture-pass@fixture-host:28417/fixture-path?fixture-query=fixture-value",
			wantErr:   invalidUpstreamError,
			forbidden: []string{"fixture-user", "fixture-pass", "fixture-host", "28417", "fixture-path", "fixture-query", "fixture-value"},
		},
		"error: worker refuses an upstream with a non-http scheme and never repeats it": {
			upstream:  "ftp://fixture-user:fixture-pass@fixture-host:28417/fixture-path?fixture-query=fixture-value",
			wantErr:   invalidUpstreamError,
			forbidden: []string{"ftp:", "fixture-user", "fixture-pass", "fixture-host", "28417", "fixture-path", "fixture-query", "fixture-value"},
		},
		"error: unclean dot state root":       {stateKind: "dot"},
		"error: symlink traversal state root": {stateKind: "symlink"},
		"error: trailing slash state root":    {stateKind: "trailing"},
		"error: relative state root":          {stateKind: "relative"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(home)
			env := gatewaytest.NewEnv(home, nil, nil, nil)
			if tt.upstream != "" {
				env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = tt.upstream
			}
			var forbiddenPaths []string
			switch tt.stateKind {
			case "dot":
				env["MODEL_GATEWAY_STATE_DIR"] = home + "/./state"
				forbiddenPaths = []string{filepath.Join(home, "state")}
			case "symlink":
				deep := filepath.Join(home, "sub", "deep")
				if err := os.MkdirAll(deep, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(deep, filepath.Join(home, "link")); err != nil {
					t.Fatal(err)
				}
				env["MODEL_GATEWAY_STATE_DIR"] = home + "/link/../escaped"
				forbiddenPaths = []string{filepath.Join(home, "escaped"), filepath.Join(home, "sub", "escaped")}
			case "trailing":
				env["MODEL_GATEWAY_STATE_DIR"] = home + "/state/"
				forbiddenPaths = []string{filepath.Join(home, "state")}
			case "relative":
				env["MODEL_GATEWAY_STATE_DIR"] = "state"
				forbiddenPaths = []string{filepath.Join(home, "state")}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			ready := false
			err = serveworker.Run(ctx, serveworker.Options{
				Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
				Home:   func() (string, error) { return home, nil },
				Cwd:    os.Getwd,
				Stdout: logSink(func(p []byte) (int, error) {
					n, err := stdout.Write(p)
					if strings.Contains(stdout.String(), "shim listening on 127.0.0.1:") {
						ready = true
						cancel()
					}
					return n, err
				}),
				Stderr: &stderr,
			})
			if tt.stateKind != "" {
				if err == nil || ready {
					t.Errorf("invalid state root: rejected=%t, ready=%t", err != nil, ready)
				}
				for _, p := range forbiddenPaths {
					if _, statErr := os.Lstat(p); !os.IsNotExist(statErr) {
						t.Errorf("invalid state root produced filesystem state at %s: stat error=%v", p, statErr)
					}
				}
				return
			}
			if tt.wantErr != "" {
				gotErr := ""
				if err != nil {
					gotErr = err.Error()
				}
				if diff := gocmp.Diff(tt.wantErr, gotErr); diff != "" {
					t.Errorf("Run error for an invalid upstream (-want +got):\n%s", diff)
				}
				if ready {
					t.Error("worker announced readiness with an invalid upstream")
				}
				if stdout.Len() != 0 {
					t.Errorf("worker wrote %d bytes to stdout before refusing an invalid upstream, want none", stdout.Len())
				}
				// The temporary home's path holds random digits, so paths under it are cut before the port is looked for.
				for stream, text := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
					text = strings.ReplaceAll(text, home, "")
					for _, marker := range tt.forbidden {
						if strings.Contains(text, marker) {
							t.Errorf("worker %s repeats %q of the refused upstream URL", stream, marker)
						}
					}
				}
				return
			}
			if err != nil || !ready {
				t.Fatalf("worker failed to announce readiness: err=%v", err)
			}
			if !strings.Contains(stdout.String(), tt.wantOrigin) {
				t.Errorf("readiness did not include sanitized upstream origin %q", tt.wantOrigin)
			}
			for _, marker := range tt.forbidden {
				if strings.Contains(stdout.String(), marker) || strings.Contains(stderr.String(), marker) {
					t.Error("worker output contains a non-origin URL component")
					break
				}
			}
		})
	}
}
