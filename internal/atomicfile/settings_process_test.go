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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSettingsReplacement(t *testing.T) {
	const crashArg = "--crash-before-settings-rename"
	const fifoArg = "--replace-settings-fifo"
	const crashStatus = 42
	const fifoTimeoutStatus = 43
	newContents := func([]byte) ([]byte, error) { return []byte(newState), nil }
	switch os.Args[len(os.Args)-1] {
	case fifoArg:
		// Only the FIFO operation is on this deadline; TestMain has finished its venue checks.
		deadline := time.AfterFunc(5*time.Second, func() { os.Exit(fifoTimeoutStatus) })
		defer deadline.Stop()
		if err := ReplaceSettings("settings.fifo", func([]byte) ([]byte, error) {
			t.Fatal("edit callback received a FIFO")
			return nil, nil
		}); err == nil {
			t.Fatal("accepted FIFO as a settings file")
		}
		return
	case crashArg:
		path, err := filepath.Abs("state.json")
		if err != nil {
			t.Fatal(err)
		}
		w := newWriter()
		w.commit = func(file preparedFile, to string) error {
			checkStagedSibling(t, file, to, path)
			checkContents(t, file.Name(), newState)
			if !t.Failed() {
				os.Exit(crashStatus)
			}
			return errors.New("staged settings incomplete")
		}
		t.Fatalf("ReplaceSettings returned before simulated crash: %v", w.replaceSettings(path, newContents, nil))
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
		edits := 0
		err = ReplaceSettings(link, func(current []byte) ([]byte, error) {
			edits++
			if edits == 1 {
				writeFixture(t, path, "{\"old\":true,\"user\":7}\n")
			}
			return bytes.Replace(current, []byte(`"old"`), []byte(`"new"`), 1), nil
		})
		if err != nil || edits != 2 {
			t.Errorf("ReplaceSettings through symlink: edits=%d err=%v, want successful merge on retry", edits, err)
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
		checkContents(t, path, "{\"new\":true,\"user\":7}\n")
	})

	t.Run("ConcurrentEdit", func(t *testing.T) {
		path := stateFile(t)
		const before = "{\"old\":true,\"user\":1}\n"
		const concurrent = "{\"old\":true,\"user\":7}\n"
		writeFixture(t, path, before)
		stamp := time.Now().Truncate(time.Second)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		w := newWriter()
		stages := 0
		// An editor racing after the final read can still win or lose by design.
		// Here the edit is already on disk before that read, so it must survive.
		afterStage := func(file preparedFile, target string) {
			stages++
			checkStagedSibling(t, file, target, path)
			if stages == 1 {
				checkContents(t, file.Name(), "{\"new\":true,\"user\":1}\n")
				writeFixture(t, path, concurrent)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
		}
		calls := 0
		err := w.replaceSettings(path, func(current []byte) ([]byte, error) {
			calls++
			return bytes.Replace(current, []byte(`"old"`), []byte(`"new"`), 1), nil
		}, afterStage)
		if err != nil || calls != 2 || stages != 2 {
			t.Errorf("concurrent edit: calls=%d stages=%d err=%v, want two merged and staged versions and success", calls, stages, err)
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
		path := filepath.Join(t.TempDir(), "settings.fifo")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		original, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestSettingsReplacement$", "--", fifoArg)
		cmd.Dir = filepath.Dir(path)
		if output, err := cmd.CombinedOutput(); err != nil {
			if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == fifoTimeoutStatus {
				t.Fatal("ReplaceSettings blocked on FIFO after the venue guard passed")
			}
			t.Fatalf("FIFO helper: %v (context: %v): %s", err, ctx.Err(), output)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 || !os.SameFile(original, info) {
			t.Fatalf("refused FIFO changed: stat=%v err=%v", info, err)
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
