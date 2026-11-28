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
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// endedPathBoundClient stages, with raw system calls, a local stream socket bound to a path of its own that connected
// to a listener. The client's peer is the end the listener accepted, and that accepted end is closed; the listener
// itself stays open until the test's cleanup, and it is not the client's peer. With listen set, the client socket is
// then put into the listening state itself, which macOS permits once its peer is gone. It returns the client socket as
// a file the test owns. A step that does not behave as staged fails the row as test infrastructure that cannot stage
// it.
func endedPathBoundClient(t *testing.T, listen bool) *os.File {
	t.Helper()
	dir := localSocketDir(t)
	socket := func(what string) int {
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatalf("cannot stage: socket for the %s: %v", what, err)
		}
		unix.CloseOnExec(fd)
		return fd
	}
	server := socket("listener")
	// The listener is closed only here, once, by the cleanup: no step closes it early, so its descriptor number cannot
	// have been reused by another owner when the cleanup runs.
	t.Cleanup(func() { _ = unix.Close(server) })
	serverAddr := &unix.SockaddrUnix{Name: filepath.Join(dir, "l")}
	if err := unix.Bind(server, serverAddr); err != nil {
		t.Fatalf("cannot stage: bind the listener: %v", err)
	}
	if err := unix.Listen(server, 1); err != nil {
		t.Fatalf("cannot stage: listen on the listener: %v", err)
	}
	client := socket("client")
	f := os.NewFile(uintptr(client), "path-bound client")
	t.Cleanup(func() { _ = f.Close() })
	if err := unix.Bind(client, &unix.SockaddrUnix{Name: filepath.Join(dir, "c")}); err != nil {
		t.Fatalf("cannot stage: bind the client to its own path: %v", err)
	}
	if err := unix.Connect(client, serverAddr); err != nil {
		t.Fatalf("cannot stage: connect the client: %v", err)
	}
	// The connection is already queued, so this accept on the blocking listener returns at once.
	accepted, _, err := unix.Accept(server)
	if err != nil {
		t.Fatalf("cannot stage: accept the client's connection: %v", err)
	}
	// Closing the accepted end ends the client's peer; the listener stays open.
	if err := unix.Close(accepted); err != nil {
		t.Fatalf("cannot stage: close the accepted end, the client's peer: %v", err)
	}
	if listen {
		if err := unix.Listen(client, 1); err != nil {
			t.Fatalf("cannot stage: listen on the client socket whose peer ended: %v; this system does not let a "+
				"once-connected socket become a listener", err)
		}
	}
	// The peer lookup the drain import would see: it fails with EINVAL on macOS once the accepted end has closed, with
	// or without the listen, which is why a check of that lookup alone cannot tell the two rows apart.
	sa, err := unix.Getpeername(client)
	if !errors.Is(err, unix.EINVAL) {
		t.Fatalf("cannot stage: the peer lookup of the staged socket gave %v, %v; want EINVAL, the answer a fix must "+
			"not take for a usable drain channel on its own", sa, err)
	}
	return f
}

// readsEnd fails the test unless the next drain read on c reports the end of the channel.
func readsEnd(t *testing.T, c net.Conn) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(watchdog)); err != nil {
		t.Fatalf("SetReadDeadline on the imported channel: %v", err)
	}
	if got, err := workerlink.DecodeDrain(bufio.NewReader(c)); !errors.Is(err, io.EOF) {
		t.Errorf("DecodeDrain = %+v, %v; want the end of the channel, io.EOF", got, err)
	}
}

// TestImportDrainChannelOnceConnectedListener pins, on macOS, that an import which tolerates the peer lookup's EINVAL
// for a channel whose peer has ended still checks the rest of the kind: a local stream socket that was connected and
// whose peer ended, and that then became a listener, is not a drain channel and is refused, although its peer lookup
// answers exactly as that of an ended channel does. The same socket without the listen is the control: it is a
// connected local stream socket whose peer ended, and it is imported and reads the end.
//
// Neither state can be staged this way on Linux, where listen on a once-connected local socket fails and the peer
// lookup of an ended channel succeeds.
func TestImportDrainChannelOnceConnectedListener(t *testing.T) {
	tests := map[string]struct {
		listen bool
		accept bool
	}{
		"success: a path-bound client whose peer ended is imported and reads the end": {
			accept: true,
		},
		"error: a path-bound client whose peer ended and that then listens is refused": {
			listen: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := endedPathBoundClient(t, tt.listen)
			c, err := workerlink.ImportDrainChannel(f)
			if c != nil {
				t.Cleanup(func() { _ = c.Close() })
			}
			checkConsumed(t, f)
			if !tt.accept {
				if c != nil {
					t.Error("ImportDrainChannel returned a connection for a once-connected socket that is now a listener")
				}
				if err == nil {
					t.Fatal("ImportDrainChannel accepted a once-connected socket that is now a listener")
				}
				return
			}
			if err != nil || c == nil {
				t.Fatalf("ImportDrainChannel = %v, %v; want the connected local stream socket whose peer ended", c, err)
			}
			readsEnd(t, c)
		})
	}
}
