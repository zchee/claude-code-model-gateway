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

// Package atomicfile replaces state and settings files and appends journal records.
// Writers that stage a sibling accept destination names of at most 227 bytes, leaving room for the
// staging suffix within a 255-byte directory entry. Names of the form
// <destination>.<positive decimal pid>.<12 lowercase hex digits>.tmp are
// reserved by exact spelling and must not be used as destinations: a successful
// write to the same base name may sweep stale staged siblings. For settings
// these rules apply to the resolved destination, not intervening symlinks.
// Stale-file cleanup checks the entry with lstat before a separate file-only unlink.
// The package does not protect directories writable by other users.
package atomicfile

import (
	"fmt"
	"io/fs"
	"time"
)

// DestinationError reports a missing, invalid, reserved, or overlong destination name.
type DestinationError struct{ Path string }

func (e *DestinationError) Error() string {
	return fmt.Sprintf("invalid atomic file destination: %q", e.Path)
}

// StageCleanupError reports that unlinking a staged sibling failed. If Published
// is true, Staged is a second name for the published inode. Otherwise publication
// did not succeed and Staged is an orphan. Err is the unlink error. Staged
// records the sibling name using the spelling of the opened directory at the
// time of failure. It may no longer resolve if the directory was moved or the
// sibling was removed.
type StageCleanupError struct {
	Staged    string
	Published bool
	Err       error
}

func (e *StageCleanupError) Error() string {
	if e.Published {
		return fmt.Sprintf("file published; removing staged sibling %s: %v", e.Staged, e.Err)
	}
	return fmt.Sprintf("file not published; removing staged sibling %s: %v", e.Staged, e.Err)
}

func (e *StageCleanupError) Unwrap() error { return e.Err }

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
	remove() error
}

type writer struct {
	now                   func() time.Time
	commit                func(preparedFile, string) error
	remove                func(preparedFile) error
	afterOpen, beforeLink func()
}

func newWriter() writer {
	return writer{now: time.Now, commit: func(file preparedFile, _ string) error { return file.CloseAtomicallyReplace() }, remove: preparedFile.remove}
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
