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

//go:build linux || darwin

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

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// peerEndedFixture builds an inherited drain descriptor whose peer, if it has one, has already ended. queued is the
// encoded drain line the peer left on the channel before it ended, empty when it left nothing.
type peerEndedFixture func(t *testing.T) (file *os.File, queued *workerlink.DrainMessage)

// endedLocalPair is the child end of a connected local stream pair whose parent end was closed before the import,
// after writing queued, when it is not nil, as one drain line.
func endedLocalPair(queued *workerlink.DrainMessage) peerEndedFixture {
	return func(t *testing.T) (*os.File, *workerlink.DrainMessage) {
		child, parent := socketPair(t, unix.SOCK_STREAM)
		if queued != nil {
			if err := workerlink.EncodeDrain(parent, *queued); err != nil {
				t.Fatalf("test infrastructure: queue the drain line on the parent end: %v", err)
			}
		}
		if err := parent.Close(); err != nil {
			t.Fatalf("test infrastructure: close the parent end: %v", err)
		}
		return child, queued
	}
}

// localSocketDir is a fresh short directory for a local socket path: a test's own temporary directory can exceed the
// length a local socket address holds on macOS.
func localSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wl")
	if err != nil {
		t.Fatalf("test infrastructure: MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// endedLocalConnect is a local stream socket connected to a listening path whose accepted end, and then the
// listener, were closed before the import: connected the way a client connects, not made as a pair.
func endedLocalConnect(t *testing.T) (*os.File, *workerlink.DrainMessage) {
	l, err := new(net.ListenConfig).Listen(t.Context(), "unix", filepath.Join(localSocketDir(t), "s"))
	if err != nil {
		t.Fatalf("test infrastructure: listen unix: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	c, err := new(net.Dialer).DialContext(t.Context(), "unix", l.Addr().String())
	if err != nil {
		t.Fatalf("test infrastructure: dial unix: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	accepted := acceptWithin(t, l)
	if err := accepted.Close(); err != nil {
		t.Fatalf("test infrastructure: close the accepted end: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("test infrastructure: close the listener: %v", err)
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		t.Fatalf("test infrastructure: dial unix returned %T", c)
	}
	f, err := uc.File()
	if err != nil {
		t.Fatalf("test infrastructure: File: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, nil
}

// endedTCPConnect is the client end of a loopback TCP connection whose accepted end was closed before the import:
// connected and stream, but not local.
func endedTCPConnect(t *testing.T) (*os.File, *workerlink.DrainMessage) {
	l := listenTCP(t, "tcp4", "127.0.0.1:0")
	c, err := realDial(t.Context(), "tcp4", l.Addr().String())
	if err != nil {
		t.Fatalf("test infrastructure: dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := acceptOne(t, l).Close(); err != nil {
		t.Fatalf("test infrastructure: close the accepted end: %v", err)
	}
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("test infrastructure: dial returned %T, want TCP", c)
	}
	f, err := tcp.File()
	if err != nil {
		t.Fatalf("test infrastructure: File: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, nil
}

// endedDatagramPair is one end of a local datagram pair whose other end was closed before the import.
func endedDatagramPair(t *testing.T) (*os.File, *workerlink.DrainMessage) {
	a, b := socketPair(t, unix.SOCK_DGRAM)
	if err := b.Close(); err != nil {
		t.Fatalf("test infrastructure: close the other end: %v", err)
	}
	return a, nil
}

// endedPipe is the read end of a pipe whose write end was closed before the import.
func endedPipe(t *testing.T) (*os.File, *workerlink.DrainMessage) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("test infrastructure: Pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := w.Close(); err != nil {
		t.Fatalf("test infrastructure: close the write end: %v", err)
	}
	return r, nil
}

// neverConnected is a local stream socket that was never connected; it has no peer to have ended.
func neverConnected(t *testing.T) (*os.File, *workerlink.DrainMessage) {
	f, _ := unconnectedUnixStreamFile(t)
	return f, nil
}

// listeningLocal is a listening local stream socket.
func listeningLocal(t *testing.T) (*os.File, *workerlink.DrainMessage) {
	f, _ := unixListenerFile(t)
	return f, nil
}

// TestImportDrainChannelPeerEnded pins the import of a drain channel whose parent end had already ended when the
// worker imported it, the same way on every system. A healthy connected local stream socket whose peer ended, as a
// pair end or connected to a path, is imported. The bytes its peer queued before it ended stay readable: one row
// queues one drain line, and the worker must read it whole. After them the worker reads the end of input. A
// descriptor of another kind is refused whatever its peer did: one never connected, a listener, a datagram pair, a
// loopback TCP connection or a pipe. Accepted or refused, the inherited file is consumed.
func TestImportDrainChannelPeerEnded(t *testing.T) {
	retired := &workerlink.DrainMessage{Type: workerlink.Retired, Generation: "generation-1"}
	tests := map[string]struct {
		fixture peerEndedFixture
		// accept is whether the import must return a connection.
		accept bool
	}{
		"success: a connected local stream pair whose parent end was closed is imported and reads the end": {
			fixture: endedLocalPair(nil),
			accept:  true,
		},
		"success: a connected local stream pair whose parent queued an acknowledgment and closed reads it, then the end": {
			fixture: endedLocalPair(retired),
			accept:  true,
		},
		"success: a local stream socket connected to a path whose accepted end was closed is imported and reads the end": {
			fixture: endedLocalConnect,
			accept:  true,
		},
		"error: a local stream socket that was never connected is refused": {
			fixture: neverConnected,
		},
		"error: a listening local stream socket is refused": {
			fixture: listeningLocal,
		},
		"error: a local datagram pair whose other end was closed is refused": {
			fixture: endedDatagramPair,
		},
		"error: a loopback TCP connection whose accepted end was closed is refused as not local": {
			fixture: endedTCPConnect,
		},
		"error: a pipe whose write end was closed is refused": {
			fixture: endedPipe,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, queued := tt.fixture(t)
			c, err := workerlink.ImportDrainChannel(f)
			if c != nil {
				t.Cleanup(func() { _ = c.Close() })
			}
			checkConsumed(t, f)
			if !tt.accept {
				if c != nil {
					t.Error("ImportDrainChannel returned a connection for a refused descriptor")
				}
				if err == nil {
					t.Fatal("ImportDrainChannel accepted the descriptor")
				}
				return
			}
			if err != nil {
				t.Fatalf("ImportDrainChannel refused a connected local stream socket whose peer had ended: %v", err)
			}
			if c == nil {
				t.Fatal("ImportDrainChannel returned neither a connection nor an error")
			}
			if err := c.SetReadDeadline(time.Now().Add(watchdog)); err != nil {
				t.Fatalf("SetReadDeadline on the imported channel: %v", err)
			}
			r := bufio.NewReader(c)
			if queued != nil {
				got, err := workerlink.DecodeDrain(r)
				if err != nil {
					t.Fatalf("the drain line the parent queued before it ended: %v", err)
				}
				if diff := gocmp.Diff(*queued, got); diff != "" {
					t.Errorf("the drain line the parent queued before it ended (-want +got):\n%s", diff)
				}
			}
			if got, err := workerlink.DecodeDrain(r); !errors.Is(err, io.EOF) {
				t.Errorf("after everything the parent left: DecodeDrain = %+v, %v; want the end of the channel, io.EOF", got, err)
			}
		})
	}
}
