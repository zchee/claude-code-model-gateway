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
	"os"
	"path/filepath"
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

func mirrorPath(root, name string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", ErrStateRoot
	}
	if !filepath.IsLocal(name) || name == "." || filepath.Base(name) != name {
		return "", fs.ErrInvalid
	}
	return filepath.Join(root, name+".pid"), nil
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

func replaceRecordFile(path string, data []byte) error {
	return atomicfile.WriteFile(path, data, 0o600)
}

// WriteRecord atomically publishes name.pid.json before updating its decimal mirror.
// ErrMirrorPublish means the JSON was committed and the mirror was not.
func WriteRecord(root, name string, record Record) error {
	return writeRecord(root, name, record, replaceRecordFile)
}

func writeRecord(root, name string, record Record, replace func(string, []byte) error) error {
	mirror, err := mirrorPath(root, name)
	if err != nil {
		return err
	}
	if err := record.validate(root); err != nil {
		return err
	}
	dir, err := openRecordRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := checkRecordEntry(dir, filepath.Base(mirror)+".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Encode the typed record directly: a JavaScript number cannot represent every inode.
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := replace(mirror+".json", data); err != nil {
		return err
	}
	if err := checkRecordEntry(dir, filepath.Base(mirror)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrMirrorPublish, err)
	}
	if err := replace(mirror, []byte(strconv.Itoa(record.PID))); err != nil {
		return fmt.Errorf("%w: %w", ErrMirrorPublish, err)
	}
	return nil
}

// ReadRecord reads the authoritative JSON and repairs a stale or missing mirror.
// It returns the valid record alongside ErrMirrorRepair if repair is impossible.
// Symlinks and special files in either position are refused with ErrRecordEntry.
func ReadRecord(root, name string) (Record, error) {
	mirror, err := mirrorPath(root, name)
	if err != nil {
		return Record{}, err
	}
	dir, err := openRecordRoot(root)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = dir.Close() }()
	data, err := readRecordEntry(dir, filepath.Base(mirror)+".json")
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
	old, err := readRecordEntry(dir, filepath.Base(mirror))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return record, fmt.Errorf("%w: %w", ErrMirrorRepair, err)
	}
	if want := strconv.Itoa(record.PID); string(old) != want {
		if err := replaceRecordFile(mirror, []byte(want)); err != nil {
			return record, fmt.Errorf("%w: %w", ErrMirrorRepair, err)
		}
	}
	return record, nil
}
