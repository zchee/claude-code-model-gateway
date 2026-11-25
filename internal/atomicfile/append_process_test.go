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
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const processRecords = 128

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{"append": func(t testing.TB, args []string) {
		if len(args) != 1 || (args[0] != "a" && args[0] != "b") {
			t.Fatalf("invalid writer identifier: %q", args)
		}
		// The launcher reserves fd 3 for the working directory. Keep readiness separate from test output.
		if _, err := unix.Write(4, []byte(args[0])); err != nil {
			t.Fatal(err)
		}
		var start [1]byte
		if _, err := io.ReadFull(os.Stdin, start[:]); err != nil {
			t.Fatal(err)
		}
		for i := range processRecords {
			if err := AppendFile("journal", fmt.Appendf(nil, "%s:%d\n", args[0], i), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}})
}

func TestAppendFileIndependentWriters(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	pipe := func() (*os.File, *os.File) {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
		return r, w
	}
	ready, output := pipe()
	input, start := pipe()
	const limit = 30 * time.Second
	if err := ready.SetReadDeadline(time.Now().Add(limit)); err != nil {
		t.Fatal(err)
	}
	var dir string
	commands := make(map[string]*exec.Cmd)
	want := map[string]int{}
	for _, id := range []string{"a", "b"} {
		cmd, err := env.HelperCommand(t, "append", id)
		if err != nil {
			t.Fatal(err)
		}
		if dir == "" {
			dir = cmd.Dir
		}
		cmd.Dir, cmd.Stdin = dir, input
		cmd.ExtraFiles = []*os.File{output}
		commands[id] = cmd
		for i := range processRecords {
			want[fmt.Sprintf("%s:%d\n", id, i)]++
		}
	}
	var workers sync.WaitGroup
	// Test cleanup cancels the helpers' t.Context before this join. Register it
	// after preparing both commands so their fixture directories are removed last.
	t.Cleanup(func() { _ = ready.Close(); workers.Wait() })
	done := make(chan error, len(commands))
	for id, cmd := range commands {
		workers.Go(func() {
			var log strings.Builder
			cmd.Stdout, cmd.Stderr = &log, &log
			err := env.RunFixtureWithin(cmd, limit)
			if err != nil {
				err = fmt.Errorf("writer %s: %w: %s", id, err, log.String())
			}
			done <- err
		})
	}
	// Both children must have entered the shared fixture directory before either writes.
	barrier := make(chan error, 1)
	workers.Go(func() {
		var ids [2]byte
		_, err := io.ReadFull(ready, ids[:])
		if err == nil && string(ids[:]) != "ab" && string(ids[:]) != "ba" {
			err = fmt.Errorf("unexpected writer IDs: %q", ids)
		}
		barrier <- err
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("writer exited successfully before barrier release")
	case err := <-barrier:
		if err != nil {
			t.Fatalf("writers did not reach the barrier: %v", err)
		}
	}
	if _, err := start.Write([]byte("++")); err != nil {
		t.Fatal(err)
	}
	for range commands {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for line := range strings.Lines(string(data)) {
		got[line]++
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("journal records (-want +got):\n%s", diff)
	}
}

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
		t.Run("creation permissions", func(t *testing.T) {
			previous := unix.Umask(0o022)
			defer unix.Umask(previous)
			tests := map[string]struct{ requested, want os.FileMode }{
				"shared":  {0o666, 0o644},
				"private": {0o600, 0o600},
			}
			for path, tt := range tests {
				if err := AppendFile(path, []byte("record"), tt.requested); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != tt.want {
					t.Errorf("%s: got %04o, want %04o", path, got, tt.want)
				}
			}
		})
		t.Run("opened leaf", func(t *testing.T) {
			deadline := time.AfterFunc(5*time.Second, func() { os.Exit(watchdogStatus) })
			defer deadline.Stop()
			testAppendOpenedLeaf(t)
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
			// Only the first append is portable: XNU's vn_write checks the descriptor offset
			// before passing IO_APPEND to the filesystem; reopening need not yield Linux's EFBIG.
			err := AppendFile("journal", []byte("data"), 0o600)
			if !errors.Is(err, io.ErrShortWrite) || !strings.Contains(err.Error(), "2 of 4 bytes") {
				t.Fatalf("partial record: %v", err)
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

func testAppendOpenedLeaf(t *testing.T) {
	// Exercise the open boundary directly so the preliminary lstat cannot mask a missing guard.
	d := openTestDir(t, ".")
	writeFixture(t, "target", oldState)
	for _, target := range []string{"target", "absent"} {
		link := "link-" + target
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := d.root.openAndAppend(link, []byte(newState), 0o600, d.root.OpenFile, unix.Write); err == nil {
			t.Fatalf("opened symlink %q", link)
		} else if _, ok := errors.AsType[*NonRegularError](err); !ok {
			t.Fatal(err)
		}
	}
	checkContents(t, "target", oldState)
	if _, err := os.Lstat("absent"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling symlink target: %v", err)
	}
	refuseFIFO := func() {
		t.Helper()
		err := d.root.openAndAppend("fifo", []byte(newState), 0o600, d.root.OpenFile, unix.Write)
		if _, ok := errors.AsType[*NonRegularError](err); !ok {
			t.Fatalf("FIFO: want NonRegularError, got %v", err)
		}
	}
	refuseFIFO()
	fd, err := unix.Open("fifo", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	refuseFIFO()
	var buf [1]byte
	if n, err := unix.Read(fd, buf[:]); n != 0 || err != nil {
		t.Fatalf("FIFO reader: want EOF without data, got %d bytes, %v", n, err)
	}
}
