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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Files locates lifecycle evidence under StateDir and optionally supplies the timestamp clock.
type Files struct {
	StateDir string
	Now      func() time.Time
	// AppendFile overrides the single append to the open logs directory.
	AppendFile func(path string, data []byte, mode fs.FileMode) error
}

const (
	stopMarker  = "intentionally-stopped.json"
	failureFile = "shim-supervisor-failure.txt"
	journal     = "lifecycle.jsonl"
)

func validStateDir(path string) bool { return filepath.IsAbs(path) && path == filepath.Clean(path) }

func compactRecord(value any) (jsontext.Value, error) {
	b, err := jsonx.EncodeState(value)
	if err != nil {
		return nil, err
	}
	data := jsontext.Value(b)
	err = data.Compact()
	return data, err
}

func decodeRecord(data []byte, dst any) bool {
	// Read legacy JSON.parse values, including repeated names and integral exponents.
	v, err := jsonx.DecodeWire(data)
	if err != nil {
		return false
	}
	b, err := jsonx.EncodeWire(v)
	return err == nil && json.Unmarshal(b, dst) == nil
}

func (f Files) timestamp() string { return clock(f.Now)().UTC().Format("2006-01-02T15:04:05.000Z") }

// Stopped reports whether the intentional stop marker exists, without consuming it.
func (f Files) Stopped() bool {
	if !validStateDir(f.StateDir) {
		return false
	}
	_, err := os.Stat(filepath.Join(f.StateDir, stopMarker))
	return err == nil
}

// StopMarkerPresent distinguishes a missing stop marker from a failure to inspect the state root.
func (f Files) StopMarkerPresent() (bool, error) {
	panic("not implemented")
}

// Stop records an intentional stop.
func (f Files) Stop() error {
	if !validStateDir(f.StateDir) {
		return fs.ErrInvalid
	}
	dir, err := atomicfile.OpenDirCreate(f.StateDir, 0o777)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	data, err := compactRecord(struct {
		StoppedAt string `json:"stoppedAt"`
	}{f.timestamp()})
	if err != nil {
		return err
	}
	return dir.WriteFile(stopMarker, data, 0o600)
}

// Resume removes the marker if present.
func (f Files) Resume() error {
	if !validStateDir(f.StateDir) {
		return fs.ErrInvalid
	}
	err := syscall.Unlink(filepath.Join(f.StateDir, stopMarker))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

type failure struct {
	PID        *int    `json:"supervisorPid"`
	Generation any     `json:"generation,omitzero"`
	Message    *string `json:"message"`
}

// WriteStartupFailure records a supervisor failure, omitting an unspecified generation.
func (f Files) WriteStartupFailure(pid int, generation *string, message string) error {
	v := failure{PID: &pid, Message: &message}
	if generation != nil {
		v.Generation = *generation
	}
	if !validStateDir(f.StateDir) {
		return fs.ErrInvalid
	}
	data, err := compactRecord(v)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(f.StateDir, failureFile), data, 0o666)
}

// StartupFailure returns the matching supervisor's message; an empty generation matches any.
func (f Files) StartupFailure(pid int, generation string) (string, bool) {
	if !validStateDir(f.StateDir) {
		return "", false
	}
	dir, err := atomicfile.OpenDir(f.StateDir)
	if err != nil {
		return "", false
	}
	defer func() { _ = dir.Close() }()
	data, err := dir.ReadFile(failureFile)
	var v failure
	if err != nil || !decodeRecord(data, &v) || v.PID == nil || *v.PID != pid || v.Message == nil || generation != "" && v.Generation != generation {
		return "", false
	}
	return *v.Message, true
}

// truthyChild applies the JavaScript test before JSON.stringify converts infinities to null.
func truthyChild(value any) bool {
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Invalid:
		return false
	case reflect.Float32, reflect.Float64:
		return v.Float() != 0 && !math.IsNaN(v.Float())
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Pointer:
		return !v.IsZero()
	default:
		return true
	}
}

func (f Files) record(event string, fields map[string]any) ([]byte, error) {
	// A present null is significant for some fields; a missing key is never emitted.
	field := func(name string) *any {
		if v, ok := fields[name]; ok {
			return &v
		}
		return nil
	}
	var child any
	if v := fields["child"]; truthyChild(v) {
		child = v
	}
	return compactRecord(struct {
		At        string `json:"at"`
		Event     string `json:"event"`
		Component *any   `json:"component,omitzero"`
		PID       *any   `json:"pid,omitzero"`
		StartedAt any    `json:"startedAt,omitzero"`
		Outcome   any    `json:"outcome,omitzero"`
		ErrorType any    `json:"errorType,omitzero"`
		Signal    *any   `json:"signal,omitzero"`
		ExitCode  *any   `json:"exitCode,omitzero"`
		Child     any    `json:"child,omitzero"`
	}{f.timestamp(), event, field("component"), field("pid"), fields["startedAt"], fields["outcome"], fields["errorType"], field("signal"), field("exitCode"), child})
}

func trimJournal(dir *atomicfile.Dir, data []byte) error {
	const bound = 200
	lines := bytes.Split(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), []byte("\n"))
	lines = slices.DeleteFunc(lines, func(line []byte) bool { return len(line) == 0 })
	if len(lines) < bound {
		return nil
	}
	trimmed := bytes.Join(lines[len(lines)-(bound-1):], []byte("\n"))
	return dir.RewriteFile(journal, append(trimmed, '\n'))
}

// Record appends an allowlisted lifecycle event on a best-effort basis.
// Trimming rewrites the existing regular file in place. If the journal disappears
// or becomes a symlink before the rewrite, Record neither recreates it nor follows
// the link, and skips this event. An interrupted trim can leave an empty or partial
// journal. The 200-line bound applies only to sequential writers: trimming and
// appending are not serialized. No ownership decision depends on the journal.
func (f Files) Record(event string, fields map[string]any) {
	if !validStateDir(f.StateDir) {
		return
	}
	logs := filepath.Join(f.StateDir, "logs")
	dir, err := atomicfile.OpenDirCreate(logs, 0o777)
	if err != nil {
		return
	}
	defer func() { _ = dir.Close() }()
	if data, err := dir.ReadFile(journal); err == nil && trimJournal(dir, data) != nil {
		return
	}
	data, err := f.record(event, fields)
	if err != nil {
		return
	}
	data = append(data, '\n')
	if f.AppendFile != nil {
		_ = f.AppendFile(filepath.Join(logs, journal), data, 0o666)
		return
	}
	_ = dir.AppendFile(journal, data, 0o666)
}

// Records returns parseable lifecycle records in file order.
func (f Files) Records() []jsontext.Value {
	if !validStateDir(f.StateDir) {
		return nil
	}
	dir, err := atomicfile.OpenDir(filepath.Join(f.StateDir, "logs"))
	if err != nil {
		return nil
	}
	defer func() { _ = dir.Close() }()
	data, err := dir.ReadFile(journal)
	if err != nil {
		return nil
	}
	var records []jsontext.Value
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		v := jsontext.Value(bytes.TrimSuffix(line, []byte("\r")))
		if v.IsValid(jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)) {
			records = append(records, v)
		}
	}
	return records
}
