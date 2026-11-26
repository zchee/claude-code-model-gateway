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
	"os"
	"path/filepath"
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
	// AppendFile appends one complete journal record. Nil uses atomicfile.AppendFile.
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

func (f Files) write(name string, value any, mode fs.FileMode) error {
	if !validStateDir(f.StateDir) {
		return fs.ErrInvalid
	}
	data, err := compactRecord(value)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(f.StateDir, name), data, mode)
}

// Stopped reports whether the intentional stop marker exists, without consuming it.
func (f Files) Stopped() bool {
	if !validStateDir(f.StateDir) {
		return false
	}
	_, err := os.Stat(filepath.Join(f.StateDir, stopMarker))
	return err == nil
}

// Stop records an intentional stop.
func (f Files) Stop() error {
	if !validStateDir(f.StateDir) {
		return fs.ErrInvalid
	}
	if err := os.MkdirAll(f.StateDir, 0o777); err != nil {
		return err
	}
	return f.write(stopMarker, struct {
		StoppedAt string `json:"stoppedAt"`
	}{f.timestamp()}, 0o600)
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
	Generation *string `json:"generation,omitzero"`
	Message    *string `json:"message"`
}

// WriteStartupFailure records a supervisor failure, omitting an unspecified generation.
func (f Files) WriteStartupFailure(pid int, generation *string, message string) error {
	return f.write(failureFile, failure{&pid, generation, &message}, 0o666)
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
	if err != nil || !decodeRecord(data, &v) || v.PID == nil || *v.PID != pid || v.Message == nil || generation != "" && (v.Generation == nil || *v.Generation != generation) {
		return "", false
	}
	return *v.Message, true
}

func (f Files) record(event string, fields map[string]any) ([]byte, error) {
	// A present null is significant for some fields; a missing key is never emitted.
	field := func(name string) *any {
		if v, ok := fields[name]; ok {
			return &v
		}
		return nil
	}
	var child jsontext.Value
	if v := fields["child"]; v != nil {
		b, err := compactRecord(v)
		if err != nil {
			return nil, err
		}
		switch string(b) {
		case "null", "false", `""`, "0":
		default:
			child = b
		}
	}
	return compactRecord(struct {
		At        string         `json:"at"`
		Event     string         `json:"event"`
		Component *any           `json:"component,omitzero"`
		PID       *any           `json:"pid,omitzero"`
		StartedAt any            `json:"startedAt,omitzero"`
		Outcome   any            `json:"outcome,omitzero"`
		ErrorType any            `json:"errorType,omitzero"`
		Signal    *any           `json:"signal,omitzero"`
		ExitCode  *any           `json:"exitCode,omitzero"`
		Child     jsontext.Value `json:"child,omitzero"`
	}{f.timestamp(), event, field("component"), field("pid"), fields["startedAt"], fields["outcome"], fields["errorType"], field("signal"), field("exitCode"), child})
}

// Record appends an allowlisted lifecycle event on a best-effort basis.
func (f Files) Record(event string, fields map[string]any) {
	if !validStateDir(f.StateDir) {
		return
	}
	logs := filepath.Join(f.StateDir, "logs")
	if err := os.MkdirAll(logs, 0o777); err != nil {
		return
	}
	dir, err := atomicfile.OpenDir(logs)
	if err != nil {
		return
	}
	defer func() { _ = dir.Close() }()
	if data, err := dir.ReadFile(journal); err == nil {
		const bound = 200
		lines := bytes.Split(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), []byte("\n"))
		lines = slices.DeleteFunc(lines, func(line []byte) bool { return len(line) == 0 })
		if len(lines) >= bound {
			trimmed := bytes.Join(lines[len(lines)-(bound-1):], []byte("\n"))
			if err := dir.WriteFile(journal, append(trimmed, '\n'), 0o666); err != nil {
				return
			}
		}
	}
	data, err := f.record(event, fields)
	if err != nil {
		return
	}
	appendFile := f.AppendFile
	if appendFile == nil {
		appendFile = atomicfile.AppendFile
	}
	_ = appendFile(filepath.Join(logs, journal), append(data, '\n'), 0o666)
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
