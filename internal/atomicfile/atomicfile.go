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
// Stale-file cleanup checks the entry with lstat before a separate file-only unlink.
// The package does not protect directories writable by other users.
package atomicfile

import (
	"fmt"
	"io/fs"
	"time"
)

// DestinationError reports a path without a destination file name.
type DestinationError struct{ Path string }

func (e *DestinationError) Error() string {
	return fmt.Sprintf("atomic replacement needs a file name: %q", e.Path)
}

// OwnershipError reports that replacement would change the file's owner or group.
type OwnershipError struct {
	Path string
	Err  error
}

func (e *OwnershipError) Error() string {
	return fmt.Sprintf("cannot preserve ownership of %s: %v", e.Path, e.Err)
}

func (e *OwnershipError) Unwrap() error { return e.Err }

// preparedFile names a synced, closed sibling ready for publication.
type preparedFile interface {
	Name() string
	CloseAtomicallyReplace() error
}

type writer struct {
	now    func() time.Time
	commit func(preparedFile, string) error
}

func newWriter() writer {
	return writer{now: time.Now, commit: func(file preparedFile, _ string) error { return file.CloseAtomicallyReplace() }}
}

// WriteFile replaces path with data using mode for the temporary sibling.
func WriteFile(path string, data []byte, mode fs.FileMode) error {
	return newWriter().writeFile(path, data, mode)
}

// ReplaceSettings follows a settings symlink and preserves the target's permissions.
// If another writer changes the file before commit, it calls edit on the new contents,
// failing with the filename after three conflicting attempts.
func ReplaceSettings(path string, edit func([]byte) ([]byte, error)) error {
	return newWriter().replaceSettings(path, edit, nil)
}
