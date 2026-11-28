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

package workerlink_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// relistenStage tries to build a once-connected local stream socket whose peer ended and that then listens. Every
// step before the final listen is a prerequisite: when one fails, the stage fails the row as "cannot stage", because
// such a failure says nothing about whether the state can exist. It returns the listening socket, or the error the
// final listen was refused with; every other descriptor it opened has exactly one owner that closes it.
type relistenStage func(t *testing.T) (fd int, listenErr error)

// socketFD opens a close-on-exec local stream socket; the caller closes it or hands it on.
func socketFD() (int, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(fd)
	return fd, nil
}

// relistenPathBoundClient stages, step by step, the state of the macOS row: a client bound to a path of its own
// connects to a listener, the accepted end and the listener are closed, and then the client is asked to listen.
//
// The listener's only owner is an *os.File: the early close and the test's cleanup both go through it, so the cleanup
// can never close a descriptor number that was reused after the early close, and a failed close is never repeated.
func relistenPathBoundClient(t *testing.T) (int, error) {
	dir := localSocketDir(t)
	server, err := socketFD()
	if err != nil {
		t.Fatalf("cannot stage: socket for the listener: %v", err)
	}
	listener := os.NewFile(uintptr(server), "listener")
	t.Cleanup(func() { _ = listener.Close() })
	serverAddr := &unix.SockaddrUnix{Name: filepath.Join(dir, "l")}
	if err := unix.Bind(server, serverAddr); err != nil {
		t.Fatalf("cannot stage: bind the listener: %v", err)
	}
	if err := unix.Listen(server, 1); err != nil {
		t.Fatalf("cannot stage: listen on the listener: %v", err)
	}
	client, err := socketFD()
	if err != nil {
		t.Fatalf("cannot stage: socket for the client: %v", err)
	}
	fail := func(step string, err error) {
		_ = unix.Close(client)
		t.Fatalf("cannot stage: %s: %v", step, err)
	}
	if err := unix.Bind(client, &unix.SockaddrUnix{Name: filepath.Join(dir, "c")}); err != nil {
		fail("bind the client to its own path", err)
	}
	if err := unix.Connect(client, serverAddr); err != nil {
		fail("connect the client", err)
	}
	accepted, _, err := unix.Accept(server)
	if err != nil {
		fail("accept the client's connection", err)
	}
	if err := unix.Close(accepted); err != nil {
		fail("close the accepted end", err)
	}
	if err := listener.Close(); err != nil {
		fail("close the listener", err)
	}
	if err := unix.Listen(client, 1); err != nil {
		_ = unix.Close(client)
		return -1, err
	}
	return client, nil
}

// relistenSocketPair stages the same with an end of a connected socket pair, which is not bound to any address: the
// other end is closed, and then the remaining end is asked to listen.
func relistenSocketPair(t *testing.T) (int, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("cannot stage: socketpair: %v", err)
	}
	unix.CloseOnExec(fds[0])
	if err := unix.Close(fds[1]); err != nil {
		_ = unix.Close(fds[0])
		t.Fatalf("cannot stage: close the other end of the pair: %v", err)
	}
	if err := unix.Listen(fds[0], 1); err != nil {
		_ = unix.Close(fds[0])
		return -1, err
	}
	return fds[0], nil
}

// TestImportDrainChannelOnceConnectedListenerLinux characterises, on Linux, the state the macOS listener row stages:
// a local stream socket that was connected, whose peer ended, and that is then made a listener. Every step before
// the final listen must succeed, or the row fails as "cannot stage". If the final listen is refused with EINVAL, the
// answer measured on Linux, the state cannot exist there and the row passes, logging the refusal: that is the
// finding, and the Linux import then needs no listening check for it. A refusal with any other error fails the row.
// If the listen succeeds, the import must refuse the socket and consume the file it was given, and the row logs what
// the peer lookup answered for it.
func TestImportDrainChannelOnceConnectedListenerLinux(t *testing.T) {
	tests := map[string]relistenStage{
		"error: a path-bound client whose peer ended and that then listens is refused or cannot exist": relistenPathBoundClient,
		"error: a socket pair end whose peer ended and that then listens is refused or cannot exist":   relistenSocketPair,
	}
	for name, stage := range tests {
		t.Run(name, func(t *testing.T) {
			fd, listenErr := stage(t)
			if listenErr != nil {
				if !errors.Is(listenErr, unix.EINVAL) {
					t.Fatalf("listen on the once-connected socket whose peer ended was refused with %v; want EINVAL, "+
						"the refusal measured on Linux, or success", listenErr)
				}
				t.Logf("the state cannot be built on this system: listen on the once-connected socket was refused with %v", listenErr)
				return
			}
			f := os.NewFile(uintptr(fd), "once-connected listener")
			t.Cleanup(func() { _ = f.Close() })
			sa, peerErr := unix.Getpeername(fd)
			t.Logf("the state was built; the peer lookup of the listening socket answered %s, %v", describePeer(sa), peerErr)
			c, err := workerlink.ImportDrainChannel(f)
			if c != nil {
				_ = c.Close()
				t.Error("ImportDrainChannel returned a connection for a once-connected socket that is now a listener")
			}
			if err == nil {
				t.Fatal("ImportDrainChannel accepted a once-connected socket that is now a listener")
			}
			checkConsumed(t, f)
		})
	}
}

// describePeer renders a peer address for the log.
func describePeer(sa unix.Sockaddr) string {
	if u, ok := sa.(*unix.SockaddrUnix); ok {
		return fmt.Sprintf("a local address %q", u.Name)
	}
	return fmt.Sprintf("%#v", sa)
}
