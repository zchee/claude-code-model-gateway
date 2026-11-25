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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDirFIFO(t *testing.T) {
	const arg = "--directory-handle-fifo"
	const watchdogStatus = 43
	if os.Args[len(os.Args)-1] == arg {
		deadline := time.AfterFunc(5*time.Second, func() { os.Exit(watchdogStatus) })
		defer deadline.Stop()
		if d, err := OpenDir("fifo"); d != nil || !errors.Is(err, unix.ENOTDIR) {
			t.Fatalf("OpenDir(fifo) = %v, %v", d, err)
		}
		d := openTestDir(t, ".")
		data, err := d.ReadFile("fifo")
		if _, ok := errors.AsType[*NonRegularError](err); !ok || data != nil {
			t.Fatalf("ReadFile(fifo) = %q, %v", data, err)
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
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDirFIFO$", "--", arg)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == watchdogStatus {
			t.Fatal("directory operation blocked on the FIFO")
		}
		t.Fatalf("FIFO helper: %v (context: %v): %s", err, ctx.Err(), output)
	}
	if after := exclusiveStat(t, path); before != after {
		t.Fatalf("FIFO changed: %+v -> %+v", before, after)
	}
}
