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

	"golang.org/x/sys/unix"
)

// RewriteFile rewrites an existing regular diagnostics file in place, not atomically.
// It accepts the same names as [Dir.AppendFile]. Unlike an ordinary truncating
// write, it neither follows a leaf symlink nor creates a missing file (the error
// matches fs.ErrNotExist). If the entry changes after the preliminary type check,
// a device or FIFO may be opened, but a nonregular file is never truncated or
// written. The inode, owner, and read, write, and execute permissions are preserved.
//
// Readers may see an empty or incomplete file. A write error reports how many
// bytes were written after truncation; they are not rolled back. A crash may also
// leave the file empty or incomplete. Use [Dir.WriteFile] for crash-sensitive state.
// Descriptors already open for append remain attached to the file. Each append
// writes at the then-current end; a later rewrite write may overwrite an append
// that occurred before the rewrite finished. Callers must serialize writes if
// every record must be retained.
func (d *Dir) RewriteFile(name string, data []byte) error {
	return d.rewriteFile(name, data, unix.Write)
}

func (d *Dir) rewriteFile(name string, data []byte, write func(int, []byte) (int, error)) error {
	if d.root.handle == nil {
		return fs.ErrClosed
	}
	if !validAppendBase(name) {
		return &DestinationError{name}
	}
	// Do not open a known device. The descriptor check below also covers a changed entry.
	st, err := d.root.lstatRetry(name)
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return &NonRegularError{d.root.path(name)}
	}
	return d.root.openAndRewrite(name, data, write)
}

// openAndRewrite checks the descriptor before modifying it, independent of the earlier lstat.
func (d *directory) openAndRewrite(name string, data []byte, write func(int, []byte) (int, error)) (err error) {
	f, err := d.OpenFile(name, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
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
	if err := f.Truncate(0); err != nil {
		return err
	}
	for written := 0; written < len(data); {
		n, err := write(int(f.Fd()), data[written:])
		if n <= 0 && errors.Is(err, unix.EINTR) {
			continue
		}
		written += max(0, n)
		if n <= 0 && err == nil {
			err = io.ErrShortWrite
		}
		if err != nil {
			return fmt.Errorf("rewrite %s: wrote %d of %d bytes: %w", f.Name(), written, len(data), err)
		}
	}
	return nil
}
