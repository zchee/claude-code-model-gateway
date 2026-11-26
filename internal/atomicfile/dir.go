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

package atomicfile

import "os"

type directory struct{ handle *os.File }

type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// Dir anchors single-component file operations to one opened directory, even
// if it is renamed. A Dir must not be copied or used concurrently. In
// particular, Close must not race any operation. Its zero value is closed.
type Dir struct {
	_    noCopy
	root directory
}

// Close releases the directory descriptor. Closing a closed Dir succeeds;
// subsequent file operations return fs.ErrClosed.
func (d *Dir) Close() error {
	f := d.root.handle
	if f == nil {
		return nil
	}
	d.root.handle = nil
	return f.Close()
}

// NonRegularError reports a refusal to read, append to, or rewrite a nonregular entry.
type NonRegularError struct{ Path string }

func (e *NonRegularError) Error() string { return e.Path + ": not a regular file" }
