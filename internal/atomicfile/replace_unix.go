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
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type stagedFile struct {
	root         *directory
	name, target string
}

func (s stagedFile) Name() string                  { return s.root.path(s.name) }
func (s stagedFile) CloseAtomicallyReplace() error { return s.root.Rename(s.name, s.target) }
func (s stagedFile) remove() error                 { return s.root.removeIfPresent(s.name) }

func createSibling(root *directory, base string, mode fs.FileMode) (*os.File, string, error) {
	for {
		var random [6]byte
		_, _ = rand.Read(random[:])
		name := base + "." + strconv.Itoa(os.Getpid()) + "." + hex.EncodeToString(random[:]) + ".tmp"
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if !errors.Is(err, fs.ErrExist) {
			return f, name, err
		}
	}
}

func generatedName(name, base string) bool {
	rest, ok := strings.CutPrefix(name, base+".")
	if !ok {
		return false
	}
	rest, ok = strings.CutSuffix(rest, ".tmp")
	if !ok {
		return false
	}
	pid, nonce, ok := strings.Cut(rest, ".")
	if !ok || len(pid) == 0 || pid[0] == '0' || len(nonce) != 12 {
		return false
	}
	for _, c := range pid {
		if c < '0' || c > '9' {
			return false
		}
	}
	for _, c := range nonce {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func sweep(root *directory, base, staged string, now time.Time) {
	names, _ := root.entryNames()
	for _, name := range names {
		if name == staged || !generatedName(name, base) {
			continue
		}
		info, err := root.Lstat(name)
		if err == nil && info.Mode&unix.S_IFMT == unix.S_IFREG && now.Sub(time.Unix(info.Mtim.Unix())) >= time.Hour {
			_ = root.Remove(name)
		}
	}
}

// creationMode observes the directory's actual creation policy without changing
// the process-wide umask or exposing data in an initially permissive file.
func creationMode(root *directory, base string, mode fs.FileMode, stage fs.FileInfo) (fs.FileMode, error) {
	if mode.Perm()&^0o600 == 0 {
		return stage.Mode().Perm() & mode.Perm(), nil
	}
	probe, name, err := createSibling(root, base, mode.Perm())
	if err != nil {
		return 0, err
	}
	info, statErr := probe.Stat()
	cleanupErr := root.removeIfPresent(name)
	if cleanupErr != nil {
		cleanupErr = &StageCleanupError{Staged: root.path(name), Err: cleanupErr}
	}
	if err := errors.Join(cleanupErr, probe.Close(), statErr); err != nil {
		return 0, err
	}
	return info.Mode().Perm(), nil
}

type ownership struct{ uid, gid uint32 }

func ownerOf(info fs.FileInfo) ownership {
	stat := info.Sys().(*syscall.Stat_t)
	return ownership{stat.Uid, stat.Gid}
}

func preserveOwner(f *os.File, path string, from, to ownership) error {
	if to.uid != uint32(os.Geteuid()) || from.uid != to.uid {
		return &OwnershipError{path, fmt.Errorf("file belongs to uid %d", to.uid)}
	}
	if from.gid != to.gid {
		if err := f.Chown(-1, int(to.gid)); err != nil {
			return &OwnershipError{path, err}
		}
	}
	return nil
}

func verifyMetadata(f *os.File, path string, mode fs.FileMode, owner ownership) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	got := ownerOf(info)
	if info.Mode() != mode || got != owner {
		return &OwnershipError{path, fmt.Errorf("staged mode %v uid:gid %d:%d, want %v %d:%d", info.Mode(), got.uid, got.gid, mode, owner.uid, owner.gid)}
	}
	return nil
}

func (w writer) replace(root *directory, base string, data []byte, mode fs.FileMode, oldOwner *ownership, retainMode bool, check func(preparedFile) error) (err error) {
	file, name, err := createSibling(root, base, 0o600)
	if err != nil {
		return err
	}
	stage := stagedFile{root, name, base}
	published := false
	defer func() {
		if !published {
			if cleanupErr := w.remove(stage); cleanupErr != nil {
				err = errors.Join(err, &StageCleanupError{Staged: stage.Name(), Err: cleanupErr})
			}
		}
	}()
	info, err := file.Stat()
	if err == nil && oldOwner != nil {
		err = preserveOwner(file, root.path(base), ownerOf(info), *oldOwner)
	}
	if err == nil && !retainMode {
		mode, err = creationMode(root, base, mode, info)
	}
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Chmod(mode)
	}
	if err == nil {
		want := ownerOf(info)
		if oldOwner != nil {
			want = *oldOwner
		}
		err = verifyMetadata(file, root.path(base), mode, want)
	}
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	delays := [...]time.Duration{20 * time.Millisecond, 60 * time.Millisecond, 140 * time.Millisecond, 300 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		if check != nil {
			if err := check(stage); err != nil {
				return err
			}
		}
		// A competing editor can still write between this final read and the rename.
		err = w.commit(stage, root.path(base))
		if err == nil {
			published = true
			sweep(root, base, name, w.now())
			return nil
		}
		retry := errors.Is(err, unix.EBUSY) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
		if !retry || attempt == len(delays) {
			return err
		}
		time.Sleep(delays[attempt])
	}
}

func (w writer) writeFile(path string, data []byte, mode fs.FileMode) error {
	root, base, err := openWriteParent(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return w.writeFileAt(root, base, data, mode)
}

func (w writer) writeFileAt(root *directory, base string, data []byte, mode fs.FileMode) error {
	info, err := root.Lstat(base)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var owner *ownership
	if info != nil {
		owner = &ownership{info.Uid, info.Gid}
	}
	return w.replace(root, base, data, mode, owner, false, nil)
}

type settingsConflict struct{ path string }

func (e *settingsConflict) Error() string { return e.path + ": settings changed before replacement" }

type snapshot struct {
	info fs.FileInfo
	data []byte
}

func sameLeaf(st *unix.Stat_t, info fs.FileInfo) bool {
	native := info.Sys().(*syscall.Stat_t)
	return st.Dev == native.Dev && st.Ino == native.Ino
}

func readSettings(root *directory, base string, conflict error, duringRead func()) (snapshot, error) {
	info, err := root.Lstat(base)
	if duringRead != nil {
		duringRead()
	}
	if errors.Is(err, fs.ErrNotExist) {
		return snapshot{data: []byte("{}")}, nil
	}
	if err != nil {
		return snapshot{}, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return snapshot{}, fmt.Errorf("%s is not a regular file", base)
	}
	file, err := root.OpenFile(base, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return snapshot{}, conflict
	}
	if err != nil {
		return snapshot{}, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return snapshot{}, err
	}
	if !opened.Mode().IsRegular() {
		return snapshot{}, fmt.Errorf("%s is not a regular file", base)
	}
	if !sameLeaf(info, opened) {
		return snapshot{}, conflict
	}
	data, err := io.ReadAll(file)
	return snapshot{opened, data}, err
}

func (w writer) replaceSettings(path string, edit func([]byte) ([]byte, error), afterStage func(preparedFile, string)) error {
	return w.replaceSettingsInfo(path, func(data []byte, _ fs.FileInfo) ([]byte, error) { return edit(data) }, afterStage)
}

func (w writer) replaceSettingsInfo(path string, edit func([]byte, fs.FileInfo) ([]byte, error), afterStage func(preparedFile, string)) error {
	root, base, err := openSettingsParent(path, true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	parent, err := root.handle.Stat()
	if err != nil {
		return err
	}
	target := root.path(base)
	conflict := &settingsConflict{path}
	for range 3 {
		before, err := readSettings(root, base, conflict, nil)
		if errors.Is(err, conflict) {
			continue
		}
		if err != nil {
			return err
		}
		if before.info != nil && ownerOf(before.info).uid != uint32(os.Geteuid()) {
			return &OwnershipError{path, fmt.Errorf("file belongs to uid %d", ownerOf(before.info).uid)}
		}
		data, err := edit(bytes.Clone(before.data), before.info)
		if err != nil {
			return err
		}
		check := func(f preparedFile) error {
			var duringRead func()
			if f != nil && afterStage != nil {
				duringRead = func() { afterStage(f, target) }
			}
			after, err := readSettings(root, base, conflict, duringRead)
			if err != nil {
				return err
			}
			if (before.info == nil) != (after.info == nil) || before.info != nil && (!os.SameFile(before.info, after.info) || before.info.Mode() != after.info.Mode() || ownerOf(before.info) != ownerOf(after.info)) || !bytes.Equal(before.data, after.data) {
				return conflict
			}
			current, name, err := openSettingsParent(path, false)
			if errors.Is(err, fs.ErrNotExist) {
				return conflict
			}
			if err != nil {
				return err
			}
			defer func() { _ = current.Close() }()
			leaf, err := current.Lstat(name)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			dir, err := current.handle.Stat()
			if err != nil {
				return err
			}
			if name != base || !os.SameFile(dir, parent) {
				return fmt.Errorf("%s: settings target directory or name changed", path)
			}
			if (leaf == nil) != (after.info == nil) || leaf != nil && !sameLeaf(leaf, after.info) {
				return conflict
			}
			return nil
		}
		if before.info != nil && bytes.Equal(before.data, data) {
			err = check(nil)
		} else {
			mode := fs.FileMode(0o666)
			var owner *ownership
			if before.info != nil {
				mode = before.info.Mode()
				owner = new(ownerOf(before.info))
			}
			err = w.replace(root, base, data, mode, owner, before.info != nil, check)
		}
		if err != conflict { //nolint:errorlint // Retry the bare conflict, never a conflict joined with a cleanup failure.
			return err
		}
	}
	return conflict
}
