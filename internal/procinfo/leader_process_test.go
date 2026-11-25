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

package procinfo

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestProbeLeaderExitsBeforeChild(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "held")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// The shell opens the FIFO before forking. Its child inherits the only writer
	// and both output pipes, regardless of whether it has reached exec at exit.
	out, err := darwinCommand(ctx, "/bin/sh", "-c", `exec 3>"$1"; /bin/sleep 8 & printf done; exit 0`, "probe", fifo)
	if err != nil || string(out) != "done" {
		t.Fatalf("leader exit: output=%q err=%v", out, err)
	}
	var buf [1]byte
	if n, err := unix.Read(fd, buf[:]); err != nil || n != 0 {
		t.Fatalf("descendant still has FIFO on return: n=%d err=%v", n, err)
	}
}
