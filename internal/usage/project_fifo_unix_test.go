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

package usage

import (
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

func TestMetadataReadersRejectFIFO(t *testing.T) {
	tests := map[string]struct {
		leaf, content string
		transcript    bool
	}{
		"error: git marker is not a regular file":     {leaf: ".git", content: "gitdir: worktree"},
		"error: common pointer is not a regular file": {leaf: "commondir", content: "../.."},
		"error: transcript is not a regular file":     {leaf: "session.jsonl", content: `{"cwd":"/project"}`, transcript: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if readFlags&unix.O_NONBLOCK == 0 {
				t.Fatal("metadata opens must be nonblocking")
			}
			path := filepath.Join(t.TempDir(), tt.leaf)
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(path, readFlags, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			// Supply a short, valid value and close its writer before invoking the reader.
			// Even an incorrect reader would reach EOF rather than waiting for more input.
			w, err := os.OpenFile(path, os.O_WRONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = w.WriteString(tt.content)
			closeErr := w.Close()
			if err != nil {
				t.Fatal(err)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			if tt.transcript {
				if diff := gocmp.Diff("", transcriptCWD(f)); diff != "" {
					t.Error(diff)
				}
				return
			}
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			value, ok := readGitPointer(f, info)
			if diff := gocmp.Diff(false, ok); diff != "" {
				t.Error(diff)
			}
			if diff := gocmp.Diff("", value); diff != "" {
				t.Error(diff)
			}
		})
	}
}
