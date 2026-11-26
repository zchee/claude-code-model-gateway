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
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func writeSparseOversizedFile(t *testing.T, path string) int64 {
	t.Helper()
	const targetSize = (16 << 20) + 64
	check(t, os.MkdirAll(filepath.Dir(path), 0o700))
	f, err := os.Create(path)
	check(t, err)
	defer func() { check(t, f.Close()) }()
	check(t, f.Truncate(targetSize))
	return targetSize
}

func TestUserWriteIgnoresLocalFileWhenLegacySharedIsNotGatewayWired(t *testing.T) {
	tests := map[string]struct {
		setupLocal func(*testing.T, fixture)
	}{
		"success: unreadable local settings file": {
			setupLocal: func(t *testing.T, f fixture) {
				put(t, f.local, `{"env":{}}`)
				check(t, os.Chmod(f.local, 0o000))
				t.Cleanup(func() { _ = os.Chmod(f.local, 0o600) })
			},
		},
		"success: fifo local settings file": {
			setupLocal: func(t *testing.T, f fixture) {
				check(t, os.MkdirAll(filepath.Dir(f.local), 0o700))
				check(t, syscall.Mkfifo(f.local, 0o600))
			},
		},
		"success: directory local settings path": {
			setupLocal: func(t *testing.T, f fixture) {
				check(t, os.MkdirAll(f.local, 0o700))
			},
		},
		"success: oversized sparse local settings file": {
			setupLocal: func(t *testing.T, f fixture) {
				writeSparseOversizedFile(t, f.local)
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			const sharedBefore = `{"permissions":{"allow":["Read"]}}`
			put(t, f.shared, sharedBefore)
			tt.setupLocal(t, f)

			check(t, f.WriteEnv("user", "default", false))
			equal(t, direct, envAt(t, f.user)[base])
			equal(t, sharedBefore, text(t, f.shared))
		})
	}
}

func TestProjectRemovalWithOversizedUserSettings(t *testing.T) {
	tests := map[string]struct {
		run func(*testing.T, fixture, int64)
	}{
		"success: unrelated project removal succeeds while user settings exceed size bound": {
			run: func(t *testing.T, f fixture, size int64) {
				putEnv(t, f.local, map[string]any{base: direct, discovery: "1", "LOCAL_KEEP": "yes"})
				putEnv(t, f.shared, map[string]any{base: compat, discovery: "1", "SHARED_KEEP": "yes"})

				check(t, f.WriteEnv("project", "default", true))
				equal(t, map[string]any{"LOCAL_KEEP": "yes"}, envAt(t, f.local))
				equal(t, map[string]any{"SHARED_KEEP": "yes"}, envAt(t, f.shared))
				info, err := os.Stat(f.user)
				check(t, err)
				equal(t, size, info.Size())
			},
		},
		"error: project-local aliasing oversized user file is refused with bound error naming target": {
			run: func(t *testing.T, f fixture, size int64) {
				check(t, os.MkdirAll(filepath.Dir(f.local), 0o700))
				check(t, os.Symlink(f.user, f.local))

				err := f.WriteEnv("project", "default", true)
				assertBoundError(t, "WriteEnv project remove", f.local, err)
				info, statErr := os.Stat(f.user)
				check(t, statErr)
				equal(t, size, info.Size())
			},
		},
		"error: project-shared aliasing oversized user file is refused with bound error naming target": {
			run: func(t *testing.T, f fixture, size int64) {
				check(t, os.MkdirAll(filepath.Dir(f.shared), 0o700))
				check(t, os.Symlink(f.user, f.shared))

				err := f.WriteEnv("project-shared", "default", true)
				assertBoundError(t, "WriteEnv project-shared remove", f.shared, err)
				info, statErr := os.Stat(f.user)
				check(t, statErr)
				equal(t, size, info.Size())
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			size := writeSparseOversizedFile(t, f.user)
			tt.run(t, f, size)
		})
	}

	t.Run("error: fifo user settings refuses project removal without blocking", func(t *testing.T) {
		f := sandbox(t)
		check(t, os.MkdirAll(filepath.Dir(f.user), 0o700))
		check(t, syscall.Mkfifo(f.user, 0o600))
		putEnv(t, f.local, map[string]any{base: direct, discovery: "1"})
		localBefore := text(t, f.local)

		done := make(chan error, 1)
		go func() {
			done <- f.WriteEnv("project", "default", true)
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected refusal when user settings identity cannot be established")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("blocked checking user settings alias against fifo")
		}
		equal(t, localBefore, text(t, f.local))
	})
}
