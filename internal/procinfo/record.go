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
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

func mirrorPath(root, name string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !filepath.IsLocal(name) || name == "." || filepath.Base(name) != name {
		return "", fs.ErrInvalid
	}
	return filepath.Join(root, name+".pid"), nil
}

func (r Record) valid(root string) bool {
	return r.PID > 0 && r.PID <= math.MaxInt32 && r.StateRoot == root && r.Command != "" && r.StartedAt != "" && r.Role != "" && r.Generation != "" && r.Version != "" && filepath.IsAbs(r.Image.Path) && r.Image.Inode != 0
}

func replaceRecordFile(path string, data []byte) error {
	return atomicfile.WriteFile(path, data, 0o600)
}

// WriteRecord atomically publishes name.pid.json before updating its decimal mirror.
func WriteRecord(root, name string, record Record) error {
	return writeRecord(root, name, record, replaceRecordFile)
}

func writeRecord(root, name string, record Record, replace func(string, []byte) error) error {
	mirror, err := mirrorPath(root, name)
	if err != nil || !record.valid(root) {
		return fs.ErrInvalid
	}
	// Encode the typed record directly: a JavaScript number cannot represent every inode.
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := replace(mirror+".json", data); err != nil {
		return err
	}
	return replace(mirror, []byte(strconv.Itoa(record.PID)))
}

// ReadRecord reads the authoritative JSON and repairs a stale or missing mirror.
func ReadRecord(root, name string) (Record, error) {
	mirror, err := mirrorPath(root, name)
	if err != nil {
		return Record{}, err
	}
	data, err := os.ReadFile(mirror + ".json")
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, err
	}
	if !record.valid(root) {
		return Record{}, fs.ErrInvalid
	}
	old, err := os.ReadFile(mirror)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Record{}, err
	}
	if want := strconv.Itoa(record.PID); string(old) != want {
		if err := replaceRecordFile(mirror, []byte(want)); err != nil {
			return Record{}, err
		}
	}
	return record, nil
}
