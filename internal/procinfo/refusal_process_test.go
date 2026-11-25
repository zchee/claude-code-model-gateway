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

package procinfo

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

func TestMirrorFIFO(t *testing.T) {
	const arg = "--probe-fifo"
	if os.Args[len(os.Args)-1] == arg {
		root := os.Args[len(os.Args)-2]
		timer := time.AfterFunc(3*time.Second, func() { os.Exit(43) })
		defer timer.Stop()
		r, err := ReadRecord(root, "guardian")
		if r.PID != 702 || !errors.Is(err, ErrMirrorRepair) || !errors.Is(err, ErrRecordEntry) {
			t.Fatalf("read fifo: pid=%d err=%v", r.PID, err)
		}
		return
	}
	root := t.TempDir()
	_, wire := recordFixture(root)
	putFile(t, filepath.Join(root, "guardian.pid.json"), wire)
	p := filepath.Join(root, "guardian.pid")
	if err := unix.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestMirrorFIFO$", "--", root, arg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("FIFO helper: %v: %s", err, out)
	}
	info, err := os.Lstat(p)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO changed: %v, %v", info, err)
	}
}

func TestProbeCommandCancellation(t *testing.T) {
	dir := t.TempDir()
	pipe := filepath.Join(dir, "held")
	if err := unix.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(pipe, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	ready := filepath.Join(dir, "ready")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := darwinCommand(ctx, "/bin/sh", "-c", `(exec 3>"$1"; : > "$2"; exec /bin/sleep 8) & wait`, "probe", pipe, ready)
		done <- err
	}()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("helper not ready")
		case err := <-done:
			t.Fatalf("exited before cancellation: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not return on cancellation")
	}
	// The helper, not the parent shell, holds the only write descriptor.
	var buf [1]byte
	limit := time.Now().Add(time.Second)
	for {
		n, err := unix.Read(fd, buf[:])
		if err == nil && n == 0 {
			break
		}
		if !errors.Is(err, unix.EAGAIN) || time.Now().After(limit) {
			t.Fatalf("helper still holds FIFO or read failed: n=%d err=%v", n, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
