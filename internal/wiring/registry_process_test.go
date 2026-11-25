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

package wiring_test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/wiring"
)

func TestAtomicProjectRegistry(t *testing.T) {
	const marker = "--interrupt-registry-write"
	const interrupted = 47
	if len(os.Args) >= 4 && os.Args[len(os.Args)-3] == marker {
		home, project := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
		lookup := func(k string) (string, bool) {
			if k == "CODEX_GATEWAY_PORT" {
				return "49123", true
			}
			return "", false
		}
		store := wiring.Store{Paths: paths.New(lookup, func() (string, error) { return home, nil }, func() (string, error) { return project, nil }), Project: project, Lookup: lookup}
		// Allow the first bytes to reach disk. The kernel interrupts the next write to a regular file.
		signal.Ignore(syscall.SIGXFSZ)
		check(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 32, Max: 32}))
		err := store.RecordProject()
		if errors.Is(err, syscall.EFBIG) || errors.Is(err, io.ErrShortWrite) {
			os.Exit(interrupted)
		}
		t.Fatalf("expected the registry writer to encounter the file size limit, got %v", err)
	}
	f := sandbox(t)
	putEnv(t, f.local, map[string]any{base: direct})
	const old = "{\"projects\":[]}\n"
	put(t, f.registry, old)
	exe, err := os.Executable()
	check(t, err)
	output, err := exec.CommandContext(t.Context(), exe, "-test.run=^TestAtomicProjectRegistry$", "--", marker, f.home, f.Project).CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != interrupted {
		t.Fatalf("writer did not encounter the interruption: %v: %s", err, output)
	}
	equal(t, old, text(t, f.registry))
	prior, err := os.Open(f.registry)
	check(t, err)
	t.Cleanup(func() { check(t, prior.Close()) })
	check(t, f.RecordProject())
	witness, err := io.ReadAll(prior)
	check(t, err)
	equal(t, old, string(witness))
	equal(t, "{\n  \"projects\": [\n    "+encoded(t, f.Project)+"\n  ]\n}\n", text(t, f.registry))
	st, err := os.Stat(f.registry)
	check(t, err)
	equal(t, os.FileMode(0o600), st.Mode().Perm())
}
