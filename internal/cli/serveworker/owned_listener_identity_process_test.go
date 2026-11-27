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

//go:build process && (linux || darwin)

package serveworker_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// listenerIdentity is what identifies a listening socket across its descriptors: the inode and device of the socket
// and its bound port.
type listenerIdentity struct {
	Dev, Ino uint64
	Port     int
}

// identityOf reads ln's identity through its descriptor, and on Linux whether the socket still listens.
func identityOf(t *testing.T, ln *net.TCPListener) (listenerIdentity, bool) {
	t.Helper()
	raw, err := ln.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var id listenerIdentity
	listening := true
	var inner error
	if err := raw.Control(func(fd uintptr) {
		var st unix.Stat_t
		if inner = unix.Fstat(int(fd), &st); inner != nil {
			return
		}
		id.Dev, id.Ino = uint64(st.Dev), st.Ino
		sa, err := unix.Getsockname(int(fd))
		if inner = err; inner != nil {
			return
		}
		if in4, ok := sa.(*unix.SockaddrInet4); ok {
			id.Port = in4.Port
		}
		if runtime.GOOS == "linux" {
			// macOS does not answer this option; there the accepted connection below is the evidence.
			v, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
			if inner = err; inner == nil {
				listening = v == 1
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if inner != nil {
		t.Fatalf("inspect the parent's listener: %v", inner)
	}
	return id, listening
}

// TestParentReservationKeepsListenerIdentity checks, after a failed drain exchange or an immediate stop on the
// parent's end, that the parent's copy of the supervisor-owned listener is still the very socket it handed over,
// listening on the same port, until the parent ends it: its identity is unchanged, a connection to the port is
// accepted by that copy (the same connection the test dialled) and can be handed to a later owner, twice. Nothing is
// asserted about the port after the parent ended its copy. The test's own listener stands in for the reservation's
// copy, which never accepts.
//
// It rejects a worker that ends the shared socket instead of its own descriptor, so that the parent's copy stops
// listening, and any path by which a different socket comes to answer on the port while the parent holds it.
func TestParentReservationKeepsListenerIdentity(t *testing.T) {
	tests := map[string]struct {
		answer     string
		parentLoss bool
	}{
		"error: after an answer for another generation the parent's listener keeps its identity until it ends it": {
			answer: `{"type":"retired","generation":"another-generation"}` + "\n",
		},
		"error: after an unanswered drain notice the parent's listener keeps its identity until it ends it": {},
		"error: after an immediate stop on the parent's end the parent's listener keeps its identity":       {parentLoss: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHandoff(t)
			parentLn, parent := manualHandoff(t, h, false)
			tcp := parentLn.(*net.TCPListener)
			before, _ := identityOf(t, tcp)
			l := h.launch(t, ownedMode)
			port := l.awaitReady(t)
			h.closeTransfer()
			if port != strconv.Itoa(before.Port) {
				l.finish(t)
				t.Fatalf("readiness on port %q; want the inherited listener's port %d; status %v: %s", port, before.Port, l.runErr, l.stderr)
			}
			if data := h.reportBytes(t); len(data) == 0 {
				t.Fatal("the worker did not report its listener")
			}
			ctx, cancel := context.WithDeadline(t.Context(), l.startedAt.Add(fixtureLifetime))
			defer cancel()
			public := "127.0.0.1:" + port
			if tc.parentLoss {
				if err := h.liveW.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				l.requireHeadroom(t, 8*time.Second, "the setup before the drain request")
				if code, _, err := postDrain(ctx, public, ownedToken); err != nil || code != http.StatusAccepted {
					t.Fatalf("direct drain: status %d, %v; want 202", code, err)
				}
				if err := parent.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := workerlink.DecodeDrain(bufio.NewReader(parent)); err != nil {
					t.Fatalf("read the worker's drain notice: %v", err)
				}
				if tc.answer != "" {
					if _, err := io.WriteString(parent, tc.answer); err != nil {
						t.Fatalf("write the wrong answer: %v", err)
					}
				}
			}
			if !l.finish(t) || l.runErr != nil {
				t.Fatalf("the worker did not return cleanly: %v: %s", l.runErr, l.stderr)
			}
			for round := range 2 {
				after, listening := identityOf(t, tcp)
				if diff := gocmp.Diff(before, after); diff != "" {
					t.Errorf("round %d: the parent's listener after the worker returned (-want +got):\n%s", round, diff)
				}
				if !listening {
					t.Errorf("round %d: the parent's listener no longer listens after the worker returned", round)
				}
				var dialer net.Dialer
				client, err := dialer.DialContext(ctx, "tcp4", public)
				if err != nil {
					t.Fatalf("round %d: dial the port while the parent holds its listener: %v", round, err)
				}
				if err := tcp.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				handed, err := tcp.Accept()
				if err != nil {
					_ = client.Close()
					t.Fatalf("round %d: the parent's listener did not hand over the connection: %v", round, err)
				}
				if diff := gocmp.Diff(client.LocalAddr().String(), handed.RemoteAddr().String()); diff != "" {
					t.Errorf("round %d: the parent's listener handed over another connection (-want +got):\n%s", round, diff)
				}
				_, werr := io.WriteString(handed, "later owner\n")
				_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
				line, rerr := bufio.NewReader(client).ReadString('\n')
				if werr != nil || rerr != nil || line != "later owner\n" {
					t.Errorf("round %d: the later owner's line: %q, %v, %v", round, line, werr, rerr)
				}
				_ = handed.Close()
				_ = client.Close()
			}
		})
	}
}
