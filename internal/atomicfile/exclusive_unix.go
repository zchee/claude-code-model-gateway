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

//go:build unix

package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// CreateExclusive publishes a synced regular file with the given data and mode
// in an existing parent directory. It never replaces an entry: an existing
// name, including a dangling symlink, causes an error matching fs.ErrExist.
// The filesystem must support hard links. A failure to unlink the staged name
// after publication is reported as a cleanup error; the final name remains.
// A process crash can likewise leave the staged sibling for a later sweep.
// The parent directory is not synced for power-loss durability.
func CreateExclusive(path string, data []byte, mode fs.FileMode) error {
	root, base, err := openParent(nil, path, false)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return createExclusiveAt(root, base, data, mode)
}

func createExclusiveAt(root *directory, base string, data []byte, mode fs.FileMode) error {
	// An existing entry does not require enough space or permission to stage a file.
	_, err := retryEINTR(func() (int, error) {
		_, err := root.Lstat(base)
		return 0, err
	})
	if err == nil {
		return &os.PathError{Op: "create", Path: root.path(base), Err: fs.ErrExist}
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	w := newWriter()
	var cleanupErr error
	w.commit = func(stage preparedFile, target string) error {
		name := filepath.Base(stage.Name())
		fd := int(root.handle.Fd())
		_, err := retryEINTR(func() (int, error) { return 0, unix.Linkat(fd, name, fd, base, 0) })
		if err != nil {
			return &os.LinkError{Op: "link", Old: stage.Name(), New: target, Err: err}
		}
		// The link has committed. Do not retry publication on an unlink error.
		if err := root.removeIfPresent(name); err != nil {
			cleanupErr = fmt.Errorf("%s published; removing staged sibling %s: %w", target, name, err)
		}
		return nil
	}
	if err := w.replace(root, base, data, mode, nil, true, nil); err != nil {
		return err
	}
	return cleanupErr
}
