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
)

// AppendFile returns errors.ErrUnsupported on non-Unix platforms.
func AppendFile(path string, data []byte, mode fs.FileMode) error { return errors.ErrUnsupported }

// AppendFile returns an error matching errors.ErrUnsupported and fs.ErrClosed
// because no Dir can be opened on this platform.
func (d *Dir) AppendFile(name string, data []byte, mode fs.FileMode) error {
	return errors.Join(errors.ErrUnsupported, fs.ErrClosed)
}
