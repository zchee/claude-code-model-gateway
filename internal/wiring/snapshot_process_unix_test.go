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

//go:build unix && process

package wiring_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotReaderRefusesUnixSocketWithoutBlocking(t *testing.T) {
	f := sandbox(t)
	putEnv(t, f.user, map[string]any{base: direct})
	check(t, os.MkdirAll(filepath.Dir(f.local), 0o700))
	sockDir, err := os.MkdirTemp("", "gw-sock-")
	check(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	check(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	check(t, os.Symlink(sock, f.local))

	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := f.EffectiveBaseURL()
		check(t, err)
		if res.Winner == nil || res.Winner.Source != "user" {
			t.Errorf("expected fallback to user, got %+v", res)
		}
		if err := f.WriteEnv("project", "default", false); err == nil {
			t.Errorf("expected WriteEnv to refuse unix socket")
		}
		changed, err := f.CleanModelCache(f.local, func(id string) (string, bool) { return id + "[1m]", true })
		if err != nil || changed {
			t.Errorf("CleanModelCache on unix socket: changed=%v err=%v", changed, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked on unix socket")
	}
}
