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

//go:build process

package readiness_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

// readyStatusScript stands in for claude-code-proxy: whatever it is asked, it records its arguments and
// then its working directory, one line each, in the file "ran" beside itself, prints a signed-in Codex
// account and exits 0, which every status check reads as ready. It uses shell built-ins only, because
// the fixture environment carries no PATH.
const readyStatusScript = `#!/bin/sh
{ printf '%s\n' "$*"; pwd -P; } > "${0%/*}/ran"
printf 'Signed in with a ChatGPT account\n'
exit 0
`

// readyStatusFixture is an executable proxy binary that reports ready, inside its own sandbox.
type readyStatusFixture struct {
	env    gatewaytest.Environment
	binary string
}

func newReadyStatusFixture(t *testing.T) readyStatusFixture {
	t.Helper()
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	binary := filepath.Join(env["HOME"], "claude-code-proxy")
	if err := os.WriteFile(binary, []byte(readyStatusScript), 0o700); err != nil {
		t.Fatalf("write the status fixture: %v", err)
	}
	return readyStatusFixture{env: env, binary: binary}
}

// statusRecord is what the ready status fixture recorded about its run.
type statusRecord struct {
	// Args is the fixture's arguments joined by spaces; it is empty for a run with no arguments.
	Args string
	// Dir is the fixture's working directory with symbolic links resolved.
	Dir string
}

// ran returns the fixture's record and whether the fixture ran, which the record's existence decides.
func (f readyStatusFixture) ran(t *testing.T) (statusRecord, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(f.binary), "ran"))
	if errors.Is(err, fs.ErrNotExist) {
		return statusRecord{}, false
	}
	if err != nil {
		t.Fatalf("read the status fixture's record: %v", err)
	}
	record, ok := strings.CutSuffix(string(data), "\n")
	args, dir, found := strings.Cut(record, "\n")
	if !ok || !found || strings.Contains(dir, "\n") {
		t.Fatalf("the status fixture's record %q is not an argument line and a directory line", data)
	}
	return statusRecord{Args: args, Dir: dir}, true
}

// checkRan reports a mismatch between the fixture's run and wantRan, then between the recorded
// arguments and wantArgs, and between the recorded working directory and the sandbox home.
func (f readyStatusFixture) checkRan(t *testing.T, wantRan bool, wantArgs string) {
	t.Helper()
	got, ran := f.ran(t)
	if ran != wantRan {
		t.Errorf("the ready status fixture ran = %t, want %t (record %+v)", ran, wantRan, got)
		return
	}
	if !ran {
		return
	}
	if diff := gocmp.Diff(wantArgs, got.Args); diff != "" {
		t.Errorf("arguments the ready status fixture ran with (-want +got):\n%s", diff)
	}
	// The fixture records its directory with symbolic links resolved, so the home is resolved too.
	home, err := filepath.EvalSymlinks(f.env["HOME"])
	if err != nil {
		t.Fatalf("resolve the sandbox home: %v", err)
	}
	if got.Dir != home {
		t.Errorf("the ready status fixture ran in %q, want the sandbox home %q", got.Dir, home)
	}
}

// command prepares binary with args for the gatewaytest fixture runner, which checks that binary lies
// inside the sandbox and starts it with the sandbox's environment only. The runner does not choose a
// working directory for such a command, so the command runs in the sandbox home.
func (f readyStatusFixture) command(ctx context.Context, binary string, args []string, stdout, stderr *bytes.Buffer) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = f.env["HOME"]
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd
}

// runStatus is a status runner that launches the requested binary through the fixture runner.
func (f readyStatusFixture) runStatus(t *testing.T) readiness.StatusRunner {
	return func(ctx context.Context, binary string, args []string) (int, error) {
		var stdout, stderr bytes.Buffer
		err := f.env.RunFixture(f.command(ctx, binary, args, &stdout, &stderr))
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return exit.ExitCode(), nil
		}
		if err != nil {
			t.Errorf("status fixture did not run: %v (stderr %q)", err, stderr.String())
			return 0, err
		}
		return 0, nil
	}
}

// runAuthStatus is a Codex auth status runner that launches the requested binary through the fixture
// runner and returns what it printed.
func (f readyStatusFixture) runAuthStatus(t *testing.T) func(context.Context, string, []string) ([]byte, []byte, error) {
	return func(ctx context.Context, binary string, args []string) ([]byte, []byte, error) {
		var stdout, stderr bytes.Buffer
		if err := f.env.RunFixture(f.command(ctx, binary, args, &stdout, &stderr)); err != nil {
			t.Errorf("auth status fixture did not run: %v (stderr %q)", err, stderr.String())
			return nil, nil, err
		}
		return stdout.Bytes(), stderr.Bytes(), nil
	}
}

// TestProviderAuthStateWithoutStatusRunner leaves the status runner unset while the proxy binary is
// an executable that reports ready. The source would start that command; without a runner configured,
// a provider whose state needs the command has no evidence, reports auth-unknown and leaves the
// command unstarted, while providers decided without the command keep their answers. With a runner
// supplied, the same fixture runs for exactly the providers that need it and reports ready.
func TestProviderAuthStateWithoutStatusRunner(t *testing.T) {
	tests := map[string]struct {
		provider string
		env      map[string]string
		runner   bool
		want     string
		// wantRan says the fixture must run; wantArgs is then the argument list it recorded.
		wantRan  bool
		wantArgs string
	}{
		"error: Kimi sign-in cannot be checked": {
			provider: "kimi",
			want:     "auth-unknown",
		},
		"error: Cursor sign-in cannot be checked": {
			provider: "cursor",
			want:     "auth-unknown",
		},
		"error: Muse sign-in cannot be checked": {
			provider: "muse",
			want:     "auth-unknown",
		},
		"error: Vertex with a project cannot be checked": {
			provider: "vertex",
			env:      map[string]string{"CCP_VERTEX_PROJECT": "runner-absent-project"},
			want:     "auth-unknown",
		},
		"error: Vertex without a project is decided before the command": {
			provider: "vertex",
			env:      map[string]string{},
			want:     "config-invalid",
		},
		"success: OpenCode key presence needs no command": {
			provider: "opencode",
			env:      map[string]string{"OPENCODE_API_KEY": "runner-absent-key"},
			want:     "ready",
		},
		"error: OpenCode without a key needs no command": {
			provider: "opencode",
			env:      map[string]string{},
			want:     "auth-missing",
		},
		"success: Kimi sign-in is checked by a supplied runner": {
			provider: "kimi",
			runner:   true,
			want:     "ready",
			wantRan:  true,
			wantArgs: "kimi auth status",
		},
		"success: Cursor sign-in is checked by a supplied runner": {
			provider: "cursor",
			runner:   true,
			want:     "ready",
			wantRan:  true,
			wantArgs: "cursor auth status",
		},
		"success: Muse sign-in is checked by a supplied runner": {
			provider: "muse",
			runner:   true,
			want:     "ready",
			wantRan:  true,
			wantArgs: "muse auth status",
		},
		"success: Vertex with a project is checked by a supplied runner": {
			provider: "vertex",
			env:      map[string]string{"CCP_VERTEX_PROJECT": "runner-present-project"},
			runner:   true,
			want:     "ready",
			wantRan:  true,
			wantArgs: "vertex auth status",
		},
		"error: Vertex without a project skips a supplied runner": {
			provider: "vertex",
			env:      map[string]string{},
			runner:   true,
			want:     "config-invalid",
		},
		"success: OpenCode key presence skips a supplied runner": {
			provider: "opencode",
			env:      map[string]string{"OPENCODE_API_KEY": "runner-present-key"},
			runner:   true,
			want:     "ready",
		},
		"error: OpenCode without a key skips a supplied runner": {
			provider: "opencode",
			env:      map[string]string{},
			runner:   true,
			want:     "auth-missing",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newReadyStatusFixture(t)
			opts := readiness.ProviderAuthOptions{
				Lookup:      lookupMap(tt.env),
				ConfigDir:   t.TempDir(),
				ProxyBinary: fixture.binary,
			}
			if tt.runner {
				opts.RunStatus = fixture.runStatus(t)
			}
			got := readiness.ProviderAuthState(t.Context(), tt.provider, opts)
			if got != tt.want {
				t.Errorf("ProviderAuthState(%q) with runner %t = %q, want %q", tt.provider, tt.runner, got, tt.want)
			}
			fixture.checkRan(t, tt.wantRan, tt.wantArgs)
		})
	}
}

// listingTransport answers /v1/models from memory with status and counts those requests; any other
// path is 404. Nothing is dialled.
type listingTransport struct {
	status int
	hits   *atomic.Int32
}

func (l listingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status := http.StatusNotFound
	if req.URL.Path == "/v1/models" {
		l.hits.Add(1)
		status = l.status
	}
	body := `{"data":[]}`
	return &http.Response{
		StatusCode:    status,
		Header:        http.Header{"Content-Type": {"application/json"}},
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(strings.NewReader(body)),
		Request:       req,
	}, nil
}

// TestProbeProxyAndAuthWithoutStatusRunner leaves the Codex auth status runner unset while the proxy
// binary is an executable that reports a signed-in account and /v1/models answers from memory: the
// shared check reports the binary and the listing, never reports Codex as signed in, and leaves the
// binary unstarted. With a runner supplied, the same fixture runs and Codex is reported signed in.
func TestProbeProxyAndAuthWithoutStatusRunner(t *testing.T) {
	tests := map[string]struct {
		status   int
		runner   bool
		want     readiness.ProxyAuthResult
		wantRan  bool
		wantArgs string
	}{
		"error: answering proxy without a runner is not signed in": {
			status: http.StatusOK,
			want:   readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: false},
		},
		"error: failing proxy without a runner is not signed in": {
			status: http.StatusBadGateway,
			want:   readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: false, CodexAuth: false},
		},
		"success: answering proxy with a supplied runner is signed in": {
			status:   http.StatusOK,
			runner:   true,
			want:     readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: true},
			wantRan:  true,
			wantArgs: "codex auth status",
		},
		"success: failing proxy with a supplied runner is still signed in": {
			status:   http.StatusBadGateway,
			runner:   true,
			want:     readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: false, CodexAuth: true},
			wantRan:  true,
			wantArgs: "codex auth status",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newReadyStatusFixture(t)
			var modelHits atomic.Int32
			probe := readiness.ProxyAuthProbe{
				ProxyBinary:    fixture.binary,
				ProxyModelsURL: "http://127.0.0.1:9/v1/models",
				Client:         &http.Client{Transport: listingTransport{status: tt.status, hits: &modelHits}},
			}
			if tt.runner {
				probe.RunAuthStatus = fixture.runAuthStatus(t)
			}
			got := readiness.ProbeProxyAndAuth(t.Context(), probe)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ProbeProxyAndAuth with runner %t mismatch (-want +got):\n%s", tt.runner, diff)
			}
			if n := modelHits.Load(); n != 1 {
				t.Errorf("test precondition: /v1/models answered %d request(s), want 1", n)
			}
			fixture.checkRan(t, tt.wantRan, tt.wantArgs)
		})
	}
}
