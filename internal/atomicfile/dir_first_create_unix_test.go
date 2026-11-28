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

package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// firstCreateResult is what one concurrent opener observed: the file it was given, identified by device and inode,
// and when its creating open began and returned.
type firstCreateResult struct {
	dev, ino     uint64
	mode         uint32
	err          error
	began, ended time.Time
}

// overlapped reports whether the Go-side intervals of at least two calls of Dir.OpenFile intersect, each measured
// from just before the call to just after it returned. It is a diagnostic of how the goroutines were scheduled: it
// does not show that the open system calls themselves overlapped, nor that they did so before the first of them
// created the entry.
func overlapped(results []firstCreateResult) bool {
	spans := slices.Clone(results)
	slices.SortFunc(spans, func(a, b firstCreateResult) int { return a.began.Compare(b.began) })
	var latestEnd time.Time
	for i, r := range spans {
		if i > 0 && r.began.Before(latestEnd) {
			return true
		}
		if r.ended.After(latestEnd) {
			latestEnd = r.ended
		}
	}
	return false
}

// TestDirOpenFileFirstCreate makes the first creation of a name frequent: every round starts its openers at once on a
// fresh directory, each asking Dir.OpenFile to create the same new name without exclusivity, as the ownership store
// opens its retained lock file. The openers of a round are released together from a spinning start line, not woken one
// by one from a channel. The test logs in how many rounds the Go-side intervals of two calls on the same directory
// intersected: a diagnostic of scheduling that makes a run without a failure easier to judge, not proof that the
// system calls overlapped, and the test never fails on its value, zero included. Every call must return
// a file, and every file of a round must be the one regular file the directory then holds under that name. Every open
// in this fixture is expected to succeed, because its directory stays linked and writable for the whole round: on
// macOS the open system call can still answer "does not exist" while another opener creates the same name, and
// Dir.OpenFile repeats that answer only a bounded number of times, so a repeat that is used up returns the error and
// fails the row.
//
// Each opener opens its own Dir of the directory after the round starts, as each ownership store operation does: a
// Dir must not be used concurrently, so openers that shared one handle would test a use the type does not promise.
func TestDirOpenFileFirstCreate(t *testing.T) {
	const (
		// rounds fresh directories per row, inFlight of them at a time, openers concurrent calls per directory. A
		// directory in flight beside another only takes processors from it, so few are in flight at once.
		rounds   = 1024
		inFlight = 2
		openers  = 8
		name     = "first.lock"
	)
	tests := map[string]struct {
		flag int
	}{
		"success: the record lock's flags through separately opened directories": {
			flag: os.O_RDWR | os.O_CREATE | unix.O_NONBLOCK | unix.O_NOCTTY,
		},
		"success: a plain read-write create through separately opened directories": {
			flag: os.O_RDWR | os.O_CREATE,
		},
	}
	for testName, tt := range tests {
		t.Run(testName, func(t *testing.T) {
			base := t.TempDir()
			raced := 0
			for batch := 0; batch < rounds; batch += inFlight {
				paths := make([]string, inFlight)
				for i := range paths {
					path, err := os.MkdirTemp(base, "round-")
					if err != nil {
						t.Fatalf("test infrastructure: fresh directory for round %d: %v", batch+i, err)
					}
					paths[i] = path
				}

				// Every opener announces itself and spins, yielding, until the whole round has arrived and is let go.
				var arrived atomic.Int32
				var released atomic.Bool
				var wg sync.WaitGroup
				results := make([][openers]firstCreateResult, inFlight)
				for i, path := range paths {
					for j := range openers {
						wg.Go(func() {
							arrived.Add(1)
							for !released.Load() {
								runtime.Gosched()
							}
							results[i][j] = openFirst(path, name, tt.flag)
						})
					}
				}
				for arrived.Load() < inFlight*openers {
					runtime.Gosched()
				}
				released.Store(true)
				wg.Wait()

				failed := false
				for i, path := range paths {
					if overlapped(results[i][:]) {
						raced++
					}
					var st unix.Stat_t
					if err := unix.Lstat(filepath.Join(path, name), &st); err != nil {
						t.Errorf("round %d: the created entry: %v", batch+i, err)
						failed = true
						continue
					}
					for j, r := range results[i] {
						switch {
						case r.err != nil:
							t.Errorf("round %d, opener %d: %v", batch+i, j, r.err)
						case r.mode&unix.S_IFMT != unix.S_IFREG:
							t.Errorf("round %d, opener %d: got a file of mode %#o, want a regular file", batch+i, j, r.mode)
						case r.dev != uint64(st.Dev) || r.ino != st.Ino:
							t.Errorf("round %d, opener %d: got file %d:%d, want the entry %d:%d the directory holds", batch+i, j, r.dev, r.ino, st.Dev, st.Ino)
						default:
							continue
						}
						failed = true
					}
				}
				if failed {
					t.FailNow()
				}
			}
			t.Logf("diagnostic only: in %d of %d rounds the Go-side intervals of at least two creating calls on the same directory intersected", raced, rounds)
		})
	}
}

// openFirst opens name with flag through its own Dir of path and returns the identity of the file it was given. Every
// descriptor it opened is closed before it returns.
func openFirst(path, name string, flag int) (r firstCreateResult) {
	d, err := OpenDir(path)
	if err != nil {
		return firstCreateResult{err: fmt.Errorf("open its own directory: %w", err)}
	}
	defer func() { r.err = errors.Join(r.err, d.Close()) }()
	began := time.Now()
	f, err := d.OpenFile(name, flag, 0o600)
	ended := time.Now()
	if err != nil {
		return firstCreateResult{err: fmt.Errorf("creating open of %s: %w", name, err), began: began, ended: ended}
	}
	if f == nil {
		return firstCreateResult{err: fmt.Errorf("creating open of %s returned neither a file nor an error", name), began: began, ended: ended}
	}
	defer func() { r.err = errors.Join(r.err, f.Close()) }()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return firstCreateResult{err: fmt.Errorf("fstat the opened %s: %w", name, err), began: began, ended: ended}
	}
	return firstCreateResult{dev: uint64(st.Dev), ino: st.Ino, mode: uint32(st.Mode), began: began, ended: ended}
}
