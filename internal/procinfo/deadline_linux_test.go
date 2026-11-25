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

//go:build linux && process

package procinfo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/common"
	"golang.org/x/sys/unix"
)

func TestListenerAfterSnapshotMismatch(t *testing.T) {
	const header = "sl local_address rem_address st tx_queue rx_queue tr uid timeout inode\n"
	const listening = "0: 0100007F:69F3 00000000:0000 0A 0 0 0 0 0 123\n"
	tests := map[string]struct {
		blockRetry bool
		want       ListenerState
	}{"error: retry reaches deadline": {true, ListenerTimedOut}, "error: retry completed but still inconsistent": {false, ListenerUnknown}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			netdir := filepath.Join(root, "net")
			if err := os.Mkdir(netdir, 0o700); err != nil {
				t.Fatal(err)
			}
			tcp := filepath.Join(netdir, "tcp")
			for _, name := range []string{tcp, tcp + ".second", tcp + ".retry"} {
				if err := unix.Mkfifo(name, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			ctx = context.WithValue(ctx, common.EnvKey, common.EnvMap{common.HostProcEnvKey: root})
			done := make(chan Listener, 1)
			go func() { done <- (Probe{}).Listener(ctx, 27123) }()
			// Each open waits for the kernel-table reader before advancing the directory
			// entry. Its current descriptor therefore cannot see the next sample.
			for i, suffix := range []string{".second", ".retry", ""} {
				var fd int
				for {
					var err error
					fd, err = unix.Open(tcp, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
					if err == nil {
						break
					}
					if !errors.Is(err, unix.ENXIO) {
						t.Fatal(err)
					}
					select {
					case <-ctx.Done():
						t.Fatal("query did not reach sample", i)
					case <-time.After(time.Millisecond):
					}
				}
				f := os.NewFile(uintptr(fd), tcp)
				defer func() { _ = f.Close() }()
				if suffix != "" {
					if err := os.Rename(tcp+suffix, tcp); err != nil {
						t.Fatal(err)
					}
				}
				if i == 2 && tt.blockRetry {
					break
				}
				data := header
				if i == 1 {
					data += listening
				}
				if _, err := f.WriteString(data); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case got := <-done:
				if got != (Listener{State: tt.want}) {
					t.Fatalf("got %+v want %s", got, tt.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("listener exceeded its deadline")
			}
		})
	}
}
