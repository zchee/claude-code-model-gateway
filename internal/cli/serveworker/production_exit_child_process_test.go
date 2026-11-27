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

package serveworker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// productionExitArg marks the test binary's invocation as the production-exit worker child; the argument after it,
	// when present, is the park FIFO.
	productionExitArg = "--serveworker-production-exit-child"
	// productionExitParked is the line the child prints when its settings lookup parks on the FIFO.
	productionExitParked = "production-exit child: settings lookup parked after the socket bind"
	// lossWaitDelay bounds the wait for a child's output once it exited or was killed, as the fixture runner's does.
	lossWaitDelay = time.Second
)

// TestProductionExitWorkerChild is the entry of the production-exit worker child, which launchProductionExit starts.
// In an ordinary test run it returns at once.
func TestProductionExitWorkerChild(t *testing.T) {
	i := slices.Index(os.Args, productionExitArg)
	if i < 0 {
		return
	}
	runProductionExitChild(os.Args[i+1:])
}

// runProductionExitChild runs a supervised worker the way the production binary does: Options.Exit is os.Exit, an error
// Run returns ends the process with status 1 after one diagnostic line, and a nil return with status 0. It never
// returns, so the test binary's own runner, its leak check included, never runs after the worker. The descriptors are
// the fixture runner's: the working directory at 3, which the child enters and closes, and the handoff from 4 on.
//
// With a park FIFO the child's settings lookup parks once, at the first call that finds the worker's Unix socket bound:
// it prints productionExitParked and blocks opening and reading the FIFO until the test writes a byte. The worker's
// own environment is otherwise read unchanged.
func runProductionExitChild(args []string) {
	dir := os.NewFile(3, "child working directory")
	if err := errors.Join(dir.Chdir(), dir.Close()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "production-exit child: enter the working directory:", err)
		os.Exit(3)
	}
	lookup := os.LookupEnv
	if len(args) == 1 {
		fifo := args[0]
		socket, _ := os.LookupEnv("CODEX_GATEWAY_SOCKET_PATH")
		var parked atomic.Bool
		lookup = func(key string) (string, bool) {
			if !parked.Load() {
				if info, err := os.Lstat(socket); err == nil && info.Mode().Type() == fs.ModeSocket && parked.CompareAndSwap(false, true) {
					_, _ = fmt.Fprintln(os.Stdout, productionExitParked)
					parkOnFIFO(fifo)
				}
			}
			return os.LookupEnv(key)
		}
	}
	err := Run(context.Background(), Options{
		Lookup: lookup, Home: os.UserHomeDir, Cwd: os.Getwd, Version: "fixture-build",
		Stdout: os.Stdout, Stderr: os.Stderr, Exit: os.Exit,
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "model-gateway:", err)
		os.Exit(1)
	}
	_, _ = fmt.Fprintln(os.Stdout, "worker returned cleanly")
	os.Exit(0)
}

// parkOnFIFO blocks until a byte can be read from the FIFO at path, or its writers are gone. The open itself blocks
// until the test opens the FIFO too.
func parkOnFIFO(path string) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "production-exit child: open the park FIFO:", err)
		os.Exit(3)
	}
	var b [1]byte
	_, _ = f.Read(b[:])
	_ = f.Close()
}

// launchProductionExit starts the production-exit worker child with h's descriptors and environment, as the fixture
// runner starts the worker helper: exactly h's environment, the child's private working directory at descriptor 3, the
// handoff from lossFirstChildFD on, and the runner's lifetime. park, when not empty, is the FIFO the child's settings
// lookup parks on; makeLossParkFIFO makes it.
//
// It starts the test binary itself instead of going through gatewaytest's fixture runner, for two reasons. The runner
// starts only the package's single TestGatewayFixtureHelper entry, and that entry, which this package may not change,
// runs the worker with Exit nil; a second entry of that name would break the runner's rule of one entry per package.
// And this child must end through os.Exit, so it needs an entry of its own that never returns to the test runner. The
// launcher reproduces the runner's protections itself: the child gets exactly h's environment and nothing inherited,
// a private working directory under h's home that it enters through descriptor 3, a kill once lossChildLifetime has
// passed, and the runner's WaitDelay, so that a child that blocks cannot outlive its test and no output pipe keeps
// the wait open. The program is always os.Executable(), this test binary, so the runner's check of which program runs
// has nothing to refuse.
func (h *lossHandoff) launchProductionExit(t *testing.T, park string) *lossChild {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workdir, err := os.MkdirTemp(h.env["HOME"], "child-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(workdir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	args := []string{"-test.run=^TestProductionExitWorkerChild$", "--", productionExitArg}
	if park != "" {
		args = append(args, park)
	}
	ctx, cancel := context.WithTimeout(t.Context(), lossChildLifetime)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = workdir
	cmd.Env = make([]string, 0, len(h.env))
	for k, v := range h.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	slices.Sort(cmd.Env)
	cmd.ExtraFiles = append([]*os.File{dir}, h.files...)
	cmd.WaitDelay = lossWaitDelay
	return h.start(t, cmd, cmd.Run)
}

// makeLossParkFIFO makes a FIFO in h's home for the production-exit child to park on, and returns its path and the
// release that lets a parked child go on. The release opens the FIFO for reading and writing, so it neither waits for
// the child nor fails when the child is not in its open yet, and writes the byte the child reads. Its descriptor stays
// open until the cleanup this function registers, which runs after the launch's own cleanup waited for the child, so
// the byte is still there when the child opens the FIFO late. The release is idempotent; the caller registers it as a
// cleanup after the launch, so that a child still parked is let go before the launch's cleanup waits for it.
func makeLossParkFIFO(t *testing.T, h *lossHandoff) (string, func()) {
	t.Helper()
	path := filepath.Join(h.env["HOME"], "park.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("make the park FIFO: %v", err)
	}
	fd := -1
	t.Cleanup(func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	})
	var released atomic.Bool
	release := func() {
		if !released.CompareAndSwap(false, true) {
			return
		}
		var err error
		if fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0); err != nil {
			fd = -1
			t.Errorf("open the park FIFO to release the child: %v", err)
			return
		}
		if _, err := unix.Write(fd, []byte{1}); err != nil {
			t.Errorf("release the parked child: %v", err)
		}
	}
	return path, release
}
