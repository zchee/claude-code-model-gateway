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
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// recordingListens records every listen call. A listener the test prepared for an address is handed over as the
// bound listener (the test bound it and kept it, so no other process can take the port in between); any other
// address is bound for real.
type recordingListens struct {
	mu       sync.Mutex
	calls    []listenCall
	prepared map[string]net.Listener
	bound    []net.Listener
}

func newRecordingListens() *recordingListens {
	return &recordingListens{prepared: map[string]net.Listener{}}
}

func (r *recordingListens) listen(ctx context.Context, network, address string) (net.Listener, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, listenCall{network, address})
	if l, ok := r.prepared[address]; ok {
		delete(r.prepared, address)
		r.bound = append(r.bound, l)
		return l, nil
	}
	l, err := realListen(ctx, network, address)
	if err == nil {
		r.bound = append(r.bound, l)
	}
	return l, err
}

func (r *recordingListens) prepare(l net.Listener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prepared[l.Addr().String()] = l
}

func (r *recordingListens) snapshot() ([]listenCall, []net.Listener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]listenCall(nil), r.calls...), append([]net.Listener(nil), r.bound...)
}

// recordingDials dials for real and records every address.
type recordingDials struct {
	mu    sync.Mutex
	calls []listenCall
}

func (r *recordingDials) dial(ctx context.Context, network, address string) (net.Conn, error) {
	r.mu.Lock()
	r.calls = append(r.calls, listenCall{network, address})
	r.mu.Unlock()
	return realDial(ctx, network, address)
}

func (r *recordingDials) addresses() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, c.Address)
	}
	return out
}

func (r *recordingDials) checkNetworks(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.Network != "tcp4" && c.Network != "tcp" {
			t.Errorf("dial network %q for %s, want a TCP stream", c.Network, c.Address)
		}
	}
}

func acquire(t *testing.T, src workerlink.Source, exclude workerlink.Generation) workerlink.Lease {
	t.Helper()
	lease, err := src.Acquire(t.Context(), exclude)
	if err != nil {
		t.Fatalf("Acquire(%q): %v", exclude, err)
	}
	if lease == nil {
		t.Fatalf("Acquire(%q) returned no lease", exclude)
	}
	t.Cleanup(func() { _ = lease.Close() })
	return lease
}

// leaseFields is what a lease tells the relay about the generation it reached.
type leaseFields struct {
	Generation workerlink.Generation
	PID        int
	Authority  string
}

func fieldsOf(l workerlink.Lease) leaseFields {
	return leaseFields{Generation: l.Generation(), PID: l.PID(), Authority: l.Authority()}
}

// TestReserveBindsLoopbackListener checks that a reservation binds one IPv4 loopback TCP listener on the
// requested port, or on a selected one for port zero, and reports the actual port.
func TestReserveBindsLoopbackListener(t *testing.T) {
	tests := map[string]struct {
		fixed bool
	}{
		"success: selected port": {},
		"success: fixed port":    {fixed: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			listens := newRecordingListens()
			var port uint16
			if tt.fixed {
				kept := listenTCP(t, "tcp4", "127.0.0.1:0")
				port = tcpPort(t, kept.Addr())
				listens.prepare(kept)
			}
			reg := workerlink.NewRegistry(listens.listen, realDial)
			res := reserve(t, reg, "gen-a", port)
			calls, bound := listens.snapshot()
			if diff := gocmp.Diff([]listenCall{{"tcp4", loopback(port)}}, calls); diff != "" {
				t.Fatalf("listen calls (-want +got):\n%s", diff)
			}
			got := res.Port()
			if want := tcpPort(t, bound[0].Addr()); got != want || got == 0 {
				t.Errorf("Port() = %d, want the bound port %d", got, want)
			}
			if tt.fixed && got != port {
				t.Errorf("Port() = %d, want the fixed port %d", got, port)
			}
			if err := res.End(); err != nil {
				t.Errorf("End: %v", err)
			}
		})
	}
}

// TestReserveRefuses checks that a failed bind gives no reservation, and that an occupied fixed port is refused
// rather than shared.
func TestReserveRefuses(t *testing.T) {
	errListen := errors.New("listen failed")
	tests := map[string]struct {
		listen func(t *testing.T) (workerlink.ListenFunc, uint16)
		want   error
	}{
		"error: listen fails": {
			listen: func(*testing.T) (workerlink.ListenFunc, uint16) {
				return func(context.Context, string, string) (net.Listener, error) { return nil, errListen }, 0
			},
			want: errListen,
		},
		"error: occupied fixed port": {
			listen: func(t *testing.T) (workerlink.ListenFunc, uint16) {
				occupant := listenTCP(t, "tcp4", "127.0.0.1:0")
				return realListen, tcpPort(t, occupant.Addr())
			},
			want: syscall.EADDRINUSE,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			listen, port := tt.listen(t)
			reg := workerlink.NewRegistry(listen, realDial)
			res, err := reg.Reserve(t.Context(), "gen-a", port)
			if res != nil {
				_ = res.End()
				t.Error("Reserve returned a reservation for a failed bind")
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("Reserve error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestReservedAddressHeldUntilEnd checks that, without a drain acknowledgment, the supervisor keeps the reserved
// address bound from Reserve until End: a second bind fails through publication and retirement, also after the
// child closed its own copy, and succeeds once End returns. An ended reservation hands out no new duplicate.
func TestReservedAddressHeldUntilEnd(t *testing.T) {
	reg := workerlink.NewRegistry(realListen, realDial)
	res := reserve(t, reg, "gen-a", 0)
	port := res.Port()
	checkBindRefused(t, port, "after Reserve")
	child := childListener(t, res)
	checkBindRefused(t, port, "with the child's copy open")
	if err := child.Close(); err != nil {
		t.Fatalf("closing the child's copy: %v", err)
	}
	checkBindRefused(t, port, "after the child closed its copy")
	publish(t, res, 4101)
	checkBindRefused(t, port, "after Publish")
	res.Retire()
	checkBindRefused(t, port, "after Retire")
	if err := res.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	bindAndKeep(t, port, "after End")
	if f, err := res.File(); err == nil || f != nil {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("File after End = %v, %v; want an error and no file", f, err)
	}
}

// TestFileIsIndependentDuplicate checks that the file handed to the child is a close-on-exec duplicate of the
// listening socket and that closing it leaves the supervisor's socket bound and listening.
func TestFileIsIndependentDuplicate(t *testing.T) {
	reg := workerlink.NewRegistry(realListen, realDial)
	res := reserve(t, reg, "gen-a", 0)
	port := res.Port()
	f, err := res.File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	want := socketKind{Family: "127.0.0.1", Type: unix.SOCK_STREAM, Listening: true, Port: int(port)}
	if diff := gocmp.Diff(want, describeSocket(t, f)); diff != "" {
		t.Errorf("duplicate (-want +got):\n%s", diff)
	}
	checkCloseOnExec(t, f, "listener duplicate")
	if err := f.Close(); err != nil {
		t.Fatalf("closing the duplicate: %v", err)
	}
	// No other copy exists now; the connection completes only if the supervisor's socket still listens.
	c, err := realDial(t.Context(), "tcp4", loopback(port))
	if err != nil {
		t.Fatalf("dial after the duplicate was closed: %v", err)
	}
	_ = c.Close()
	checkBindRefused(t, port, "after the duplicate was closed")
}

// TestLeaseReachesItsGeneration checks that every lease is a connection to the listener reserved for the
// generation it names, that an excluded generation is never returned, and that a retired generation is never
// offered again, even when nothing else is eligible. The child side accepts every connection, so the
// reservation accepted none of them.
func TestLeaseReachesItsGeneration(t *testing.T) {
	dials := &recordingDials{}
	reg := workerlink.NewRegistry(realListen, dials.dial)
	a := reserve(t, reg, "gen-a", 0)
	b := reserve(t, reg, "gen-b", 0)
	childA, childB := childListener(t, a), childListener(t, b)
	publish(t, a, 4101)
	var wantDials []string
	for i := range 3 {
		lease := acquire(t, reg, "")
		if diff := gocmp.Diff(leaseFields{"gen-a", 4101, loopback(a.Port())}, fieldsOf(lease)); diff != "" {
			t.Fatalf("lease %d (-want +got):\n%s", i, diff)
		}
		wantDials = append(wantDials, loopback(a.Port()))
		msg := fmt.Sprintf("lease %d\n", i)
		if _, err := lease.Write([]byte(msg)); err != nil {
			t.Fatalf("lease %d write: %v", i, err)
		}
		if got := readExactly(t, acceptOne(t, childA), len(msg)); got != msg {
			t.Fatalf("gen-a's listener read %q, want %q", got, msg)
		}
	}
	a.Retire()
	publish(t, b, 4102)
	lease := acquire(t, reg, "")
	if diff := gocmp.Diff(leaseFields{"gen-b", 4102, loopback(b.Port())}, fieldsOf(lease)); diff != "" {
		t.Fatalf("lease after the replacement (-want +got):\n%s", diff)
	}
	wantDials = append(wantDials, loopback(b.Port()))
	if _, err := lease.Write([]byte("to gen-b\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readExactly(t, acceptOne(t, childB), 9); got != "to gen-b\n" {
		t.Fatalf("gen-b's listener read %q", got)
	}
	if lease, err := reg.Acquire(t.Context(), "gen-b"); err == nil || lease != nil {
		t.Fatalf("Acquire excluding gen-b = %v, %v; the retired gen-a must not be offered", lease, err)
	}
	if err := b.End(); err != nil {
		t.Fatalf("End gen-b: %v", err)
	}
	if lease, err := reg.Acquire(t.Context(), ""); err == nil || lease != nil {
		t.Fatalf("Acquire with only the retired gen-a left = %v, %v; want no lease", lease, err)
	}
	if diff := gocmp.Diff(wantDials, dials.addresses()); diff != "" {
		t.Errorf("dialed addresses (-want +got):\n%s", diff)
	}
	dials.checkNetworks(t)
}

// TestNewGenerationOnTheSamePort checks that a replacement bound on the port its ended predecessor used is a new
// reservation: the lease names the new generation and reaches the new listener, never the old accept queue.
func TestNewGenerationOnTheSamePort(t *testing.T) {
	listens := newRecordingListens()
	dials := &recordingDials{}
	reg := workerlink.NewRegistry(listens.listen, dials.dial)
	a := reserve(t, reg, "gen-a", 0)
	port := a.Port()
	childA := childListener(t, a)
	publish(t, a, 4101)
	acquire(t, reg, "")
	// The old worker has exited: its copy of the listener is gone before the supervisor ends the generation.
	if err := childA.Close(); err != nil {
		t.Fatalf("closing gen-a's child copy: %v", err)
	}
	if err := a.End(); err != nil {
		t.Fatalf("End gen-a: %v", err)
	}
	// The test takes the freed address itself and hands exactly that socket to the replacement.
	listens.prepare(bindAndKeep(t, port, "after End"))
	c := reserve(t, reg, "gen-c", port)
	if c.Port() != port {
		t.Fatalf("replacement Port() = %d, want %d", c.Port(), port)
	}
	childC := childListener(t, c)
	publish(t, c, 4103)
	lease := acquire(t, reg, "gen-a")
	if diff := gocmp.Diff(leaseFields{"gen-c", 4103, loopback(port)}, fieldsOf(lease)); diff != "" {
		t.Fatalf("lease (-want +got):\n%s", diff)
	}
	if _, err := lease.Write([]byte("to gen-c\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readExactly(t, acceptOne(t, childC), 9); got != "to gen-c\n" {
		t.Fatalf("gen-c's listener read %q", got)
	}
	if diff := gocmp.Diff([]string{loopback(port), loopback(port)}, dials.addresses()); diff != "" {
		t.Errorf("dialed addresses (-want +got):\n%s", diff)
	}
}
