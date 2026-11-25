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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNonDirectoryParent(t *testing.T) {
	const arg = "--check-parent-type"
	const watchdogStatus = 43
	if os.Args[len(os.Args)-1] == arg {
		deadline := time.AfterFunc(5*time.Second, func() { os.Exit(watchdogStatus) })
		defer deadline.Stop()
		tests := map[string]struct{ write func() error }{"state": {func() error { return WriteFile("parent/file", []byte(newState), 0o600) }}, "settings": {func() error {
			return ReplaceSettings("parent/file", func([]byte) ([]byte, error) { t.Fatal("unexpected edit"); return nil, nil })
		}}}
		for name, tt := range tests {
			if err := tt.write(); !errors.Is(err, unix.ENOTDIR) {
				t.Fatalf("%s: expected ENOTDIR, got %v", name, err)
			}
		}
		return
	}
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	child := exec.CommandContext(ctx, exe, "-test.run=^TestNonDirectoryParent$", "--", arg)
	child.Dir = dir
	if output, err := child.CombinedOutput(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == watchdogStatus {
			t.Fatal("directory open did not return before watchdog")
		}
		t.Fatalf("parent-type child: %v: %s", err, output)
	}
}
