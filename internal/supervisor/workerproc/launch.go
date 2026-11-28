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
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

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
	image procinfo.Image
	// version is the version text the executable reported, verbatim and without a line end.
	version string
	// auth records how the choice was authorised; its zero value means no authorisation.
	auth uint8
	// owner is the owner a stable binary was accepted with and must keep; it is not read for any other authorisation.
	owner int
}

// The authorisations a Launch can carry, stored in Launch.auth; the zero value is none.
const (
	// authConfigured is the worker executable the supervisor was started with.
	authConfigured uint8 = iota + 1
	// authStable is a stable binary accepted by a restart, with the owner it must keep.
	authStable
)

// validPath reports whether p is absolute, clean and free of NUL bytes.
func validPath(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsRune(p, 0)
}

// valid reports whether l carries an authorisation and an image with a valid path and a nonzero inode.
func (l Launch) valid() bool {
	return (l.auth == authConfigured || l.auth == authStable) && validPath(l.image.Path) && l.image.Inode != 0
}

// permits reports whether e has the owner and permission bits the authorisation of l requires. A stable binary must
// keep the owner it was accepted with and have no group or other write permission; the configured worker executable
// has no such condition. A launch without an authorisation permits nothing.
func (l Launch) permits(e Executable) bool {
	switch l.auth {
	case authConfigured:
		return true
	case authStable:
		return e.UID == l.owner && e.Mode.Perm()&0o022 == 0
	default:
		return false
	}
}

// Image returns the resolved absolute path, device and inode of the executable l runs; the zero Launch gives the zero Image.
func (l Launch) Image() procinfo.Image {
	return l.image
}

// Version returns the version text the executable of l reported, without a line end; the zero Launch gives "".
func (l Launch) Version() string {
	return l.version
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
	if !validPath(e.Image.Path) || e.Image.Inode == 0 || !e.Mode.IsRegular() {
		return Launch{}, fmt.Errorf("%w: worker executable %q (inode %d, mode %v) is not a regular file at an absolute, clean path", fs.ErrInvalid, e.Image.Path, e.Image.Inode, e.Mode)
	}
	return Launch{image: e.Image, version: version, auth: authConfigured}, nil
}

// Matches reports whether e still shows the file that l was chosen for, with the owner and mode its authorisation requires.
//
// It is false for an invalid l. For a valid l it requires e's path to equal l's byte for byte, the same device and
// inode, and a regular file; for a stable binary it also requires the owner l was accepted with and no group or
// other write permission. The owner and permission bits of the configured worker executable are not compared.
func (l Launch) Matches(e Executable) bool {
	return l.valid() && e.Image == l.image && e.Mode.IsRegular() && l.permits(e)
}
