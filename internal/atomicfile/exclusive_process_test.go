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

//go:build process && unix

package atomicfile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCreateExclusiveFIFO(t *testing.T) {
	const arg = "--create-exclusive-fifo"
	const watchdogStatus = 43
	if os.Args[len(os.Args)-1] == arg {
		deadline := time.AfterFunc(5*time.Second, func() { os.Exit(watchdogStatus) })
		defer deadline.Stop()
		if err := CreateExclusive("fifo", []byte("data"), 0o640); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("want fs.ErrExist, got %v", err)
		}
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	before := exclusiveStat(t, path)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestCreateExclusiveFIFO$", "--", arg)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == watchdogStatus {
			t.Fatal("CreateExclusive blocked on the FIFO")
		}
		t.Fatalf("FIFO helper: %v (context: %v): %s", err, ctx.Err(), output)
	}
	if after := exclusiveStat(t, path); after != before {
		t.Fatalf("FIFO metadata changed: %+v -> %+v", before, after)
	}
	checkOnlyTarget(t, path)
}
