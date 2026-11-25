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

// Package atomicfile replaces state and settings files without truncating the destination.
package atomicfile

import (
	"errors"
	"io/fs"
	"os"
)

var errNotImplemented = errors.New("atomic file replacement not implemented")

// renameFile is the commit point shared by state and settings replacements.
var renameFile = os.Rename

// WriteFile replaces path with data using mode for the temporary sibling.
func WriteFile(path string, data []byte, mode fs.FileMode) error {
	return errNotImplemented
}

// ReplaceSettings follows a settings symlink and preserves the target's permissions.
// If another writer changes the file before commit, it calls edit on the new contents,
// failing with the filename after three conflicting attempts.
func ReplaceSettings(path string, edit func([]byte) ([]byte, error)) error {
	return errNotImplemented
}
