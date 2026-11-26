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

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveworker"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

type logSink func([]byte) (int, error)

func (f logSink) Write(p []byte) (int, error) { return f(p) }

func TestWorkerStartupBoundaries(t *testing.T) {
	tests := map[string]struct {
		upstream   string
		wantOrigin string
		forbidden  []string
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
