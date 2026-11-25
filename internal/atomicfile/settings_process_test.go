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

//go:build process

package atomicfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsReplacement(t *testing.T) {
	const crashArg = "--crash-before-settings-rename"
	const crashStatus = 42
	newContents := func([]byte) ([]byte, error) { return []byte(newState), nil }
	if os.Args[len(os.Args)-1] == crashArg {
		path, err := filepath.Abs("state.json")
		if err != nil {
			t.Fatal(err)
		}
		interceptRename(t, func(from, to string) error {
			if to != path || from == path || filepath.Dir(from) != filepath.Dir(path) {
				t.Fatalf("replacement is not a sibling rename: %s -> %s", from, to)
			}
			checkContents(t, from, newState)
			if !t.Failed() {
				os.Exit(crashStatus)
			}
			return errors.New("staged settings incomplete")
		})
		t.Fatalf("ReplaceSettings returned before simulated crash: %v", ReplaceSettings(path, newContents))
	}

	t.Run("CrashBeforeRename", func(t *testing.T) {
		path := stateFile(t)
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestSettingsReplacement$", "--", crashArg)
		cmd.Dir = filepath.Dir(path)
		output, err := cmd.CombinedOutput()
		if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != crashStatus {
			t.Errorf("writer did not exit at rename: %v: %s", err, output)
		}
		checkContents(t, path, oldState)
	})

	t.Run("SymlinkTargetAndMode", func(t *testing.T) {
		path := stateFile(t)
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		original, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(filepath.Dir(path), "settings.json")
		if err := os.Symlink(filepath.Base(path), link); err != nil {
			t.Fatal(err)
		}
		if err := ReplaceSettings(link, newContents); err != nil {
			t.Fatal(err)
		}
		dest, err := os.Readlink(link)
		if err != nil || dest != filepath.Base(path) {
			t.Errorf("settings symlink changed: target=%q, err=%v", dest, err)
		}
		replaced, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(original, replaced) || replaced.Mode().Perm() != 0o640 {
			t.Errorf("want replacement inode with mode 0640; same inode=%t mode=%v", os.SameFile(original, replaced), replaced.Mode())
		}
		checkContents(t, path, newState)
	})

	t.Run("ConcurrentEdit", func(t *testing.T) {
		path := stateFile(t)
		const concurrent = "{\"old\":true,\"user\":7}\n"
		calls := 0
		err := ReplaceSettings(path, func(current []byte) ([]byte, error) {
			calls++
			if calls == 1 {
				writeFixture(t, path, concurrent)
			}
			return bytes.Replace(current, []byte(`"old"`), []byte(`"new"`), 1), nil
		})
		if err != nil || calls != 2 {
			t.Errorf("concurrent edit: calls=%d err=%v, want two calls and success", calls, err)
		}
		checkContents(t, path, "{\"new\":true,\"user\":7}\n")
	})

	t.Run("RepeatedConflict", func(t *testing.T) {
		path := stateFile(t)
		calls := 0
		err := ReplaceSettings(path, func([]byte) ([]byte, error) {
			calls++
			if calls > 3 {
				t.Fatal("more than three attempts to overwrite concurrent edits")
			}
			writeFixture(t, path, fmt.Sprintf("{\"user\":%d}\n", calls))
			return []byte(newState), nil
		})
		if err == nil || !strings.Contains(err.Error(), filepath.Base(path)) || calls != 3 {
			t.Errorf("persistent conflict: calls=%d err=%v, want three attempts and an error naming the file", calls, err)
		}
		checkContents(t, path, "{\"user\":3}\n")
	})

	t.Run("NonregularTarget", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := ReplaceSettings(path, newContents); err == nil {
			t.Fatal("replaced a directory as if it were a settings file")
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Fatalf("refused directory changed: stat=%v err=%v", info, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, path, oldState)
		if err := ReplaceSettings(path, newContents); err != nil {
			t.Fatalf("ordinary file at the same path: %v", err)
		}
		checkContents(t, path, newState)
	})
}
