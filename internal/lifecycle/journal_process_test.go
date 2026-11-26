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

package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

func TestTrimPreservesJournalDescriptor(t *testing.T) {
	// This test is sequential because the creation mask belongs to the test process.
	old := unix.Umask(0o022)
	defer unix.Umask(old)
	f := filesFor(t.TempDir())
	logs := filepath.Join(f.StateDir, "logs")
	noError(t, os.Mkdir(logs, 0o777))
	path := filepath.Join(logs, "lifecycle.jsonl")
	const kept = `{"event":"kept"}` + "\n"
	const dropped = `{"event":"dropped"}` + "\n"
	const late = `{"event":"late"}` + "\n"
	noError(t, atomicfile.WriteFile(path, []byte(dropped+strings.Repeat(kept, 199)), 0o600))
	appender, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	noError(t, err)
	defer func() { noError(t, appender.Close()) }()
	before, err := appender.Stat()
	noError(t, err)
	f.Record("tick", nil)
	_, err = appender.WriteString(late)
	noError(t, err)
	after, err := os.Stat(path)
	noError(t, err)
	equal(t, true, os.SameFile(before, after))
	equal(t, os.FileMode(0o600), after.Mode().Perm())
	equal(t, strings.Repeat(kept, 199)+`{"at":"1970-01-01T00:00:00.123Z","event":"tick"}`+"\n"+late, bytesAt(t, path))
}
