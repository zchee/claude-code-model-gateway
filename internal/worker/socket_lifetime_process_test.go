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
	"bufio"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
)

// lifetimeWatchdog bounds a call that must return at once. It only aborts a hang; the checks after it decide the case.
const lifetimeWatchdog = 10 * time.Second

// noPanic runs call and turns a panic into a test failure, so that one unimplemented function cannot abort the binary
// and hide the results of the package's other tests.
func noPanic[T any](t testing.TB, what string, call func() T) (result T) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("%s panicked: %v", what, p)
		}
	}()
	return call()
}

// socketDir returns a new directory of its own for one case's socket, removed when the case ends. A subtest's
// descriptive name can by itself exceed sockaddr_un.sun_path, so the directory is not under t.TempDir.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mg-life-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

// pathState describes path compared with before, the state the test recorded for it: absent, unchanged, created, or
// replaced. A file counts as unchanged only while it is the same file with the same mode and modification time.
func pathState(path string, before fs.FileInfo) string {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "absent"
	case err != nil:
		return "unreadable: " + err.Error()
	case before == nil:
		return "created with mode " + info.Mode().String()
	case !os.SameFile(before, info) || info.Mode() != before.Mode() || !info.ModTime().Equal(before.ModTime()):
		return "replaced by mode " + info.Mode().String() + " modified " + info.ModTime().UTC().String()
	}
	return "unchanged"
}

// fileKind names the kind of file at path, or "absent".
func fileKind(path string) string {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "absent"
	case err != nil:
		return "unreadable: " + err.Error()
	case info.Mode().IsRegular():
		return "regular file"
	case info.Mode()&fs.ModeSocket != 0:
		return "socket"
	case info.Mode()&fs.ModeNamedPipe != 0:
		return "named pipe"
	}
	return info.Mode().String()
}

// markOld gives the file at path an old modification time, so that a file unlinked and created again is told apart
// even when it reuses the inode number, and returns its state.
func markOld(t *testing.T, path string) fs.FileInfo {
	t.Helper()
	if err := os.Chtimes(path, staleModTime, staleModTime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// lockDescriptors returns the descriptors of this process that are open on the file at lockPath, found by comparing
// the device and inode of every descriptor listed in /dev/fd with those of lockPath. The test's own reference
// descriptors in except are left out.
func lockDescriptors(t *testing.T, lockPath string, except ...int) []int {
	t.Helper()
	var want unix.Stat_t
	if err := unix.Lstat(lockPath, &want); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		t.Fatalf("inspect %s: %v", lockPath, err)
	}
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	var fds []int
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		var st unix.Stat_t
		// The descriptor that listed the directory is closed by now; it is never the lock file.
		if unix.Fstat(fd, &st) != nil {
			continue
		}
		if st.Dev == want.Dev && st.Ino == want.Ino && !slices.Contains(except, fd) {
			fds = append(fds, fd)
		}
	}
	return fds
}

// closeOnExec reports whether every descriptor in fds is marked close-on-exec, and false when fds is empty.
func closeOnExec(t *testing.T, fds []int) bool {
	t.Helper()
	for _, fd := range fds {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("descriptor flags of %d: %v", fd, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			return false
		}
	}
	return len(fds) > 0
}

// lockReference opens an unlocked reference descriptor on the lock file at lockPath, closed when the test ends. While
// it is open the file's inode cannot be freed, so no new file can reuse its number, and referenceState can tell
// whether the path still names that file. An unlocked descriptor takes no part in the advisory lock.
func lockReference(t *testing.T, lockPath string) int {
	t.Helper()
	fd, err := unix.Open(lockPath, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open a reference to %s: %v", lockPath, err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

// referenceState describes path compared with the file the reference descriptor ref is open on: unchanged while
// the path names that same file, absent, or replaced.
func referenceState(t *testing.T, path string, ref int) string {
	t.Helper()
	var held, named unix.Stat_t
	if err := unix.Fstat(ref, &held); err != nil {
		t.Fatalf("inspect the reference to %s: %v", path, err)
	}
	switch err := unix.Lstat(path, &named); {
	case errors.Is(err, unix.ENOENT):
		return "absent"
	case err != nil:
		return "unreadable: " + err.Error()
	case named.Dev != held.Dev || named.Ino != held.Ino:
		return "replaced"
	}
	return "unchanged"
}

// flockRetry is a flock call of the test's own, retried when a signal interrupts it.
func flockRetry(fd, how int) error {
	for {
		err := unix.Flock(fd, how)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// lockHeld reports whether some descriptor holds the advisory lock on the regular file lockPath, by trying to take it
// without waiting through a descriptor of the test's own and giving it up at once. A missing lock file is not held.
func lockHeld(t *testing.T, lockPath string) bool {
	t.Helper()
	fd, err := unix.Open(lockPath, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return false
	}
	if err != nil {
		t.Fatalf("open %s to test its lock: %v", lockPath, err)
	}
	defer func() { _ = unix.Close(fd) }()
	switch err := flockRetry(fd, unix.LOCK_EX|unix.LOCK_NB); {
	case err == nil:
		if err := flockRetry(fd, unix.LOCK_UN); err != nil {
			t.Fatalf("give up the test's lock on %s: %v", lockPath, err)
		}
		return false
	case errors.Is(err, unix.EWOULDBLOCK):
		return true
	default:
		t.Fatalf("test the lock on %s: %v", lockPath, err)
	}
	return false
}

// holdSocketLock takes the advisory lock on lockPath, creating the file, as a live owner of the socket holds it, and
// returns its release. The release gives the lock up once; the case's cleanup calls it too.
func holdSocketLock(t *testing.T, lockPath string) (release func()) {
	t.Helper()
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := flockRetry(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("the test could not take the free lock %s: %v", lockPath, err)
	}
	release = sync.OnceFunc(func() {
		_ = flockRetry(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	})
	t.Cleanup(release)
	return release
}

// startWithin calls ListenUnix on path and returns its result if it comes within lifetimeWatchdog. Otherwise the start
// waits for something it should not wait for: startWithin calls unblock to end that wait, closes any listener the late
// start returns, and fails the test.
func startWithin(t *testing.T, path string, unblock func()) (net.Listener, error) {
	t.Helper()
	type started struct {
		ln  net.Listener
		err error
	}
	done := make(chan started, 1)
	go func() {
		ln, err := worker.ListenUnix(path)
		done <- started{ln, err}
	}()
	select {
	case s := <-done:
		return s.ln, s.err
	case <-time.After(lifetimeWatchdog):
	}
	unblock()
	select {
	case s := <-done:
		if s.ln != nil {
			_ = s.ln.Close()
		}
		t.Fatalf("ListenUnix(%q) did not return within %v while another owner held the socket lock; it returned only after the lock was given up (error %v)", path, lifetimeWatchdog, s.err)
	case <-time.After(lifetimeWatchdog):
		t.Fatalf("ListenUnix(%q) did not return within %v, nor within %v after the socket lock was given up", path, lifetimeWatchdog, lifetimeWatchdog)
	}
	return nil, nil
}

// reaches reports whether a connection to path arrives at ln, accepted through ln's own Accept. A connection that the
// path leads elsewhere is never accepted; the watchdog then ends the wait. The Accept is always joined: at once when
// it returned, otherwise when the test ends, after closing ln.
func reaches(t *testing.T, path string, ln net.Listener) bool {
	t.Helper()
	accepted := make(chan []byte, 1)
	finished := make(chan struct{})
	joined := false
	defer func() {
		if joined {
			<-finished
			return
		}
		t.Cleanup(func() {
			_ = ln.Close()
			<-finished
		})
	}()
	go func() {
		defer close(finished)
		conn, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(lifetimeWatchdog))
		got := make([]byte, 4)
		if _, err := io.ReadFull(conn, got); err != nil {
			accepted <- nil
			return
		}
		accepted <- got
	}()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Errorf("dial %s: %v", path, err)
		return false
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Errorf("write to %s: %v", path, err)
		return false
	}
	select {
	case got := <-accepted:
		joined = true
		return string(got) == "ping"
	case <-time.After(lifetimeWatchdog):
		return false
	}
}

// answerAt connects to path and returns the line its owner answers with, or the failure.
func answerAt(t *testing.T, path string) string {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "unix", path)
	if err != nil {
		return "dial: " + err.Error()
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(lifetimeWatchdog)); err != nil {
		return "deadline: " + err.Error()
	}
	line := bufio.NewScanner(conn)
	if !line.Scan() {
		return "no answer"
	}
	return line.Text()
}

// errnoName names the first system error number in err's chain, the code the worker's bind diagnostic prints, or "".
func errnoName(err error) string {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return unix.ErrnoName(errno)
	}
	return ""
}

// backlogListener binds path as a listening Unix socket with the given accept backlog that the test never accepts on
// through Go, and returns it. Its socket file is left to the case's directory cleanup.
func backlogListener(t *testing.T, path string, backlog int) *net.UnixListener {
	t.Helper()
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	if err := unix.Listen(fd, backlog); err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), path)
	ln, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.(*net.UnixListener)
}

// fullQueueAnswer is the error a nonblocking connection to a Unix stream listener whose accept queue is full gets at
// once, as Go dials: Linux's unix_stream_connect answers EAGAIN when the receive queue of the listener is full and the
// connect may not wait, and XNU's unp_connect answers ECONNREFUSED when sonewconn finds the queue full.
func fullQueueAnswer(t *testing.T) syscall.Errno {
	t.Helper()
	switch runtime.GOOS {
	case "linux", "android":
		return syscall.EAGAIN
	case "darwin", "ios":
		return syscall.ECONNREFUSED
	}
	t.Fatalf("no known answer of %s to a connection to a full accept queue", runtime.GOOS)
	return 0
}

// fillQueue connects to path until the accept queue of its listener is full and returns how many connections it holds.
// The connection that finds the queue full must fail with fullQueueAnswer; any other failure, such as running out of
// descriptors, fails the test instead of passing for a full queue.
func fillQueue(t *testing.T, path string) int {
	t.Helper()
	full := fullQueueAnswer(t)
	const most = 256
	for n := range most {
		var dialer net.Dialer
		conn, err := dialer.DialContext(t.Context(), "unix", path)
		if err != nil {
			if !errors.Is(err, full) {
				t.Fatalf("connection %d to %s failed with %v, not with %v, the answer to a full accept queue", n+1, path, err, full)
			}
			if n == 0 {
				t.Fatalf("the first connection to %s found the accept queue full: %v", path, err)
			}
			return n
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	t.Fatalf("the accept queue of %s did not fill within %d connections", path, most)
	return 0
}

// lowerBacklog finds the one descriptor of this process that is a socket bound to path, the listener ListenUnix
// returned for it, and listens on it again with a backlog of one, so that a few connections fill its accept queue.
// Linux and XNU both let a listening socket listen again and take the new backlog as its queue limit.
func lowerBacklog(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	var found []int
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if sa, err := unix.Getsockname(fd); err == nil {
			if unixAddr, ok := sa.(*unix.SockaddrUnix); ok && unixAddr.Name == path {
				found = append(found, fd)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d descriptors bound to %s, want the one listener: %v", len(found), path, found)
	}
	if err := unix.Listen(found[0], 1); err != nil {
		t.Fatalf("lower the backlog of the listener on %s: %v", path, err)
	}
}

// queuedConnections accepts, without waiting, every connection already in ln's accept queue, closes each, and returns
// how many there were. It asks the socket directly: a Go accept deadline that has passed rejects queued connections
// too and says nothing about the queue. A listener's raw connection supports only Control; the descriptor is already
// nonblocking, so an empty queue answers EAGAIN at once.
func queuedConnections(t *testing.T, ln *net.UnixListener) int {
	t.Helper()
	raw, err := ln.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	var acceptErr error
	err = raw.Control(func(fd uintptr) {
		for {
			syscall.ForkLock.RLock()
			nfd, _, err := unix.Accept(int(fd))
			if err == nil {
				unix.CloseOnExec(nfd)
			}
			syscall.ForkLock.RUnlock()
			switch {
			case err == nil:
				_ = unix.Close(nfd)
				n++
			case errors.Is(err, unix.EINTR):
			case errors.Is(err, unix.ECONNABORTED):
				// A connection that was closed before it was accepted still came in.
				n++
			case errors.Is(err, unix.EAGAIN):
				return
			default:
				acceptErr = err
				return
			}
		}
	})
	if err = errors.Join(err, acceptErr); err != nil {
		t.Fatal(err)
	}
	return n
}

// heldStart is what a ListenUnix call returned and left behind while another owner held the socket lock.
type heldStart struct {
	// Listener reports that ListenUnix returned a listener.
	Listener bool
	// AddressInUse reports that the error matches syscall.EADDRINUSE.
	AddressInUse bool
	// Diagnostic is the error code the worker's bind diagnostic prints for the error.
	Diagnostic string
	// Socket describes the socket path after the call compared with before it.
	Socket string
	// Lock describes the lock file after the call compared with before it.
	Lock string
	// LockDescriptors counts this process's descriptors open on the lock file after the call.
	LockDescriptors int
	// Queued counts the connections waiting at the occupant of the path after the call.
	Queued int
}

// TestSocketStartRefusesWhileLockHeld starts the socket while a live owner, played by the test, holds the socket lock.
// A starter that finds the lock held tries once without waiting and gives up with the ordinary "address in use" error:
// it neither probes, binds nor unlinks the socket path, and it leaves the lock file and the owner's lock alone. A
// probe is what would take a live owner whose accept queue is full for stale, because macOS refuses a connection to a
// full queue as it refuses one to a dead socket; a starter that never probes cannot make that mistake.
func TestSocketStartRefusesWhileLockHeld(t *testing.T) {
	tests := map[string]struct {
		// occupy puts what the owner has at path and returns the listener the test keeps there, or nil, and the number
		// of connections the test left waiting in its accept queue.
		occupy func(t *testing.T, path string) (*net.UnixListener, int)
	}{
		"error: a free path is not bound": {
			occupy: func(*testing.T, string) (*net.UnixListener, int) { return nil, 0 },
		},
		"error: a socket that refuses connections is not unlinked": {
			occupy: func(t *testing.T, path string) (*net.UnixListener, int) {
				staleSocket(t, path)
				return nil, 0
			},
		},
		"error: a listener that is not accepting gets no probe": {
			occupy: func(t *testing.T, path string) (*net.UnixListener, int) {
				return backlogListener(t, path, 16), 0
			},
		},
		"error: a listener whose accept queue is full is not taken for stale": {
			occupy: func(t *testing.T, path string) (*net.UnixListener, int) {
				ln := backlogListener(t, path, 1)
				return ln, fillQueue(t, path)
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "gw.sock")
			lockPath := path + ".lock"
			release := holdSocketLock(t, lockPath)
			occupant, waiting := tc.occupy(t, path)
			var socketBefore fs.FileInfo
			if fileKind(path) != "absent" {
				socketBefore = markOld(t, path)
			}
			lockBefore := markOld(t, lockPath)

			ln, err := startWithin(t, path, release)
			got := heldStart{
				Listener:        ln != nil,
				AddressInUse:    errors.Is(err, syscall.EADDRINUSE),
				Diagnostic:      errnoName(err),
				Socket:          pathState(path, socketBefore),
				Lock:            pathState(lockPath, lockBefore),
				LockDescriptors: len(lockDescriptors(t, lockPath)),
			}
			if occupant != nil {
				got.Queued = queuedConnections(t, occupant)
			}
			if ln != nil {
				_ = ln.Close()
			}
			want := heldStart{
				AddressInUse:    true,
				Diagnostic:      "EADDRINUSE",
				Socket:          "absent",
				Lock:            "unchanged",
				LockDescriptors: 1,
				Queued:          waiting,
			}
			if socketBefore != nil {
				want.Socket = "unchanged"
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("ListenUnix returned error %v (-want +got):\n%s", err, diff)
			}
			if !lockHeld(t, lockPath) {
				t.Error("the owner's lock was given up by the refused start")
			}
		})
	}
}

// liveStart is what a second ListenUnix call returned and left behind while a listener ListenUnix returned is open.
type liveStart struct {
	// OwnerHolds reports that the socket lock was held after the first start returned.
	OwnerHolds bool
	// Listener reports that the second ListenUnix returned a listener.
	Listener bool
	// AddressInUse reports that the second start's error matches syscall.EADDRINUSE.
	AddressInUse bool
	// Diagnostic is the error code the worker's bind diagnostic prints for the second start's error.
	Diagnostic string
	// Socket describes the socket path after the second start compared with before it.
	Socket string
	// Lock describes the lock file after the second start compared with before it.
	Lock string
	// StillHeld reports that the socket lock was still held after the second start.
	StillHeld bool
	// Answer is the owner's answer to a connection to the path after the second start.
	Answer string
}

// TestSocketStartRefusesLiveStarter starts the socket while a listener that ListenUnix returned, in this process or in
// another one, is still open. That listener holds the socket lock for its lifetime, so the second start gets "address
// in use" at once and the path still leads to the first owner.
func TestSocketStartRefusesLiveStarter(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	tests := map[string]struct {
		// own starts the first owner on path, which answers every connection with want, and returns its end.
		own func(t *testing.T, path string) (want string, end func() error)
	}{
		"error: a listener of this process holds the path": {
			own: func(t *testing.T, path string) (string, func() error) {
				ln, err := worker.ListenUnix(path)
				if err != nil {
					t.Fatalf("ListenUnix on a free path: %v", err)
				}
				var served sync.WaitGroup
				served.Go(func() {
					for {
						conn, err := ln.Accept()
						if err != nil {
							return
						}
						_, _ = io.WriteString(conn, "this process\n")
						_ = conn.Close()
					}
				})
				end := sync.OnceValue(func() error {
					err := ln.Close()
					served.Wait()
					return err
				})
				t.Cleanup(func() { _ = end() })
				return "this process", end
			},
		},
		"error: a listener of another process holds the path": {
			own: func(t *testing.T, path string) (string, func() error) {
				_, result, join := startInOwnDirectory(t, env, path)
				pid, ok := strings.CutPrefix(result, "owner ")
				if !ok {
					t.Fatalf("the first starter on a free path: %q, want an owner", result)
				}
				return pid, join
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "gw.sock")
			lockPath := path + ".lock"
			answer, end := tc.own(t, path)
			got := liveStart{OwnerHolds: lockHeld(t, lockPath)}
			socketBefore, lockBefore := markOld(t, path), markOld(t, lockPath)

			ln, err := startWithin(t, path, func() { _ = end() })
			got.Listener = ln != nil
			got.AddressInUse = errors.Is(err, syscall.EADDRINUSE)
			got.Diagnostic = errnoName(err)
			got.Socket = pathState(path, socketBefore)
			got.Lock = pathState(lockPath, lockBefore)
			got.StillHeld = lockHeld(t, lockPath)
			got.Answer = answerAt(t, path)
			if ln != nil {
				_ = ln.Close()
			}
			want := liveStart{
				OwnerHolds:   true,
				AddressInUse: true,
				Diagnostic:   "EADDRINUSE",
				Socket:       "unchanged",
				Lock:         "unchanged",
				StillHeld:    true,
				Answer:       answer,
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("second ListenUnix returned error %v (-want +got):\n%s", err, diff)
			}
			if err := end(); err != nil {
				t.Errorf("ending the first owner: %v", err)
			}
		})
	}
}

// acceptQueued accepts n connections through ln, which must all be waiting in its queue already, and returns how
// many it accepted before the watchdog. The Accept goroutine is joined before acceptQueued returns: when the watchdog
// ends the wait, ln is closed to end the Accept.
func acceptQueued(t *testing.T, ln net.Listener, n int) int {
	t.Helper()
	var accepted atomic.Int32
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for range n {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
			accepted.Add(1)
		}
	}()
	select {
	case <-finished:
	case <-time.After(lifetimeWatchdog):
		_ = ln.Close()
		<-finished
	}
	return int(accepted.Load())
}

// fullOwnerStart is what a second ListenUnix call returned and left behind while the first listener, which is not
// accepting, had a full accept queue.
type fullOwnerStart struct {
	// OwnerHolds reports that the socket lock was held after the first start returned.
	OwnerHolds bool
	// Listener reports that the second ListenUnix returned a listener.
	Listener bool
	// AddressInUse reports that the second start's error matches syscall.EADDRINUSE.
	AddressInUse bool
	// Diagnostic is the error code the worker's bind diagnostic prints for the second start's error.
	Diagnostic string
	// Socket describes the socket path after the second start compared with before it.
	Socket string
	// Lock describes the lock file after the second start compared with before it.
	Lock string
	// StillHeld reports that the socket lock was still held after the second start.
	StillHeld bool
	// Drained counts the queued connections the first listener accepted afterwards.
	Drained int
	// ReachedAfter reports that a new connection to the path arrived at the first listener after the drain.
	ReachedAfter bool
}

// TestSocketStartRefusesOwnerWithFullQueue starts the socket while a listener ListenUnix returned is not accepting and
// its accept queue is full; the test lowers that listener's backlog so that a few connections fill it. macOS refuses
// a connection to a full queue as it refuses one to a dead socket, so a starter that probed would take this live
// owner for stale and unlink its socket. The owner holds the lock, so the second start gets "address in use" at once,
// the owner keeps its socket, and afterwards it accepts every connection that was waiting and then a new one.
func TestSocketStartRefusesOwnerWithFullQueue(t *testing.T) {
	tests := map[string]struct{}{
		"error: a non-accepting owner with a full accept queue keeps its socket": {},
	}
	for name := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "gw.sock")
			lockPath := path + ".lock"
			owner, err := worker.ListenUnix(path)
			if err != nil {
				t.Fatalf("ListenUnix on a free path: %v", err)
			}
			t.Cleanup(func() { _ = owner.Close() })
			got := fullOwnerStart{OwnerHolds: lockHeld(t, lockPath)}
			lowerBacklog(t, path)
			waiting := fillQueue(t, path)
			socketBefore, lockBefore := markOld(t, path), markOld(t, lockPath)

			ln, err := startWithin(t, path, func() { _ = owner.Close() })
			got.Listener = ln != nil
			got.AddressInUse = errors.Is(err, syscall.EADDRINUSE)
			got.Diagnostic = errnoName(err)
			got.Socket = pathState(path, socketBefore)
			got.Lock = pathState(lockPath, lockBefore)
			got.StillHeld = lockHeld(t, lockPath)
			if ln != nil {
				_ = ln.Close()
			}
			got.Drained = acceptQueued(t, owner, waiting)
			got.ReachedAfter = reaches(t, path, owner)
			want := fullOwnerStart{
				OwnerHolds:   true,
				AddressInUse: true,
				Diagnostic:   "EADDRINUSE",
				Socket:       "unchanged",
				Lock:         "unchanged",
				StillHeld:    true,
				Drained:      waiting,
				ReachedAfter: true,
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("second ListenUnix returned error %v with %d connections waiting at the owner (-want +got):\n%s", err, waiting, diff)
			}
		})
	}
}

// lifetime is what one listener's lock looked like while it was open and after it was closed.
type lifetime struct {
	// Held reports that the socket lock was held while the listener was open.
	Held bool
	// Descriptors counts this process's descriptors open on the lock file while the listener was open.
	Descriptors int
	// CloseOnExec reports that each of those descriptors is marked close-on-exec.
	CloseOnExec bool
	// LockKind is the kind of file at the lock path while the listener was open.
	LockKind string
	// Reached reports that a connection to the path arrived at the listener.
	Reached bool
	// CloseResult classifies the result of the listener's Close.
	CloseResult string
	// SocketAfterClose describes the socket path after Close.
	SocketAfterClose string
	// HeldAfterClose reports that the socket lock was still held after Close.
	HeldAfterClose bool
	// DescriptorsAfterClose counts this process's descriptors open on the lock file after Close.
	DescriptorsAfterClose int
	// LockAfterClose describes the lock file after Close compared with while the listener was open.
	LockAfterClose string
	// NextHeld reports that the next start on the path succeeded and held the same lock file.
	NextHeld bool
}

// TestSocketLockHeldForListenerLifetime starts the socket on a free path and over a stale socket, the two ways a start
// binds. The lock is held from the bind until the listener's Close, through one close-on-exec descriptor on the
// retained regular lock file. Close removes the socket file, gives the lock up, and leaves the lock file in place for
// the next start, which locks the same file. The same file is told by a reference descriptor the test holds on it.
func TestSocketLockHeldForListenerLifetime(t *testing.T) {
	tests := map[string]struct {
		// stale leaves a socket file at the path that no process listens on before the start.
		stale bool
	}{
		"success: a start on a free path holds the lock until Close":               {},
		"success: a start that reclaims a stale socket holds the lock until Close": {stale: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "gw.sock")
			lockPath := path + ".lock"
			if tc.stale {
				staleSocket(t, path)
				markOld(t, path)
			}
			ln, err := worker.ListenUnix(path)
			if err != nil {
				t.Fatalf("ListenUnix: %v", err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			fds := lockDescriptors(t, lockPath)
			got := lifetime{
				Held:        lockHeld(t, lockPath),
				Descriptors: len(fds),
				CloseOnExec: closeOnExec(t, fds),
				LockKind:    fileKind(lockPath),
				Reached:     reaches(t, path, ln),
			}
			ref := lockReference(t, lockPath)

			got.CloseResult = classifyClose(ln.Close())
			got.SocketAfterClose = fileKind(path)
			got.HeldAfterClose = lockHeld(t, lockPath)
			got.DescriptorsAfterClose = len(lockDescriptors(t, lockPath, ref))
			got.LockAfterClose = referenceState(t, lockPath, ref)
			next, err := worker.ListenUnix(path)
			if err != nil {
				t.Errorf("the next ListenUnix after Close: %v", err)
			} else {
				got.NextHeld = lockHeld(t, lockPath) && referenceState(t, lockPath, ref) == "unchanged"
				if err := next.Close(); err != nil {
					t.Error(err)
				}
			}
			want := lifetime{
				Held:             true,
				Descriptors:      1,
				CloseOnExec:      true,
				LockKind:         "regular file",
				Reached:          true,
				CloseResult:      "nil",
				SocketAfterClose: "absent",
				LockAfterClose:   "unchanged",
				NextHeld:         true,
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("lock of the listener ListenUnix returned (-want +got):\n%s", diff)
			}
		})
	}
}

// successor is the state of a second listener's lock after the first listener was closed again.
type successor struct {
	// Held reports that the socket lock was still held.
	Held bool
	// Descriptors counts this process's descriptors open on the lock file.
	Descriptors int
	// Reached reports that a connection to the path arrived at the second listener.
	Reached bool
	// Results counts the further Close results of the first listener by classifyClose.
	Results map[string]int
	// OldNumber describes what the descriptor number of the first listener's lock is open on after the further
	// closes, by numberState.
	OldNumber string
}

// numberState describes what descriptor fd is open on: closed, the lock file at lockPath, the file at sentinelPath,
// or another file.
func numberState(fd int, lockPath, sentinelPath string) string {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "closed: " + err.Error()
	}
	for name, path := range map[string]string{"the successor's lock": lockPath, "the sentinel": sentinelPath} {
		var named unix.Stat_t
		if unix.Lstat(path, &named) == nil && named.Dev == st.Dev && named.Ino == st.Ino {
			return name
		}
	}
	return "another file"
}

// occupyNumber opens the file at sentinelPath, close-on-exec, until the open lands on the free descriptor number fd,
// keeps that descriptor until the test ends and closes the others. An open takes the lowest free number, so it lands
// on fd once the lower free numbers are taken.
func occupyNumber(t *testing.T, fd int, sentinelPath string) {
	t.Helper()
	if err := os.WriteFile(sentinelPath, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	var extra []int
	defer func() {
		for _, other := range extra {
			_ = unix.Close(other)
		}
	}()
	for range 4096 {
		got, err := unix.Open(sentinelPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case got == fd:
			t.Cleanup(func() { _ = unix.Close(got) })
			return
		case got > fd:
			_ = unix.Close(got)
			t.Fatalf("descriptor %d was taken by another file before the sentinel could occupy it", fd)
		}
		extra = append(extra, got)
	}
	t.Fatalf("the sentinel did not land on descriptor %d", fd)
}

// TestSocketLockReleasedOnce closes a listener, starts a successor on the same path in the same process, and then
// closes the first listener again, repeatedly or from several goroutines at once. The lock is given up exactly once:
// a later Close neither unlocks nor closes a descriptor, which may by then be the successor's lock descriptor reusing
// the same number, so the successor keeps its lock and its socket. When the successor got another number, the test
// puts a sentinel file of its own on the old number, which a second unlock or close would hit instead, and the
// sentinel must stay open. Every later Close returns the result of the one real close, which succeeded: nil.
func TestSocketLockReleasedOnce(t *testing.T) {
	tests := map[string]struct {
		// closes is how many further Close calls the first listener gets.
		closes int
		// concurrent makes those calls at once from separate goroutines.
		concurrent bool
	}{
		"success: repeated closes leave the successor's lock alone":   {closes: 3},
		"success: concurrent closes leave the successor's lock alone": {closes: 8, concurrent: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "gw.sock")
			lockPath := path + ".lock"
			first, err := worker.ListenUnix(path)
			if err != nil {
				t.Fatalf("first ListenUnix: %v", err)
			}
			firstLock := lockDescriptors(t, lockPath)
			if len(firstLock) != 1 {
				t.Fatalf("the first listener has %d descriptors on its lock file, want 1: %v", len(firstLock), firstLock)
			}
			if err := first.Close(); err != nil {
				t.Fatalf("first Close: %v", err)
			}
			second, err := worker.ListenUnix(path)
			if err != nil {
				t.Fatalf("ListenUnix after the first listener closed: %v", err)
			}
			t.Cleanup(func() { _ = second.Close() })
			sentinelPath := filepath.Join(filepath.Dir(path), "sentinel")
			// The old number is the successor's lock descriptor, another descriptor of this process, or free; a free
			// number gets the sentinel. A second unlock or close of the first listener would hit whichever it is.
			wantOldNumber := "the successor's lock"
			if _, err := unix.FcntlInt(uintptr(firstLock[0]), unix.F_GETFD, 0); err != nil {
				occupyNumber(t, firstLock[0], sentinelPath)
				wantOldNumber = "the sentinel"
			} else if !slices.Contains(lockDescriptors(t, lockPath), firstLock[0]) {
				wantOldNumber = "another file"
			}

			results := make([]error, tc.closes)
			if tc.concurrent {
				start := make(chan struct{})
				var closing sync.WaitGroup
				for i := range tc.closes {
					closing.Go(func() {
						<-start
						results[i] = first.Close()
					})
				}
				close(start)
				closing.Wait()
			} else {
				for i := range tc.closes {
					results[i] = first.Close()
				}
			}
			got := successor{
				Held:        lockHeld(t, lockPath),
				Descriptors: len(lockDescriptors(t, lockPath)),
				Reached:     reaches(t, path, second),
				Results:     classifyAll(results),
				OldNumber:   numberState(firstLock[0], lockPath, sentinelPath),
			}
			want := successor{Held: true, Descriptors: 1, Reached: true, Results: map[string]int{"nil": tc.closes}, OldNumber: wantOldNumber}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("the successor after %d further closes of the first listener (-want +got):\n%s", tc.closes, diff)
			}
		})
	}
}

// failedStart is what a ListenUnix call that failed returned and left behind.
type failedStart struct {
	// Listener reports that ListenUnix returned a listener.
	Listener bool
	// AddressInUse reports that the error matches syscall.EADDRINUSE.
	AddressInUse bool
	// ErrorNamesLock reports that the error names the lock file.
	ErrorNamesLock bool
	// Socket describes the socket path after the call compared with before it.
	Socket string
	// LockKind is the kind of file at the lock path after the call.
	LockKind string
	// Held reports that the socket lock was held after the call; only a regular lock file is tested.
	Held bool
	// LockDescriptors counts this process's descriptors open on the lock path after the call.
	LockDescriptors int
}

// TestSocketStartFailureReleasesLock fails a start in each way it can fail after taking the lock, and in the way it
// fails before taking it when the lock path is not a regular file. Every failure gives the lock up before ListenUnix
// returns and leaves the lock file, whatever it is, in place; nothing at the socket path is touched.
func TestSocketStartFailureReleasesLock(t *testing.T) {
	tests := map[string]struct {
		// name is the socket's file name in the case's directory; empty means gw.sock.
		name string
		// occupy puts what the start must fail on at path or at its lock path.
		occupy func(t *testing.T, path string)
		// want is the expected result.
		want failedStart
	}{
		"error: a regular file at the path is kept": {
			occupy: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: failedStart{AddressInUse: true, Socket: "unchanged", LockKind: "regular file"},
		},
		"error: a live socket bound without the lock is kept": {
			occupy: func(t *testing.T, path string) { backlogListener(t, path, 16) },
			want:   failedStart{AddressInUse: true, Socket: "unchanged", LockKind: "regular file"},
		},
		"error: a path too long for a socket address is not bound": {
			name:   strings.Repeat("s", 120),
			occupy: func(*testing.T, string) {},
			want:   failedStart{Socket: "absent", LockKind: "regular file"},
		},
		"error: a named pipe at the lock path is not a lock": {
			occupy: func(t *testing.T, path string) {
				if err := unix.Mkfifo(path+".lock", 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: failedStart{ErrorNamesLock: true, Socket: "absent", LockKind: "named pipe"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			socket := tc.name
			if socket == "" {
				socket = "gw.sock"
			}
			path := filepath.Join(socketDir(t), socket)
			lockPath := path + ".lock"
			tc.occupy(t, path)
			var socketBefore fs.FileInfo
			if fileKind(path) != "absent" {
				socketBefore = markOld(t, path)
			}

			// A start that waits on the named pipe is released by a writer of the test's own.
			ln, err := startWithin(t, path, func() {
				if fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); err == nil {
					_ = unix.Close(fd)
				}
			})
			got := failedStart{
				Listener:        ln != nil,
				AddressInUse:    errors.Is(err, syscall.EADDRINUSE),
				ErrorNamesLock:  err != nil && strings.Contains(err.Error(), lockPath),
				Socket:          pathState(path, socketBefore),
				LockKind:        fileKind(lockPath),
				LockDescriptors: len(lockDescriptors(t, lockPath)),
			}
			if got.LockKind == "regular file" {
				got.Held = lockHeld(t, lockPath)
			}
			if ln != nil {
				_ = ln.Close()
			}
			if err == nil {
				t.Error("ListenUnix returned no error")
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ListenUnix returned error %v (-want +got):\n%s", err, diff)
			}
		})
	}
}

// scriptedStart is what a ListenUnix call returned and left behind when its lock attempts got scripted results.
type scriptedStart struct {
	// Attempts lists the operation of every lock attempt ListenUnix made, before it returned.
	Attempts []string
	// Listener reports that ListenUnix returned a listener.
	Listener bool
	// AddressInUse reports that the error matches syscall.EADDRINUSE.
	AddressInUse bool
	// Diagnostic is the error code the worker's bind diagnostic prints for the error.
	Diagnostic string
	// Cause reports that the error wraps the scripted failure that ended the attempts, when there was one.
	Cause bool
	// ErrorNamesLock reports that the error names the lock file; it is judged only for a failure that is not contention.
	ErrorNamesLock bool
	// Socket describes the socket path after the call compared with the stale socket, or names the kind of file there
	// when the path was free.
	Socket string
	// Held reports that the socket lock was held after the call.
	Held bool
	// LockDescriptors counts this process's descriptors open on the lock file after the call.
	LockDescriptors int
}

// lockOperation names the operation of a flock call.
func lockOperation(how int) string {
	switch how {
	case unix.LOCK_EX | unix.LOCK_NB:
		return "exclusive, nonblocking"
	case unix.LOCK_EX:
		return "exclusive, blocking"
	case unix.LOCK_SH | unix.LOCK_NB:
		return "shared, nonblocking"
	case unix.LOCK_SH:
		return "shared, blocking"
	}
	return "operation " + strconv.Itoa(how)
}

// TestSocketLockAttemptIsNonblocking scripts the results of the lock attempts, which a local lock file does not produce
// on its own. Every attempt is exclusive and does not wait. An interrupted attempt is repeated. Contention gives the
// "address in use" error without probing, binding or unlinking the socket path. Any other lock failure is returned
// naming the lock file. A failed start keeps no descriptor of the lock file.
func TestSocketLockAttemptIsNonblocking(t *testing.T) {
	const nonblocking = "exclusive, nonblocking"
	tests := map[string]struct {
		// script is the result of each lock attempt in turn; later attempts reach the real flock.
		script []error
		// cause is the scripted failure the error must wrap, or nil.
		cause error
		// stale leaves a socket file at the path that no process listens on before the start.
		stale bool
		want  scriptedStart
	}{
		"success: interrupted attempts are repeated until the lock is taken": {
			script: []error{unix.EINTR, unix.EINTR},
			want: scriptedStart{
				Attempts:        []string{nonblocking, nonblocking, nonblocking},
				Listener:        true,
				Socket:          "socket",
				Held:            true,
				LockDescriptors: 1,
			},
		},
		"error: contention after an interrupted attempt is an address in use and keeps a stale socket": {
			script: []error{unix.EINTR, unix.EWOULDBLOCK},
			stale:  true,
			want: scriptedStart{
				Attempts:     []string{nonblocking, nonblocking},
				AddressInUse: true,
				Diagnostic:   "EADDRINUSE",
				Socket:       "unchanged",
			},
		},
		"error: contention on a free path binds nothing": {
			script: []error{unix.EWOULDBLOCK},
			want: scriptedStart{
				Attempts:     []string{nonblocking},
				AddressInUse: true,
				Diagnostic:   "EADDRINUSE",
				Socket:       "absent",
			},
		},
		"error: a lock failure that is not contention names the lock file": {
			script: []error{unix.ENOLCK},
			cause:  unix.ENOLCK,
			stale:  true,
			want: scriptedStart{
				Attempts:       []string{nonblocking},
				Diagnostic:     "ENOLCK",
				Cause:          true,
				ErrorNamesLock: true,
				Socket:         "unchanged",
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "gw.sock")
			lockPath := path + ".lock"
			var socketBefore fs.FileInfo
			if tc.stale {
				staleSocket(t, path)
				socketBefore = markOld(t, path)
			}
			var (
				mu       sync.Mutex
				attempts []string
				starting = true
			)
			worker.ReplaceSocketLockCall(t, func(fd, how int) error {
				mu.Lock()
				defer mu.Unlock()
				if !starting || how&unix.LOCK_UN != 0 {
					return unix.Flock(fd, how)
				}
				attempts = append(attempts, lockOperation(how))
				if n := len(attempts); n <= len(tc.script) {
					return tc.script[n-1]
				}
				return unix.Flock(fd, how)
			})

			ln, err := worker.ListenUnix(path)
			mu.Lock()
			starting = false
			got := scriptedStart{Attempts: attempts}
			mu.Unlock()
			got.Listener = ln != nil
			got.AddressInUse = errors.Is(err, syscall.EADDRINUSE)
			got.Diagnostic = errnoName(err)
			if tc.cause != nil {
				got.Cause = errors.Is(err, tc.cause)
				got.ErrorNamesLock = err != nil && strings.Contains(err.Error(), lockPath)
			}
			if socketBefore != nil {
				got.Socket = pathState(path, socketBefore)
			} else {
				got.Socket = fileKind(path)
			}
			got.Held = lockHeld(t, lockPath)
			got.LockDescriptors = len(lockDescriptors(t, lockPath))
			if ln != nil {
				if err := ln.Close(); err != nil {
					t.Error(err)
				}
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ListenUnix returned error %v (-want +got):\n%s", err, diff)
			}
			if kind := fileKind(lockPath); kind != "regular file" {
				t.Errorf("the lock file is %s after the start, want the retained regular file", kind)
			}
		})
	}
}

// errInjectedClose is the failure a test listener's Close returns after it closed the real listener.
var errInjectedClose = errors.New("the test listener's close failed")

// countingListener is a real Unix listener whose Close is counted and, when fail is set, reports fail after the real
// close instead of the real result.
type countingListener struct {
	*net.UnixListener
	closes atomic.Int32
	fail   error
}

// Close closes the real listener and counts the call.
func (l *countingListener) Close() error {
	l.closes.Add(1)
	err := l.UnixListener.Close()
	if l.fail != nil {
		return l.fail
	}
	return err
}

// classifyClose names a Close or Accept result: nil, injected (it matches errInjectedClose), closed (it matches
// net.ErrClosed), or the error itself.
func classifyClose(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, errInjectedClose):
		return "injected"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	}
	return err.Error()
}

// classifyAll counts results by classifyClose.
func classifyAll(results []error) map[string]int {
	counts := make(map[string]int)
	for _, err := range results {
		counts[classifyClose(err)]++
	}
	return counts
}

// closeOrder is what the release of a locked listener saw and what its Close calls returned.
type closeOrder struct {
	// SameAddr reports that the wrapper's Addr is the listener's.
	SameAddr bool
	// Delegated reports that a connection to the path was accepted through the wrapper.
	Delegated bool
	// Unlocks counts the calls of the release.
	Unlocks int
	// ListenerCloses counts the Close calls the wrapped listener received.
	ListenerCloses int
	// SocketAtUnlock is the kind of file at the socket path when the release ran.
	SocketAtUnlock string
	// DialAtUnlock reports that a connection to the path succeeded when the release ran.
	DialAtUnlock bool
	// Results counts the results of the wrapper's Close calls by classifyClose.
	Results map[string]int
	// AcceptAfterClose classifies the error of an Accept after Close.
	AcceptAfterClose string
}

// TestLockedUnixListenerCloseOrder wraps a real Unix listener as ListenUnix wraps one whose lock it holds, with a release
// that records what it sees. Close closes the listener first, which removes its socket file and stops connections,
// and only then releases the lock. The listener is closed and the lock released exactly once across repeated and
// concurrent calls, and every call returns the result of that one close: nil after a successful close, the failure
// after a failed one. Accept and Addr are the listener's.
func TestLockedUnixListenerCloseOrder(t *testing.T) {
	tests := map[string]struct {
		// closes is how many Close calls the wrapper gets.
		closes int
		// concurrent makes those calls at once from separate goroutines.
		concurrent bool
		// fail makes the wrapped listener's close report this failure.
		fail error
		// results is the expected count of Close results by classifyClose.
		results map[string]int
	}{
		"success: one close releases after the socket is gone": {
			closes:  1,
			results: map[string]int{"nil": 1},
		},
		"success: repeated closes return the one close's nil and release once": {
			closes:  4,
			results: map[string]int{"nil": 4},
		},
		"success: concurrent closes return the one close's nil and release once": {
			closes:     8,
			concurrent: true,
			results:    map[string]int{"nil": 8},
		},
		"error: a failed close is returned by every repeated Close and still releases": {
			closes:  3,
			fail:    errInjectedClose,
			results: map[string]int{"injected": 3},
		},
		"error: a failed close is returned by every concurrent Close and still releases": {
			closes:     8,
			concurrent: true,
			fail:       errInjectedClose,
			results:    map[string]int{"injected": 8},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "gw.sock")
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			inner := &countingListener{UnixListener: ln, fail: tc.fail}
			var (
				mu  sync.Mutex
				got closeOrder
			)
			unlock := func() {
				kind := fileKind(path)
				var dialer net.Dialer
				conn, dialErr := dialer.DialContext(t.Context(), "unix", path)
				if dialErr == nil {
					_ = conn.Close()
				}
				mu.Lock()
				defer mu.Unlock()
				got.Unlocks++
				if got.Unlocks == 1 {
					got.SocketAtUnlock, got.DialAtUnlock = kind, dialErr == nil
				}
			}
			wrapped := noPanic(t, "the locked listener", func() net.Listener { return worker.LockedUnixListener(inner, unlock) })

			got.SameAddr = wrapped.Addr() != nil && wrapped.Addr().Network() == ln.Addr().Network() && wrapped.Addr().String() == ln.Addr().String()
			got.Delegated = reaches(t, path, wrapped)
			results := make([]error, tc.closes)
			if tc.concurrent {
				start := make(chan struct{})
				var closing sync.WaitGroup
				for i := range tc.closes {
					closing.Go(func() {
						<-start
						results[i] = wrapped.Close()
					})
				}
				close(start)
				closing.Wait()
			} else {
				for i := range tc.closes {
					results[i] = wrapped.Close()
				}
			}
			_, acceptErr := wrapped.Accept()

			mu.Lock()
			defer mu.Unlock()
			got.ListenerCloses = int(inner.closes.Load())
			got.Results = classifyAll(results)
			got.AcceptAfterClose = classifyClose(acceptErr)
			want := closeOrder{
				SameAddr:         true,
				Delegated:        true,
				Unlocks:          1,
				ListenerCloses:   1,
				SocketAtUnlock:   "absent",
				Results:          tc.results,
				AcceptAfterClose: "closed",
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("Close results %v (-want +got):\n%s", results, diff)
			}
		})
	}
}

// closeReturn is what one Close call returned and whether the release had finished when it returned.
type closeReturn struct {
	err      error
	released bool
}

// secondClose is the Close that the release-wait test makes while the first Close holds the release open. Its name
// identifies its goroutine in a dump of every goroutine.
func secondClose(ln net.Listener, released *atomic.Bool, result chan<- closeReturn) {
	err := ln.Close()
	result <- closeReturn{err: err, released: released.Load()}
}

// goroutineParked reports whether the goroutine whose stack holds a call of the package test function fn is parked,
// that is in a waiting state rather than running, runnable or in a system call.
func goroutineParked(fn string) bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for block := range strings.SplitSeq(string(buf), "\n\n") {
		header, frames, _ := strings.Cut(block, "\n")
		if !strings.Contains(frames, "worker_test."+fn+"(") {
			continue
		}
		_, state, _ := strings.Cut(header, "[")
		state, _, _ = strings.Cut(state, "]")
		state, _, _ = strings.Cut(state, ",")
		switch {
		case state == "running", state == "runnable", state == "syscall", state == "preempted", strings.HasPrefix(state, "GC "):
			return false
		}
		return true
	}
	return false
}

// awaitReturnedOrParked waits until the second Close has returned or its goroutine is parked inside the call. A
// parked goroutine cannot return before something wakes it, so the test may then let the release finish and judge
// what each Close saw when it returned. The watchdog only aborts a call that neither returns nor parks.
func awaitReturnedOrParked(t *testing.T, result chan closeReturn) {
	t.Helper()
	deadline := time.Now().Add(lifetimeWatchdog)
	for len(result) == 0 && !goroutineParked("secondClose") {
		if time.Now().After(deadline) {
			t.Fatalf("the second Close neither returned nor waited within %v", lifetimeWatchdog)
		}
		runtime.Gosched()
	}
}

// releaseWait is what two overlapping Close calls of one locked listener returned and saw.
type releaseWait struct {
	// FirstReleased reports that the release had finished when the first Close returned.
	FirstReleased bool
	// SecondReleased reports that the release had finished when the second Close returned.
	SecondReleased bool
	// Unlocks counts the calls of the release.
	Unlocks int
	// ListenerCloses counts the Close calls the wrapped listener received.
	ListenerCloses int
	// Results counts the results of the two Close calls by classifyClose.
	Results map[string]int
}

// TestLockedUnixListenerCloseWaitsForRelease holds the release of a locked listener open at a barrier inside it, from
// the first Close, and makes a second Close meanwhile. No Close returns before the release has finished: the second
// one waits for it instead of returning early with the retained result, so a caller that sees any Close return may
// rely on the lock being free. Both return the result of the one close of the listener.
func TestLockedUnixListenerCloseWaitsForRelease(t *testing.T) {
	tests := map[string]struct {
		// fail makes the wrapped listener's close report this failure.
		fail error
		// results is the expected count of the two Close results by classifyClose.
		results map[string]int
	}{
		"success: a second Close waits for the release the first one is making": {
			results: map[string]int{"nil": 2},
		},
		"error: a second Close waits for the release and returns the failed close": {
			fail:    errInjectedClose,
			results: map[string]int{"injected": 2},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "gw.sock")
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			inner := &countingListener{UnixListener: ln, fail: tc.fail}
			entered, gate := make(chan struct{}), make(chan struct{})
			openGate := sync.OnceFunc(func() { close(gate) })
			// A case that stops early still lets a waiting release finish.
			t.Cleanup(openGate)
			var (
				released atomic.Bool
				unlocks  atomic.Int32
			)
			unlock := func() {
				if unlocks.Add(1) == 1 {
					close(entered)
				}
				<-gate
				released.Store(true)
			}
			wrapped := noPanic(t, "the locked listener", func() net.Listener { return worker.LockedUnixListener(inner, unlock) })

			first := make(chan closeReturn, 1)
			go func() {
				err := wrapped.Close()
				first <- closeReturn{err: err, released: released.Load()}
			}()
			select {
			case <-entered:
			case <-time.After(lifetimeWatchdog):
				t.Fatalf("the first Close did not start the release within %v", lifetimeWatchdog)
			}
			second := make(chan closeReturn, 1)
			go secondClose(wrapped, &released, second)
			awaitReturnedOrParked(t, second)
			openGate()

			var returns [2]closeReturn
			for i, ch := range []chan closeReturn{first, second} {
				select {
				case returns[i] = <-ch:
				case <-time.After(lifetimeWatchdog):
					t.Fatalf("Close %d did not return within %v after the release was let through", i+1, lifetimeWatchdog)
				}
			}
			got := releaseWait{
				FirstReleased:  returns[0].released,
				SecondReleased: returns[1].released,
				Unlocks:        int(unlocks.Load()),
				ListenerCloses: int(inner.closes.Load()),
				Results:        classifyAll([]error{returns[0].err, returns[1].err}),
			}
			want := releaseWait{FirstReleased: true, SecondReleased: true, Unlocks: 1, ListenerCloses: 1, Results: tc.results}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("two overlapping Close calls (-want +got):\n%s", diff)
			}
		})
	}
}

// restart is what the Close calls of a listener returned and whether a new start on its path succeeded after each.
type restart struct {
	// Results counts the Close results by classifyClose.
	Results map[string]int
	// Restarts counts the starts after a Close that returned a listener.
	Restarts int
}

// TestSocketFreeWhenAnyCloseReturns closes a listener ListenUnix returned, repeatedly or from several goroutines at
// once, and starts the socket again on the same path right after each Close returns, one start at a time. Whichever
// Close returns, the lock is already free and the socket file gone, so every such start succeeds at once, and every
// Close returns nil, the result of the one successful close.
func TestSocketFreeWhenAnyCloseReturns(t *testing.T) {
	tests := map[string]struct {
		// closes is how many Close calls the listener gets.
		closes int
		// concurrent makes those calls at once from separate goroutines.
		concurrent bool
	}{
		"success: a start after each repeated Close succeeds":   {closes: 3},
		"success: a start after each concurrent Close succeeds": {closes: 8, concurrent: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "gw.sock")
			ln, err := worker.ListenUnix(path)
			if err != nil {
				t.Fatalf("ListenUnix: %v", err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			var (
				mu       sync.Mutex
				results  []error
				restarts int
				failures []error
			)
			closeAndRestart := func() {
				err := ln.Close()
				mu.Lock()
				defer mu.Unlock()
				results = append(results, err)
				// Called from the closing goroutines, so no helper here may stop the test.
				next, err := worker.ListenUnix(path)
				if err != nil {
					failures = append(failures, err)
					return
				}
				restarts++
				if err := next.Close(); err != nil {
					failures = append(failures, err)
				}
			}
			if tc.concurrent {
				start := make(chan struct{})
				var closing sync.WaitGroup
				for range tc.closes {
					closing.Go(func() {
						<-start
						closeAndRestart()
					})
				}
				close(start)
				closing.Wait()
			} else {
				for range tc.closes {
					closeAndRestart()
				}
			}
			mu.Lock()
			defer mu.Unlock()
			got := restart{Results: classifyAll(results), Restarts: restarts}
			want := restart{Results: map[string]int{"nil": tc.closes}, Restarts: tc.closes}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("starts after Close failed with %v (-want +got):\n%s", failures, diff)
			}
		})
	}
}

// TestListenUnixReturnsTheLockedListener checks that both ways a pathname start binds, on a free path and over a
// stale socket it reclaims, return the same kind of listener that the close-order tests examine, so that what those
// tests decide holds for every listener ListenUnix returns with the lock.
func TestListenUnixReturnsTheLockedListener(t *testing.T) {
	tests := map[string]struct {
		// stale leaves a socket file at the path that no process listens on before the start.
		stale bool
	}{
		"success: a start on a free path returns the locked listener":               {},
		"success: a start that reclaims a stale socket returns the locked listener": {stale: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := socketDir(t)
			path := filepath.Join(dir, "gw.sock")
			var stale fs.FileInfo
			if tc.stale {
				staleSocket(t, path)
				stale = markOld(t, path)
			}
			plain, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "plain.sock"), Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = plain.Close() })
			wrapped := noPanic(t, "the locked listener", func() net.Listener { return worker.LockedUnixListener(plain, func() {}) })
			ln, err := worker.ListenUnix(path)
			if err != nil {
				t.Fatalf("ListenUnix: %v", err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			if state := pathState(path, stale); tc.stale && state == "unchanged" {
				t.Errorf("the stale socket at %s was not reclaimed: it is %s", path, state)
			}
			if diff := gocmp.Diff(reflect.TypeOf(wrapped).String(), reflect.TypeOf(ln).String()); diff != "" {
				t.Errorf("dynamic type of the listener ListenUnix returned (-locked listener +ListenUnix):\n%s", diff)
			}
		})
	}
}

// deadHolder is the state of the path and its lock around the end of a holder that exited without closing.
type deadHolder struct {
	// HeldByHolder reports that the socket lock was held while the holder lived.
	HeldByHolder bool
	// HeldAfterExit reports that the socket lock was still held after the holder exited.
	HeldAfterExit bool
	// SocketAfterExit is the kind of file the holder left at the socket path.
	SocketAfterExit string
	// Started reports that a new start on the path succeeded.
	Started bool
	// Lock describes the lock file after the new start compared with the file a reference descriptor held open while
	// the holder lived and after it exited.
	Lock string
	// Held reports that the new start held the lock.
	Held bool
	// Reached reports that a connection to the path arrived at the new listener.
	Reached bool
}

// TestSocketLockOfDeadHolderIsReused starts the socket in a helper process that exits without closing, as a worker
// that dies does. The kernel gives up a dead holder's lock, so the lock file it leaves is reused as it is, and the
// stale socket is reclaimed by the next start.
func TestSocketLockOfDeadHolderIsReused(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	tests := map[string]struct{}{
		"success: the lock file of a holder that died is locked again": {},
	}
	for name := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "gw.sock")
			lockPath := path + ".lock"
			_, result, join := startInOwnDirectory(t, env, path)
			if !strings.HasPrefix(result, "owner ") {
				t.Fatalf("the holder on a free path: %q, want an owner", result)
			}
			got := deadHolder{HeldByHolder: lockHeld(t, lockPath)}
			ref := lockReference(t, lockPath)
			if err := join(); err != nil {
				t.Fatal(err)
			}
			got.HeldAfterExit = lockHeld(t, lockPath)
			got.SocketAfterExit = fileKind(path)

			ln, err := worker.ListenUnix(path)
			got.Started = err == nil
			if err != nil {
				t.Errorf("ListenUnix after the holder died: %v", err)
			} else {
				got.Lock = referenceState(t, lockPath, ref)
				got.Held = lockHeld(t, lockPath)
				got.Reached = reaches(t, path, ln)
				if err := ln.Close(); err != nil {
					t.Error(err)
				}
			}
			want := deadHolder{
				HeldByHolder:    true,
				SocketAfterExit: "socket",
				Started:         true,
				Lock:            "unchanged",
				Held:            true,
				Reached:         true,
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("start after the holder died (-want +got):\n%s", diff)
			}
		})
	}
}

// parkedProbe runs wait, the join under test, in a goroutine of the package test that goroutineParked can find by this
// function's name.
func parkedProbe(wait func()) {
	wait()
}

// TestGoroutineParkedRecognizesEveryJoin checks the reading of goroutine states that the release-wait test relies
// on: a goroutine that waits inside a call on a mutex, a sync.Once, a channel, a select, a WaitGroup or a Cond counts
// as parked, so the release-wait test accepts every such way of making a second Close wait, and one that keeps
// running does not.
func TestGoroutineParkedRecognizesEveryJoin(t *testing.T) {
	tests := map[string]struct {
		// park returns a wait that blocks until release is called, and release.
		park func() (wait, release func())
		// parked is whether the probe must count as parked while it waits.
		parked bool
	}{
		"success: a wait on a held mutex is parked": {
			park: func() (func(), func()) {
				var (
					mu       sync.Mutex
					acquired int
				)
				mu.Lock()
				return func() {
					mu.Lock()
					acquired++
					mu.Unlock()
				}, mu.Unlock
			},
			parked: true,
		},
		"success: a second call of a sync.Once in progress is parked": {
			park: func() (func(), func()) {
				var once sync.Once
				inside, gate := make(chan struct{}), make(chan struct{})
				go once.Do(func() { close(inside); <-gate })
				<-inside
				return func() { once.Do(func() {}) }, func() { close(gate) }
			},
			parked: true,
		},
		"success: a channel receive is parked": {
			park: func() (func(), func()) {
				ch := make(chan struct{})
				return func() { <-ch }, func() { close(ch) }
			},
			parked: true,
		},
		"success: a select is parked": {
			park: func() (func(), func()) {
				a, b := make(chan struct{}), make(chan struct{})
				return func() {
					select {
					case <-a:
					case <-b:
					}
				}, func() { close(a) }
			},
			parked: true,
		},
		"success: a WaitGroup wait is parked": {
			park: func() (func(), func()) {
				var wg sync.WaitGroup
				wg.Add(1)
				return wg.Wait, wg.Done
			},
			parked: true,
		},
		"success: a Cond wait is parked": {
			park: func() (func(), func()) {
				var mu sync.Mutex
				cond := sync.NewCond(&mu)
				done := false
				return func() {
						mu.Lock()
						for !done {
							cond.Wait()
						}
						mu.Unlock()
					}, func() {
						mu.Lock()
						done = true
						mu.Unlock()
						cond.Broadcast()
					}
			},
			parked: true,
		},
		"error: a goroutine that keeps running is not parked": {
			park: func() (func(), func()) {
				var stop atomic.Bool
				return func() {
					for !stop.Load() {
						runtime.Gosched()
					}
				}, func() { stop.Store(true) }
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			wait, release := tc.park()
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				parkedProbe(wait)
			}()
			got := false
			if tc.parked {
				deadline := time.Now().Add(lifetimeWatchdog)
				for !got && time.Now().Before(deadline) {
					got = goroutineParked("parkedProbe")
					runtime.Gosched()
				}
			} else {
				// A running goroutine is sampled many times; it must never read as parked.
				for range 200 {
					if goroutineParked("parkedProbe") {
						got = true
						break
					}
					runtime.Gosched()
				}
			}
			release()
			<-finished
			if got != tc.parked {
				t.Errorf("goroutineParked = %t while the probe waits, want %t", got, tc.parked)
			}
		})
	}
}
