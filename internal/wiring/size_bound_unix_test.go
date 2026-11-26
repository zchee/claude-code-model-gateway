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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeValidOversizedJSON(t *testing.T, path, body string) int64 {
	t.Helper()
	const targetSize = (16 << 20) + 64
	check(t, os.MkdirAll(filepath.Dir(path), 0o700))
	f, err := os.Create(path)
	check(t, err)
	defer func() { check(t, f.Close()) }()
	_, err = f.WriteString(body)
	check(t, err)
	pad := bytes.Repeat([]byte(" "), 64<<10)
	written := int64(len(body))
	for written < targetSize {
		n := min(int64(len(pad)), targetSize-written)
		_, err = f.Write(pad[:n])
		check(t, err)
		written += n
	}
	return written
}

func assertBoundError(t *testing.T, context, file string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected size-bound error for %s, got nil", context, file)
	}
	msg := err.Error()
	if !strings.Contains(msg, file) {
		t.Fatalf("%s: error %q does not name file %q", context, msg, file)
	}
	if !strings.Contains(msg, "16777216") && !strings.Contains(msg, "16 MiB") {
		t.Fatalf("%s: error %q does not name size bound", context, msg)
	}
}

func TestOversizedSettingsAndRegistryReturnBoundError(t *testing.T) {
	t.Run("error: oversized valid settings file errors through resolution and writable scans", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.user, map[string]any{base: direct})
		writeValidOversizedJSON(t, f.local, `{"env":{"ANTHROPIC_BASE_URL":"https://personal.example"}}`)
		regBefore := encoded(t, map[string]any{"projects": []string{f.Project}})
		put(t, f.registry, regBefore)

		_, err := f.EffectiveBaseURL()
		assertBoundError(t, "EffectiveBaseURL", f.local, err)

		_, err = f.IsWired()
		assertBoundError(t, "IsWired", f.local, err)

		_, err = f.WiredMode()
		assertBoundError(t, "WiredMode", f.local, err)

		err = f.WriteEnv("project", "default", false)
		assertBoundError(t, "WriteEnv", f.local, err)

		_, _, err = f.Projects(false)
		assertBoundError(t, "Projects(false)", f.local, err)
		equal(t, regBefore, text(t, f.registry))

		err = f.RecordProject()
		assertBoundError(t, "RecordProject", f.local, err)
		equal(t, regBefore, text(t, f.registry))

		_, err = f.SyncWindow([]string{f.local}, new(400000), new(500000))
		assertBoundError(t, "SyncWindow", f.local, err)
	})

	t.Run("error: oversized valid registry file returns error and stays untouched", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.local, map[string]any{base: direct})
		size := writeValidOversizedJSON(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))

		_, _, err := f.Projects(true)
		assertBoundError(t, "Projects(true)", f.registry, err)

		_, _, err = f.Projects(false)
		assertBoundError(t, "Projects(false)", f.registry, err)

		_, err = f.SyncPins()
		assertBoundError(t, "SyncPins", f.registry, err)

		_, _, err = f.PinDisagreements()
		assertBoundError(t, "PinDisagreements", f.registry, err)

		_, _, err = f.Reconcile(direct, false)
		assertBoundError(t, "Reconcile", f.registry, err)

		err = f.RecordProject()
		assertBoundError(t, "RecordProject", f.registry, err)

		err = f.Repair(nil)
		assertBoundError(t, "Repair", f.registry, err)

		info, err := os.Stat(f.registry)
		check(t, err)
		equal(t, size, info.Size())
	})
}
