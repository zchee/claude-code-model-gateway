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

package serveshim_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveshim"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

type outputFunc func([]byte) (int, error)

func (f outputFunc) Write(p []byte) (int, error) { return f(p) }

func writeStubWorker(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "stub-worker")
	// Exit cleanly as soon as the supervisor closes the liveness pipe on descriptor 4.
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec cat <&4 >/dev/null 2>&1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFixtureControlDescriptor(t *testing.T) {
	tests := map[string]struct {
		kind          string
		guarded       bool
		wantErr       bool
		stopByChannel bool
	}{
		"error: guarded regular file is rejected untouched":    {kind: "file", guarded: true, wantErr: true},
		"error: guarded write-only pipe is rejected untouched": {kind: "write-pipe", guarded: true, wantErr: true},
		"success: unguarded regular file is left untouched":    {kind: "file", guarded: false},
		"success: unguarded write-only pipe is left untouched": {kind: "write-pipe", guarded: false},
		"success: guarded readable pipe stops supervisor":      {kind: "read-pipe", guarded: true, stopByChannel: true},
		"success: guarded Unix socket stops supervisor":        {kind: "socket", guarded: true, stopByChannel: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(home, "state")
			stub := writeStubWorker(t, home)

			var fd, peerFD, dupFD int
			peerFD = -1
			dupFD = -1
			var origStat unix.Stat_t

			switch tt.kind {
			case "file":
				filePath := filepath.Join(home, "unrelated.txt")
				if err := os.WriteFile(filePath, []byte(fixtureStop), 0o600); err != nil {
					t.Fatal(err)
				}
				fd, err = unix.Open(filePath, unix.O_RDONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
			case "write-pipe":
				var fds [2]int
				if err := unix.Pipe(fds[:]); err != nil {
					t.Fatal(err)
				}
				peerFD, fd = fds[0], fds[1]
				unix.CloseOnExec(peerFD)
			case "read-pipe":
				var fds [2]int
				if err := unix.Pipe(fds[:]); err != nil {
					t.Fatal(err)
				}
				fd, peerFD = fds[0], fds[1]
				unix.CloseOnExec(peerFD)
			case "socket":
				fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
				if err != nil {
					t.Fatal(err)
				}
				fd, peerFD = fds[0], fds[1]
				unix.CloseOnExec(peerFD)
			default:
				t.Fatalf("unknown descriptor kind %q", tt.kind)
			}

			if peerFD >= 0 {
				defer func() { _ = unix.Close(peerFD) }()
			}
			// Clear FD_CLOEXEC so we can detect if the supervisor modifies an unowned descriptor.
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
				_ = unix.Close(fd)
				t.Fatal(err)
			}
			dupFD, err = unix.Dup(fd)
			if err != nil {
				_ = unix.Close(fd)
				t.Fatal(err)
			}
			unix.CloseOnExec(dupFD)
			defer func() { _ = unix.Close(dupFD) }()
			if err := unix.Fstat(dupFD, &origStat); err != nil {
				_ = unix.Close(fd)
				t.Fatal(err)
			}
			// Close fd only if it still refers to our original open file, never a reused slot.
			defer func() {
				var st unix.Stat_t
				if unix.Fstat(fd, &st) == nil && st.Dev == origStat.Dev && st.Ino == origStat.Ino {
					_ = unix.Close(fd)
				}
			}()

			env := gatewaytest.NewEnv(home, nil, nil, map[string]string{
				"MODEL_GATEWAY_STATE_DIR": state,
				"NODE_CHANNEL_FD":         strconv.Itoa(fd),
			})
			if !tt.guarded {
				delete(env, "MODEL_GATEWAY_TEST_SANDBOX")
				delete(env, "NODE_TEST_CONTEXT")
			}
			entries := make([]string, 0, len(env))
			for k, v := range env {
				entries = append(entries, k+"="+v)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var ready atomic.Bool
			var stdout, stderr bytes.Buffer
			sendStop := sync.OnceFunc(func() {
				_, _ = unix.Write(peerFD, []byte(fixtureStop))
			})
			err = serveshim.Run(ctx, serveshim.Options{
				Environment: entries,
				Home:        func() (string, error) { return home, nil },
				Executable:  stub,
				Version:     fixtureVersion,
				Stdout: outputFunc(func(p []byte) (int, error) {
					n, werr := stdout.Write(p)
					if strings.Contains(stdout.String(), "model-gateway shim supervisor listening on 127.0.0.1:") {
						ready.Store(true)
						if tt.stopByChannel {
							sendStop()
						} else {
							cancel()
						}
					}
					return n, werr
				}),
				Stderr: &stderr,
			})

			if tt.stopByChannel {
				if err != nil || !ready.Load() {
					t.Fatalf("valid fixture channel did not stop cleanly: ready=%t err=%v stderr=%s", ready.Load(), err, &stderr)
				}
				return
			}

			if tt.wantErr {
				if err == nil || ready.Load() {
					t.Errorf("invalid guarded descriptor: err=%v ready=%t; want startup refusal before serving", err, ready.Load())
				}
			} else if err != nil || !ready.Load() {
				t.Errorf("unguarded startup with unrelated descriptor: err=%v ready=%t stderr=%s", err, ready.Load(), &stderr)
			}

			// Verify the unowned descriptor was neither closed, modified, nor read.
			var st unix.Stat_t
			if fstatErr := unix.Fstat(fd, &st); fstatErr != nil || st.Dev != origStat.Dev || st.Ino != origStat.Ino {
				t.Errorf("unrelated descriptor %d was closed or replaced (fstat err=%v)", fd, fstatErr)
			} else if fdFlags, flagErr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); flagErr != nil || fdFlags&unix.FD_CLOEXEC != 0 {
				t.Errorf("unrelated descriptor %d had FD_CLOEXEC modified (flags=%#x err=%v)", fd, fdFlags, flagErr)
			}
			if fl, flErr := unix.FcntlInt(uintptr(dupFD), unix.F_GETFL, 0); flErr != nil || fl&unix.O_NONBLOCK != 0 {
				t.Errorf("unrelated descriptor had O_NONBLOCK modified (flags=%#x err=%v)", fl, flErr)
			}
			if tt.kind == "file" {
				if off, seekErr := unix.Seek(dupFD, 0, io.SeekCurrent); seekErr != nil || off != 0 {
					t.Errorf("unrelated regular file was read (offset=%d err=%v)", off, seekErr)
				}
			}
		})
	}
}
