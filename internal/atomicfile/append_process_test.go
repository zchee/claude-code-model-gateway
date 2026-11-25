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
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAppendFileProcess(t *testing.T) {
	const arg = "--append-file-checks"
	const watchdogStatus = 43
	if os.Args[len(os.Args)-1] == arg {
		t.Run("error: FIFO without a reader", func(t *testing.T) {
			deadline := time.AfterFunc(5*time.Second, func() { os.Exit(watchdogStatus) })
			defer deadline.Stop()
			if err := AppendFile("fifo/journal", []byte("data"), 0o600); !errors.Is(err, unix.ENOTDIR) {
				t.Fatalf("FIFO parent: %v", err)
			}
			err := AppendFile("fifo", []byte("data"), 0o600)
			if _, ok := errors.AsType[*NonRegularError](err); !ok {
				t.Fatalf("FIFO leaf: %v", err)
			}
		})
		t.Run("error: file size limit", func(t *testing.T) {
			var limit unix.Rlimit
			if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
				t.Fatal(err)
			}
			limit.Cur = 2
			signal.Ignore(unix.SIGXFSZ)
			if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
				t.Fatal(err)
			}
			err := AppendFile("journal", []byte("data"), 0o600)
			if !errors.Is(err, io.ErrShortWrite) || !strings.Contains(err.Error(), "2 of 4 bytes") {
				t.Fatalf("partial record: %v", err)
			}
			err = AppendFile("journal", []byte("data"), 0o600)
			if !errors.Is(err, unix.EFBIG) || !strings.Contains(err.Error(), "0 of 4 bytes") {
				t.Fatalf("full file: %v", err)
			}
		})
		return
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestAppendFileProcess$", "--", arg)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == watchdogStatus {
			t.Fatal("append blocked on the FIFO")
		}
		t.Fatalf("append helper: %v (context: %v): %s", err, ctx.Err(), output)
	}
	checkContents(t, filepath.Join(dir, "journal"), "da")
}
