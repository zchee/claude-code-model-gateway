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

package procinfo

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

// These errors distinguish invalid input, refused directory entries, and failures
// after a valid authoritative record was read or published.
const (
	ErrStateRoot     = probeError("state root must be a clean absolute path")
	ErrRecordEntry   = probeError("record entry is not a regular file")
	ErrStartedAt     = probeError("invalid process start timestamp")
	ErrMirrorRepair  = probeError("cannot repair process ID mirror")
	ErrMirrorPublish = probeError("record published but process ID mirror not updated")
)

func openRecordDir(root, name string) (*atomicfile.Dir, string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, "", ErrStateRoot
	}
	if !filepath.IsLocal(name) || name == "." || filepath.Base(name) != name {
		return nil, "", fs.ErrInvalid
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, "", ErrUnsupported
	}
	dir, err := atomicfile.OpenDir(root)
	return dir, name + ".pid", err
}

func (r Record) validate(root string) error {
	start, err := time.Parse(time.ANSIC, r.StartedAt)
	if err != nil || LegacyStart(start) != r.StartedAt {
		return ErrStartedAt
	}
	if r.PID <= 0 || r.PID > math.MaxInt32 || r.StateRoot != root || r.Command == "" || r.Role == "" || r.Generation == "" || r.Version == "" || !filepath.IsAbs(r.Image.Path) || r.Image.Inode == 0 {
		return fs.ErrInvalid
	}
	return nil
}

type recordIO struct{ beforeMirror func() }

func readRecordEntry(dir *atomicfile.Dir, name string) ([]byte, error) {
	data, err := dir.ReadFile(name)
	if _, ok := errors.AsType[*atomicfile.NonRegularError](err); ok {
		return nil, fmt.Errorf("%w: %w", ErrRecordEntry, err)
	}
	return data, err
}

func replaceRecordEntry(dir *atomicfile.Dir, name string, data []byte) error {
	// Dir.WriteFile replaces a symlink rather than following it. Records refuse
	// such entries altogether, including when replacing an existing record.
	if _, err := readRecordEntry(dir, name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return dir.WriteFile(name, data, 0o600)
}

// WriteRecord atomically publishes name.pid.json before updating its decimal mirror.
// ErrMirrorPublish means the JSON was committed and the mirror was not.
func WriteRecord(root, name string, record Record) error {
	return (recordIO{}).write(root, name, record, replaceRecordEntry)
}

func (r recordIO) write(root, name string, record Record, replace func(*atomicfile.Dir, string, []byte) error) error {
	dir, mirror, err := openRecordDir(root, name)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := record.validate(root); err != nil {
		return err
	}
	// Encode the typed record directly: a JavaScript number cannot represent every inode.
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := replace(dir, mirror+".json", data); err != nil {
		return err
	}
	if r.beforeMirror != nil {
		r.beforeMirror()
	}
	if err := replace(dir, mirror, []byte(strconv.Itoa(record.PID))); err != nil {
		return fmt.Errorf("%w: %w", ErrMirrorPublish, err)
	}
	return nil
}

// ReadRecord reads the authoritative JSON and repairs a stale or missing mirror.
// A repair racing a writer may leave an older mirror until the next read or write;
// the JSON remains authoritative.
// It returns the valid record alongside ErrMirrorRepair if repair is impossible.
// Symlinks and special files in either position are refused with ErrRecordEntry.
func ReadRecord(root, name string) (Record, error) {
	return (recordIO{}).read(root, name)
}

func (r recordIO) read(root, name string) (Record, error) {
	dir, mirror, err := openRecordDir(root, name)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = dir.Close() }()
	data, err := readRecordEntry(dir, mirror+".json")
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, err
	}
	if err := record.validate(root); err != nil {
		return Record{}, err
	}
	if r.beforeMirror != nil {
		r.beforeMirror()
	}
	old, err := readRecordEntry(dir, mirror)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return record, fmt.Errorf("%w: %w", ErrMirrorRepair, err)
	}
	if want := strconv.Itoa(record.PID); string(old) != want {
		if err := dir.WriteFile(mirror, []byte(want), 0o600); err != nil {
			return record, fmt.Errorf("%w: %w", ErrMirrorRepair, err)
		}
	}
	return record, nil
}
