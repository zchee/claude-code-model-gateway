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
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

func publish(t testing.TB, s ownership.Store, name string, r procinfo.Record) error {
	t.Helper()
	return noPanic(t, "Store.Publish", func() error { return s.Publish(name, r) })
}

func read(t testing.TB, s ownership.Store, name string) (procinfo.Record, error) {
	t.Helper()
	type result struct {
		r   procinfo.Record
		err error
	}
	got := noPanic(t, "Store.Read", func() result {
		r, err := s.Read(name)
		return result{r, err}
	})
	return got.r, got.err
}

func removeIf(t testing.TB, s ownership.Store, name string, expected procinfo.Record) (bool, error) {
	t.Helper()
	type result struct {
		removed bool
		err     error
	}
	got := noPanic(t, "Store.RemoveIf", func() result {
		removed, err := s.RemoveIf(name, expected)
		return result{removed, err}
	})
	return got.removed, got.err
}

// checkStored fails unless the record files on disk, read without repair, hold exactly r and its decimal mirror.
func checkStored(t testing.TB, root, name string, r procinfo.Record) {
	t.Helper()
	s := readRaw(t, root, name)
	if !s.HasJSON || !s.HasMirror {
		t.Fatalf("record files present: record %v, mirror %v; want both", s.HasJSON, s.HasMirror)
	}
	if diff := gocmp.Diff(r, s.decoded(t)); diff != "" {
		t.Errorf("stored record (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(strconv.Itoa(r.PID), s.Mirror); diff != "" {
		t.Errorf("stored decimal mirror (-want +got):\n%s", diff)
	}
}

func checkAbsent(t testing.TB, root, name string) {
	t.Helper()
	if s := readRaw(t, root, name); s.HasJSON || s.HasMirror {
		t.Errorf("record files present: %+v; want neither", s)
	}
}

func TestStorePublish(t *testing.T) {
	tests := map[string]struct {
		// prior is stored with the existing writer before the call, when set.
		prior   bool
		publish func(root string) procinfo.Record
		wantErr bool
	}{
		"success: a first record with a start token is published with its mirror": {
			publish: func(root string) procinfo.Record {
				return recordFor(root, workerSample(workerPID), roleWorker, workerPort)
			},
		},
		"success: a newer record replaces the previous one and its mirror": {
			prior: true,
			publish: func(root string) procinfo.Record {
				r := recordFor(root, workerSample(decoyPID), roleWorker, workerPort)
				r.Generation = "generation-2"
				return r
			},
		},
		"error: a first record without a start token leaves neither file": {
			publish: func(root string) procinfo.Record {
				r := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
				r.StartToken = ""
				return r
			},
			wantErr: true,
		},
		"error: a record without a start token keeps the previous one exactly": {
			prior: true,
			publish: func(root string) procinfo.Record {
				r := recordFor(root, workerSample(decoyPID), roleWorker, workerPort)
				r.StartToken = ""
				return r
			},
			wantErr: true,
		},
		"error: a record of another state root is refused": {
			publish: func(string) procinfo.Record {
				return recordFor("/elsewhere/state", workerSample(workerPID), roleWorker, workerPort)
			},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := stateRoot(t)
			if tt.prior {
				seed(t, root, workerName, recordFor(root, workerSample(workerPID), roleWorker, workerPort))
			}
			before := readRaw(t, root, workerName)
			r := tt.publish(root)
			err := publish(t, ownership.Store{Root: root}, workerName, r)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Publish = %v; want error %v", err, tt.wantErr)
			}
			switch {
			case !tt.wantErr:
				checkStored(t, root, workerName, r)
			case tt.prior:
				if diff := gocmp.Diff(before, readRaw(t, root, workerName)); diff != "" {
					t.Errorf("a refused publication changed the files (-before +after):\n%s", diff)
				}
			default:
				checkAbsent(t, root, workerName)
			}
		})
	}
}

func TestStoreRead(t *testing.T) {
	tests := map[string]struct {
		// mirror, when set, replaces the mirror's content before the read; removeMirror removes it.
		mirror       *string
		removeMirror bool
		token        bool
		missing      bool
		onlyPID      bool
		wantErr      error
	}{
		"success: a record and its mirror":                                  {token: true},
		"success: a historical record without a start token stays readable": {},
		"success: a stale mirror is repaired":                               {token: true, mirror: new("999")},
		"success: an empty mirror is repaired":                              {token: true, mirror: new("")},
		"success: a missing mirror is repaired":                             {token: true, removeMirror: true},
		"error: a missing record does not exist":                            {missing: true, wantErr: fs.ErrNotExist},
		"error: a decimal mirror alone is not a record":                     {onlyPID: true, wantErr: fs.ErrNotExist},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := stateRoot(t)
			want := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
			if !tt.token {
				want.StartToken = ""
			}
			mirrorPath := filepath.Join(root, workerName+".pid")
			switch {
			case tt.missing:
			case tt.onlyPID:
				if err := os.WriteFile(mirrorPath, []byte(strconv.Itoa(workerPID)), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				seed(t, root, workerName, want)
			}
			switch {
			case tt.removeMirror:
				if err := os.Remove(mirrorPath); err != nil {
					t.Fatal(err)
				}
			case tt.mirror != nil:
				if err := os.WriteFile(mirrorPath, []byte(*tt.mirror), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := read(t, ownership.Store{Root: root}, workerName)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Read = %+v, %v; want error %v", got, err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if diff := gocmp.Diff(procinfo.Record{}, got); diff != "" {
					t.Errorf("a failed read returned a record (-want +got):\n%s", diff)
				}
				return
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("record (-want +got):\n%s", diff)
			}
			checkStored(t, root, workerName, want)
		})
	}
}

func TestStoreRemoveIf(t *testing.T) {
	tests := map[string]struct {
		expected    func(procinfo.Record) procinfo.Record
		noRecord    bool
		wantRemoved bool
	}{
		"success: the expected record is removed with its mirror": {
			expected:    func(r procinfo.Record) procinfo.Record { return r },
			wantRemoved: true,
		},
		"success: fields other than number, generation and token do not decide": {
			expected: func(r procinfo.Record) procinfo.Record {
				r.Command, r.Version, r.Port = "another command", "9.9.9", 1
				return r
			},
			wantRemoved: true,
		},
		"error: another process number keeps the record": {
			expected: func(r procinfo.Record) procinfo.Record { r.PID = decoyPID; return r },
		},
		"error: another generation keeps the record": {
			expected: func(r procinfo.Record) procinfo.Record { r.Generation = "generation-2"; return r },
		},
		"error: another start token keeps the record": {
			expected: func(r procinfo.Record) procinfo.Record {
				r.StartToken = "2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/9"
				return r
			},
		},
		"success: a missing record is nothing to remove": {
			expected: func(r procinfo.Record) procinfo.Record { return r },
			noRecord: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := stateRoot(t)
			current := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
			if !tt.noRecord {
				seed(t, root, workerName, current)
			}
			removed, err := removeIf(t, ownership.Store{Root: root}, workerName, tt.expected(current))
			if err != nil {
				t.Fatalf("RemoveIf: %v", err)
			}
			if removed != tt.wantRemoved {
				t.Errorf("RemoveIf removed = %v; want %v", removed, tt.wantRemoved)
			}
			if tt.wantRemoved || tt.noRecord {
				checkAbsent(t, root, workerName)
			} else {
				checkStored(t, root, workerName, current)
			}
		})
	}
}

func TestStoreInvalidRoot(t *testing.T) {
	tests := map[string]string{
		"error: a relative state root": "relative/state",
		"error: an unclean state root": "/tmp/../tmp/state",
	}
	for name, root := range tests {
		t.Run(name, func(t *testing.T) {
			s := ownership.Store{Root: root}
			r := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
			if err := publish(t, s, workerName, r); err == nil {
				t.Error("Publish succeeded")
			}
			if _, err := read(t, s, workerName); err == nil {
				t.Error("Read succeeded")
			}
			if removed, err := removeIf(t, s, workerName, r); err == nil || removed {
				t.Errorf("RemoveIf = %v, %v; want an error and nothing removed", removed, err)
			}
		})
	}
}

// raceGenerations returns the old record the remover asks for and the new one the publisher writes.
func raceGenerations(root string) (old, fresh procinfo.Record) {
	old = recordFor(root, workerSample(workerPID), roleWorker, workerPort)
	old.Generation = "old"
	fresh = recordFor(root, workerSample(decoyPID), roleWorker, workerPort)
	fresh.Generation = "new"
	return old, fresh
}

// holdRemover starts RemoveIf(old) on a copy of s whose hook holds it after its match and before its unlink. It
// returns once the remover is held, a function that lets it go, and the remover's result.
func holdRemover(t *testing.T, s ownership.Store, old procinfo.Record) (release func(), result <-chan error) {
	t.Helper()
	held, let := make(chan struct{}), make(chan struct{})
	hooked := ownership.WithBeforeRemove(s, func() {
		close(held)
		<-let
	})
	done := make(chan error, 1)
	go func() {
		done <- guarded("Store.RemoveIf", func() error {
			removed, err := hooked.RemoveIf(workerName, old)
			if err == nil && !removed {
				err = errors.New("RemoveIf matched the old record at the barrier and then reported nothing removed")
			}
			return err
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("RemoveIf returned without reaching the barrier between match and unlink: %v", err)
	}
	release = sync.OnceFunc(func() { close(let) })
	t.Cleanup(release)
	return release, done
}

// TestStoreRemovalRacingPublication starts a publication and a read while a removal is held between its match and
// its unlink, then lets the removal go. Whatever order the store's exclusion imposes, the outcome must be whole: the
// removal took the old record, the new record and its mirror survive, and the read saw one whole record or none.
// That the publisher actually contends for the same exclusion, also from another process, is not observable here
// and is left to the implementation review.
func TestStoreRemovalRacingPublication(t *testing.T) {
	root := stateRoot(t)
	s := ownership.Store{Root: root}
	old, fresh := raceGenerations(root)
	seed(t, root, workerName, old)
	release, removed := holdRemover(t, s, old)
	var wg sync.WaitGroup
	var publishErr, readErr error
	var seen procinfo.Record
	started := make(chan struct{}, 2)
	wg.Go(func() {
		started <- struct{}{}
		publishErr = guarded("Store.Publish", func() error { return s.Publish(workerName, fresh) })
	})
	wg.Go(func() {
		started <- struct{}{}
		readErr = guarded("Store.Read", func() (err error) { seen, err = s.Read(workerName); return err })
	})
	<-started
	<-started
	release()
	wg.Wait()
	if err := <-removed; err != nil {
		t.Fatalf("RemoveIf: %v", err)
	}
	if publishErr != nil {
		t.Fatalf("Publish: %v", publishErr)
	}
	switch {
	case errors.Is(readErr, fs.ErrNotExist):
	case readErr != nil:
		t.Errorf("Read during the race: %v", readErr)
	case gocmp.Diff(old, seen) != "" && gocmp.Diff(fresh, seen) != "":
		t.Errorf("Read during the race returned %+v, neither whole record", seen)
	}
	checkStored(t, root, workerName, fresh)
}

// TestStoreConcurrentPublishers decides that concurrent publications never leave a mixed record: the record and
// its decimal mirror, read without repair, always name the same publication.
func TestStoreConcurrentPublishers(t *testing.T) {
	root := stateRoot(t)
	s := ownership.Store{Root: root}
	a, b := recordFor(root, workerSample(workerPID), roleWorker, workerPort), recordFor(root, workerSample(decoyPID), roleWorker, workerPort)
	b.Generation = "generation-b"
	for i := range 100 {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j, r := range []procinfo.Record{a, b} {
			wg.Go(func() { errs[j] = guarded("Store.Publish", func() error { return s.Publish(workerName, r) }) })
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		raw := readRaw(t, root, workerName)
		got := raw.decoded(t)
		if gocmp.Diff(a, got) != "" && gocmp.Diff(b, got) != "" {
			t.Fatalf("round %d: record %+v is neither publication", i, got)
		}
		if !raw.HasMirror || raw.Mirror != strconv.Itoa(got.PID) {
			t.Fatalf("round %d: decimal mirror %q (present %v) names another publication than the record (pid %d)", i, raw.Mirror, raw.HasMirror, got.PID)
		}
	}
}
