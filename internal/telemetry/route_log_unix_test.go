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

//go:build unix

package telemetry

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

const routeLogDiagnostic = "model-gateway: could not write request route log: "

// checkRouteLogDiagnostics requires one diagnostic line per failed append, each naming only the
// error and never the log path, as the source's diagnostic names only the error code.
func checkRouteLogDiagnostics(t *testing.T, diagnostics, root string, failures int) {
	t.Helper()
	lines := strings.SplitAfter(diagnostics, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != failures {
		t.Errorf("got %d diagnostic lines, want one per failed append (%d): %q", len(lines), failures, diagnostics)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, routeLogDiagnostic) || !strings.HasSuffix(line, "\n") || len(line) == len(routeLogDiagnostic)+1 {
			t.Errorf("malformed diagnostic line: %q", line)
		}
	}
	for _, private := range []string{root, "request-routes", "sentinel", "header-session"} {
		if strings.Contains(diagnostics, private) {
			t.Errorf("diagnostic contains %q: %q", private, diagnostics)
		}
	}
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists after a refused append (err=%v)", path, err)
	}
}

func perm(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode=%o, want %o", path, got, want)
	}
}

func TestRouteLogPathSafety(t *testing.T) {
	tests := map[string]struct {
		// setup prepares the temporary root and returns the configured log path; the path is
		// spelled by concatenation because filepath.Join would clean it.
		setup func(t *testing.T, root string) string
		// written reports whether both appends must land in the file at the log path.
		written bool
		// prior holds the rows the file at the log path has before the appends.
		prior []map[string]any
		// code, when set, is the exact error name every diagnostic line of a refused append ends with.
		code  string
		check func(t *testing.T, root string)
	}{
		"success: missing parent directories are created private": {
			setup:   func(t *testing.T, root string) string { return root + "/state/logs/request-routes.jsonl" },
			written: true,
			check: func(t *testing.T, root string) {
				perm(t, root+"/state", 0o700)
				perm(t, root+"/state/logs", 0o700)
				perm(t, root+"/state/logs/request-routes.jsonl", 0o600)
			},
		},
		"success: existing regular file is appended to": {
			setup: func(t *testing.T, root string) string {
				if err := os.Mkdir(root+"/logs", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(root+"/logs/request-routes.jsonl", []byte(`{"prior":true}`+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return root + "/logs/request-routes.jsonl"
			},
			written: true,
			prior:   []map[string]any{{"prior": true}},
			check: func(t *testing.T, root string) {
				raw, err := os.ReadFile(root + "/logs/request-routes.jsonl")
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.HasPrefix(raw, []byte(`{"prior":true}`+"\n")) {
					t.Errorf("existing line was not kept: %q", raw)
				}
			},
		},
		"error: symbolic link followed by a parent reference is refused": {
			setup: func(t *testing.T, root string) string {
				if err := os.MkdirAll(root+"/real/sub", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root+"/real/sub", root+"/link"); err != nil {
					t.Fatal(err)
				}
				return root + "/link/../logs/request-routes.jsonl"
			},
			check: func(t *testing.T, root string) {
				// The lexical parent and the parent the kernel resolves are both left alone.
				absent(t, root+"/logs")
				absent(t, root+"/real/logs")
			},
		},
		"error: path with a dot component is refused": {
			setup: func(t *testing.T, root string) string { return root + "/logs/./request-routes.jsonl" },
			check: func(t *testing.T, root string) { absent(t, root+"/logs") },
		},
		"error: path with a doubled separator is refused": {
			setup: func(t *testing.T, root string) string { return root + "/logs//request-routes.jsonl" },
			check: func(t *testing.T, root string) { absent(t, root+"/logs") },
		},
		"error: relative path is refused": {
			setup: func(t *testing.T, root string) string {
				wd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				rel, err := filepath.Rel(wd, root+"/logs/request-routes.jsonl")
				if err != nil {
					t.Fatal(err)
				}
				return rel
			},
			check: func(t *testing.T, root string) { absent(t, root+"/logs") },
		},
		"error: final symbolic link to a regular file is not followed": {
			setup: func(t *testing.T, root string) string {
				if err := os.Mkdir(root+"/logs", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(root+"/target.jsonl", nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root+"/target.jsonl", root+"/logs/request-routes.jsonl"); err != nil {
					t.Fatal(err)
				}
				return root + "/logs/request-routes.jsonl"
			},
			check: func(t *testing.T, root string) {
				raw, err := os.ReadFile(root + "/target.jsonl")
				if err != nil {
					t.Fatal(err)
				}
				if len(raw) != 0 {
					t.Errorf("append followed the final symbolic link: %q", raw)
				}
				if info, err := os.Lstat(root + "/logs/request-routes.jsonl"); err != nil || info.Mode()&fs.ModeSymlink == 0 {
					t.Errorf("log path is no longer the symbolic link: %v %v", info, err)
				}
			},
		},
		"error: dangling final symbolic link is not followed": {
			setup: func(t *testing.T, root string) string {
				if err := os.Mkdir(root+"/logs", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root+"/missing.jsonl", root+"/logs/request-routes.jsonl"); err != nil {
					t.Fatal(err)
				}
				return root + "/logs/request-routes.jsonl"
			},
			check: func(t *testing.T, root string) { absent(t, root+"/missing.jsonl") },
		},
		"error: reserved temporary-file name at the log path is refused": {
			// The name has the spelling of an atomic replacement's staging file, which the
			// recorder's append refuses by name before opening anything; the source appends to it.
			setup: func(t *testing.T, root string) string {
				if err := os.Mkdir(root+"/logs", 0o700); err != nil {
					t.Fatal(err)
				}
				return root + "/logs/request-routes.jsonl.42.0123456789ab.tmp"
			},
			code:  "unknown error",
			check: func(t *testing.T, root string) { absent(t, root+"/logs/request-routes.jsonl.42.0123456789ab.tmp") },
		},
		"error: reserved temporary-file name in another letter case is refused": {
			setup: func(t *testing.T, root string) string {
				if err := os.Mkdir(root+"/logs", 0o700); err != nil {
					t.Fatal(err)
				}
				return root + "/logs/request-routes.jsonl.42.0123456789AB.TMP"
			},
			code:  "unknown error",
			check: func(t *testing.T, root string) { absent(t, root+"/logs/request-routes.jsonl.42.0123456789AB.TMP") },
		},
		"error: existing parent directory without read permission is refused": {
			// The parent grants write and search but not read. The recorder opens the final
			// directory for reading, so the append fails; the source appends by name and needs
			// only search permission there.
			setup: func(t *testing.T, root string) string {
				if os.Geteuid() == 0 {
					t.Fatal("the venue must not run as root to test a directory without read permission")
				}
				if err := os.Mkdir(root+"/logs", 0o300); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(root+"/logs", 0o700); err != nil {
						t.Error(err)
					}
				})
				if _, err := os.ReadDir(root + "/logs"); !errors.Is(err, fs.ErrPermission) {
					t.Fatalf("test precondition: the parent directory is readable: %v", err)
				}
				return root + "/logs/request-routes.jsonl"
			},
			code:  "EACCES",
			check: func(t *testing.T, root string) { absent(t, root+"/logs/request-routes.jsonl") },
		},
		"error: directory at the log path": {
			setup: func(t *testing.T, root string) string {
				if err := os.MkdirAll(root+"/logs/request-routes.jsonl", 0o700); err != nil {
					t.Fatal(err)
				}
				return root + "/logs/request-routes.jsonl"
			},
			check: func(t *testing.T, root string) {
				entries, err := os.ReadDir(root + "/logs/request-routes.jsonl")
				if err != nil || len(entries) != 0 {
					t.Errorf("directory at the log path changed: %v %v", entries, err)
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// The test owns the creation mask, so the modes it checks are the modes the recorder
			// requested and not the host's default mask. No test of this package runs in parallel.
			previous := unix.Umask(0)
			defer unix.Umask(previous)
			root := t.TempDir()
			opts := recorderOptions(nil)
			opts.LogPath = tt.setup(t, root)
			var diagnostics bytes.Buffer
			opts.Diagnostics = &diagnostics
			r := NewRecorder(opts)
			for range 2 {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages?key=query-sentinel", nil)
				req.Header.Set("X-Claude-Code-Session-Id", "header-session")
				r.LogRoute(req, LogEntry{Backend: "anthropic", Model: new("claude-fable-5")})
			}
			if tt.written {
				if diagnostics.Len() != 0 {
					t.Errorf("unexpected diagnostic: %q", diagnostics.String())
				}
				raw, err := os.ReadFile(opts.LogPath)
				if err != nil {
					t.Fatal(err)
				}
				entry := map[string]any{"at": "1970-01-01T00:00:01.000Z", "backend": "anthropic", "model": "claude-fable-5", "path": "/v1/messages", "sessionId": "header-session"}
				want := append(slices.Clone(tt.prior), entry, entry)
				if d := gocmp.Diff(want, routeLogRows(t, raw)); d != "" {
					t.Errorf("whole route log (-want +got):\n%s", d)
				}
			} else {
				checkRouteLogDiagnostics(t, diagnostics.String(), root, 2)
				if tt.code != "" {
					line := routeLogDiagnostic + tt.code + "\n"
					if d := gocmp.Diff(line+line, diagnostics.String()); d != "" {
						t.Errorf("diagnostics (-want +got):\n%s", d)
					}
				}
			}
			tt.check(t, root)
		})
	}
}
