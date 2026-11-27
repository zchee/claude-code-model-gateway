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

//go:build !unix

package atomicfile

import (
	"errors"
	"io/fs"
	"os"
)

// OpenDir returns errors.ErrUnsupported on non-Unix platforms.
func OpenDir(path string) (*Dir, error) { return nil, errors.ErrUnsupported }

// OpenDirCreate returns errors.ErrUnsupported on non-Unix platforms.
func OpenDirCreate(path string, perm fs.FileMode) (*Dir, error) { return nil, errors.ErrUnsupported }

// OpenDirectoryFile returns errors.ErrUnsupported on non-Unix platforms.
func OpenDirectoryFile(path string, perm fs.FileMode) (*os.File, error) {
	return nil, errors.ErrUnsupported
}

// ReadFile returns fs.ErrClosed because no Dir can be opened on this platform.
func (d *Dir) ReadFile(name string) ([]byte, error) { return nil, fs.ErrClosed }

// WriteFile returns fs.ErrClosed because no Dir can be opened on this platform.
func (d *Dir) WriteFile(name string, data []byte, mode fs.FileMode, opts ...WriteOption) error {
	return fs.ErrClosed
}

// RewriteFile returns fs.ErrClosed because no Dir can be opened on this platform.
func (d *Dir) RewriteFile(name string, data []byte) error { return fs.ErrClosed }

// CreateExclusive returns fs.ErrClosed because no Dir can be opened on this platform.
func (d *Dir) CreateExclusive(name string, data []byte, mode fs.FileMode) error { return fs.ErrClosed }
