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

package ownership

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// storeEndedTName names an outcome form in a failure message.
func storeEndedTName(op storeOutTOp) string {
	switch op {
	case storeOutTPublish:
		return "PublishContext"
	case storeOutTReconcile:
		return "Reconcile"
	case storeOutTRead:
		return "ReadContext"
	default:
		return "RemoveIfContext"
	}
}

// storeEndedTResult is what one call of an outcome form gave: its boolean result (published, wrote, removed; false for
// ReadContext), the record ReadContext returned (the zero record for the other forms) and the error.
type storeEndedTResult struct {
	ok     bool
	record procinfo.Record
	err    error
}

// storeEndedTCall calls op on s for the record name storeOutTName with r, on a context whose parent is t.Context() and
// which ended before the call as wait says: canceled, or with a deadline in the past. ok is false, and the row failed,
// when the context had not ended or the call panicked; the panic is recovered here so that it fails only its row.
func storeEndedTCall(t *testing.T, what string, op storeOutTOp, wait storeOutTWait, s Store, r procinfo.Record) (res storeEndedTResult, ok bool) {
	t.Helper()
	var ctx context.Context
	var cancel context.CancelFunc
	switch wait {
	case storeOutTCanceled:
		ctx, cancel = context.WithCancel(t.Context())
		cancel()
	case storeOutTExpired:
		ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	default:
		t.Errorf("%s: the row's context must end before the call; wait %d does not", what, wait)
		return res, false
	}
	defer cancel()
	if ctx.Err() == nil {
		t.Errorf("%s: the row's context had not ended before the call", what)
		return res, false
	}
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("%s panicked: %v", what, p)
			res, ok = storeEndedTResult{}, false
		}
	}()
	switch op {
	case storeOutTPublish:
		res.ok, res.err = s.PublishContext(ctx, storeOutTName, r)
	case storeOutTReconcile:
		res.ok, res.err = s.Reconcile(ctx, storeOutTName, r)
	case storeOutTRead:
		res.record, res.err = s.ReadContext(ctx, storeOutTName)
	default:
		res.ok, res.err = s.RemoveIfContext(ctx, storeOutTName, r)
	}
	return res, true
}

// storeEndedTParent returns every entry of the fixture's parent directory by its name.
func storeEndedTParent(fx *storeOutTFixture) map[string]storeOutTEntry {
	t := fx.t
	t.Helper()
	entries, err := os.ReadDir(fx.parent)
	if err != nil {
		t.Fatal(err)
	}
	state := make(map[string]storeOutTEntry, len(entries))
	for _, entry := range entries {
		state[entry.Name()] = storeOutTEntryOf(t, filepath.Join(fx.parent, entry.Name()), false)
	}
	return state
}

// TestStoreEndedContext decides that an outcome form whose arguments are valid and whose context ended before the call
// returns that context's error and creates nothing, also when the state root does not exist: the root stays missing,
// its parent keeps exactly its entries, no record lock file appears, the boolean result is false and ReadContext
// returns the zero record. The rows on an existing empty state root are the controls.
func TestStoreEndedContext(t *testing.T) {
	tests := map[string]struct {
		op   storeOutTOp
		wait storeOutTWait
		// missingRoot removes the fixture's state root before the call, leaving a clean absolute path that does not
		// exist inside an existing parent.
		missingRoot bool
		wantErr     error
	}{
		"error: PublishContext with a context canceled before the call on a missing state root returns the cancellation and creates nothing": {
			op: storeOutTPublish, wait: storeOutTCanceled, missingRoot: true, wantErr: context.Canceled,
		},
		"error: PublishContext with a deadline passed before the call on a missing state root returns the deadline and creates nothing": {
			op: storeOutTPublish, wait: storeOutTExpired, missingRoot: true, wantErr: context.DeadlineExceeded,
		},
		"error: Reconcile with a context canceled before the call on a missing state root returns the cancellation and creates nothing": {
			op: storeOutTReconcile, wait: storeOutTCanceled, missingRoot: true, wantErr: context.Canceled,
		},
		"error: Reconcile with a deadline passed before the call on a missing state root returns the deadline and creates nothing": {
			op: storeOutTReconcile, wait: storeOutTExpired, missingRoot: true, wantErr: context.DeadlineExceeded,
		},
		"error: RemoveIfContext with a context canceled before the call on a missing state root returns the cancellation and creates nothing": {
			op: storeOutTRemove, wait: storeOutTCanceled, missingRoot: true, wantErr: context.Canceled,
		},
		"error: RemoveIfContext with a deadline passed before the call on a missing state root returns the deadline and creates nothing": {
			op: storeOutTRemove, wait: storeOutTExpired, missingRoot: true, wantErr: context.DeadlineExceeded,
		},
		"error: ReadContext with a context canceled before the call on a missing state root returns the cancellation and creates nothing": {
			op: storeOutTRead, wait: storeOutTCanceled, missingRoot: true, wantErr: context.Canceled,
		},
		"error: ReadContext with a deadline passed before the call on a missing state root returns the deadline and creates nothing": {
			op: storeOutTRead, wait: storeOutTExpired, missingRoot: true, wantErr: context.DeadlineExceeded,
		},
		"error: PublishContext with a context canceled before the call on an existing empty state root returns the cancellation and creates no lock file": {
			op: storeOutTPublish, wait: storeOutTCanceled, wantErr: context.Canceled,
		},
		"error: PublishContext with a deadline passed before the call on an existing empty state root returns the deadline and creates no lock file": {
			op: storeOutTPublish, wait: storeOutTExpired, wantErr: context.DeadlineExceeded,
		},
		"error: Reconcile with a context canceled before the call on an existing empty state root returns the cancellation and creates no lock file": {
			op: storeOutTReconcile, wait: storeOutTCanceled, wantErr: context.Canceled,
		},
		"error: Reconcile with a deadline passed before the call on an existing empty state root returns the deadline and creates no lock file": {
			op: storeOutTReconcile, wait: storeOutTExpired, wantErr: context.DeadlineExceeded,
		},
		"error: RemoveIfContext with a context canceled before the call on an existing empty state root returns the cancellation and creates no lock file": {
			op: storeOutTRemove, wait: storeOutTCanceled, wantErr: context.Canceled,
		},
		"error: RemoveIfContext with a deadline passed before the call on an existing empty state root returns the deadline and creates no lock file": {
			op: storeOutTRemove, wait: storeOutTExpired, wantErr: context.DeadlineExceeded,
		},
		"error: ReadContext with a context canceled before the call on an existing empty state root returns the cancellation and creates no lock file": {
			op: storeOutTRead, wait: storeOutTCanceled, wantErr: context.Canceled,
		},
		"error: ReadContext with a deadline passed before the call on an existing empty state root returns the deadline and creates no lock file": {
			op: storeOutTRead, wait: storeOutTExpired, wantErr: context.DeadlineExceeded,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fx := storeOutTNewFixture(t)
			r := storeOutTRecord(fx.root)
			what := storeEndedTName(tt.op)
			var beforeParent map[string]storeOutTEntry
			var before storeOutTState
			if tt.missingRoot {
				if err := os.Remove(fx.root); err != nil {
					t.Fatal(err)
				}
				beforeParent = storeEndedTParent(fx)
			} else {
				before = storeOutTCapture(fx, false)
			}

			got, ok := storeEndedTCall(t, what, tt.op, tt.wait, fx.store(fx.root), r)
			if !ok {
				return
			}

			if got.ok {
				t.Errorf("%s = true, %v; want false", what, got.err)
			}
			if diff := gocmp.Diff(procinfo.Record{}, got.record); diff != "" {
				t.Errorf("%s record (-want +got):\n%s", what, diff)
			}
			storeOutTCheckErr(fx, what, got.err, []error{tt.wantErr}, nil, nil)
			storeOutTCheckSteps(fx, what, tt.wait, false, false)
			storeOutTCheckLock(fx, what, storeOutTNotAcquired)
			if !tt.missingRoot {
				storeOutTCheckAfter(t, storeOutTKept, before, storeOutTCapture(fx, false), storeOutTName, r)
				return
			}
			if _, err := os.Lstat(fx.root); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s: the missing state root %s exists after the call (lstat: %v); want it still missing", what, fx.root, err)
			}
			afterParent := storeEndedTParent(fx)
			if diff := gocmp.Diff(slices.Sorted(maps.Keys(beforeParent)), slices.Sorted(maps.Keys(afterParent))); diff != "" {
				t.Errorf("%s: the entries of the state root's parent changed (-before +after):\n%s", what, diff)
			}
			for entry, e := range beforeParent {
				if now, ok := afterParent[entry]; ok {
					storeOutTSame(t, entry, e, now)
				}
			}
		})
	}
}
