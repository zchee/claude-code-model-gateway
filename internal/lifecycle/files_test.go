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

package lifecycle_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

func filesFor(state string) lifecycle.Files {
	return lifecycle.Files{StateDir: state, Now: func() time.Time { return time.Unix(0, 123456789).In(time.FixedZone("offset", 3600)) }}
}

func TestStopMarker(t *testing.T) {
	files := filesFor(filepath.Join(t.TempDir(), "state"))
	noError(t, files.Resume())
	equal(t, false, files.Stopped())
	noError(t, files.Stop())
	equal(t, true, files.Stopped())
	equal(t, true, files.Stopped())
	path := filepath.Join(files.StateDir, "intentionally-stopped.json")
	equal(t, `{"stoppedAt":"1970-01-01T00:00:00.123Z"}`, bytesAt(t, path))
	info, err := os.Stat(path)
	noError(t, err)
	equal(t, fs.FileMode(0o600), info.Mode().Perm())
	noError(t, atomicfile.WriteFile(path, []byte("not JSON"), 0o600))
	equal(t, true, files.Stopped())
	noError(t, files.Resume())
	equal(t, false, files.Stopped())
	mustBeAbsent(t, path)
	noError(t, files.Resume())
}

func TestStartupFailure(t *testing.T) {
	tests := map[string]struct {
		generation *string
		wire       string
	}{"success: unspecified generation": {nil, `{"supervisorPid":41,"message":"owned failure"}`}, "success: supplied generation": {new("birth"), `{"supervisorPid":41,"generation":"birth","message":"owned failure"}`}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := filesFor(t.TempDir())
			noError(t, f.WriteStartupFailure(41, tc.generation, "owned failure"))
			equal(t, tc.wire, bytesAt(t, filepath.Join(f.StateDir, "shim-supervisor-failure.txt")))
			msg, ok := f.StartupFailure(41, "")
			equal(t, true, ok)
			equal(t, "owned failure", msg)
			_, ok = f.StartupFailure(42, "")
			equal(t, false, ok)
			_, ok = f.StartupFailure(41, "different-generation")
			equal(t, false, ok)
			if tc.generation != nil {
				msg, ok = f.StartupFailure(41, *tc.generation)
				equal(t, true, ok)
				equal(t, "owned failure", msg)
			}
		})
	}
}

func TestUnreadableStartupFailure(t *testing.T) {
	tests := map[string]struct {
		create bool
		data   string
	}{"error: absent file": {}, "error: malformed JSON": {true, `{"supervisorPid":41,"message":"partial"`}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := filesFor(t.TempDir())
			if tc.create {
				noError(t, atomicfile.WriteFile(filepath.Join(f.StateDir, "shim-supervisor-failure.txt"), []byte(tc.data), 0o600))
			}
			_, ok := f.StartupFailure(41, "")
			equal(t, false, ok)
		})
	}
}

func TestLifecycleLine(t *testing.T) {
	tests := map[string]struct {
		fields map[string]any
		line   string
	}{
		"success: absent fields": {nil, `{"at":"1970-01-01T00:00:00.123Z","event":"worker-exit"}`},
		"success: allowlisted fields only": {map[string]any{"pid": 41, "component": "supervisor", "startedAt": "Mon Jan  2 15:04:05 2006", "outcome": "<lost>", "errorType": "EOF", "signal": nil, "exitCode": 0, "child": struct {
			Component string `json:"component"`
			PID       int    `json:"pid"`
		}{"worker", 10}, "supervisorPid": 99, "extra": "omit"}, `{"at":"1970-01-01T00:00:00.123Z","event":"worker-exit","component":"supervisor","pid":41,"startedAt":"Mon Jan  2 15:04:05 2006","outcome":"<lost>","errorType":"EOF","signal":null,"exitCode":0,"child":{"component":"worker","pid":10}}`},
		"success: optional nulls": {map[string]any{"startedAt": nil, "outcome": nil, "errorType": nil, "signal": "SIGTERM", "exitCode": nil, "child": nil}, `{"at":"1970-01-01T00:00:00.123Z","event":"worker-exit","signal":"SIGTERM","exitCode":null}`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := filesFor(t.TempDir())
			f.Record("worker-exit", tc.fields)
			equal(t, tc.line+"\n", bytesAt(t, filepath.Join(f.StateDir, "logs", "lifecycle.jsonl")))
		})
	}
}

func TestSingleRecordAppend(t *testing.T) {
	f := filesFor(t.TempDir())
	calls := 0
	f.AppendFile = func(path string, data []byte, mode fs.FileMode) error {
		calls++
		equal(t, filepath.Join(f.StateDir, "logs", "lifecycle.jsonl"), path)
		equal(t, fs.FileMode(0o666), mode)
		equal(t, `{"at":"1970-01-01T00:00:00.123Z","event":"tick"}`+"\n", string(data))
		return fs.ErrPermission
	}
	f.Record("tick", nil)
	equal(t, 1, calls)
}

func TestLifecycleBound(t *testing.T) {
	tests := map[string]struct{ count int }{"success: below trim threshold": {199}, "success: at trim threshold": {200}, "success: oversized log": {208}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := filesFor(t.TempDir())
			dir := filepath.Join(f.StateDir, "logs")
			noError(t, os.Mkdir(dir, 0o700))
			path := filepath.Join(dir, "lifecycle.jsonl")
			lines := make([]string, tc.count)
			for i := range lines {
				lines[i] = fmt.Sprintf(`{"index":%d}`, i)
			}
			lines[len(lines)-1] = "not JSON"
			prefix := strings.Join(lines, "\r\n\r\n") + "\r\n"
			noError(t, atomicfile.WriteFile(path, []byte(prefix), 0o600))
			f.Record("tick", nil)
			if tc.count >= 200 {
				prefix = strings.Join(lines[len(lines)-199:], "\n") + "\n"
			}
			const last = `{"at":"1970-01-01T00:00:00.123Z","event":"tick"}`
			wantLines := strings.Split(prefix+last+"\n", "\n")
			gotLines := strings.Split(bytesAt(t, path), "\n")
			equal(t, len(wantLines), len(gotLines))
			for i := range wantLines {
				equal(t, wantLines[i], gotLines[i])
			}
			records := f.Records()
			want := append([]string{}, lines[max(0, len(lines)-199):len(lines)-1]...)
			want = append(want, last)
			var got []string
			for _, record := range records {
				got = append(got, string(record))
			}
			equal(t, want, got)
		})
	}
}

func TestUnavailableLog(t *testing.T) {
	f := filesFor(t.TempDir())
	noError(t, atomicfile.WriteFile(filepath.Join(f.StateDir, "logs"), []byte("not a directory"), 0o600))
	f.Record("tick", nil)
	equal(t, 0, len(f.Records()))
	equal(t, "not a directory", bytesAt(t, filepath.Join(f.StateDir, "logs")))
}
