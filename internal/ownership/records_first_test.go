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

package ownership_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// recordLockFile is the retained lock file every Store operation opens, and creates when it is missing, in the state
// root before it touches a record. Only the first operation in a fresh state root creates it.
const recordLockFile = "process-records.lock"

// firstOp is one Store operation that races the others of its round for the first open of a fresh state root.
type firstOp int

const (
	// publishFirst publishes the first of the round's two records under workerName.
	publishFirst firstOp = iota
	// publishSecond publishes the second record under the same name.
	publishSecond
	// readAbsent reads unrelatedName, which nothing in the round publishes.
	readAbsent
	// removeAbsent removes unrelatedName conditionally; nothing in the round publishes it.
	removeAbsent
)

func (op firstOp) String() string {
	switch op {
	case publishFirst:
		return "Publish of the first record"
	case publishSecond:
		return "Publish of the second record"
	case readAbsent:
		return "Read of an absent record"
	case removeAbsent:
		return "RemoveIf of an absent record"
	}
	return "operation " + strconv.Itoa(int(op))
}

// namesLock reports whether err names the record lock file. The open of that file failing with "no such file or
// directory" matches fs.ErrNotExist exactly as an absent record does, so an absent result is only the documented one
// when the error does not come from the lock.
func namesLock(err error) bool {
	if err == nil {
		return false
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && filepath.Base(pathErr.Path) == recordLockFile {
		return true
	}
	return strings.Contains(err.Error(), recordLockFile)
}

// TestStoreFirstOperationsInFreshRoot makes the first creation of the record lock frequent: every round starts the
// round's operations at once on a fresh state root, in which only the first open finds no lock file. The operations
// are released together from a spinning start line. Several fresh roots are in flight at a time, so that the waits on
// the held lock of one root overlap those of the others. Every operation must succeed, or return the documented
// absent result, and never fail on the lock file; a publication must leave one of the published records with its own
// decimal mirror and the retained lock file behind.
//
// All operations of a round use the same fresh state root, each through a Store value of it. Store is a value with
// value receivers and every operation opens the state root itself, so whether the operations share one Store value
// or make their own is not something a row can vary: the rows vary the mix of operations that race.
func TestStoreFirstOperationsInFreshRoot(t *testing.T) {
	const (
		// rounds fresh state roots per row; inFlight of them race at a time. Contended operations of one root wait
		// for each other on the lock's retry timer, so a root costs a few retry periods however fast its first open
		// is; the roots of a batch overlap those waits.
		rounds   = 256
		inFlight = 16
	)
	tests := map[string]struct {
		ops []firstOp
	}{
		"success: two publications of one record, as the supervisor's publishers race": {
			ops: []firstOp{publishFirst, publishSecond},
		},
		"success: publications racing reads and removals of an absent record": {
			ops: []firstOp{publishFirst, publishSecond, readAbsent, readAbsent, removeAbsent, removeAbsent},
		},
		"success: reads and removals of an absent record alone create the lock": {
			ops: []firstOp{readAbsent, readAbsent, readAbsent, removeAbsent, removeAbsent, removeAbsent},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			published := false
			for _, op := range tt.ops {
				published = published || op == publishFirst || op == publishSecond
			}
			for batch := 0; batch < rounds; batch += inFlight {
				roots := make([]string, inFlight)
				for i := range roots {
					roots[i] = stateRoot(t)
				}
				// records[i] holds the round's two publications; their process numbers differ, so the mirror names
				// which one the record file holds.
				records := make([][2]procinfo.Record, inFlight)
				for i, root := range roots {
					first := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
					second := recordFor(root, workerSample(decoyPID), roleWorker, workerPort)
					second.Generation = "generation-b"
					records[i] = [2]procinfo.Record{first, second}
				}

				// Every operation announces itself and spins, yielding, until the whole batch has arrived and is let go, so
				// that the first opens of one root start within moments of each other instead of one after another as a
				// channel close would wake them.
				var arrived atomic.Int32
				var released atomic.Bool
				total := int32(0)
				var wg sync.WaitGroup
				errs := make([][]error, inFlight)
				for i, root := range roots {
					errs[i] = make([]error, len(tt.ops))
					s := ownership.Store{Root: root}
					for j, op := range tt.ops {
						total++
						wg.Go(func() {
							arrived.Add(1)
							for !released.Load() {
								runtime.Gosched()
							}
							errs[i][j] = guarded(op.String(), func() error {
								return runFirstOp(s, op, records[i])
							})
						})
					}
				}
				for arrived.Load() < total {
					runtime.Gosched()
				}
				released.Store(true)
				wg.Wait()

				failed := false
				for i := range roots {
					for j, err := range errs[i] {
						if err != nil {
							failed = true
							t.Errorf("round %d, %s: %v", batch+i, tt.ops[j], err)
						}
					}
				}
				if failed {
					t.FailNow()
				}
				for i, root := range roots {
					checkFirstRound(t, batch+i, root, published, records[i])
				}
			}
		})
	}
}

// runFirstOp runs op on s and returns an error unless its outcome is the one the store documents for it: a
// publication succeeds, a read of an absent record matches fs.ErrNotExist without naming the lock, and a removal of an
// absent record removes nothing and succeeds.
func runFirstOp(s ownership.Store, op firstOp, records [2]procinfo.Record) error {
	switch op {
	case publishFirst:
		return s.Publish(workerName, records[0])
	case publishSecond:
		return s.Publish(workerName, records[1])
	case readAbsent:
		r, err := s.Read(unrelatedName)
		switch {
		case namesLock(err):
			return fmt.Errorf("failed on the record lock instead of reporting the absent record: %w", err)
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("error %w, want one matching fs.ErrNotExist", err)
		case !gocmp.Equal(procinfo.Record{}, r):
			return fmt.Errorf("record %+v returned with the absent result, want the zero record", r)
		}
		return nil
	case removeAbsent:
		removed, err := s.RemoveIf(unrelatedName, records[0])
		switch {
		case err != nil:
			return fmt.Errorf("error %w, want none for an absent record", err)
		case removed:
			return errors.New("reported a removal of an absent record")
		}
		return nil
	}
	return fmt.Errorf("unknown %v", op)
}

// checkFirstRound fails the test unless root holds what the round's operations must leave: the retained lock file as
// a regular file, no record under the absent name, and, when the round published, one of its two records with the
// decimal mirror of that same record.
func checkFirstRound(t *testing.T, round int, root string, published bool, records [2]procinfo.Record) {
	t.Helper()
	info, err := os.Lstat(filepath.Join(root, recordLockFile))
	switch {
	case err != nil:
		t.Fatalf("round %d: the record lock is not retained: %v", round, err)
	case !info.Mode().IsRegular():
		t.Fatalf("round %d: the record lock is %v, want a regular file", round, info.Mode())
	}
	if absent := readRaw(t, root, unrelatedName); absent.HasJSON || absent.HasMirror {
		t.Fatalf("round %d: the absent record exists after the round: %+v", round, absent)
	}
	raw := readRaw(t, root, workerName)
	if !published {
		if raw.HasJSON || raw.HasMirror {
			t.Fatalf("round %d: a record exists although the round published none: %+v", round, raw)
		}
		return
	}
	got := raw.decoded(t)
	if gocmp.Diff(records[0], got) != "" && gocmp.Diff(records[1], got) != "" {
		t.Fatalf("round %d: record %+v is neither publication", round, got)
	}
	if !raw.HasMirror || raw.Mirror != strconv.Itoa(got.PID) {
		t.Fatalf("round %d: decimal mirror %q (present %v) names another publication than the record (pid %d)", round, raw.Mirror, raw.HasMirror, got.PID)
	}
}
