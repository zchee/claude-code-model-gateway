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

package ownership

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// recordLockName is the retained lock file in the state root that every Store operation of every process holds while
// it touches a record. It is never removed: its presence says nothing about whether it is held.
const recordLockName = "process-records.lock"

// errNoStartToken refuses a publication that could never be matched against a live process.
var errNoStartToken = errors.New("a process record without a start token cannot be published")

// errRecordsBusy means another operation held the record lock when an operation that must not wait tried to take it.
var errRecordsBusy = errors.New("the ownership records were busy")

// errLockWait means the wait for the record lock ended with its context before the lock was free; nothing was read or
// changed.
var errLockWait = errors.New("the wait for the record lock ended before the lock was free")

// lockWaitEnded is the error of a lock wait that ctx ended: it matches errLockWait and ctx's error.
func lockWaitEnded(ctx context.Context) error { return fmt.Errorf("%w: %w", errLockWait, ctx.Err()) }

// Store publishes, reads and conditionally removes the process records of one state root. Its operations exclude
// one another across processes, not only within one.
//
// Every operation opens the state root once and does everything through that one directory: it takes the lock, reads
// and writes the record and its decimal mirror, and unlinks them. A state root renamed or replaced while an operation
// runs therefore cannot make it compare one directory's record and remove another's. The exclusion is an advisory
// lock on a retained regular file in the state root, taken on a fresh open file description for every operation, so
// that two operations exclude each other within one process as well as between the supervisor that publishes and a
// command that reads or removes. It is held only for the file operations themselves, never while waiting for a
// process or a network answer, and the kernel releases it when its holder dies. An operation waits for another
// holder on a timer, never in a blocking system call.
type Store struct {
	// Root is the clean absolute state root.
	Root string
	// beforeRemove, when the package's own tests set it on their store, runs inside RemoveIf after the stored record
	// has matched the expected one and before anything is unlinked, while the store's exclusion is held.
	beforeRemove func()
	// fault, when the package's own tests set it on their store, is called at every step of a Store operation that
	// storeStep names, with that step and the name in the state root of the entry it concerns: the authoritative record
	// (such as "worker.pid.json"), its decimal mirror (such as "worker.pid") or, at the release, the record lock. It
	// is nil otherwise, and then nothing differs. At a lookup or a write it is called before the step; an error it
	// returns is that step's error, and the step does not happen. At a release it is called after the release; an
	// error it returns is joined to the operation's error as an error of that release.
	fault func(step storeStep, name string) error
}

// storeStep is a step of a Store operation that the package's own tests observe or fail through Store.fault.
type storeStep int

const (
	// storeLookup is a lookup of a record entry: finding out whether the entry exists, whether it is a regular file and
	// what it holds. Every lookup of the authoritative record or of its mirror is one, also one that finds the entry
	// missing; how many lookups an operation makes is not fixed.
	storeLookup storeStep = iota + 1
	// storeWrite is the replacement of a record entry, the authoritative record or its mirror.
	storeWrite
	// storeRelease is the release of the store's exclusion, after the lock was given up.
	storeRelease
)

// Publish replaces the record name with r. A record without a start token is refused and nothing is written.
func (s Store) Publish(name string, r procinfo.Record) error {
	if err := s.validate(name); err != nil {
		return err
	}
	if r.StartToken == "" {
		return errNoStartToken
	}
	if err := r.Validate(s.Root); err != nil {
		return err
	}
	// Encode the typed record directly, as procinfo.WriteRecord does: a JavaScript number cannot hold every inode.
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.locked(context.Background(), true, func(dir *atomicfile.Dir) error {
		// The authoritative record first, then its mirror, each refusing to replace anything but a regular file.
		if err := replaceEntry(dir, name+".pid.json", data); err != nil {
			return err
		}
		if err := replaceEntry(dir, name+".pid", []byte(strconv.Itoa(r.PID))); err != nil {
			return fmt.Errorf("%w: %w", procinfo.ErrMirrorPublish, err)
		}
		return nil
	})
}

// PublishContext is Publish with a lock wait that ends with ctx; published reports whether the authoritative record was replaced.
func (s Store) PublishContext(ctx context.Context, name string, r procinfo.Record) (published bool, err error) {
	panic("not implemented")
}

// Read returns the record name and repairs a stale or missing decimal mirror; a missing record is an error matching
// fs.ErrNotExist. The mirror is repaired under the store's exclusion, so a repair never overwrites the mirror of a
// publication that came after the record it read. A record that is valid but whose mirror could not be repaired is
// returned with an error matching procinfo.ErrMirrorRepair.
func (s Store) Read(name string) (procinfo.Record, error) {
	return s.read(context.Background(), name)
}

// ReadContext is Read with a lock wait that ends with ctx.
func (s Store) ReadContext(ctx context.Context, name string) (procinfo.Record, error) {
	panic("not implemented")
}

// read is Read with a lock wait that ends with ctx.
func (s Store) read(ctx context.Context, name string) (procinfo.Record, error) {
	if err := s.validate(name); err != nil {
		return procinfo.Record{}, err
	}
	var r procinfo.Record
	err := s.locked(ctx, true, func(dir *atomicfile.Dir) error {
		var err error
		if r, err = s.readEntry(dir, name); err != nil {
			r = procinfo.Record{}
			return err
		}
		want := []byte(strconv.Itoa(r.PID))
		old, err := readEntry(dir, name+".pid")
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %w", procinfo.ErrMirrorRepair, err)
		}
		if string(old) != string(want) {
			if err := dir.WriteFile(name+".pid", want, 0o600); err != nil {
				return fmt.Errorf("%w: %w", procinfo.ErrMirrorRepair, err)
			}
		}
		return nil
	})
	return r, err
}

// RemoveIf removes the record name only while it names the expected process number, generation and start token,
// and reports whether it removed it. The comparison and the unlink are one operation under the store's exclusion and
// in the same directory, so a record published after the comparison is never the one removed.
func (s Store) RemoveIf(name string, expected procinfo.Record) (bool, error) {
	return s.removeIf(context.Background(), name, expected)
}

// RemoveIfContext is RemoveIf with a lock wait that ends with ctx.
func (s Store) RemoveIfContext(ctx context.Context, name string, expected procinfo.Record) (removed bool, err error) {
	panic("not implemented")
}

// ErrRepair reports that a missing, differing, or readable but invalid record was established and its replacement could not be written.
var ErrRepair = errors.New("model-gateway: a required repair of a process record could not be written")

// Reconcile replaces the record name with r unless it already equals r, and reports whether it replaced the authoritative record.
func (s Store) Reconcile(ctx context.Context, name string, r procinfo.Record) (wrote bool, err error) {
	panic("not implemented")
}

// removeIf is RemoveIf with a lock wait that ends with ctx.
func (s Store) removeIf(ctx context.Context, name string, expected procinfo.Record) (bool, error) {
	if err := s.validate(name); err != nil {
		return false, err
	}
	removed := false
	err := s.locked(ctx, true, func(dir *atomicfile.Dir) error {
		current, err := s.readEntry(dir, name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case !sameRecord(current, expected):
			return nil
		}
		if s.beforeRemove != nil {
			s.beforeRemove()
		}
		// The authoritative record goes first: a mirror left behind by a failure below names nothing on its own.
		if err := dir.Remove(name + ".pid.json"); err != nil {
			return err
		}
		removed = true
		if err := dir.Remove(name + ".pid"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("record %s removed but its process ID mirror was not: %w", name, err)
		}
		return nil
	})
	return removed, err
}

// load reads the record name under the store's exclusion, waiting for it no longer than ctx. A mirror that could not
// be repaired does not make the authoritative record unusable.
func (s Store) load(ctx context.Context, name string) (procinfo.Record, error) {
	r, err := s.read(ctx, name)
	if errors.Is(err, procinfo.ErrMirrorRepair) {
		err = nil
	}
	return r, err
}

// stillStored reports whether the record name in dir is still exactly r, reading only the authoritative file and
// never repairing the mirror. The caller holds the exclusion through dir. r was validated when it was first read, so
// bytes that decode to an equal record are valid too; anything else, a missing record included, is not r.
func stillStored(dir *atomicfile.Dir, name string, r procinfo.Record) bool {
	data, err := readEntry(dir, name+".pid.json")
	if err != nil {
		return false
	}
	var current procinfo.Record
	return json.Unmarshal(data, &current) == nil && current == r
}

// readEntry decodes and validates the authoritative record name in dir.
func (s Store) readEntry(dir *atomicfile.Dir, name string) (procinfo.Record, error) {
	data, err := readEntry(dir, name+".pid.json")
	if err != nil {
		return procinfo.Record{}, err
	}
	var r procinfo.Record
	if err := json.Unmarshal(data, &r); err != nil {
		return procinfo.Record{}, err
	}
	if err := r.Validate(s.Root); err != nil {
		return procinfo.Record{}, err
	}
	return r, nil
}

// readEntry reads one record file, refusing a symbolic link or special file the way procinfo's record reader does.
func readEntry(dir *atomicfile.Dir, name string) ([]byte, error) {
	data, err := dir.ReadFile(name)
	if _, ok := errors.AsType[*atomicfile.NonRegularError](err); ok {
		return nil, fmt.Errorf("%w: %w", procinfo.ErrRecordEntry, err)
	}
	return data, err
}

// replaceEntry replaces one record file atomically. A symbolic link or special file in its place is refused rather
// than replaced, as procinfo's record writer refuses it.
func replaceEntry(dir *atomicfile.Dir, name string, data []byte) error {
	if _, err := readEntry(dir, name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return dir.WriteFile(name, data, 0o600)
}

// sameRecord reports whether two records name the same publication: the same process number, generation and start
// token. No other field decides.
func sameRecord(a, b procinfo.Record) bool {
	return a.PID == b.PID && a.Generation == b.Generation && a.StartToken == b.StartToken
}

// validate refuses a root or record name that the store must not resolve, before any file is opened.
func (s Store) validate(name string) error {
	if !filepath.IsAbs(s.Root) || filepath.Clean(s.Root) != s.Root {
		return procinfo.ErrStateRoot
	}
	if name == "" || name == "." || !filepath.IsLocal(name) || filepath.Base(name) != name {
		return fs.ErrInvalid
	}
	return nil
}

// locked opens the state root once and runs op on that directory while holding the store's exclusion, taken through
// the same directory. With wait, it waits for another holder until ctx ends; without, it fails at once with
// errRecordsBusy. The release is deferred as soon as the lock is held, so an op that panics or exits its goroutine
// still gives the lock up. op must not call another Store method: the exclusion is not reentrant, and a second
// acquisition in the same process waits for the first.
func (s Store) locked(ctx context.Context, wait bool, op func(dir *atomicfile.Dir) error) (err error) {
	dir, err := atomicfile.OpenDir(s.Root)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	release, err := lockRecords(ctx, dir, s.Root, wait)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	return op(dir)
}
