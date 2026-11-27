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

//go:build process && linux

package worker_test

import (
	"bufio"
	"crypto/rand"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
)

// abstractID returns a name for the abstract namespace that no other test or run uses, without its leading marker.
func abstractID() string {
	return "mg-abstract-" + rand.Text()
}

// entryNames lists the names in dir.
func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// answerOf connects to name and returns the line its owner answers with.
func answerOf(t *testing.T, name string) string {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "unix", name)
	if err != nil {
		t.Fatalf("connect to %q: %v", name, err)
	}
	defer func() { _ = conn.Close() }()
	// The owner writes at once; the deadline only bounds an owner that never answers.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	answer := bufio.NewScanner(conn)
	if !answer.Scan() {
		t.Fatal(errors.Join(errors.New("no answer from the owner"), answer.Err()))
	}
	return answer.Text()
}

// TestAbstractSocketStartsWithoutFiles starts the socket on a Linux abstract address in a working directory of its
// own. An abstract address has no file to reclaim, so the start binds once and creates neither a socket file nor a
// lock file named after the address; a second start on the held address refuses and creates nothing either. Both
// spellings of the address, with '@' or a NUL byte, reach the same owner.
func TestAbstractSocketStartsWithoutFiles(t *testing.T) {
	tests := map[string]struct {
		// marker spells the address the test connects to; the starters always receive the '@' spelling, since a
		// process argument cannot carry a NUL byte.
		marker string
	}{
		"success: the at-sign spelling reaches the owner and no start creates a file": {marker: "@"},
		"success: the NUL spelling reaches the owner and no start creates a file":     {marker: "\x00"},
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			id := abstractID()
			ownerDir, result, joinOwner := startInOwnDirectory(t, env, "@"+id)
			pid, owned := strings.CutPrefix(result, "owner ")
			if !owned {
				t.Fatalf("the first start on a free abstract address: %q, want an owner", result)
			}
			if diff := gocmp.Diff(pid, answerOf(t, tc.marker+id)); diff != "" {
				t.Errorf("a connection did not reach the owner (-want +got):\n%s", diff)
			}
			secondDir, result, joinSecond := startInOwnDirectory(t, env, "@"+id)
			if !strings.HasPrefix(result, "refused ") {
				t.Errorf("a second start on the held abstract address: %q, want a refusal", result)
			}
			if diff := gocmp.Diff(pid, answerOf(t, tc.marker+id)); diff != "" {
				t.Errorf("after the second start a connection did not reach the first owner (-want +got):\n%s", diff)
			}
			// Both starters must exit cleanly; the directories are judged after they have.
			for _, join := range []func() error{joinSecond, joinOwner} {
				if err := join(); err != nil {
					t.Error(err)
				}
			}
			for dir, start := range map[string]string{ownerDir: "owner", secondDir: "refused start"} {
				if diff := gocmp.Diff([]string(nil), entryNames(t, dir)); diff != "" {
					t.Errorf("files in the working directory of the %s (-want +got):\n%s", start, diff)
				}
			}
		})
	}
}

// TestAbstractSocketSecondStartGetsBindError holds a Linux abstract address with ListenUnix and starts a second
// ListenUnix on it. Nothing at an abstract address can be reclaimed, so the second start returns the error of its
// single bind exactly as a plain bind reports it, and creates no file named after the address in the working
// directory.
func TestAbstractSocketSecondStartGetsBindError(t *testing.T) {
	tests := map[string]struct {
		// owner and second spell the address for the holding and the second start.
		owner, second string
	}{
		"error: a NUL-spelled second start on a NUL-spelled owner":      {owner: "\x00", second: "\x00"},
		"error: an at-sign-spelled second start on a NUL-spelled owner": {owner: "\x00", second: "@"},
		"error: a NUL-spelled second start on an at-sign-spelled owner": {owner: "@", second: "\x00"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			id := abstractID()
			// The test binary's working directory is the package source directory. A start that took the '@'
			// spelling for a relative path would leave these files there; remove them however the test ends.
			t.Cleanup(func() {
				for _, leftover := range []string{"@" + id, "@" + id + ".lock"} {
					if err := os.Remove(leftover); err != nil && !errors.Is(err, fs.ErrNotExist) {
						t.Error(err)
					}
				}
			})
			owner, err := worker.ListenUnix(tc.owner + id)
			if err != nil {
				t.Fatalf("ListenUnix on a free abstract address: %v", err)
			}
			defer func() { _ = owner.Close() }()
			reached, err := reachedBy(t, "@"+id, owner.(*net.UnixListener))
			if err != nil {
				t.Fatal(err)
			}
			if !reached {
				t.Error("a connection to the abstract address did not reach the owner")
			}

			ln, err := worker.ListenUnix(tc.second + id)
			if ln != nil {
				_ = ln.Close()
				t.Fatal("a second ListenUnix on a held abstract address returned a listener")
			}
			if err == nil {
				t.Fatal("a second ListenUnix on a held abstract address returned neither a listener nor an error")
			}
			if !errors.Is(err, syscall.EADDRINUSE) {
				t.Errorf("second ListenUnix: %v, want EADDRINUSE", err)
			}
			plain, plainErr := net.ListenUnix("unix", &net.UnixAddr{Name: tc.second + id, Net: "unix"})
			if plain != nil {
				_ = plain.Close()
				t.Fatal("a plain bind on a held abstract address succeeded")
			}
			if diff := gocmp.Diff(plainErr.Error(), err.Error()); diff != "" {
				t.Errorf("the second start's error is not the bind's own (-plain bind +ListenUnix):\n%s", diff)
			}
			// Only the two random names are judged: other files in the shared package directory are not this test's.
			var created []string
			for _, leftover := range []string{"@" + id, "@" + id + ".lock"} {
				switch _, err := os.Lstat(leftover); {
				case err == nil:
					created = append(created, leftover)
				case !errors.Is(err, fs.ErrNotExist):
					created = append(created, leftover+": "+err.Error())
				}
			}
			if diff := gocmp.Diff([]string(nil), created); diff != "" {
				t.Errorf("files named after the address in the working directory (-want +got):\n%s", diff)
			}
		})
	}
}
