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
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeOversizedValidJSON(t *testing.T, path string) int64 {
	t.Helper()
	const targetSize = (16 << 20) + 64
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	const body = `{"env":{"KEEP":"yes"}}`
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	pad := bytes.Repeat([]byte(" "), 64<<10)
	written := int64(len(body))
	for written < targetSize {
		n := min(int64(len(pad)), targetSize-written)
		if _, err := f.Write(pad[:n]); err != nil {
			t.Fatal(err)
		}
		written += n
	}
	return written
}

func TestReplaceSettingsInfoSizeBound(t *testing.T) {
	t.Run("error: existing oversized file refused on before read", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.local.json")
		size := writeOversizedValidJSON(t, path)
		called := false
		err := ReplaceSettingsInfo(path, func([]byte, fs.FileInfo) ([]byte, error) {
			called = true
			return []byte(`{"overwritten":true}`), nil
		})
		if err == nil {
			t.Fatal("expected size-bound error on oversized file, got nil")
		}
		if !strings.Contains(err.Error(), path) || (!strings.Contains(err.Error(), "16777216") && !strings.Contains(err.Error(), "16 MiB")) {
			t.Fatalf("error %q should name path %q and size bound", err.Error(), path)
		}
		if called {
			t.Fatal("edit callback must not run on oversized file")
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Size() != size {
			t.Fatalf("oversized file size changed: got %d want %d", info.Size(), size)
		}
	})

	t.Run("error: file growing past bound before verification read is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.local.json")
		writeFixture(t, path, "{}")
		var grownSize int64
		grew := false
		calls := 0
		err := newWriter().replaceSettingsInfo(path, func([]byte, fs.FileInfo) ([]byte, error) {
			calls++
			return []byte(`{"edited":true}`), nil
		}, func(preparedFile, string) {
			if grew {
				return
			}
			grew = true
			grownSize = writeOversizedValidJSON(t, path)
		})
		if err == nil {
			t.Fatal("expected size-bound error when file grew past bound, got nil")
		}
		if !strings.Contains(err.Error(), path) || (!strings.Contains(err.Error(), "16777216") && !strings.Contains(err.Error(), "16 MiB")) {
			t.Fatalf("error %q should name path %q and size bound", err.Error(), path)
		}
		if calls != 1 {
			t.Fatalf("expected 1 edit call before growth refusal, got %d", calls)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Size() != grownSize {
			t.Fatalf("grown file was modified: got %d want %d", info.Size(), grownSize)
		}
		checkOnlyTarget(t, path)
	})
}
