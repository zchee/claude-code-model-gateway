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

package workerproc_test

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/workerproc"
)

const (
	// birthTPath is the resolved path of the worker executable every birth case launches.
	birthTPath = "/opt/model-gateway/bin/model-gateway"
	// birthTRoot is the state root the birth cases pass.
	birthTRoot = "/var/lib/model-gateway"
	// birthTPad is the length of a third worker argument that makes the encoding of the valid birth exactly 65,536
	// bytes: the encoding without it has 471 bytes, and the argument adds its quotes and a comma.
	birthTPad = 65062
)

// birthTGuard runs call and fails the calling test with the panic's text when call panics, so that one panicking
// unit fails only the row that called it.
func birthTGuard(t *testing.T, what string, call func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	call()
}

// birthTInput is the evidence one NewBirth case passes.
type birthTInput struct {
	worker, parent            procinfo.Identity
	root, generation, version string
	// noLaunch passes the zero Launch instead of the configured launch with version.
	noLaunch bool
}

// birthTValid returns evidence from which NewBirth must build birthTWant.
func birthTValid() birthTInput {
	return birthTInput{
		worker: procinfo.Identity{
			PID:       4242,
			ParentPID: 4100,
			Argv:      []string{birthTPath, "serve-worker"},
			Birth:     time.Date(2026, time.November, 28, 12, 0, 0, 0, time.UTC),
			Image: procinfo.ImageEvidence{
				Image:                procinfo.Image{Path: birthTPath, Device: 64769, Inode: 1311},
				DirectFromExecutable: true,
			},
			StartToken: "boot-1:12345",
		},
		parent: procinfo.Identity{
			PID:        4100,
			ParentPID:  1,
			Argv:       []string{birthTPath, "serve-shim"},
			Birth:      time.Date(2026, time.November, 28, 11, 59, 30, 0, time.UTC),
			Image:      procinfo.ImageEvidence{Image: procinfo.Image{Path: birthTPath, Device: 64769, Inode: 1311}},
			StartToken: "boot-1:12000",
		},
		root:       birthTRoot,
		generation: "g-0001",
		version:    "1.2.3",
	}
}

// birthTWant is the birth NewBirth must build from birthTValid.
func birthTWant() workerproc.Birth {
	return workerproc.Birth{
		SchemaVersion: 1,
		PID:           4242,
		Argv:          []string{birthTPath, "serve-worker"},
		StartedAt:     "Sat Nov 28 12:00:00 2026",
		StartToken:    "boot-1:12345",
		StateRoot:     birthTRoot,
		Generation:    "g-0001",
		Version:       "1.2.3",
		Image:         procinfo.Image{Path: birthTPath, Device: 64769, Inode: 1311},
		Parent: workerproc.BirthParent{
			PID:        4100,
			Argv:       []string{birthTPath, "serve-shim"},
			StartedAt:  "Sat Nov 28 11:59:30 2026",
			StartToken: "boot-1:12000",
		},
	}
}

// birthTLaunch returns the configured launch of the worker executable at birthTPath with version.
func birthTLaunch(t *testing.T, version string) workerproc.Launch {
	t.Helper()
	var l workerproc.Launch
	var err error
	e := workerproc.Executable{Image: procinfo.Image{Path: birthTPath, Device: 64769, Inode: 1311}, Mode: 0o755, UID: 1000}
	birthTGuard(t, "NewConfiguredLaunch", func() { l, err = workerproc.NewConfiguredLaunch(e, version) })
	if err != nil {
		t.Fatalf("fixture: NewConfiguredLaunch of a regular file with version %q: %v", version, err)
	}
	return l
}

// birthTCase is one NewBirth case.
type birthTCase struct {
	edit func(in *birthTInput)
	// want changes birthTWant into the expected birth; nil keeps it.
	want func(b *workerproc.Birth)
	// err is true when NewBirth must refuse the evidence with ErrBirth.
	err bool
	// limit marks the case whose expected birth must encode to exactly 65,536 bytes, which the test checks of its own
	// fixture first.
	limit bool
}

func TestBirthRecordOnExit(t *testing.T) {
	tests := map[string]birthTCase{
		"success: the current child of the sampled supervisor": {},
		"success: a version with one lowercase v is kept as reported": {
			edit: func(in *birthTInput) { in.version = "v1.2.3" },
			want: func(b *workerproc.Birth) { b.Version = "v1.2.3" },
		},
		"success: a version with leading zeros": {
			edit: func(in *birthTInput) { in.version = "01.2.3" },
			want: func(b *workerproc.Birth) { b.Version = "01.2.3" },
		},
		"success: a version with a prerelease suffix": {
			edit: func(in *birthTInput) { in.version = "1.2.3-rc.1" },
			want: func(b *workerproc.Birth) { b.Version = "1.2.3-rc.1" },
		},
		"success: a version with build metadata": {
			edit: func(in *birthTInput) { in.version = "1.2.3+abc" },
			want: func(b *workerproc.Birth) { b.Version = "1.2.3+abc" },
		},
		"success: the version of a development build": {
			edit: func(in *birthTInput) { in.version = "0.0.0-dev+0123abcd" },
			want: func(b *workerproc.Birth) { b.Version = "0.0.0-dev+0123abcd" },
		},
		"success: a correlated worker image is not judged": {
			edit: func(in *birthTInput) {
				in.worker.Image = procinfo.ImageEvidence{Path: "/usr/bin/other", Device: 1, Inode: 2}
			},
		},
		"success: a direct worker image of the launched device and inode under another path": {
			edit: func(in *birthTInput) { in.worker.Image.Path = birthTPath + " (deleted)" },
		},
		"success: the largest process numbers": {
			edit: func(in *birthTInput) {
				in.worker.PID = math.MaxInt32
				in.worker.ParentPID = math.MaxInt32 - 1
				in.parent.PID = math.MaxInt32 - 1
			},
			want: func(b *workerproc.Birth) {
				b.PID = math.MaxInt32
				b.Parent.PID = math.MaxInt32 - 1
			},
		},
		"success: worker process number 1": {
			edit: func(in *birthTInput) { in.worker.PID = 1 },
			want: func(b *workerproc.Birth) { b.PID = 1 },
		},
		"success: supervisor process number 1": {
			edit: func(in *birthTInput) {
				in.worker.ParentPID = 1
				in.parent.PID = 1
			},
			want: func(b *workerproc.Birth) { b.Parent.PID = 1 },
		},
		"success: the largest supervisor process number": {
			edit: func(in *birthTInput) {
				in.worker.ParentPID = math.MaxInt32
				in.parent.PID = math.MaxInt32
			},
			want: func(b *workerproc.Birth) { b.Parent.PID = math.MaxInt32 },
		},
		"success: births are written in the time zone of their sample": {
			edit: func(in *birthTInput) {
				in.worker.Birth = time.Date(2026, time.November, 28, 21, 0, 0, 0, time.FixedZone("JST", 9*60*60))
				in.parent.Birth = time.Date(2026, time.November, 28, 20, 59, 30, 0, time.FixedZone("JST", 9*60*60))
			},
			want: func(b *workerproc.Birth) {
				b.StartedAt = "Sat Nov 28 21:00:00 2026"
				b.Parent.StartedAt = "Sat Nov 28 20:59:30 2026"
			},
		},
		"success: a supervisor with a one-element command line": {
			edit: func(in *birthTInput) { in.parent.Argv = []string{"/usr/local/bin/model-gateway"} },
			want: func(b *workerproc.Birth) { b.Parent.Argv = []string{"/usr/local/bin/model-gateway"} },
		},
		"success: arguments after the role are kept": {
			edit: func(in *birthTInput) { in.worker.Argv = []string{birthTPath, "serve-worker", "--trace"} },
			want: func(b *workerproc.Birth) { b.Argv = []string{birthTPath, "serve-worker", "--trace"} },
		},
		"success: an encoding of exactly 65,536 bytes": {
			edit: func(in *birthTInput) {
				in.worker.Argv = []string{birthTPath, "serve-worker", strings.Repeat("a", birthTPad)}
			},
			want: func(b *workerproc.Birth) {
				b.Argv = []string{birthTPath, "serve-worker", strings.Repeat("a", birthTPad)}
			},
			limit: true,
		},
		"error: the worker is a child of another process": {
			edit: func(in *birthTInput) { in.worker.ParentPID = 1 },
			err:  true,
		},
		"error: the worker is a proven root": {
			edit: func(in *birthTInput) { in.worker.ParentPID = 0 },
			err:  true,
		},
		"error: the zero launch": {
			edit: func(in *birthTInput) { in.noLaunch = true },
			err:  true,
		},
		"error: the worker birth is the zero time": {
			edit: func(in *birthTInput) { in.worker.Birth = time.Time{} },
			err:  true,
		},
		"error: the supervisor birth is the zero time": {
			edit: func(in *birthTInput) { in.parent.Birth = time.Time{} },
			err:  true,
		},
		"error: a direct worker image of another device": {
			edit: func(in *birthTInput) { in.worker.Image.Device = 64770 },
			err:  true,
		},
		"error: a direct worker image of another inode": {
			edit: func(in *birthTInput) { in.worker.Image.Inode = 1312 },
			err:  true,
		},
		"error: worker process number zero": {
			edit: func(in *birthTInput) { in.worker.PID = 0 },
			err:  true,
		},
		"error: a negative worker process number": {
			edit: func(in *birthTInput) { in.worker.PID = -4242 },
			err:  true,
		},
		"error: an empty worker start token": {
			edit: func(in *birthTInput) { in.worker.StartToken = "" },
			err:  true,
		},
		"error: an empty supervisor start token": {
			edit: func(in *birthTInput) { in.parent.StartToken = "" },
			err:  true,
		},
		"error: a worker command line of one element": {
			edit: func(in *birthTInput) { in.worker.Argv = []string{birthTPath} },
			err:  true,
		},
		"error: an empty worker command line": {
			edit: func(in *birthTInput) { in.worker.Argv = nil },
			err:  true,
		},
		"error: a worker command line that does not start with the launched path": {
			edit: func(in *birthTInput) { in.worker.Argv = []string{"/usr/local/bin/model-gateway", "serve-worker"} },
			err:  true,
		},
		"error: a worker run by a test wrapper": {
			edit: func(in *birthTInput) { in.worker.Argv = []string{birthTPath, "-test.run=^TestWorker$"} },
			err:  true,
		},
		"error: a worker in the supervisor role": {
			edit: func(in *birthTInput) { in.worker.Argv = []string{birthTPath, "serve-shim"} },
			err:  true,
		},
		"error: a relative state root": {
			edit: func(in *birthTInput) { in.root = "var/lib/model-gateway" },
			err:  true,
		},
		"error: a state root with a trailing slash": {
			edit: func(in *birthTInput) { in.root = birthTRoot + "/" },
			err:  true,
		},
		"error: a state root with a dot-dot component": {
			edit: func(in *birthTInput) { in.root = "/var/lib/../lib/model-gateway" },
			err:  true,
		},
		"error: a state root holding a NUL byte": {
			edit: func(in *birthTInput) { in.root = birthTRoot + "\x00" },
			err:  true,
		},
		"error: an empty state root": {
			edit: func(in *birthTInput) { in.root = "" },
			err:  true,
		},
		"error: an empty generation": {
			edit: func(in *birthTInput) { in.generation = "" },
			err:  true,
		},
		"error: an empty version": {
			edit: func(in *birthTInput) { in.version = "" },
			err:  true,
		},
		"error: a version of two numbers": {
			edit: func(in *birthTInput) { in.version = "1.2" },
			err:  true,
		},
		"error: a version of four numbers": {
			edit: func(in *birthTInput) { in.version = "1.2.3.4" },
			err:  true,
		},
		"error: a version with an uppercase V": {
			edit: func(in *birthTInput) { in.version = "V1.2.3" },
			err:  true,
		},
		"error: a version with two v": {
			edit: func(in *birthTInput) { in.version = "vv1.2.3" },
			err:  true,
		},
		"error: a version with an empty prerelease suffix": {
			edit: func(in *birthTInput) { in.version = "1.2.3-" },
			err:  true,
		},
		"error: a version with an empty build suffix": {
			edit: func(in *birthTInput) { in.version = "1.2.3+" },
			err:  true,
		},
		"error: a version with a trailing space": {
			edit: func(in *birthTInput) { in.version = "1.2.3 " },
			err:  true,
		},
		"error: a version with a leading space": {
			edit: func(in *birthTInput) { in.version = " 1.2.3" },
			err:  true,
		},
		"error: a version with a line end": {
			edit: func(in *birthTInput) { in.version = "1.2.3\n" },
			err:  true,
		},
		"error: a version number of ten digits": {
			edit: func(in *birthTInput) { in.version = "1234567890.0.0" },
			err:  true,
		},
		"error: a version suffix with an underscore": {
			edit: func(in *birthTInput) { in.version = "1.2.3_x" },
			err:  true,
		},
		"error: supervisor process number zero": {
			edit: func(in *birthTInput) {
				in.worker.ParentPID = 0
				in.parent.PID = 0
			},
			err: true,
		},
		"error: a supervisor with the worker's process number": {
			edit: func(in *birthTInput) {
				in.worker.ParentPID = 4242
				in.parent.PID = 4242
			},
			err: true,
		},
		"error: an empty supervisor command line": {
			edit: func(in *birthTInput) { in.parent.Argv = nil },
			err:  true,
		},
		"error: an empty first supervisor argument": {
			edit: func(in *birthTInput) { in.parent.Argv = []string{"", "serve-shim"} },
			err:  true,
		},
		"error: a worker birth after the year 9999": {
			edit: func(in *birthTInput) { in.worker.Birth = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC) },
			err:  true,
		},
		"error: a supervisor birth after the year 9999": {
			edit: func(in *birthTInput) { in.parent.Birth = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC) },
			err:  true,
		},
		"error: an encoding of 65,537 bytes": {
			edit: func(in *birthTInput) {
				in.worker.Argv = []string{birthTPath, "serve-worker", strings.Repeat("a", birthTPad+1)}
			},
			err: true,
		},
		"error: a worker argument holding invalid UTF-8": {
			edit: func(in *birthTInput) { in.worker.Argv = []string{birthTPath, "serve-worker", "\xff"} },
			err:  true,
		},
		"error: a supervisor argument holding invalid UTF-8": {
			edit: func(in *birthTInput) { in.parent.Argv = []string{birthTPath, "\xff"} },
			err:  true,
		},
		"error: a worker start token holding invalid UTF-8": {
			edit: func(in *birthTInput) { in.worker.StartToken = "boot-1:\xff" },
			err:  true,
		},
		"error: a generation holding invalid UTF-8": {
			edit: func(in *birthTInput) { in.generation = "g-\xff" },
			err:  true,
		},
	}
	// A process number one above the largest exists only where int is wider than 32 bits; the value is converted at
	// run time so that this file also builds for a 32-bit target.
	if strconv.IntSize > 32 {
		over := int64(math.MaxInt32) + 1
		tests["error: a worker process number above the largest"] = birthTCase{
			edit: func(in *birthTInput) { in.worker.PID = int(over) },
			err:  true,
		}
		tests["error: a supervisor process number above the largest"] = birthTCase{
			edit: func(in *birthTInput) {
				in.worker.ParentPID = int(over)
				in.parent.PID = int(over)
			},
			err: true,
		}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := birthTValid()
			if tt.edit != nil {
				tt.edit(&in)
			}
			var l workerproc.Launch
			if !in.noLaunch {
				l = birthTLaunch(t, in.version)
			}
			var got workerproc.Birth
			var err error
			birthTGuard(t, "NewBirth", func() { got, err = workerproc.NewBirth(in.worker, in.parent, in.root, in.generation, l) })
			if tt.err {
				if !errors.Is(err, workerproc.ErrBirth) {
					t.Errorf("NewBirth error = %v, want an error matching ErrBirth", err)
				}
				if diff := gocmp.Diff(workerproc.Birth{}, got); diff != "" {
					t.Errorf("NewBirth refused the evidence but returned a birth (-want +got):\n%s", diff)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewBirth: %v", err)
			}
			want := birthTWant()
			if tt.want != nil {
				tt.want(&want)
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("NewBirth (-want +got):\n%s", diff)
			}
			if tt.limit {
				encoded, err := jsonv2.Marshal(want)
				if err != nil || len(encoded) != 65536 {
					t.Fatalf("fixture: the expected birth encodes to %d bytes (error %v), want 65,536", len(encoded), err)
				}
			}
			in.worker.Argv[0] = "/changed"
			in.parent.Argv[0] = "/changed"
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("a change to the caller's command lines changed the birth (-want +got):\n%s", diff)
			}
		})
	}
	storeTRunAll(t, storeTExitCases())
}
