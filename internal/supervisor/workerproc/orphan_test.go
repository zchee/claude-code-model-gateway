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
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/workerproc"
)

// The evidence of the fixture sweep: a supervisor 701 of the current version protects the state root, and a worker
// 702 of an older version, launched by the supervisor 700 that has since exited, now runs reparented under 1.
const (
	orphanTRoot       = "/var/lib/model-gateway/state"
	orphanTStable     = "/opt/model-gateway/bin/model-gateway"
	orphanTCLI        = "/opt/model-gateway/cli/model-gateway"
	orphanTSupCommand = "/opt/model-gateway/bin/model-gateway serve-shim"

	orphanTWorkerStart = "Sun Oct 18 09:30:15 2026"
	orphanTSupStart    = "Sun Oct 18 08:00:00 2026"
	orphanTParentStart = "Sat Oct 17 07:00:00 2026"
	// orphanTZeroStart is the legacy start text of the zero time and of every instant in its first second.
	orphanTZeroStart = "Mon Jan  1 00:00:00 0001"

	orphanTWorkerToken procinfo.StartToken = "boot-a:702:6000"
	orphanTSupToken    procinfo.StartToken = "boot-a:701:5000"
	orphanTParentToken procinfo.StartToken = "boot-a:700:4000"

	// orphanTBeyond is the first process number above the range a process number may take.
	orphanTBeyond = math.MaxInt32 + 1
)

var (
	// The sampled births carry sub-second parts that the legacy start text drops.
	orphanTWorkerBirth = time.Date(2026, time.October, 18, 9, 30, 15, 250_000_000, time.UTC)
	orphanTSupBirth    = time.Date(2026, time.October, 18, 8, 0, 0, 750_000_000, time.UTC)
	orphanTInitBirth   = time.Date(2026, time.October, 1, 6, 0, 0, 0, time.UTC)
	// orphanTNearZero is not the zero time, yet its legacy start text is orphanTZeroStart.
	orphanTNearZero = time.Date(1, time.January, 1, 0, 0, 0, 500, time.UTC)
)

// orphanTVerdicts are the verdicts of the five staged Decide calls: with no sample, then after the worker, the
// parent, the supervisor and the recheck sample.
type orphanTVerdicts [5]workerproc.OrphanVerdict

// orphanTFixture is the evidence of one sweep and one birth found in it.
type orphanTFixture struct {
	rule    workerproc.OrphanRule
	birth   workerproc.Birth
	samples workerproc.OrphanSamples
}

// row edits the table row of pid in place; it fails the test when the fixture has no such row.
func (f *orphanTFixture) row(t *testing.T, pid int, edit func(*procinfo.Identity)) {
	t.Helper()
	id, ok := f.rule.Table[pid]
	if !ok {
		t.Fatalf("fixture has no table row %d", pid)
	}
	edit(&id)
	f.rule.Table[pid] = id
}

// worker applies edit to every observation of the worker: its table row and both worker samples.
func (f *orphanTFixture) worker(t *testing.T, edit func(*procinfo.Identity)) {
	t.Helper()
	f.row(t, 702, edit)
	edit(&f.samples.Worker.Identity)
	edit(&f.samples.Recheck.Identity)
}

// supervisor applies edit to every observation of the supervisor: its table row, the fresh sample and the second
// sample.
func (f *orphanTFixture) supervisor(t *testing.T, edit func(*procinfo.Identity)) {
	t.Helper()
	f.row(t, 701, edit)
	edit(&f.rule.Fresh.Identity)
	edit(&f.samples.Supervisor.Identity)
}

func orphanTWorkerIdentity() procinfo.Identity {
	return procinfo.Identity{
		PID:        702,
		ParentPID:  1,
		Argv:       []string{orphanTStable, "serve-worker"},
		Birth:      orphanTWorkerBirth,
		Image:      procinfo.ImageEvidence{Image: procinfo.Image{Path: orphanTStable, Device: 64769, Inode: 900}, DirectFromExecutable: true},
		StartToken: orphanTWorkerToken,
	}
}

func orphanTSupIdentity() procinfo.Identity {
	return procinfo.Identity{
		PID:        701,
		ParentPID:  1,
		Argv:       []string{orphanTStable, "serve-shim"},
		Birth:      orphanTSupBirth,
		Image:      procinfo.ImageEvidence{Image: procinfo.Image{Path: orphanTStable, Device: 64769, Inode: 1001}, DirectFromExecutable: true},
		StartToken: orphanTSupToken,
	}
}

// orphanTAncestor is a table row of an unrelated process pid whose parent is parent.
func orphanTAncestor(pid, parent int) procinfo.Identity {
	return procinfo.Identity{
		PID:        pid,
		ParentPID:  parent,
		Argv:       []string{"/usr/sbin/unrelated", "--daemon"},
		Birth:      orphanTInitBirth,
		Image:      procinfo.ImageEvidence{Image: procinfo.Image{Path: "/usr/sbin/unrelated", Device: 64769, Inode: 77}, DirectFromExecutable: true},
		StartToken: procinfo.StartToken(fmt.Sprintf("boot-a:%d:10", pid)),
	}
}

// orphanTBase returns, freshly built, the evidence under which the fixture worker is to be terminated once every
// sample is taken.
func orphanTBase() orphanTFixture {
	initRow := orphanTAncestor(1, 0)
	initRow.Argv = []string{"/sbin/init"}
	return orphanTFixture{
		rule: workerproc.OrphanRule{
			Root:        orphanTRoot,
			Version:     "0.50.0",
			Executables: []string{orphanTStable, orphanTCLI},
			Supervisor: procinfo.Record{
				PID:        701,
				Command:    orphanTSupCommand,
				StartedAt:  orphanTSupStart,
				Role:       "supervisor",
				StateRoot:  orphanTRoot,
				Generation: "sup-gen-3",
				Port:       41783,
				Version:    "0.50.0",
				Image:      procinfo.Image{Path: orphanTStable, Device: 64769, Inode: 1001},
				StartToken: orphanTSupToken,
			},
			Table: map[int]procinfo.Identity{
				1:   initRow,
				701: orphanTSupIdentity(),
				702: orphanTWorkerIdentity(),
			},
			Fresh: workerproc.Sample{PID: 701, Identity: orphanTSupIdentity()},
		},
		birth: workerproc.Birth{
			SchemaVersion: 1,
			PID:           702,
			Argv:          []string{orphanTStable, "serve-worker"},
			StartedAt:     orphanTWorkerStart,
			StartToken:    orphanTWorkerToken,
			StateRoot:     orphanTRoot,
			Generation:    "gen-7",
			Version:       "0.49.0",
			Image:         procinfo.Image{Path: orphanTStable, Device: 64769, Inode: 900},
			Parent: workerproc.BirthParent{
				PID:        700,
				Argv:       []string{orphanTStable, "serve-shim"},
				StartedAt:  orphanTParentStart,
				StartToken: orphanTParentToken,
			},
		},
		samples: workerproc.OrphanSamples{
			Worker:     workerproc.Sample{PID: 702, Identity: orphanTWorkerIdentity()},
			Parent:     workerproc.Sample{PID: 700, Err: os.ErrNotExist},
			Supervisor: workerproc.Sample{PID: 701, Identity: orphanTSupIdentity()},
			Recheck:    workerproc.Sample{PID: 702, Identity: orphanTWorkerIdentity()},
		},
	}
}

// orphanTCall runs f and reports its result; a panic fails the calling row with the panic's text instead.
func orphanTCall[T any](t *testing.T, call string, f func() T) (v T, ok bool) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("%s panicked: %v", call, p)
			ok = false
		}
	}()
	return f(), true
}

// orphanTFirst returns the first n samples of s in field order; the others are not taken.
func orphanTFirst(s workerproc.OrphanSamples, n int) workerproc.OrphanSamples {
	var out workerproc.OrphanSamples
	if n > 0 {
		out.Worker = s.Worker
	}
	if n > 1 {
		out.Parent = s.Parent
	}
	if n > 2 {
		out.Supervisor = s.Supervisor
	}
	if n > 3 {
		out.Recheck = s.Recheck
	}
	return out
}

func TestOrphanDecision(t *testing.T) {
	t.Run("Ended", orphanTEnded)
	t.Run("Protected", orphanTProtected)
	t.Run("Decide", orphanTDecide)
	t.Run("Staging", orphanTStaging)
}

// orphanTStaging supplies sample sets the integration never builds, with a gap in field order or with a sample that
// carries evidence but no requested process number, to show that a sample counts as taken by its requested process
// number alone and that the first rule needing a missing sample decides the verdict.
func orphanTStaging(t *testing.T) {
	tests := map[string]struct {
		samples func(base workerproc.OrphanSamples) workerproc.OrphanSamples
		want    workerproc.OrphanVerdict
	}{
		"success: every sample taken gives the verdict": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples { return base },
			want:    workerproc.OrphanTerminate,
		},
		"success: a worker proven gone is retired although a later sample in field order is missing": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples {
				base.Worker = workerproc.Sample{PID: 702, Err: os.ErrNotExist}
				base.Parent = workerproc.Sample{}
				return base
			},
			want: workerproc.OrphanRetire,
		},
		"error: later samples do not stand in for a missing worker sample": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples {
				base.Worker = workerproc.Sample{}
				return base
			},
			want: workerproc.OrphanPending,
		},
		"error: a worker sample with confirmed absence but no requested process number is not taken": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples {
				base.Worker = workerproc.Sample{Err: os.ErrNotExist}
				return base
			},
			want: workerproc.OrphanPending,
		},
		"error: a worker sample with the worker's identity but no requested process number is not taken": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples {
				base.Worker.PID = 0
				return base
			},
			want: workerproc.OrphanPending,
		},
		"error: a missing parent sample is needed although the supervisor and the recheck are taken": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples {
				base.Parent = workerproc.Sample{}
				return base
			},
			want: workerproc.OrphanPending,
		},
		"error: a parent sample with confirmed absence but no requested process number is not taken": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples {
				base.Parent = workerproc.Sample{Err: os.ErrNotExist}
				return base
			},
			want: workerproc.OrphanPending,
		},
		"error: a missing supervisor sample is needed although the recheck is taken": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples {
				base.Supervisor = workerproc.Sample{}
				return base
			},
			want: workerproc.OrphanPending,
		},
		"error: a supervisor sample with the supervisor's identity but no requested process number is not taken": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples {
				base.Supervisor.PID = 0
				return base
			},
			want: workerproc.OrphanPending,
		},
		"error: a recheck with the worker's identity but no requested process number is not taken": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples {
				base.Recheck.PID = 0
				return base
			},
			want: workerproc.OrphanPending,
		},
		"error: a recheck with confirmed absence but no requested process number is not taken": {
			samples: func(base workerproc.OrphanSamples) workerproc.OrphanSamples {
				base.Recheck = workerproc.Sample{Err: os.ErrNotExist}
				return base
			},
			want: workerproc.OrphanPending,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := orphanTBase()
			s := tt.samples(f.samples)
			got, ok := orphanTCall(t, "Decide", func() workerproc.OrphanVerdict { return f.rule.Decide(f.birth, s) })
			if !ok {
				return
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Decide(worker pid %d, parent pid %d, supervisor pid %d, recheck pid %d) mismatch (-want +got; keep=%d retire=%d terminate=%d pending=%d):\n%s",
					s.Worker.PID, s.Parent.PID, s.Supervisor.PID, s.Recheck.PID,
					workerproc.OrphanKeep, workerproc.OrphanRetire, workerproc.OrphanTerminate, workerproc.OrphanPending, diff)
			}
		})
	}
}

func orphanTEnded(t *testing.T) {
	tests := map[string]struct {
		birth  func(*workerproc.Birth)
		sample workerproc.Sample
		want   bool
	}{
		"success: confirmed absence of the worker's process number": {
			sample: workerproc.Sample{PID: 702, Err: os.ErrNotExist},
			want:   true,
		},
		"success: a wrapped confirmed absence": {
			sample: workerproc.Sample{PID: 702, Err: fmt.Errorf("inspect pid 702: %w", os.ErrNotExist)},
			want:   true,
		},
		"success: confirmed absence ignores the identity the failed probe carried": {
			sample: workerproc.Sample{PID: 702, Identity: orphanTWorkerIdentity(), Err: os.ErrNotExist},
			want:   true,
		},
		"success: the process number now carries another start token": {
			sample: workerproc.Sample{PID: 702, Identity: procinfo.Identity{
				PID: 702, ParentPID: 1, Argv: []string{orphanTStable, "serve-worker"}, Birth: orphanTWorkerBirth,
				StartToken: "boot-a:702:9000",
			}},
			want: true,
		},
		"success: another start token proves another lifetime whatever the rest of the sample shows": {
			sample: workerproc.Sample{PID: 702, Identity: procinfo.Identity{
				PID: 702, Argv: []string{"/usr/bin/other"}, StartToken: "boot-b:702:1",
			}},
			want: true,
		},
		"success: the largest process number in range": {
			birth:  func(b *workerproc.Birth) { b.PID = math.MaxInt32 },
			sample: workerproc.Sample{PID: math.MaxInt32, Err: os.ErrNotExist},
			want:   true,
		},
		"success: a birth holding only process number one and a token, confirmed absent": {
			birth:  func(b *workerproc.Birth) { *b = workerproc.Birth{PID: 1, StartToken: "old"} },
			sample: workerproc.Sample{PID: 1, Err: fmt.Errorf("inspect pid 1: %w", os.ErrNotExist)},
			want:   true,
		},
		"success: confirmed absence whatever process number the failed probe's identity names": {
			sample: workerproc.Sample{PID: 702, Identity: procinfo.Identity{PID: 703, StartToken: "boot-a:703:1"}, Err: os.ErrNotExist},
			want:   true,
		},
		"error: a sample that was not taken": {
			sample: workerproc.Sample{},
			want:   false,
		},
		"error: confirmed absence of another process number": {
			sample: workerproc.Sample{PID: 703, Err: os.ErrNotExist},
			want:   false,
		},
		"error: another token sampled for another process number": {
			sample: workerproc.Sample{PID: 703, Identity: procinfo.Identity{PID: 703, StartToken: "boot-a:703:1"}},
			want:   false,
		},
		"error: an inconclusive inspection": {
			sample: workerproc.Sample{PID: 702, Err: fmt.Errorf("%w: pid 702: incomplete process sample", procinfo.ErrUnknown)},
			want:   false,
		},
		"error: an inconclusive inspection that carries another token": {
			sample: workerproc.Sample{PID: 702, Identity: procinfo.Identity{PID: 702, StartToken: "boot-a:702:9000"}, Err: procinfo.ErrUnknown},
			want:   false,
		},
		"error: a permission error": {
			sample: workerproc.Sample{PID: 702, Err: os.ErrPermission},
			want:   false,
		},
		"error: a deadline": {
			sample: workerproc.Sample{PID: 702, Err: fmt.Errorf("%w: pid 702: %w", procinfo.ErrUnknown, context.DeadlineExceeded)},
			want:   false,
		},
		"error: a bare deadline": {
			sample: workerproc.Sample{PID: 702, Err: context.DeadlineExceeded},
			want:   false,
		},
		"error: a successful sample with an empty token": {
			sample: workerproc.Sample{PID: 702, Identity: procinfo.Identity{PID: 702, ParentPID: 1, Argv: []string{"/usr/bin/other"}, Birth: orphanTWorkerBirth}},
			want:   false,
		},
		"error: a successful sample with the birth's own token": {
			sample: workerproc.Sample{PID: 702, Identity: orphanTWorkerIdentity()},
			want:   false,
		},
		"error: a successful sample whose identity names another process number": {
			sample: workerproc.Sample{PID: 702, Identity: procinfo.Identity{PID: 703, StartToken: "boot-a:703:1"}},
			want:   false,
		},
		"error: a birth without a start token": {
			birth:  func(b *workerproc.Birth) { b.StartToken = "" },
			sample: workerproc.Sample{PID: 702, Err: os.ErrNotExist},
			want:   false,
		},
		"error: a birth without a start token against a sample with a token": {
			birth:  func(b *workerproc.Birth) { b.StartToken = "" },
			sample: workerproc.Sample{PID: 702, Identity: orphanTWorkerIdentity()},
			want:   false,
		},
		"error: a birth of process number zero": {
			birth:  func(b *workerproc.Birth) { b.PID = 0 },
			sample: workerproc.Sample{PID: 0, Err: os.ErrNotExist},
			want:   false,
		},
		"error: a birth of a negative process number": {
			birth:  func(b *workerproc.Birth) { b.PID = -702 },
			sample: workerproc.Sample{PID: -702, Err: os.ErrNotExist},
			want:   false,
		},
		"error: a birth of a process number above the range": {
			birth:  func(b *workerproc.Birth) { b.PID = orphanTBeyond },
			sample: workerproc.Sample{PID: orphanTBeyond, Err: os.ErrNotExist},
			want:   false,
		},
		"error: a birth of a process number above the range against another token": {
			birth:  func(b *workerproc.Birth) { b.PID = orphanTBeyond },
			sample: workerproc.Sample{PID: orphanTBeyond, Identity: procinfo.Identity{PID: orphanTBeyond, StartToken: "boot-a:x:1"}},
			want:   false,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := orphanTBase()
			if tt.birth != nil {
				tt.birth(&f.birth)
			}
			got, ok := orphanTCall(t, "Ended", func() bool { return workerproc.Ended(f.birth, tt.sample) })
			if !ok {
				return
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Ended(birth pid %d token %q, sample pid %d err %v identity pid %d token %q) mismatch (-want +got):\n%s",
					f.birth.PID, f.birth.StartToken, tt.sample.PID, tt.sample.Err, tt.sample.Identity.PID, tt.sample.Identity.StartToken, diff)
			}
		})
	}
}

func orphanTProtected(t *testing.T) {
	tests := map[string]struct {
		mutate func(t *testing.T, f *orphanTFixture)
		want   bool
	}{
		"success: the table row and the fresh sample show the recorded supervisor": {
			want: true,
		},
		"success: a correlated image is not judged against the record": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.supervisor(t, func(id *procinfo.Identity) {
					id.Image = procinfo.ImageEvidence{Path: "/private/var/folders/x/model-gateway", Device: 5, Inode: 5555}
				})
			},
			want: true,
		},
		"success: the sampled parent of the supervisor is not judged": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Fresh.Identity.ParentPID = 4242
			},
			want: true,
		},
		"success: the record's own version text is not judged by the version grammar": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.Version = "dev-build" },
			want:   true,
		},
		"success: no executables are needed to protect the supervisor": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Executables = nil },
			want:   true,
		},
		"success: a sweeping version with a lowercase v": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "v0.50.0" },
			want:   true,
		},
		"success: a sweeping version with a prerelease suffix": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "0.50.0-rc.1" },
			want:   true,
		},
		"success: a sweeping version with build metadata": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "0.50.0+abc" },
			want:   true,
		},
		"success: a development build's sweeping version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "0.0.0-dev+0123abcd" },
			want:   true,
		},
		"success: a sweeping version with nine-digit numerals": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "000000001.2.999999999" },
			want:   true,
		},
		"success: a supervisor born in the first second of year one": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.StartedAt = orphanTZeroStart
				f.supervisor(t, func(id *procinfo.Identity) { id.Birth = orphanTNearZero })
			},
			want: true,
		},
		"success: the largest process number in range": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				id := f.rule.Table[701]
				delete(f.rule.Table, 701)
				id.PID = math.MaxInt32
				f.rule.Table[math.MaxInt32] = id
				f.rule.Supervisor.PID = math.MaxInt32
				f.rule.Fresh.PID = math.MaxInt32
				f.rule.Fresh.Identity.PID = math.MaxInt32
			},
			want: true,
		},
		"error: a relative root": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Root = "var/lib/model-gateway/state"
				f.rule.Supervisor.StateRoot = "var/lib/model-gateway/state"
			},
		},
		"error: an empty root": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Root = ""
				f.rule.Supervisor.StateRoot = ""
			},
		},
		"error: a root with a trailing separator": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Root = orphanTRoot + "/"
				f.rule.Supervisor.StateRoot = orphanTRoot + "/"
			},
		},
		"error: a root with a parent element": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Root = "/var/lib/model-gateway/../model-gateway/state"
				f.rule.Supervisor.StateRoot = "/var/lib/model-gateway/../model-gateway/state"
			},
		},
		"error: a root holding a NUL byte": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Root = "/var/lib/model-gateway/st\x00ate"
				f.rule.Supervisor.StateRoot = "/var/lib/model-gateway/st\x00ate"
			},
		},
		"error: a record of another state root": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.StateRoot = "/var/lib/model-gateway/other" },
		},
		"error: a record with an empty command": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.Command = "" },
		},
		"error: a record with an empty role": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.Role = "" },
		},
		"error: a record with an empty generation": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.Generation = "" },
		},
		"error: a record with an empty version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.Version = "" },
		},
		"error: a record with a relative image path": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.Image.Path = "opt/model-gateway/bin/model-gateway"
			},
		},
		"error: a record with a zero inode": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.Image.Inode = 0
				f.supervisor(t, func(id *procinfo.Identity) { id.Image.Inode = 0 })
			},
		},
		"error: a record of process number zero": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				id := f.rule.Table[701]
				delete(f.rule.Table, 701)
				id.PID = 0
				f.rule.Table[0] = id
				f.rule.Supervisor.PID = 0
				f.rule.Fresh.PID = 0
				f.rule.Fresh.Identity.PID = 0
			},
		},
		"error: a record of a process number above the range": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				id := f.rule.Table[701]
				delete(f.rule.Table, 701)
				id.PID = orphanTBeyond
				f.rule.Table[orphanTBeyond] = id
				f.rule.Supervisor.PID = orphanTBeyond
				f.rule.Fresh.PID = orphanTBeyond
				f.rule.Fresh.Identity.PID = orphanTBeyond
			},
		},
		"error: a record start text that does not parse": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.StartedAt = "2026-10-18T08:00:00Z" },
		},
		"error: a record start text naming the wrong weekday": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.StartedAt = "Mon Oct 18 08:00:00 2026" },
		},
		"error: a record start text of year 10000, which the legacy form writes but cannot read back": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.StartedAt = "Sat Jan  1 00:00:00 10000"
				f.supervisor(t, func(id *procinfo.Identity) { id.Birth = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC) })
			},
			want: false,
		},
		"error: a record without a port": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.Port = 0 },
		},
		"error: a record of another role": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.Role = "guardian" },
		},
		"error: a supervisor record whose command, table row and samples agree on the worker role": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.Command = orphanTStable + " serve-worker"
				f.supervisor(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-worker"} })
			},
			want: false,
		},
		"error: a record without a start token, sampled without one": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.StartToken = ""
				f.supervisor(t, func(id *procinfo.Identity) { id.StartToken = "" })
			},
		},
		"error: an unreadable sweeping version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "unknown" },
		},
		"error: an empty sweeping version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "" },
		},
		"error: a sweeping version of two numerals": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "0.50" },
		},
		"error: a sweeping version of four numerals": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "0.50.0.1" },
		},
		"error: a sweeping version with an uppercase V": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "V0.50.0" },
		},
		"error: a sweeping version with two v": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "vv0.50.0" },
		},
		"error: a sweeping version with an empty prerelease suffix": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "0.50.0-" },
		},
		"error: a sweeping version with an empty build suffix": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "0.50.0+" },
		},
		"error: a sweeping version with leading white space": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = " 0.50.0" },
		},
		"error: a sweeping version with a trailing newline": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "0.50.0\n" },
		},
		"error: a sweeping version with a ten-digit numeral": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "1234567890.0.0" },
		},
		"error: a sweeping version with a disallowed suffix character": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "0.50.0_x" },
		},
		"error: a sweeping version whose prerelease suffix holds a disallowed character": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "0.50.0-rc_1" },
			want:   false,
		},
		"error: no process table": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Table = nil },
		},
		"error: the supervisor missing from the table": {
			mutate: func(t *testing.T, f *orphanTFixture) { delete(f.rule.Table, 701) },
		},
		"error: a table row of the supervisor whose own process number is not its key": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.row(t, 701, func(id *procinfo.Identity) { id.PID = 7010 }) },
		},
		"error: a table row of another lifetime than the record": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 701, func(id *procinfo.Identity) { id.StartToken = "boot-a:701:1234" })
			},
		},
		"error: a table row of the same token in another role": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 701, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-worker"} })
			},
		},
		"error: a table row of the same token with another command line": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 701, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-shim", "--port", "1"} })
			},
		},
		"error: a table row of the same token with another birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 701, func(id *procinfo.Identity) { id.Birth = orphanTSupBirth.Add(time.Second) })
			},
		},
		"error: a table row whose direct image contradicts the record": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 701, func(id *procinfo.Identity) { id.Image.Inode = 1002 })
			},
		},
		"error: a table row whose direct image is on another device": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 701, func(id *procinfo.Identity) { id.Image.Device = 64770 })
			},
		},
		"error: a table row with a zero birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.StartedAt = orphanTZeroStart
				f.supervisor(t, func(id *procinfo.Identity) { id.Birth = orphanTNearZero })
				f.row(t, 701, func(id *procinfo.Identity) { id.Birth = time.Time{} })
			},
		},
		"error: a table row without readable arguments": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.row(t, 701, func(id *procinfo.Identity) { id.Argv = nil }) },
		},
		"error: a table row of the supervisor's command without its role argument": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.Command = orphanTStable
				f.supervisor(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable} })
			},
		},
		"error: no fresh sample": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Fresh = workerproc.Sample{} },
		},
		"error: a fresh sample of another process number": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Fresh.PID = 799 },
		},
		"error: a fresh sample confirming absence": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Fresh = workerproc.Sample{PID: 701, Err: os.ErrNotExist} },
		},
		"error: an inconclusive fresh sample": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Fresh.Err = fmt.Errorf("%w: pid 701: incomplete process sample", procinfo.ErrUnknown)
			},
		},
		"error: a fresh sample of another lifetime": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Fresh.Identity.StartToken = "boot-a:701:9999" },
		},
		"error: a fresh sample without a start token": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Fresh.Identity.StartToken = "" },
		},
		"error: a fresh sample of the same token in another role": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Fresh.Identity.Argv = []string{orphanTStable, "serve-worker"}
			},
		},
		"error: a fresh sample of the same token with another birth": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Fresh.Identity.Birth = orphanTSupBirth.Add(-time.Second) },
		},
		"error: a fresh sample whose direct image contradicts the record": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Fresh.Identity.Image.Inode = 1002 },
		},
		"error: a fresh sample whose identity names another process number": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Fresh.Identity.PID = 799 },
		},
		"error: a fresh sample with a zero birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.StartedAt = orphanTZeroStart
				f.supervisor(t, func(id *procinfo.Identity) { id.Birth = orphanTNearZero })
				f.rule.Fresh.Identity.Birth = time.Time{}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := orphanTBase()
			if tt.mutate != nil {
				tt.mutate(t, &f)
			}
			if got, ok := orphanTCall(t, "Protected", f.rule.Protected); ok {
				if diff := gocmp.Diff(tt.want, got); diff != "" {
					t.Errorf("Protected() mismatch (-want +got):\n%s", diff)
				}
			}
			if tt.want {
				return
			}
			// Decide applies the same gate: a sweep that is not protected keeps the birth at every call.
			keep := workerproc.OrphanKeep
			want := orphanTVerdicts{keep, keep, keep, keep, keep}
			got, ok := orphanTStaged(t, f)
			if !ok {
				return
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("Decide verdicts of an unprotected sweep with 0 to 4 samples (worker, parent, supervisor, recheck; keep=%d retire=%d terminate=%d pending=%d) mismatch (-want +got):\n%s",
					workerproc.OrphanKeep, workerproc.OrphanRetire, workerproc.OrphanTerminate, workerproc.OrphanPending, diff)
			}
		})
	}
}

// orphanTStaged calls Decide with every prefix of the samples in field order, from no sample to all four, so that a row
// checks both the first decision and its stability; the integration itself stops at the first terminal verdict. It
// returns the five verdicts; a panic fails the row instead.
func orphanTStaged(t *testing.T, f orphanTFixture) (orphanTVerdicts, bool) {
	t.Helper()
	var got orphanTVerdicts
	for n := range len(got) {
		s := orphanTFirst(f.samples, n)
		v, ok := orphanTCall(t, fmt.Sprintf("Decide with %d samples", n), func() workerproc.OrphanVerdict { return f.rule.Decide(f.birth, s) })
		if !ok {
			return got, false
		}
		got[n] = v
	}
	return got, true
}

func orphanTDecide(t *testing.T) {
	const (
		pend   = workerproc.OrphanPending
		keep   = workerproc.OrphanKeep
		retire = workerproc.OrphanRetire
		term   = workerproc.OrphanTerminate
	)
	tests := map[string]struct {
		mutate func(t *testing.T, f *orphanTFixture)
		want   orphanTVerdicts
	}{
		// The orphan of the fixture and its accepted variations.
		"success: a stale worker whose launching supervisor exited is terminated once every sample is taken": {
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a recorded parent whose number now carries another token has exited": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Parent = workerproc.Sample{PID: 700, Identity: procinfo.Identity{
					PID: 700, ParentPID: 1, Argv: []string{"/usr/bin/other"}, Birth: orphanTSupBirth, StartToken: "boot-a:700:8000",
				}}
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a wrapped confirmed absence of the recorded parent": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Parent.Err = fmt.Errorf("inspect pid 700: %w", os.ErrNotExist)
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a worker launched from the running CLI executable": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Argv = []string{orphanTCLI, "serve-worker"}
				f.birth.Image.Path = orphanTCLI
				f.worker(t, func(id *procinfo.Identity) {
					id.Argv = []string{orphanTCLI, "serve-worker"}
					id.Image.Path = orphanTCLI
				})
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a worker with further arguments after its role": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Argv = []string{orphanTStable, "serve-worker", "--generation", "gen-7"}
				f.worker(t, func(id *procinfo.Identity) {
					id.Argv = []string{orphanTStable, "serve-worker", "--generation", "gen-7"}
				})
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a correlated worker image is not judged against the birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) {
					id.Image = procinfo.ImageEvidence{Path: "/private/var/folders/x/model-gateway", Device: 5, Inode: 5555}
				})
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a worker born in the first second of year one": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.StartedAt = orphanTZeroStart
				f.worker(t, func(id *procinfo.Identity) { id.Birth = orphanTNearZero })
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a supervisor born in the first second of year one": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.StartedAt = orphanTZeroStart
				f.supervisor(t, func(id *procinfo.Identity) { id.Birth = orphanTNearZero })
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a worker table row without readable arguments still places the worker": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.row(t, 702, func(id *procinfo.Identity) { id.Argv = nil }) },
			want:   orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: the largest worker process number in range": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				id := f.rule.Table[702]
				delete(f.rule.Table, 702)
				id.PID = math.MaxInt32
				f.rule.Table[math.MaxInt32] = id
				f.birth.PID = math.MaxInt32
				f.samples.Worker.PID = math.MaxInt32
				f.samples.Worker.Identity.PID = math.MaxInt32
				f.samples.Recheck.PID = math.MaxInt32
				f.samples.Recheck.Identity.PID = math.MaxInt32
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: an older version by numeric order, not by text order": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Version = "0.10.0"
				f.rule.Supervisor.Version = "0.10.0"
				f.birth.Version = "0.9.0"
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: an older minor version with a higher patch": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "0.49.99" },
			want:   orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: an older patch version of the same minor": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Version = "0.50.2"
				f.birth.Version = "0.50.1"
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: births sampled in zones other than UTC are compared in their own zone": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				workerBirth := time.Date(2026, time.October, 18, 18, 30, 15, 250_000_000, time.FixedZone("JST", 9*60*60))
				supBirth := time.Date(2026, time.October, 18, 2, 30, 15, 250_000_000, time.FixedZone("PDT", -7*60*60))
				f.birth.StartedAt = "Sun Oct 18 18:30:15 2026"
				f.rule.Supervisor.StartedAt = "Sun Oct 18 02:30:15 2026"
				f.worker(t, func(id *procinfo.Identity) { id.Birth = workerBirth })
				f.supervisor(t, func(id *procinfo.Identity) { id.Birth = supBirth })
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a birth whose encoding is exactly 65,536 bytes": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				// The fixture's birth with an empty third argument encodes to 483 bytes.
				long := strings.Repeat("a", 65_053)
				f.birth.Argv = []string{orphanTStable, "serve-worker", long}
				f.worker(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-worker", long} })
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a recorded parent that ran another program than a supervisor": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Parent.Argv = []string{"/historical/launcher"} },
			want:   orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a worker table row with a zero birth still places the worker": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 702, func(id *procinfo.Identity) { id.Birth = time.Time{} })
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a second supervisor sample whose direct image is observed at another path": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Supervisor.Identity.Image.Path = orphanTStable + " (deleted)"
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a recheck whose direct image is observed at another path": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Recheck.Identity.Image.Path = orphanTStable + " (deleted)"
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: an older version with a lowercase v and build metadata": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "v0.49.0+abc" },
			want:   orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a development build of the worker is older than a release": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "0.0.0-dev+0123abcd" },
			want:   orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a sweeping version with a lowercase v and a prerelease suffix": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "v0.50.0-rc.1" },
			want:   orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a nine-digit minor numeral of the worker is older than the next major": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Version = "1.0.0"
				f.birth.Version = "0.999999999.0"
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a longer chain to the root": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) { id.ParentPID = 900 })
				f.rule.Table[900] = orphanTAncestor(900, 901)
				f.rule.Table[901] = orphanTAncestor(901, 1)
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: an ancestor row without readable arguments still links the chain": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.row(t, 1, func(id *procinfo.Identity) { id.Argv = nil }) },
			want:   orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: a worker that is itself a proven root": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) { id.ParentPID = 0 })
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},
		"success: the second supervisor sample may show a correlated image": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Supervisor.Identity.Image = procinfo.ImageEvidence{Path: "/x", Device: 5, Inode: 5555}
			},
			want: orphanTVerdicts{pend, pend, pend, pend, term},
		},

		// The worker's lifetime is over: the record is retired without a signal.
		"success: a worker confirmed absent is retired": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker = workerproc.Sample{PID: 702, Err: os.ErrNotExist}
			},
			want: orphanTVerdicts{pend, retire, retire, retire, retire},
		},
		"success: a worker confirmed absent through a wrapped error is retired": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker = workerproc.Sample{PID: 702, Err: fmt.Errorf("inspect pid 702: %w", os.ErrNotExist)}
			},
			want: orphanTVerdicts{pend, retire, retire, retire, retire},
		},
		"success: a worker number reused by another lifetime is retired": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Worker.Identity.StartToken = "boot-a:702:9000" },
			want:   orphanTVerdicts{pend, retire, retire, retire, retire},
		},
		"success: a worker number reused by another lifetime and command is retired": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker.Identity = procinfo.Identity{
					PID: 702, ParentPID: 1, Argv: []string{"/usr/bin/other"}, Birth: orphanTSupBirth, StartToken: "boot-a:702:9000",
				}
			},
			want: orphanTVerdicts{pend, retire, retire, retire, retire},
		},
		"success: a retired birth stays retired when the later samples would keep it": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker = workerproc.Sample{PID: 702, Err: os.ErrNotExist}
				f.samples.Parent = workerproc.Sample{PID: 700, Identity: procinfo.Identity{
					PID: 700, ParentPID: 1, Argv: []string{orphanTStable, "serve-shim"}, Birth: orphanTSupBirth, StartToken: orphanTParentToken,
				}}
				f.samples.Supervisor = workerproc.Sample{PID: 701, Err: procinfo.ErrUnknown}
			},
			want: orphanTVerdicts{pend, retire, retire, retire, retire},
		},
		"success: a worker that exited is retired even under the supervisor": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 702, func(id *procinfo.Identity) { id.ParentPID = 701 })
				f.samples.Worker = workerproc.Sample{PID: 702, Err: os.ErrNotExist}
			},
			want: orphanTVerdicts{pend, retire, retire, retire, retire},
		},

		// The sweep is not protected: every birth is kept and no sample is needed.
		"error: a record without a port keeps every birth": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Supervisor.Port = 0 },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: no process table keeps every birth": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Table = nil },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: no fresh supervisor sample keeps every birth": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Fresh = workerproc.Sample{} },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: an inconclusive fresh supervisor sample keeps every birth": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Fresh.Err = procinfo.ErrUnknown },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: an unreadable sweeping version keeps every birth": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Version = "unknown" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a table of another supervisor lifetime keeps every birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 701, func(id *procinfo.Identity) { id.StartToken = "boot-a:701:1234" })
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: an unprotected sweep keeps even a birth whose worker exited": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.Role = "guardian"
				f.samples.Worker = workerproc.Sample{PID: 702, Err: os.ErrNotExist}
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},

		// The birth is not a valid birth of this state root.
		"error: a birth of schema version 2": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.SchemaVersion = 2 },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth without a schema version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.SchemaVersion = 0 },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of process number zero": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.PID = 0 },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of a negative process number": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.PID = -702 },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of a process number above the range": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				id := f.rule.Table[702]
				delete(f.rule.Table, 702)
				id.PID = orphanTBeyond
				f.rule.Table[orphanTBeyond] = id
				f.birth.PID = orphanTBeyond
				f.samples.Worker.PID = orphanTBeyond
				f.samples.Worker.Identity.PID = orphanTBeyond
				f.samples.Recheck.PID = orphanTBeyond
				f.samples.Recheck.Identity.PID = orphanTBeyond
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth without a start token": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.StartToken = "" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose command line is only the executable": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Argv = []string{orphanTStable}
				f.worker(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable} })
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose command does not start with its image": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Argv = []string{orphanTCLI, "serve-worker"}
				f.worker(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTCLI, "serve-worker"} })
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose command is not a worker": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Argv = []string{orphanTStable, "serve-shim"}
				f.worker(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-shim"} })
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose command is a test binary's wrapper": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Argv = []string{orphanTStable, "-test.run=TestHelper", "serve-worker"}
				f.worker(t, func(id *procinfo.Identity) {
					id.Argv = []string{orphanTStable, "-test.run=TestHelper", "serve-worker"}
				})
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of another state root": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.StateRoot = "/var/lib/model-gateway/other" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth without a generation": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Generation = "" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth with an unreadable version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "unknown" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth without a version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth with an uppercase V version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "V0.49.0" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth with a version of two numerals": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "0.49" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth with an empty version suffix": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "0.49.0-" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose build suffix holds a disallowed character": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "0.49.0+build/1" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth with a relative image path": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				const relative = "opt/model-gateway/bin/model-gateway"
				f.rule.Executables = append(f.rule.Executables, relative)
				f.birth.Argv = []string{relative, "serve-worker"}
				f.birth.Image.Path = relative
				f.worker(t, func(id *procinfo.Identity) {
					id.Argv = []string{relative, "serve-worker"}
					id.Image.Path = relative
				})
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth with an unclean image path": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				const unclean = "/opt/model-gateway/bin/./model-gateway"
				f.rule.Executables = append(f.rule.Executables, unclean)
				f.birth.Argv = []string{unclean, "serve-worker"}
				f.birth.Image.Path = unclean
				f.worker(t, func(id *procinfo.Identity) {
					id.Argv = []string{unclean, "serve-worker"}
					id.Image.Path = unclean
				})
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth with an image path holding a NUL byte": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				const nul = "/opt/model-gateway/bin/model\x00gateway"
				f.rule.Executables = append(f.rule.Executables, nul)
				f.birth.Argv = []string{nul, "serve-worker"}
				f.birth.Image.Path = nul
				f.worker(t, func(id *procinfo.Identity) {
					id.Argv = []string{nul, "serve-worker"}
					id.Image.Path = nul
				})
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth with a zero image inode": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Image.Inode = 0
				f.worker(t, func(id *procinfo.Identity) { id.Image.Inode = 0 })
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose parent is process number zero": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Parent.PID = 0 },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose parent is above the range": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Parent.PID = orphanTBeyond
				f.samples.Parent.PID = orphanTBeyond
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose parent is the worker itself": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Parent.PID = 702
				f.samples.Parent.PID = 702
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose parent has no start token": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Parent.StartToken = "" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose parent has no command line": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Parent.Argv = nil },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose parent's executable is empty": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Parent.Argv = []string{"", "serve-shim"} },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose start text does not parse": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.StartedAt = "Sun Oct 18 09:30:15 2026 UTC" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth without a start text": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.StartedAt = "" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose parent start text does not parse": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Parent.StartedAt = "Sat Oct 17 7:00:00 2026" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose parent start text names the wrong weekday": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Parent.StartedAt = "Mon Oct 17 07:00:00 2026" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth without a parent start text": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Parent.StartedAt = "" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose command holds invalid UTF-8": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Argv = []string{orphanTStable, "serve-worker", "\xff"}
				f.worker(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-worker", "\xff"} })
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose encoding is one byte over 65,536": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				// The fixture's birth with an empty third argument encodes to 483 bytes.
				long := strings.Repeat("a", 65_054)
				f.birth.Argv = []string{orphanTStable, "serve-worker", long}
				f.worker(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-worker", long} })
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth whose encoding exceeds 65,536 bytes": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				long := strings.Repeat("a", 70_000)
				f.birth.Argv = []string{orphanTStable, "serve-worker", long}
				f.worker(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-worker", long} })
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},

		// Another installation.
		"error: a birth launched from an executable this installation does not use": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Executables = []string{orphanTCLI} },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a sweep with no executables keeps every birth": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Executables = nil },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: an executable that differs only by a trailing separator": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.rule.Executables = []string{orphanTStable + "/", orphanTCLI} },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: an executable that names the same file through another spelling": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Executables = []string{"/opt/model-gateway/cli/../bin/model-gateway", orphanTCLI}
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: an executable that differs in case": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Executables = []string{"/opt/Model-Gateway/bin/model-gateway", orphanTCLI}
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},

		// Not an older version.
		"error: a birth of the sweeping version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "0.50.0" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of the sweeping version with a prerelease suffix": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "0.50.0-rc.1" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of the sweeping version with a lowercase v": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "v0.50.0" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of the sweeping version with leading zeros": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "000.050.000" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of a newer minor version with a lower patch": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Version = "0.50.2"
				f.birth.Version = "0.51.0"
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of a newer patch version of the same minor": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Version = "0.50.2"
				f.birth.Version = "0.50.3"
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of a newer version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "0.51.0" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth of a newer major version": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.birth.Version = "1.0.0" },
			want:   orphanTVerdicts{keep, keep, keep, keep, keep},
		},
		"error: a birth newer by numeric order though older by text order": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Version = "0.9.0"
				f.rule.Supervisor.Version = "0.9.0"
				f.birth.Version = "0.10.0"
			},
			want: orphanTVerdicts{keep, keep, keep, keep, keep},
		},

		// The first worker sample does not show the recorded worker.
		"error: an inconclusive worker sample keeps the birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker = workerproc.Sample{PID: 702, Err: fmt.Errorf("%w: pid 702: incomplete process sample", procinfo.ErrUnknown)}
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a denied worker inspection keeps the birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker = workerproc.Sample{PID: 702, Err: os.ErrPermission}
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: an inconclusive worker sample that carries a matching identity": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Worker.Err = procinfo.ErrUnknown },
			want:   orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: absence of another process number is no evidence about the worker": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker = workerproc.Sample{PID: 703, Err: os.ErrNotExist}
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a sample of another process number that matches the worker": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Worker.PID = 703 },
			want:   orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker sample whose identity names another process number": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Worker.Identity.PID = 703 },
			want:   orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker sample without a start token": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Worker.Identity.StartToken = "" },
			want:   orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: the worker's number running another command with the same token": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker.Identity.Argv = []string{orphanTStable, "serve-worker", "--foreign"}
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker sample that regroups the recorded arguments into other elements": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Argv = []string{orphanTStable, "serve-worker", "a b", "c"}
				f.worker(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-worker", "a b", "c"} })
				f.samples.Worker.Identity.Argv = []string{orphanTStable, "serve-worker", "a", "b c"}
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: the worker's number running a foreign program with the same token": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker.Identity.Argv = []string{"/usr/bin/foreign-worker"}
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a same-token worker with another birth is a contradiction, not an exit": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker.Identity.Birth = orphanTWorkerBirth.Add(time.Second)
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a same-token worker sample with a zero birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.StartedAt = orphanTZeroStart
				f.worker(t, func(id *procinfo.Identity) { id.Birth = orphanTNearZero })
				f.samples.Worker.Identity.Birth = time.Time{}
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker sample whose direct image contradicts the birth": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Worker.Identity.Image.Inode = 901 },
			want:   orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker sample whose direct image is on another device": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Worker.Identity.Image.Device = 64770 },
			want:   orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker missing from the table": {
			mutate: func(t *testing.T, f *orphanTFixture) { delete(f.rule.Table, 702) },
			want:   orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker table row whose own process number is not its key": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.row(t, 702, func(id *procinfo.Identity) { id.PID = 7020 }) },
			want:   orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker table row of another lifetime": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 702, func(id *procinfo.Identity) { id.StartToken = "boot-a:702:1" })
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker table row without a start token": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.row(t, 702, func(id *procinfo.Identity) { id.StartToken = "" })
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker sample whose parent differs from its table row": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Table[900] = orphanTAncestor(900, 1)
				f.samples.Worker.Identity.ParentPID = 900
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: an unknown worker sample never retires the birth whatever follows": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Worker = workerproc.Sample{PID: 702, Err: procinfo.ErrUnknown}
				f.samples.Recheck = workerproc.Sample{PID: 702, Err: os.ErrNotExist}
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},

		// The process table does not prove the worker an orphan.
		"error: a current worker under the protected supervisor": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) { id.ParentPID = 701 })
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker descending from the protected supervisor": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) { id.ParentPID = 900 })
				f.rule.Table[900] = orphanTAncestor(900, 701)
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a missing node in the worker's ancestry": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) { id.ParentPID = 900 })
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a missing root in the worker's ancestry": {
			mutate: func(t *testing.T, f *orphanTFixture) { delete(f.rule.Table, 1) },
			want:   orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: an ancestry row whose own process number is not its key": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) { id.ParentPID = 900 })
				f.rule.Table[900] = orphanTAncestor(9000, 1)
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker that is its own parent": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) { id.ParentPID = 702 })
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a cycle above the worker": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) { id.ParentPID = 900 })
				f.rule.Table[900] = orphanTAncestor(900, 901)
				f.rule.Table[901] = orphanTAncestor(901, 900)
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a cycle through the worker": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) { id.ParentPID = 900 })
				f.rule.Table[900] = orphanTAncestor(900, 702)
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker whose own process number is the protected supervisor's": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.PID = 701
				f.birth.StartToken = orphanTSupToken
				f.birth.StartedAt = orphanTSupStart
				worker := orphanTWorkerIdentity()
				worker.PID = 701
				worker.Birth = orphanTSupBirth
				worker.StartToken = orphanTSupToken
				f.samples.Worker = workerproc.Sample{PID: 701, Identity: worker}
				f.samples.Recheck = workerproc.Sample{PID: 701, Identity: worker}
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},
		"error: a worker still at its dead parent's number is not a rooted chain": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.worker(t, func(id *procinfo.Identity) { id.ParentPID = 700 })
			},
			want: orphanTVerdicts{pend, keep, keep, keep, keep},
		},

		// The recorded parent is not proven gone.
		"error: a still-live recorded parent": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Parent = workerproc.Sample{PID: 700, Identity: procinfo.Identity{
					PID: 700, ParentPID: 1, Argv: []string{orphanTStable, "serve-shim"}, Birth: orphanTSupBirth, StartToken: orphanTParentToken,
				}}
			},
			want: orphanTVerdicts{pend, pend, keep, keep, keep},
		},
		"error: an inconclusive parent sample": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Parent = workerproc.Sample{PID: 700, Err: fmt.Errorf("%w: pid 700: incomplete process sample", procinfo.ErrUnknown)}
			},
			want: orphanTVerdicts{pend, pend, keep, keep, keep},
		},
		"error: a denied parent inspection": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Parent = workerproc.Sample{PID: 700, Err: os.ErrPermission}
			},
			want: orphanTVerdicts{pend, pend, keep, keep, keep},
		},
		"error: a parent sample without a start token": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Parent = workerproc.Sample{PID: 700, Identity: procinfo.Identity{
					PID: 700, ParentPID: 1, Argv: []string{"/usr/bin/other"}, Birth: orphanTSupBirth,
				}}
			},
			want: orphanTVerdicts{pend, pend, keep, keep, keep},
		},
		"error: absence of another process number is no evidence about the parent": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Parent = workerproc.Sample{PID: 799, Err: os.ErrNotExist}
			},
			want: orphanTVerdicts{pend, pend, keep, keep, keep},
		},
		"error: a parent sample whose identity names another process number": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Parent = workerproc.Sample{PID: 700, Identity: procinfo.Identity{PID: 799, StartToken: "boot-a:799:1"}}
			},
			want: orphanTVerdicts{pend, pend, keep, keep, keep},
		},
		"error: an inconclusive parent sample that carries another token": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Parent = workerproc.Sample{PID: 700, Identity: procinfo.Identity{PID: 700, StartToken: "boot-a:700:8000"}, Err: procinfo.ErrUnknown}
			},
			want: orphanTVerdicts{pend, pend, keep, keep, keep},
		},

		// The second supervisor sample does not show the protected supervisor.
		"error: a second supervisor sample of another process number": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Supervisor.PID = 799
				f.samples.Supervisor.Identity.PID = 799
			},
			want: orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample requested for another process number": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Supervisor.PID = 799 },
			want:   orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample that regroups the role argument with the next one": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.Command = orphanTSupCommand + " x"
				f.supervisor(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-shim", "x"} })
				f.samples.Supervisor.Identity.Argv = []string{orphanTStable, "serve-shim x"}
			},
			want: orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample whose direct image is on another device": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Supervisor.Identity.Image.Device = 64770 },
			want:   orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample of the right number whose identity names another": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Supervisor.Identity.PID = 799 },
			want:   orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample confirming absence": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Supervisor = workerproc.Sample{PID: 701, Err: os.ErrNotExist}
			},
			want: orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: an inconclusive second supervisor sample that carries the identity": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Supervisor.Err = procinfo.ErrUnknown },
			want:   orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample of another lifetime": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Supervisor.Identity.StartToken = "boot-a:701:9999" },
			want:   orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample of the same token in another role": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Supervisor.Identity.Argv = []string{orphanTStable, "serve-worker"}
			},
			want: orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample with another command line": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Supervisor.Identity.Argv = []string{orphanTStable, "serve-shim", "--port", "1"}
			},
			want: orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample with another birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Supervisor.Identity.Birth = orphanTSupBirth.Add(time.Minute)
			},
			want: orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample whose direct image contradicts the record": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Supervisor.Identity.Image.Inode = 1002 },
			want:   orphanTVerdicts{pend, pend, pend, keep, keep},
		},
		"error: a second supervisor sample with a zero birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.rule.Supervisor.StartedAt = orphanTZeroStart
				f.supervisor(t, func(id *procinfo.Identity) { id.Birth = orphanTNearZero })
				f.samples.Supervisor.Identity.Birth = time.Time{}
			},
			want: orphanTVerdicts{pend, pend, pend, keep, keep},
		},

		// The recheck of the worker, taken after the handle was opened, does not show the same worker.
		"error: a recheck confirming absence keeps the birth rather than retiring it": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Recheck = workerproc.Sample{PID: 702, Err: os.ErrNotExist}
			},
			want: orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck of another lifetime keeps the birth rather than retiring it": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Recheck.Identity.StartToken = "boot-a:702:9000" },
			want:   orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck with another birth before the signal": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Recheck.Identity.Birth = orphanTWorkerBirth.Add(time.Second)
			},
			want: orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a parent change between the table and the recheck": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Recheck.Identity.ParentPID = 701 },
			want:   orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: an inconclusive recheck": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Recheck = workerproc.Sample{PID: 702, Err: procinfo.ErrUnknown}
			},
			want: orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: an inconclusive recheck that carries the identity": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Recheck.Err = os.ErrPermission },
			want:   orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck of another process number": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Recheck.PID = 703
				f.samples.Recheck.Identity.PID = 703
			},
			want: orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck requested for another process number": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Recheck.PID = 703 },
			want:   orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck that regroups the recorded arguments into other elements": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.Argv = []string{orphanTStable, "serve-worker", "a b", "c"}
				f.worker(t, func(id *procinfo.Identity) { id.Argv = []string{orphanTStable, "serve-worker", "a b", "c"} })
				f.samples.Recheck.Identity.Argv = []string{orphanTStable, "serve-worker", "a", "b c"}
			},
			want: orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck whose direct image is on another device": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Recheck.Identity.Image.Device = 64770 },
			want:   orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck of the worker's number whose identity names another": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Recheck.Identity.PID = 703 },
			want:   orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck with another command line": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.samples.Recheck.Identity.Argv = []string{orphanTStable, "serve-worker", "--foreign"}
			},
			want: orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck whose direct image contradicts the birth": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Recheck.Identity.Image.Inode = 901 },
			want:   orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck without a start token": {
			mutate: func(t *testing.T, f *orphanTFixture) { f.samples.Recheck.Identity.StartToken = "" },
			want:   orphanTVerdicts{pend, pend, pend, pend, keep},
		},
		"error: a recheck with a zero birth": {
			mutate: func(t *testing.T, f *orphanTFixture) {
				f.birth.StartedAt = orphanTZeroStart
				f.worker(t, func(id *procinfo.Identity) { id.Birth = orphanTNearZero })
				f.samples.Recheck.Identity.Birth = time.Time{}
			},
			want: orphanTVerdicts{pend, pend, pend, pend, keep},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := orphanTBase()
			if tt.mutate != nil {
				tt.mutate(t, &f)
			}
			got, ok := orphanTStaged(t, f)
			if !ok {
				return
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Decide verdicts with 0 to 4 samples (worker, parent, supervisor, recheck; keep=%d retire=%d terminate=%d pending=%d) mismatch (-want +got):\n%s",
					keep, retire, term, pend, diff)
			}
		})
	}
}
