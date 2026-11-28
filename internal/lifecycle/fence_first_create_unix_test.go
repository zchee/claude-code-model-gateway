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

package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

// acquireOnce makes its own fence for root, acquires it and releases it, and returns the first failure. A panic is
// returned as an error, because it runs on a goroutine where the test must not stop.
func acquireOnce(ctx context.Context, root string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panicked: %v", p)
		}
	}()
	f, err := lifecycle.NewStartFence(root)
	if err != nil {
		return fmt.Errorf("NewStartFence: %w", err)
	}
	release, err := f.Acquire(ctx)
	switch {
	case err != nil && release != nil:
		_ = release()
		return fmt.Errorf("Acquire returned a release closure with error %w", err)
	case err != nil:
		return fmt.Errorf("Acquire: %w", err)
	case release == nil:
		return errors.New("Acquire returned neither a release closure nor an error")
	}
	if err := release(); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// TestStartFenceFirstCreation makes the first creation of the fence file frequent: every round starts its
// acquisitions at once on a fresh state root, each through its own fence value made with NewStartFence, so that the
// first opens of the round race to create the retained file. The acquisitions are released together from a spinning
// start line, and several fresh roots are in flight at a time, so that the waits on the held fence of one root overlap
// those of the others. Every Acquire must succeed and every release with it, and afterwards the fence file must be one
// regular file with mode 0600.
//
// Whether two first opens of one root were in progress at the same moment is not observable here: the open happens
// inside Acquire, whose duration also covers its wait for the other holders, so the overlap of whole Acquire calls
// says nothing about the overlap of their opens.
//
// The creation mask is emptied for the whole test, as the sequential fence test does for its first acquisition, so
// that the new file shows exactly the mode Acquire asks for and an inherited mask can neither change that mode nor
// keep a later holder from opening the file for writing. The mask belongs to the test process, so this test, like
// every test of this package, must not run in parallel with another. A batch keeps up to 96 acquisitions in flight at
// once (16 roots of 6 holders), which can need about 200 descriptors at the same time: on a host whose descriptor
// limit is lower, the failures name the descriptor limit, not the fence.
func TestStartFenceFirstCreation(t *testing.T) {
	const (
		// rounds fresh state roots per row, inFlight of them at a time, holders concurrent acquisitions per root.
		rounds   = 256
		inFlight = 16
		holders  = 6
	)
	tests := map[string]struct {
		// missingRoot leaves the state root to the acquisitions, which create it as the lifecycle lock does, so that
		// the root's own creation races too.
		missingRoot bool
	}{
		"success: first acquisitions in an existing empty state root": {},
		"success: first acquisitions in a state root that does not exist yet": {
			missingRoot: true,
		},
	}
	previous := unix.Umask(0)
	t.Cleanup(func() { unix.Umask(previous) })
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			for batch := 0; batch < rounds; batch += inFlight {
				roots := make([]string, inFlight)
				for i := range roots {
					root, err := os.MkdirTemp(base, "round-")
					if err != nil {
						t.Fatalf("test infrastructure: fresh directory for round %d: %v", batch+i, err)
					}
					if tt.missingRoot {
						root = filepath.Join(root, "state")
					}
					roots[i] = root
				}

				// Every acquisition announces itself and spins, yielding, until the whole batch has arrived and is
				// let go, so that the first opens of one root start within moments of each other.
				var arrived atomic.Int32
				var released atomic.Bool
				// A watchdog for this batch alone, ended once the batch is joined, aborts a hang in it; no correct batch
				// comes near it, and a slow earlier batch leaves later ones their whole budget.
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				var wg sync.WaitGroup
				errs := make([][holders]error, inFlight)
				for i, root := range roots {
					for j := range holders {
						wg.Go(func() {
							arrived.Add(1)
							for !released.Load() {
								runtime.Gosched()
							}
							errs[i][j] = acquireOnce(ctx, root)
						})
					}
				}
				for arrived.Load() < inFlight*holders {
					runtime.Gosched()
				}
				released.Store(true)
				wg.Wait()
				cancel()

				failed := false
				for i, root := range roots {
					for j, err := range errs[i] {
						if err != nil {
							failed = true
							t.Errorf("round %d, holder %d: %v", batch+i, j, err)
						}
					}
					var st unix.Stat_t
					path := filepath.Join(root, fenceName)
					switch err := unix.Lstat(path, &st); {
					case err != nil:
						failed = true
						t.Errorf("round %d: the fence file after the round: %v", batch+i, err)
					case uint32(st.Mode) != unix.S_IFREG|0o600:
						failed = true
						t.Errorf("round %d: the fence file has mode %#o, want a regular file with mode 0600 (%#o)", batch+i, st.Mode, unix.S_IFREG|0o600)
					case st.Nlink != 1:
						failed = true
						t.Errorf("round %d: the fence file has %d links, want one file", batch+i, st.Nlink)
					}
				}
				if failed {
					t.FailNow()
				}
			}
		})
	}
}
