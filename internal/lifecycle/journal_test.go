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

package lifecycle

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

func TestStopKeepsCreatedDirectory(t *testing.T) {
	base := t.TempDir()
	state, moved := filepath.Join(base, "state"), filepath.Join(base, "moved")
	f := Files{StateDir: state, Now: func() time.Time {
		if err := os.Rename(state, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(state, 0o700); err != nil {
			t.Fatal(err)
		}
		return time.Unix(0, 0)
	}}
	if err := f.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, stopMarker)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(state, stopMarker)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("marker at replacement path: got %v, want file not found", err)
	}
}

func TestTrimVanishedJournal(t *testing.T) {
	logs := t.TempDir()
	dir, err := atomicfile.OpenDir(logs)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.WriteFile(journal, bytes.Repeat([]byte("{}\n"), 200), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := dir.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, journal)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := trimJournal(dir, data); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("trim after unlink: got %v, want file not found", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("journal after failed trim: got %v, want file not found", err)
	}
}
