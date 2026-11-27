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

//go:build process && unix

package worker_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker"
)

// staleModTime marks a stale socket file, so that a socket unlinked and bound again is told apart even when the new
// file reuses the old inode number.
var staleModTime = time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)

// lockStart is what one ListenUnix call left behind.
type lockStart struct {
	// Listener reports that ListenUnix returned a listener.
	Listener bool
	// ErrorNamesLock reports that the returned error names the lock file.
	ErrorNamesLock bool
	// Socket describes the socket path after the call.
	Socket string
	// Lock describes the lock path after the call compared with before it.
	Lock string
}

// socketState describes path after a start: absent, the untouched stale socket, or what took its place.
func socketState(path string, stale fs.FileInfo) string {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "absent"
	case err != nil:
		return "unreadable: " + err.Error()
	case stale == nil:
		return "created with mode " + info.Mode().String()
	case !os.SameFile(stale, info) || info.Mode() != stale.Mode() || !info.ModTime().Equal(stale.ModTime()):
		return "replaced by mode " + info.Mode().String() + " modified " + info.ModTime().UTC().String()
	}
	return "the stale socket"
}

// lockState describes the lock path after a start compared with its state before it.
func lockState(path string, before fs.FileInfo) string {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return "unreadable: " + err.Error()
	case before == nil:
		return "created with mode " + info.Mode().String()
	case !os.SameFile(before, info) || info.Mode() != before.Mode():
		return "replaced by mode " + info.Mode().String()
	}
	return "unchanged"
}

// TestSocketStartRefusesUnusableLock starts the socket on a path whose sibling lock file cannot be used. A starter
// without the lock could take another starter's half-bound socket for a stale one, so it must not bind at all: the
// start fails with an error naming the lock file, a free socket path stays free, and a stale socket already at the
// path is neither unlinked nor replaced. With a usable lock the same stale socket is reclaimed.
func TestSocketStartRefusesUnusableLock(t *testing.T) {
	tests := map[string]struct {
		// spoil makes lockPath unusable as a lock and returns a check of anything else it made, or nil.
		spoil func(t *testing.T, lockPath string) func(*testing.T)
		// stale leaves a socket file at the path that no process listens on before the start.
		stale bool
		// needsPermissions marks a case that file permissions must stop, which they do not for a superuser.
		needsPermissions bool
	}{
		"error: lock path is a directory and the socket path is free":                        {spoil: lockDirectory},
		"error: lock path is a directory and a stale socket is kept":                         {spoil: lockDirectory, stale: true},
		"error: lock path is a symbolic link and the socket path is free":                    {spoil: lockSymlink},
		"error: lock path is a symbolic link and a stale socket is kept":                     {spoil: lockSymlink, stale: true},
		"error: lock file cannot be opened and the socket path is free":                      {spoil: lockInaccessible, needsPermissions: true},
		"error: lock file cannot be opened and a stale socket is kept":                       {spoil: lockInaccessible, stale: true, needsPermissions: true},
		"success: a usable lock reclaims the stale socket and a client reaches its listener": {stale: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if tc.needsPermissions && os.Geteuid() == 0 {
				t.Fatal("the venue must not run as root to test a lock file its owner may neither read nor write")
			}
			// A subtest's descriptive name can by itself exceed sockaddr_un.sun_path.
			dir, err := os.MkdirTemp("", "mg-lock-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(dir); err != nil {
					t.Error(err)
				}
			})
			path := filepath.Join(dir, "gw.sock")
			lockPath := path + ".lock"
			var stale fs.FileInfo
			if tc.stale {
				staleSocket(t, path)
				if err := os.Chtimes(path, staleModTime, staleModTime); err != nil {
					t.Fatal(err)
				}
				if stale, err = os.Lstat(path); err != nil {
					t.Fatal(err)
				}
			}
			if tc.spoil == nil {
				ln, err := worker.ListenUnix(path)
				if err != nil {
					t.Fatalf("ListenUnix with a usable lock over a stale socket: %v", err)
				}
				defer func() { _ = ln.Close() }()
				reached, err := reachedBy(t, path, ln)
				if err != nil {
					t.Fatal(err)
				}
				if !reached {
					t.Fatal("a connection to the reclaimed path did not reach the returned listener")
				}
				return
			}
			check := tc.spoil(t, lockPath)
			lockBefore, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}

			ln, err := worker.ListenUnix(path)
			// Judge the path before closing a listener: closing unlinks the socket file it bound.
			got := lockStart{
				Listener:       ln != nil,
				ErrorNamesLock: err != nil && strings.Contains(err.Error(), filepath.Base(lockPath)),
				Socket:         socketState(path, stale),
				Lock:           lockState(lockPath, lockBefore),
			}
			if ln != nil {
				if err := ln.Close(); err != nil {
					t.Error(err)
				}
			}
			want := lockStart{Socket: "absent", ErrorNamesLock: true, Lock: "unchanged"}
			if tc.stale {
				want.Socket = "the stale socket"
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("ListenUnix returned error %v (-want +got):\n%s", err, diff)
			}
			if check != nil {
				check(t)
			}
		})
	}
}

// lockDirectory makes the lock path a directory.
func lockDirectory(t *testing.T, lockPath string) func(*testing.T) {
	t.Helper()
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	return nil
}

// lockSymlink makes the lock path a symbolic link to a regular file, which a lock must not follow, and checks that
// the file it names is left as it was.
func lockSymlink(t *testing.T, lockPath string) func(*testing.T) {
	t.Helper()
	target := filepath.Join(filepath.Dir(lockPath), "elsewhere")
	const content = "not the socket lock"
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}
	return func(t *testing.T) {
		t.Helper()
		body, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if diff := gocmp.Diff(content, string(body)); diff != "" {
			t.Errorf("the file the lock link names changed (-want +got):\n%s", diff)
		}
	}
}

// lockInaccessible makes the lock path a regular file its owner may neither read nor write.
func lockInaccessible(t *testing.T, lockPath string) func(*testing.T) {
	t.Helper()
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockPath, 0); err != nil {
		t.Fatal(err)
	}
	return nil
}

// missingParentStart is what one ListenUnix call on a path under a missing directory returned and left behind.
type missingParentStart struct {
	// Listener reports that ListenUnix returned a listener.
	Listener bool
	// NotExist reports that the returned error wraps ENOENT.
	NotExist bool
	// ErrorNamesLock reports that the returned error names the lock file.
	ErrorNamesLock bool
	// Missing describes the outermost missing directory after the call.
	Missing string
}

// TestSocketStartUnderMissingDirectory starts the socket on a path whose parent directory does not exist. The lock is
// taken before any bind, so the start fails opening the lock file: no listener is returned, the error wraps ENOENT and
// names the lock file, and no directory on the way to the path is created.
func TestSocketStartUnderMissingDirectory(t *testing.T) {
	tests := map[string]struct {
		// missing are the directories between the existing temporary directory and the socket, outermost first.
		missing []string
	}{
		"error: the parent directory is missing":                   {missing: []string{"absent"}},
		"error: the parent and its parent directories are missing": {missing: []string{"absent", "nested"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// A subtest's descriptive name can by itself exceed sockaddr_un.sun_path.
			dir, err := os.MkdirTemp("", "mg-lock-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(dir); err != nil {
					t.Error(err)
				}
			})
			outermost := filepath.Join(dir, tc.missing[0])
			path := filepath.Join(append([]string{dir}, append(tc.missing, "gw.sock")...)...)

			ln, err := worker.ListenUnix(path)
			got := missingParentStart{
				Listener:       ln != nil,
				NotExist:       errors.Is(err, syscall.ENOENT),
				ErrorNamesLock: err != nil && strings.Contains(err.Error(), filepath.Base(path)+".lock"),
				Missing:        socketState(outermost, nil),
			}
			if ln != nil {
				if err := ln.Close(); err != nil {
					t.Error(err)
				}
			}
			want := missingParentStart{NotExist: true, ErrorNamesLock: true, Missing: "absent"}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("ListenUnix returned error %v (-want +got):\n%s", err, diff)
			}
		})
	}
}
