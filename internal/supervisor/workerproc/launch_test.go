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

package workerproc_test

import (
	"errors"
	"io/fs"
	"math"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/workerproc"
)

// launchTOwner is the owner of every fixture executable and the owner a stable candidate must have.
const launchTOwner = 501

// launchTOtherOwner is an owner that is not launchTOwner.
const launchTOtherOwner = 502

// The fixture executables are inspection results built by hand: the units under test only compare values, so no file
// is created and none is run.
var (
	// launchTConfiguredExe is the worker executable the supervisor was started with.
	launchTConfiguredExe = workerproc.Executable{
		Image: procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
		Mode:  0o755,
		UID:   launchTOwner,
	}
	// launchTStableExe is the installed stable binary.
	launchTStableExe = workerproc.Executable{
		Image: procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2002},
		Mode:  0o755,
		UID:   launchTOwner,
	}
	// launchTReplacedStableExe is the stable binary after an update renamed a new file over it: same path, new inode.
	launchTReplacedStableExe = workerproc.Executable{
		Image: procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2003},
		Mode:  0o755,
		UID:   launchTOwner,
	}
	// launchTOtherExe is a regular file of the same owner that is neither the configured executable nor the stable binary.
	launchTOtherExe = workerproc.Executable{
		Image: procinfo.Image{Path: "/home/user/projects/model-gateway/bin/model-gateway", Device: 64770, Inode: 3003},
		Mode:  0o755,
		UID:   launchTOwner,
	}
)

// launchTGuard runs f and fails the calling row with the panic's text when f panics, so that a unit that panics fails
// each row separately and leaves the other rows and tests running.
func launchTGuard(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("the unit under test panicked: %v", r)
		}
	}()
	f()
}

// launchTPath returns e with its image path replaced.
func launchTPath(e workerproc.Executable, path string) workerproc.Executable {
	e.Image.Path = path
	return e
}

// launchTDevice returns e with its device replaced.
func launchTDevice(e workerproc.Executable, device uint64) workerproc.Executable {
	e.Image.Device = device
	return e
}

// launchTInode returns e with its inode replaced.
func launchTInode(e workerproc.Executable, inode uint64) workerproc.Executable {
	e.Image.Inode = inode
	return e
}

// launchTMode returns e with its mode replaced.
func launchTMode(e workerproc.Executable, mode fs.FileMode) workerproc.Executable {
	e.Mode = mode
	return e
}

// launchTUID returns e with its owner replaced.
func launchTUID(e workerproc.Executable, uid int) workerproc.Executable {
	e.UID = uid
	return e
}

// launchTConfigured returns the configured launch of e with version and fails the row when it is refused.
func launchTConfigured(t *testing.T, e workerproc.Executable, version string) workerproc.Launch {
	t.Helper()
	l, err := workerproc.NewConfiguredLaunch(e, version)
	if err != nil {
		t.Fatalf("setup: NewConfiguredLaunch(%+v, %q) = %v, want nil error", e, version, err)
	}
	return l
}

// launchTStable returns a launch authorised as a stable binary for e, accepted with owner and version by a restart
// whose current choice is the configured executable at version 0.0.0, and fails the row when any step refuses. e must
// not show the device and inode of launchTConfiguredExe.
func launchTStable(t *testing.T, e workerproc.Executable, owner int, version string) workerproc.Launch {
	t.Helper()
	configured := launchTConfigured(t, launchTConfiguredExe, "0.0.0")
	r := workerproc.RestartRule{Configured: configured, Current: configured, Stable: e, UID: owner}
	l, needVersion, err := r.Judge(e)
	if err != nil || !needVersion || l != (workerproc.Launch{}) {
		t.Fatalf("setup: Judge(%+v) = (%+v, %t, %v), want (zero Launch, true, nil)", e, l, needVersion, err)
	}
	l, err = r.AcceptVersion(e, []byte(version+"\n"), e)
	if err != nil {
		t.Fatalf("setup: AcceptVersion(%+v, %q) = %v, want nil error", e, version+"\n", err)
	}
	return l
}

// launchTMatch is one inspection result and what Matches must report for it.
type launchTMatch struct {
	e    workerproc.Executable
	want bool
}

// launchTCheckMatches reports every inspection result for which l.Matches disagrees with the expectation.
func launchTCheckMatches(t *testing.T, what string, l workerproc.Launch, matches []launchTMatch) {
	t.Helper()
	for i, m := range matches {
		if got := l.Matches(m.e); got != m.want {
			t.Errorf("%s: Matches(%+v) (inspection %d) = %t, want %t", what, m.e, i+1, got, m.want)
		}
	}
}

func TestLaunchMatches(t *testing.T) {
	t.Run("NewConfiguredLaunch", func(t *testing.T) {
		tests := map[string]struct {
			e           workerproc.Executable
			version     string
			wantErr     bool
			wantImage   procinfo.Image
			wantVersion string
		}{
			"success: a regular file at a clean absolute path with a valid version": {
				e:           launchTConfiguredExe,
				version:     "1.2.3",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "1.2.3",
			},
			"success: the version text is kept verbatim with its v and suffix": {
				e:           launchTConfiguredExe,
				version:     "v1.2.3-rc.1+build.5",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "v1.2.3-rc.1+build.5",
			},
			"success: an invalid version text is kept": {
				e:           launchTConfiguredExe,
				version:     "not a version",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "not a version",
			},
			"success: an empty version text is kept": {
				e:           launchTConfiguredExe,
				version:     "",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "",
			},
			"success: a version text with surrounding spaces is kept": {
				e:           launchTConfiguredExe,
				version:     " 1.2.3 ",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
				wantVersion: " 1.2.3 ",
			},
			"success: device zero is accepted": {
				e:           launchTDevice(launchTConfiguredExe, 0),
				version:     "1.2.3",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 0, Inode: 1001},
				wantVersion: "1.2.3",
			},
			"success: the largest device and inode are accepted": {
				e:           launchTInode(launchTDevice(launchTConfiguredExe, math.MaxUint64), math.MaxUint64),
				version:     "1.2.3",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: math.MaxUint64, Inode: math.MaxUint64},
				wantVersion: "1.2.3",
			},
			"success: a file directly below the root is accepted": {
				e:           launchTPath(launchTConfiguredExe, "/model-gateway"),
				version:     "1.2.3",
				wantImage:   procinfo.Image{Path: "/model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "1.2.3",
			},
			"success: a path with non-ASCII names is accepted": {
				e:           launchTPath(launchTConfiguredExe, "/opt/ゲートウェイ/bin/model-gateway"),
				version:     "1.2.3",
				wantImage:   procinfo.Image{Path: "/opt/ゲートウェイ/bin/model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "1.2.3",
			},
			"success: a path holding a byte that is not UTF-8 is accepted": {
				e:           launchTPath(launchTConfiguredExe, "/opt/model-gateway/bin/\xffclaude-code-model-gateway"),
				version:     "1.2.3",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/\xffclaude-code-model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "1.2.3",
			},
			"success: group and world write permission and another owner are accepted": {
				e:           launchTUID(launchTMode(launchTConfiguredExe, 0o777), 0),
				version:     "1.2.3",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "1.2.3",
			},
			"success: a regular file without permission bits is accepted": {
				e:           launchTMode(launchTConfiguredExe, 0),
				version:     "1.2.3",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "1.2.3",
			},
			"success: a regular file with the set-user-ID bit is a regular file": {
				e:           launchTMode(launchTConfiguredExe, fs.ModeSetuid|0o755),
				version:     "1.2.3",
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "1.2.3",
			},
			"error: the zero inspection": {
				e:       workerproc.Executable{},
				version: "1.2.3",
				wantErr: true,
			},
			"error: an empty path": {
				e:       launchTPath(launchTConfiguredExe, ""),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a relative path": {
				e:       launchTPath(launchTConfiguredExe, "bin/claude-code-model-gateway"),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a bare file name": {
				e:       launchTPath(launchTConfiguredExe, "claude-code-model-gateway"),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a path starting with a dot element": {
				e:       launchTPath(launchTConfiguredExe, "./claude-code-model-gateway"),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a path with a trailing slash": {
				e:       launchTPath(launchTConfiguredExe, "/opt/model-gateway/bin/claude-code-model-gateway/"),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a path with a doubled slash": {
				e:       launchTPath(launchTConfiguredExe, "/opt/model-gateway//bin/claude-code-model-gateway"),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a path starting with two slashes": {
				e:       launchTPath(launchTConfiguredExe, "//opt/model-gateway/bin/claude-code-model-gateway"),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a path with a dot element": {
				e:       launchTPath(launchTConfiguredExe, "/opt/model-gateway/./bin/claude-code-model-gateway"),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a path with a dot-dot element": {
				e:       launchTPath(launchTConfiguredExe, "/opt/model-gateway/lib/../bin/claude-code-model-gateway"),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a path with a NUL byte": {
				e:       launchTPath(launchTConfiguredExe, "/opt/model-gateway/bin/claude-code\x00model-gateway"),
				version: "1.2.3",
				wantErr: true,
			},
			"error: inode zero": {
				e:       launchTInode(launchTConfiguredExe, 0),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a directory": {
				e:       launchTMode(launchTConfiguredExe, fs.ModeDir|0o755),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a symbolic link": {
				e:       launchTMode(launchTConfiguredExe, fs.ModeSymlink|0o777),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a named pipe": {
				e:       launchTMode(launchTConfiguredExe, fs.ModeNamedPipe|0o755),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a socket": {
				e:       launchTMode(launchTConfiguredExe, fs.ModeSocket|0o755),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a block device": {
				e:       launchTMode(launchTConfiguredExe, fs.ModeDevice|0o755),
				version: "1.2.3",
				wantErr: true,
			},
			"error: a character device": {
				e:       launchTMode(launchTConfiguredExe, fs.ModeDevice|fs.ModeCharDevice|0o755),
				version: "1.2.3",
				wantErr: true,
			},
			"error: an irregular file": {
				e:       launchTMode(launchTConfiguredExe, fs.ModeIrregular|0o755),
				version: "1.2.3",
				wantErr: true,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				launchTGuard(t, func() {
					l, err := workerproc.NewConfiguredLaunch(tt.e, tt.version)
					if tt.wantErr {
						if !errors.Is(err, fs.ErrInvalid) {
							t.Errorf("NewConfiguredLaunch(%+v, %q) error = %v, want an error matching fs.ErrInvalid", tt.e, tt.version, err)
						}
						if l != (workerproc.Launch{}) {
							t.Errorf("NewConfiguredLaunch(%+v, %q) launch = %+v, want the zero Launch", tt.e, tt.version, l)
						}
						return
					}
					if err != nil {
						t.Fatalf("NewConfiguredLaunch(%+v, %q) error = %v, want nil", tt.e, tt.version, err)
					}
					if diff := gocmp.Diff(tt.wantImage, l.Image()); diff != "" {
						t.Errorf("Image() mismatch (-want +got):\n%s", diff)
					}
					if diff := gocmp.Diff(tt.wantVersion, l.Version()); diff != "" {
						t.Errorf("Version() mismatch (-want +got):\n%s", diff)
					}
					if l == (workerproc.Launch{}) {
						t.Errorf("NewConfiguredLaunch(%+v, %q) returned the zero Launch with a nil error", tt.e, tt.version)
					}
					if !l.Matches(tt.e) {
						t.Errorf("Matches of the inspection the launch was made from = false, want true")
					}
				})
			})
		}
	})

	t.Run("zero launch", func(t *testing.T) {
		tests := map[string]struct {
			e workerproc.Executable
		}{
			"error: the zero inspection does not match": {
				e: workerproc.Executable{},
			},
			"error: a regular file with the zero image does not match": {
				e: workerproc.Executable{Mode: 0o755, UID: launchTOwner},
			},
			"error: a regular file with the zero image and owner does not match": {
				e: workerproc.Executable{Mode: 0o644},
			},
			"error: the configured executable does not match": {
				e: launchTConfiguredExe,
			},
			"error: the stable binary does not match": {
				e: launchTStableExe,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				launchTGuard(t, func() {
					var l workerproc.Launch
					if got := l.Matches(tt.e); got {
						t.Errorf("Launch{}.Matches(%+v) = true, want false", tt.e)
					}
					if diff := gocmp.Diff(procinfo.Image{}, l.Image()); diff != "" {
						t.Errorf("Launch{}.Image() mismatch (-want +got):\n%s", diff)
					}
					if diff := gocmp.Diff("", l.Version()); diff != "" {
						t.Errorf("Launch{}.Version() mismatch (-want +got):\n%s", diff)
					}
				})
			})
		}
	})

	t.Run("configured launch", func(t *testing.T) {
		tests := map[string]struct {
			e    workerproc.Executable
			want bool
		}{
			"success: the inspection the launch was made from": {
				e:    launchTConfiguredExe,
				want: true,
			},
			"success: group and world write permission are not compared": {
				e:    launchTMode(launchTConfiguredExe, 0o777),
				want: true,
			},
			"success: group write permission is not compared": {
				e:    launchTMode(launchTConfiguredExe, 0o775),
				want: true,
			},
			"success: no permission bits at all are not compared": {
				e:    launchTMode(launchTConfiguredExe, 0),
				want: true,
			},
			"success: another owner is not compared": {
				e:    launchTUID(launchTConfiguredExe, launchTOtherOwner),
				want: true,
			},
			"success: the root owner is not compared": {
				e:    launchTUID(launchTConfiguredExe, 0),
				want: true,
			},
			"success: the set-user-ID bit keeps a regular file": {
				e:    launchTMode(launchTConfiguredExe, fs.ModeSetuid|0o755),
				want: true,
			},
			"error: another path to the same device and inode": {
				e:    launchTPath(launchTConfiguredExe, "/usr/local/bin/claude-code-model-gateway"),
				want: false,
			},
			"error: a path that names the same file only after cleaning": {
				e:    launchTPath(launchTConfiguredExe, "/opt/model-gateway//bin/claude-code-model-gateway"),
				want: false,
			},
			"error: a path that differs only in letter case": {
				e:    launchTPath(launchTConfiguredExe, "/opt/Model-Gateway/bin/claude-code-model-gateway"),
				want: false,
			},
			"error: a path with one more byte": {
				e:    launchTPath(launchTConfiguredExe, "/opt/model-gateway/bin/claude-code-model-gateway2"),
				want: false,
			},
			"error: an empty path with the same device and inode": {
				e:    launchTPath(launchTConfiguredExe, ""),
				want: false,
			},
			"error: another inode at the same path": {
				e:    launchTInode(launchTConfiguredExe, 1002),
				want: false,
			},
			"error: another device at the same path": {
				e:    launchTDevice(launchTConfiguredExe, 64770),
				want: false,
			},
			"error: a symbolic link now at the path": {
				e:    launchTMode(launchTConfiguredExe, fs.ModeSymlink|0o777),
				want: false,
			},
			"error: a directory now at the path": {
				e:    launchTMode(launchTConfiguredExe, fs.ModeDir|0o755),
				want: false,
			},
			"error: a named pipe now at the path": {
				e:    launchTMode(launchTConfiguredExe, fs.ModeNamedPipe|0o755),
				want: false,
			},
			"error: a socket now at the path": {
				e:    launchTMode(launchTConfiguredExe, fs.ModeSocket|0o755),
				want: false,
			},
			"error: a device now at the path": {
				e:    launchTMode(launchTConfiguredExe, fs.ModeDevice|0o755),
				want: false,
			},
			"error: a character device now at the path": {
				e:    launchTMode(launchTConfiguredExe, fs.ModeDevice|fs.ModeCharDevice|0o755),
				want: false,
			},
			"error: an irregular file now at the path": {
				e:    launchTMode(launchTConfiguredExe, fs.ModeIrregular|0o755),
				want: false,
			},
			"error: the zero inspection": {
				e:    workerproc.Executable{},
				want: false,
			},
			"error: the stable binary": {
				e:    launchTStableExe,
				want: false,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				launchTGuard(t, func() {
					l := launchTConfigured(t, launchTConfiguredExe, "1.2.3")
					if got := l.Matches(tt.e); got != tt.want {
						t.Errorf("Matches(%+v) = %t, want %t", tt.e, got, tt.want)
					}
				})
			})
		}
	})

	t.Run("stable launch", func(t *testing.T) {
		tests := map[string]struct {
			e    workerproc.Executable
			want bool
		}{
			"success: the inspection the launch was accepted from": {
				e:    launchTStableExe,
				want: true,
			},
			"success: owner-only write permission": {
				e:    launchTMode(launchTStableExe, 0o700),
				want: true,
			},
			"success: no write permission at all": {
				e:    launchTMode(launchTStableExe, 0o555),
				want: true,
			},
			"success: owner read and execute only": {
				e:    launchTMode(launchTStableExe, 0o500),
				want: true,
			},
			"success: group and world read without write": {
				e:    launchTMode(launchTStableExe, 0o744),
				want: true,
			},
			"success: no permission bits at all": {
				e:    launchTMode(launchTStableExe, 0),
				want: true,
			},
			"success: the set-user-ID bit is not a write permission": {
				e:    launchTMode(launchTStableExe, fs.ModeSetuid|0o755),
				want: true,
			},
			"success: the sticky bit is not a write permission": {
				e:    launchTMode(launchTStableExe, fs.ModeSticky|0o755),
				want: true,
			},
			"error: group write permission": {
				e:    launchTMode(launchTStableExe, 0o775),
				want: false,
			},
			"error: world write permission": {
				e:    launchTMode(launchTStableExe, 0o757),
				want: false,
			},
			"error: group and world write permission": {
				e:    launchTMode(launchTStableExe, 0o777),
				want: false,
			},
			"error: group write permission alone": {
				e:    launchTMode(launchTStableExe, 0o020),
				want: false,
			},
			"error: world write permission alone": {
				e:    launchTMode(launchTStableExe, 0o002),
				want: false,
			},
			"error: group and world write without read": {
				e:    launchTMode(launchTStableExe, 0o722),
				want: false,
			},
			"error: another owner": {
				e:    launchTUID(launchTStableExe, launchTOtherOwner),
				want: false,
			},
			"error: the root owner": {
				e:    launchTUID(launchTStableExe, 0),
				want: false,
			},
			"error: another path to the same device and inode": {
				e:    launchTPath(launchTStableExe, "/home/user/.local/bin/model-gateway"),
				want: false,
			},
			"error: the replaced file at the same path": {
				e:    launchTReplacedStableExe,
				want: false,
			},
			"error: another device at the same path": {
				e:    launchTDevice(launchTStableExe, 64769),
				want: false,
			},
			"error: a symbolic link now at the path": {
				e:    launchTMode(launchTStableExe, fs.ModeSymlink|0o755),
				want: false,
			},
			"error: a directory now at the path": {
				e:    launchTMode(launchTStableExe, fs.ModeDir|0o755),
				want: false,
			},
			"error: a named pipe now at the path": {
				e:    launchTMode(launchTStableExe, fs.ModeNamedPipe|0o755),
				want: false,
			},
			"error: the zero inspection": {
				e:    workerproc.Executable{},
				want: false,
			},
			"error: the configured executable": {
				e:    launchTConfiguredExe,
				want: false,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				launchTGuard(t, func() {
					l := launchTStable(t, launchTStableExe, launchTOwner, "1.2.3")
					if got := l.Matches(tt.e); got != tt.want {
						t.Errorf("Matches(%+v) = %t, want %t", tt.e, got, tt.want)
					}
				})
			})
		}
	})

	t.Run("one image under each authorisation", func(t *testing.T) {
		tests := map[string]struct {
			e              workerproc.Executable
			wantConfigured bool
			wantStable     bool
		}{
			"success: the inspection both launches were made from matches both": {
				e:              launchTStableExe,
				wantConfigured: true,
				wantStable:     true,
			},
			"success: another owner matches only the configured launch": {
				e:              launchTUID(launchTStableExe, launchTOtherOwner),
				wantConfigured: true,
				wantStable:     false,
			},
			"success: group write permission matches only the configured launch": {
				e:              launchTMode(launchTStableExe, 0o775),
				wantConfigured: true,
				wantStable:     false,
			},
			"success: world write permission matches only the configured launch": {
				e:              launchTMode(launchTStableExe, 0o757),
				wantConfigured: true,
				wantStable:     false,
			},
			"error: the replaced file at the same path matches neither": {
				e:              launchTReplacedStableExe,
				wantConfigured: false,
				wantStable:     false,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				launchTGuard(t, func() {
					configured := launchTConfigured(t, launchTStableExe, "1.2.3")
					stable := launchTStable(t, launchTStableExe, launchTOwner, "1.2.3")
					if diff := gocmp.Diff(configured.Image(), stable.Image()); diff != "" {
						t.Errorf("the two launches have different images (-configured +stable):\n%s", diff)
					}
					if diff := gocmp.Diff(configured.Version(), stable.Version()); diff != "" {
						t.Errorf("the two launches have different versions (-configured +stable):\n%s", diff)
					}
					if got := configured.Matches(tt.e); got != tt.wantConfigured {
						t.Errorf("configured launch: Matches(%+v) = %t, want %t", tt.e, got, tt.wantConfigured)
					}
					if got := stable.Matches(tt.e); got != tt.wantStable {
						t.Errorf("stable launch: Matches(%+v) = %t, want %t", tt.e, got, tt.wantStable)
					}
				})
			})
		}
	})

	t.Run("copies", func(t *testing.T) {
		tests := map[string]struct {
			// stable makes the original by accepting the stable binary; otherwise it is the configured executable's launch.
			stable      bool
			wantImage   procinfo.Image
			wantVersion string
			matches     []launchTMatch
		}{
			"success: a copy of a configured launch behaves as the original": {
				wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
				wantVersion: "1.2.3",
				matches: []launchTMatch{
					{e: launchTConfiguredExe, want: true},
					{e: launchTUID(launchTMode(launchTConfiguredExe, 0o777), launchTOtherOwner), want: true},
					{e: launchTInode(launchTConfiguredExe, 1002), want: false},
					{e: launchTPath(launchTConfiguredExe, "/usr/local/bin/claude-code-model-gateway"), want: false},
					{e: launchTMode(launchTConfiguredExe, fs.ModeSymlink|0o777), want: false},
					{e: workerproc.Executable{}, want: false},
				},
			},
			"success: a copy of a stable launch behaves as the original": {
				stable:      true,
				wantImage:   procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2002},
				wantVersion: "1.2.3",
				matches: []launchTMatch{
					{e: launchTStableExe, want: true},
					{e: launchTMode(launchTStableExe, 0o700), want: true},
					{e: launchTMode(launchTStableExe, 0o775), want: false},
					{e: launchTUID(launchTStableExe, launchTOtherOwner), want: false},
					{e: launchTReplacedStableExe, want: false},
					{e: workerproc.Executable{}, want: false},
				},
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				launchTGuard(t, func() {
					var original workerproc.Launch
					if tt.stable {
						original = launchTStable(t, launchTStableExe, launchTOwner, "1.2.3")
					} else {
						original = launchTConfigured(t, launchTConfiguredExe, "1.2.3")
					}
					assigned := original
					held := struct{ l workerproc.Launch }{l: original}
					var boxed any = original
					unboxed, ok := boxed.(workerproc.Launch)
					if !ok {
						t.Fatalf("a Launch stored in an interface value is no longer a Launch")
					}
					copies := map[string]workerproc.Launch{
						"original":                  original,
						"assigned copy":             assigned,
						"copy held in a struct":     held.l,
						"copy through an interface": unboxed,
						"copy through a slice":      []workerproc.Launch{original}[0],
					}
					for what, l := range copies {
						if l != original {
							t.Errorf("%s: not equal to the original", what)
						}
						if diff := gocmp.Diff(tt.wantImage, l.Image()); diff != "" {
							t.Errorf("%s: Image() mismatch (-want +got):\n%s", what, diff)
						}
						if diff := gocmp.Diff(tt.wantVersion, l.Version()); diff != "" {
							t.Errorf("%s: Version() mismatch (-want +got):\n%s", what, diff)
						}
						launchTCheckMatches(t, what, l, tt.matches)
					}
				})
			})
		}
	})
}
