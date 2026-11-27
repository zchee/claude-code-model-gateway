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
	"errors"
	"io/fs"
	"path/filepath"
	"syscall"
)

// ClearStartupFailure removes the supervisor failure file if present and reports other filesystem errors.
// The entry at the record's name is unlinked whatever it is, a symbolic link or a named pipe included, without being
// opened or followed; a directory there is refused, not removed. Only a confirmed missing entry counts as already
// clear: a state root that is not a directory is an error.
func (f Files) ClearStartupFailure() error {
	if !validStateDir(f.StateDir) {
		return &fs.PathError{Op: "clear startup failure", Path: f.StateDir, Err: fs.ErrInvalid}
	}
	path := filepath.Join(f.StateDir, failureFile)
	// unlink, not os.Remove: os.Remove falls back to removing an empty directory.
	if err := syscall.Unlink(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &fs.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}
