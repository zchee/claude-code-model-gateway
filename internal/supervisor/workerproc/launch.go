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

package workerproc

import (
	"io/fs"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// Launch is the executable one worker generation runs, the version it reported, and how it was authorised.
//
// Its fields are unexported: a Launch is made by NewConfiguredLaunch or RestartRule.AcceptVersion and read through
// its methods, so no caller can change the image, the version or the authorisation of a chosen launch.
//
// A Launch is valid when it carries an authorisation, either as the configured worker executable or as a stable
// binary with the owner it was accepted with, and its image has an absolute, clean path without a NUL byte and a
// nonzero inode. The zero Launch carries no authorisation and is invalid. A copy of a valid Launch is valid and
// behaves exactly as the original.
type Launch struct {
	// image is the resolved image the launch runs: its absolute path, device and inode.
	image procinfo.Image //nolint:unused // Unused only until the methods are written: Image and Matches read it.
	// version is the version text the executable reported, verbatim and without a line end.
	version string //nolint:unused // Unused only until the methods are written: Version reads it.
	// auth records how the choice was authorised; its zero value means no authorisation.
	auth uint8 //nolint:unused // Unused only until the methods are written: Matches reads it.
	// owner is the owner a stable binary was accepted with and must keep; it is not read for any other authorisation.
	owner int //nolint:unused // Unused only until the methods are written: Matches reads it.
}

// Image returns the resolved absolute path, device and inode of the executable l runs; the zero Launch gives the zero Image.
func (l Launch) Image() procinfo.Image {
	panic("not implemented")
}

// Version returns the version text the executable of l reported, without a line end; the zero Launch gives "".
func (l Launch) Version() string {
	panic("not implemented")
}

// Executable is what an inspection found at one path after every symbolic link in it was resolved.
type Executable struct {
	// Image is the resolved absolute path and the device and inode of the file it names.
	Image procinfo.Image
	// Mode is the file's type and permission bits, as lstat of the resolved path reports them.
	Mode fs.FileMode
	// UID is the file's owner.
	UID int
}

// NewConfiguredLaunch returns the launch choice of the worker executable the supervisor was started with.
//
// It returns a valid launch authorised as the configured worker executable, with e.Image as its image and version
// kept verbatim, when e.Image.Path is absolute, clean and free of NUL bytes, e.Image.Inode is nonzero and e.Mode
// shows a regular file. Otherwise it returns the zero Launch and an error matching fs.ErrInvalid. The version text is
// not judged here: an invalid or empty text is kept, and only makes every later version comparison refuse.
func NewConfiguredLaunch(e Executable, version string) (Launch, error) {
	panic("not implemented")
}

// Matches reports whether e still shows the file that l was chosen for, with the owner and mode its authorisation requires.
//
// It is false for an invalid l. For a valid l it requires e's path to equal l's byte for byte, the same device and
// inode, and a regular file; for a stable binary it also requires the owner l was accepted with and no group or
// other write permission. The owner and permission bits of the configured worker executable are not compared.
func (l Launch) Matches(e Executable) bool {
	panic("not implemented")
}
