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
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/wiring"
)

const (
	registryHelper      = "interrupt-registry-write"
	registryInterrupted = 47
	registryMarker      = "registry write hit the file size limit\n"
)

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{registryHelper: interruptRegistryWrite})
}

func interruptRegistryWrite(t testing.TB, args []string) {
	if len(args) != 1 {
		t.Fatalf("expected project directory, got %q", args)
	}
	project := args[0]
	// The guarded launcher provides HOME and the test gateway port without inheriting the parent environment.
	store := wiring.Store{Paths: paths.New(os.LookupEnv, nil, func() (string, error) { return project, nil }), Project: project, Lookup: os.LookupEnv}
	// Ignore the signal so the writer must report the kernel's error, not just die during the write.
	signal.Ignore(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 32, Max: 32}); err != nil {
		t.Fatal(err)
	}
	err := store.RecordProject()
	if !errors.Is(err, syscall.EFBIG) && !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected the registry writer to encounter the file size limit, got %v", err)
	}
	if _, err := io.WriteString(os.Stdout, registryMarker); err != nil {
		t.Fatal(err)
	}
	os.Exit(registryInterrupted)
}

func TestAtomicProjectRegistry(t *testing.T) {
	f := sandbox(t)
	putEnv(t, f.local, map[string]any{base: direct})
	const old = "{\"projects\":[]}\n"
	put(t, f.registry, old)
	env := gatewaytest.NewEnv(f.home, nil, nil, f.env)
	cmd, err := env.HelperCommand(t, registryHelper, f.Project)
	check(t, err)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err = env.RunFixture(cmd)
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != registryInterrupted {
		t.Fatalf("writer did not encounter the interruption: %v: %s", err, output.String())
	}
	equal(t, registryMarker, output.String())
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
