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
	"io"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// AppendFile appends data to a regular file in one write without replacing it.
// It creates a missing file with mode's permission bits subject to the creation
// mask. It does not create parent directories or follow a final symlink.
// A write error reports the number of bytes appended; a partial record is not retried.
func AppendFile(path string, data []byte, mode fs.FileMode) error {
	if _, base := filepath.Split(path); !validAppendBase(base) {
		return &DestinationError{path}
	}
	root, base, err := openParent(nil, path, false)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.appendFile(base, data, mode)
}

// AppendFile appends to a single named entry with the semantics of [AppendFile].
func (d *Dir) AppendFile(name string, data []byte, mode fs.FileMode) error {
	if d.root.handle == nil {
		return fs.ErrClosed
	}
	if !validAppendBase(name) {
		return &DestinationError{name}
	}
	return d.root.appendFile(name, data, mode)
}

func (d *directory) appendFile(name string, data []byte, mode fs.FileMode) (err error) {
	// Avoid opening a known device; the opened descriptor is authoritative if the entry changes.
	st, err := d.lstatRetry(name)
	if err == nil && st.Mode&unix.S_IFMT != unix.S_IFREG {
		return &NonRegularError{d.path(name)}
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := d.OpenFile(name, unix.O_APPEND|unix.O_WRONLY|unix.O_CREAT|unix.O_NONBLOCK|unix.O_NOCTTY, mode)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EISDIR) || errors.Is(err, unix.ENXIO) {
		return &NonRegularError{d.path(name)}
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return &NonRegularError{f.Name()}
	}
	// os.File.Write may split a large buffer or continue after a short write.
	for {
		n, err := unix.Write(int(f.Fd()), data)
		if n <= 0 && errors.Is(err, unix.EINTR) {
			continue
		}
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return fmt.Errorf("append %s: wrote %d of %d bytes: %w", f.Name(), max(0, n), len(data), err)
		}
		return nil
	}
}
