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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	renameio "github.com/google/renameio/v2"
	"golang.org/x/sys/unix"
)

func (w writer) writeFile(path string, data []byte, mode fs.FileMode) error {
	return w.replace(path, data, renameio.WithPermissions(mode), nil)
}

func (w writer) replace(path string, data []byte, permission renameio.Option, check func(*renameio.PendingFile) error) error {
	delays := [...]time.Duration{20 * time.Millisecond, 60 * time.Millisecond, 140 * time.Millisecond, 300 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		// A failed rename leaves a closed PendingFile; stage a fresh one for each retry.
		f, err := renameio.NewPendingFile(path, renameio.WithTempDir(filepath.Dir(path)), permission)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if err == nil && check != nil {
			err = check(f)
		}
		if err != nil {
			return errors.Join(err, f.Cleanup())
		}
		err = w.commit(f, path)
		if cleanup := f.Cleanup(); cleanup != nil {
			return errors.Join(err, cleanup)
		}
		if err == nil {
			sweep(path, w.now())
			return nil
		}
		retry := errors.Is(err, unix.EBUSY) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
		if attempt == len(delays) || !retry {
			return err
		}
		time.Sleep(delays[attempt])
	}
}

func sweep(path string, now time.Time) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), base+".") || !strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		sibling := filepath.Join(dir, entry.Name())
		info, err := os.Stat(sibling)
		if err == nil && now.Sub(info.ModTime()) >= time.Hour {
			_ = unix.Unlink(sibling)
		}
	}
}

type snapshot struct {
	path string
	mode fs.FileMode
	info fs.FileInfo
	data []byte
}

func readSettings(path string) (snapshot, error) {
	target, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		// A dangling symlink is not a new file: replacing it would destroy the link.
		if _, e := os.Lstat(path); errors.Is(e, fs.ErrNotExist) {
			return snapshot{path: path, mode: 0o666, data: []byte("{}")}, nil
		}
	}
	if err != nil {
		return snapshot{}, err
	}
	info, err := os.Lstat(target)
	if err != nil {
		return snapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return snapshot{}, fmt.Errorf("%s is not a regular file", path)
	}
	// A FIFO or symlink substituted after Lstat must not block or redirect this open.
	f, err := os.OpenFile(target, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return snapshot{}, err
	}
	defer func() { _ = f.Close() }()
	info, err = f.Stat()
	if err != nil {
		return snapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return snapshot{}, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(f)
	return snapshot{target, info.Mode(), info, data}, err
}

func (w writer) replaceSettings(path string, edit func([]byte) ([]byte, error), afterStage func(preparedFile, string)) error {
	conflict := fmt.Errorf("%s: settings changed before replacement", path)
	for range 3 {
		before, err := readSettings(path)
		if err != nil {
			return err
		}
		data, err := edit(bytes.Clone(before.data))
		if err != nil {
			return err
		}
		check := func(f *renameio.PendingFile) error {
			// renameio preserves permission bits; special mode bits must be set after writing.
			if f != nil && before.mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
				if err := f.Chmod(before.mode); err != nil {
					return err
				}
			}
			if f != nil && afterStage != nil {
				afterStage(f, before.path)
			}
			after, err := readSettings(path)
			if err != nil {
				return err
			}
			if before.path != after.path || before.mode != after.mode || (before.info == nil) != (after.info == nil) || before.info != nil && !os.SameFile(before.info, after.info) || !bytes.Equal(before.data, after.data) {
				return conflict
			}
			return nil
		}
		if before.info != nil && bytes.Equal(data, before.data) {
			err = check(nil)
		} else {
			permission := renameio.WithPermissions(before.mode)
			if before.info != nil {
				permission = renameio.WithStaticPermissions(before.mode)
			} else if err := os.MkdirAll(filepath.Dir(before.path), 0o777); err != nil {
				return err
			}
			err = w.replace(before.path, data, permission, check)
		}
		if !errors.Is(err, conflict) {
			return err
		}
	}
	return conflict
}
