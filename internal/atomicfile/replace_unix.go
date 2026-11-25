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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type stagedFile struct {
	root         *os.Root
	name, target string
}

func (s stagedFile) Name() string                  { return filepath.Join(s.root.Name(), s.name) }
func (s stagedFile) CloseAtomicallyReplace() error { return s.root.Rename(s.name, s.target) }

func createSibling(root *os.Root, base string, mode fs.FileMode) (*os.File, string, error) {
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

func sweep(root *os.Root, base, staged string, now time.Time) {
	entries, _ := fs.ReadDir(root.FS(), ".")
	for _, e := range entries {
		name := e.Name()
		if name == staged || !generatedName(name, base) {
			continue
		}
		info, err := root.Lstat(name)
		if err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) >= time.Hour {
			_ = root.Remove(name)
		}
	}
}

// creationMode observes the directory's actual creation policy without changing
// the process-wide umask or exposing data in an initially permissive file.
func creationMode(root *os.Root, base string, mode fs.FileMode, stage fs.FileInfo) (fs.FileMode, error) {
	if mode.Perm()&^0o600 == 0 {
		return stage.Mode().Perm() & mode.Perm(), nil
	}
	probe, name, err := createSibling(root, base, mode.Perm())
	if err != nil {
		return 0, err
	}
	info, statErr := probe.Stat()
	if err := errors.Join(root.Remove(name), probe.Close(), statErr); err != nil {
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

func (w writer) replace(root *os.Root, base string, data []byte, mode fs.FileMode, existing fs.FileInfo, retainMode bool, check func(preparedFile) error) (err error) {
	file, name, err := createSibling(root, base, 0o600)
	if err != nil {
		return err
	}
	stage := stagedFile{root, name, base}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, root.Remove(name))
		}
	}()
	info, err := file.Stat()
	if err == nil && existing != nil {
		err = preserveOwner(file, filepath.Join(root.Name(), base), ownerOf(info), ownerOf(existing))
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
		err = w.commit(stage, filepath.Join(root.Name(), base))
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
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	base := filepath.Base(path)
	info, err := root.Lstat(base)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return w.replace(root, base, data, mode, info, false, nil)
}

func settingsTarget(path string) (string, error) {
	target, err := filepath.EvalSymlinks(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return target, err
	}
	if _, e := os.Lstat(path); !errors.Is(e, fs.ErrNotExist) {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(path)), nil
}

type snapshot struct {
	info fs.FileInfo
	data []byte
}

func readSettings(root *os.Root, base string) (snapshot, error) {
	info, err := root.Lstat(base)
	if errors.Is(err, fs.ErrNotExist) {
		return snapshot{data: []byte("{}")}, nil
	}
	if err != nil {
		return snapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return snapshot{}, fmt.Errorf("%s is not a regular file", base)
	}
	file, err := root.OpenFile(base, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return snapshot{}, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return snapshot{}, err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return snapshot{}, fmt.Errorf("%s changed during open", base)
	}
	data, err := io.ReadAll(file)
	return snapshot{opened, data}, err
}

func (w writer) replaceSettings(path string, edit func([]byte) ([]byte, error), afterStage func(preparedFile, string)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	target, err := settingsTarget(path)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	parent, err := root.Stat(".")
	if err != nil {
		return err
	}
	base := filepath.Base(target)
	conflict := fmt.Errorf("%s: settings changed before replacement", path)
	for range 3 {
		before, err := readSettings(root, base)
		if err != nil {
			return err
		}
		if before.info != nil && ownerOf(before.info).uid != uint32(os.Geteuid()) {
			return &OwnershipError{path, fmt.Errorf("file belongs to uid %d", ownerOf(before.info).uid)}
		}
		data, err := edit(bytes.Clone(before.data))
		if err != nil {
			return err
		}
		check := func(f preparedFile) error {
			if f != nil && afterStage != nil {
				afterStage(f, target)
			}
			current, err := settingsTarget(path)
			if err != nil {
				return err
			}
			dir, err := os.Stat(filepath.Dir(current))
			if err != nil {
				return err
			}
			if base != filepath.Base(current) || !os.SameFile(dir, parent) {
				return fmt.Errorf("%s: settings target directory or name changed", path)
			}
			after, err := readSettings(root, base)
			if err != nil {
				return err
			}
			if (before.info == nil) != (after.info == nil) || before.info != nil && (!os.SameFile(before.info, after.info) || before.info.Mode() != after.info.Mode() || ownerOf(before.info) != ownerOf(after.info)) || !bytes.Equal(before.data, after.data) {
				return conflict
			}
			return nil
		}
		if before.info != nil && bytes.Equal(before.data, data) {
			err = check(nil)
		} else {
			mode := fs.FileMode(0o666)
			if before.info != nil {
				mode = before.info.Mode()
			}
			err = w.replace(root, base, data, mode, before.info, before.info != nil, check)
		}
		if !errors.Is(err, conflict) {
			return err
		}
	}
	return conflict
}
