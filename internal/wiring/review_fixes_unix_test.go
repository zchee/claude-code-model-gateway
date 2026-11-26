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

package wiring_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSnapshotReaderRefusesNonRegularWithoutBlocking(t *testing.T) {
	makeKind := func(t *testing.T, kind, target string) {
		t.Helper()
		check(t, os.MkdirAll(filepath.Dir(target), 0o700))
		switch kind {
		case "fifo":
			check(t, syscall.Mkfifo(target, 0o600))
		case "symlink-to-fifo":
			fifo := filepath.Join(t.TempDir(), "pipe")
			check(t, syscall.Mkfifo(fifo, 0o600))
			check(t, os.Symlink(fifo, target))
		case "directory":
			check(t, os.Mkdir(target, 0o700))
		case "device":
			check(t, os.Symlink("/dev/null", target))
		case "socket":
			// Unix socket paths have a short length limit on Unix, so create in a short temp dir and symlink.
			sockDir, err := os.MkdirTemp("", "gw-sock-")
			check(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
			sock := filepath.Join(sockDir, "s")
			ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
			check(t, err)
			t.Cleanup(func() { _ = ln.Close() })
			check(t, os.Symlink(sock, target))
		}
	}

	tests := map[string]struct{ kind string }{
		"error: fifo without writer": {"fifo"},
		"error: symlink to fifo":     {"symlink-to-fifo"},
		"error: directory":           {"directory"},
		"error: device":              {"device"},
		"error: unix socket":         {"socket"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.user, map[string]any{base: direct})
			makeKind(t, tt.kind, f.local)

			done := make(chan struct{})
			go func() {
				defer close(done)
				// Readiness-path scans skip unreadable/non-regular entries without blocking.
				res, err := f.EffectiveBaseURL()
				check(t, err)
				if res.Winner == nil || res.Winner.Source != "user" {
					t.Errorf("expected fallback to user, got %+v", res)
				}
				// Write path refuses non-regular file with an error.
				if err := f.WriteEnv("project", "default", false); err == nil {
					t.Errorf("expected WriteEnv to refuse %s", tt.kind)
				}
				// Cache helper refuses non-regular file without blocking.
				changed, err := f.CleanModelCache(f.local, func(id string) (string, bool) { return id + "[1m]", true })
				if err != nil || changed {
					t.Errorf("CleanModelCache on %s: changed=%v err=%v", tt.kind, changed, err)
				}
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("blocked on %s", tt.kind)
			}
		})
	}

	t.Run("success: symlink to regular file still works", func(t *testing.T) {
		f := sandbox(t)
		realFile := filepath.Join(t.TempDir(), "real-settings.json")
		putEnv(t, realFile, map[string]any{base: compat, "KEEP": "yes"})
		check(t, os.MkdirAll(filepath.Dir(f.local), 0o700))
		check(t, os.Symlink(realFile, f.local))

		res, err := f.EffectiveBaseURL()
		check(t, err)
		equal(t, compat, res.Winner.Value)

		check(t, f.WriteEnv("project", "default", false))
		equal(t, direct, envAt(t, realFile)[base])
		equal(t, "yes", envAt(t, realFile)["KEEP"])
	})

	t.Run("error: oversized regular file exceeds snapshot bound", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.user, map[string]any{base: direct})
		check(t, os.MkdirAll(filepath.Dir(f.local), 0o700))
		big, err := os.Create(f.local)
		check(t, err)
		check(t, big.Truncate((16<<20)+1))
		check(t, big.Close())

		res, err := f.EffectiveBaseURL()
		check(t, err)
		equal(t, "user", res.Winner.Source)

		if err := f.WriteEnv("project", "default", false); err == nil {
			t.Fatal("expected WriteEnv to refuse oversized settings file")
		}
	})
}

func TestCleanModelCacheReturnsFalseOnWriteFailure(t *testing.T) {
	f := sandbox(t)
	dir := filepath.Join(f.home, "readonly-cache")
	file := filepath.Join(dir, "cache.json")
	original := `{"baseUrl":"` + direct + `","fetchedAt":1,"models":[{"id":"claude-gpt-6-astra","display_name":"GPT-6 Astra"}]}`
	put(t, file, original)
	check(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	changed, err := f.CleanModelCache(file, func(id string) (string, bool) {
		if strings.HasPrefix(id, "claude-gpt-") {
			return id + "[1m]", true
		}
		return id, false
	})
	check(t, err)
	equal(t, false, changed)
	equal(t, original, text(t, file))
}
