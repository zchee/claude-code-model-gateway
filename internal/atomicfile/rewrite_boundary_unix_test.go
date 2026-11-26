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

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRewriteOpenedLeaf(t *testing.T) {
	dir := t.TempDir()
	d := openTestDir(t, dir)
	target := filepath.Join(dir, "target")
	writeFixture(t, target, oldState)
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// A nonblocking reader lets the write-only open succeed. Only the descriptor
	// type check should refuse the FIFO, not an open error for a missing reader.
	reader, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(reader) }()
	tests := map[string]struct{ leaf string }{
		"error: symlink to a regular file": {"link"},
		"error: FIFO with a reader":        {"fifo"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := d.root.openAndRewrite(tt.leaf, []byte(newState), func(int, []byte) (int, error) {
				t.Fatal("reached write for a nonregular entry")
				return 0, nil
			})
			if refusal, ok := errors.AsType[*NonRegularError](err); !ok || refusal.Path != filepath.Join(dir, tt.leaf) {
				t.Fatalf("want NonRegularError for %q, got %v", tt.leaf, err)
			}
		})
	}
	checkContents(t, target, oldState)
	var b [1]byte
	if n, err := unix.Read(reader, b[:]); n != 0 || err != nil {
		t.Fatalf("expected EOF without FIFO data, got %d bytes, %v", n, err)
	}
}

func TestRewriteInterruptedWrite(t *testing.T) {
	tests := map[string]struct{ n int }{
		"success: zero bytes before interrupt":        {0},
		"success: syscall interrupted before writing": {-1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := stateFile(t)
			d := openTestDir(t, filepath.Dir(path))
			calls := 0
			err := d.rewriteFile(filepath.Base(path), []byte(newState), func(fd int, p []byte) (int, error) {
				calls++
				if calls == 1 {
					return tt.n, unix.EINTR
				}
				return unix.Write(fd, p)
			})
			if err != nil || calls != 2 {
				t.Fatalf("rewrite: %v after %d writes, want success in two calls", err, calls)
			}
			checkContents(t, path, newState)
		})
	}
}
