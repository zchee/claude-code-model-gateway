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
	"strconv"
	"sync"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// recordingListens records every listen call. A listener the test prepared for a port is handed over as the
// bound listener when the call asks for that port on the IPv4 loopback (the test bound it and kept it, so no other
// process can take the port in between); any other call is bound for real.
type recordingListens struct {
	mu       sync.Mutex
	calls    []listenCall
	prepared map[uint16]net.Listener
	bound    []net.Listener
}

func newRecordingListens() *recordingListens {
	return &recordingListens{prepared: map[uint16]net.Listener{}}
}

func (r *recordingListens) listen(ctx context.Context, network, address string) (net.Listener, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, listenCall{network, address})
	if port, ok := ipv4LoopbackPort(address); ok {
		if l, ok := r.prepared[port]; ok {
			delete(r.prepared, port)
			r.bound = append(r.bound, l)
			return l, nil
		}
	}
	l, err := realListen(ctx, network, address)
	if err == nil {
		r.bound = append(r.bound, l)
	}
	return l, err
}

func (r *recordingListens) prepare(t *testing.T, l net.Listener) {
	t.Helper()
	port := tcpPort(t, l.Addr())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prepared[port] = l
}

func (r *recordingListens) snapshot() ([]listenCall, []net.Listener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]listenCall(nil), r.calls...), append([]net.Listener(nil), r.bound...)
}

// ipv4LoopbackPort returns the port of an address whose host is the numeric IPv4 loopback 127.0.0.1.
func ipv4LoopbackPort(address string) (uint16, bool) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return 0, false
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil || !ip.Equal(net.IPv4(127, 0, 0, 1)) {
		return 0, false
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return 0, false
	}
	return uint16(port), true
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
	if lease != nil {
		t.Cleanup(func() { _ = lease.Close() })
	}
	if err != nil {
		t.Fatalf("Acquire(%q): %v", exclude, err)
	}
	if lease == nil {
		t.Fatalf("Acquire(%q) returned no lease", exclude)
	}
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

// TestReserveBindsLoopbackListener checks that a reservation makes one listen call for a TCP stream on the IPv4
// loopback at the requested port, or at port zero to have one selected, and that what it holds is an IPv4 socket
// listening on 127.0.0.1 at the port Port reports. Which TCP network name the call uses is not decided here.
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
				listens.prepare(t, kept)
			}
			reg := workerlink.NewRegistry(listens.listen, realDial)
			res := reserve(t, reg, "gen-a", port)
			calls, bound := listens.snapshot()
			if len(calls) != 1 || len(bound) != 1 {
				t.Fatalf("listen calls %+v, bound %d listeners; want one call and one listener", calls, len(bound))
			}
			if calls[0].Network != "tcp" && calls[0].Network != "tcp4" {
				t.Errorf("listen network %q, want a TCP network that carries IPv4", calls[0].Network)
			}
			if got, ok := ipv4LoopbackPort(calls[0].Address); !ok || got != port {
				t.Errorf("listen address %q, want 127.0.0.1 at port %d", calls[0].Address, port)
			}
			got := res.Port()
			if got == 0 || (tt.fixed && got != port) {
				t.Errorf("Port() = %d, want the bound port (requested %d)", got, port)
			}
			sc, ok := bound[0].(syscall.Conn)
			if !ok {
				t.Fatalf("bound listener %T exposes no descriptor", bound[0])
			}
			want := socketKind{Family: "127.0.0.1", Type: unix.SOCK_STREAM, Listening: wantListening(true), Port: int(got)}
			if diff := gocmp.Diff(want, describeSocket(t, sc)); diff != "" {
				t.Errorf("the reserved socket (-want +got):\n%s", diff)
			}
			if err := res.End(); err != nil {
				t.Errorf("End: %v", err)
			}
		})
	}
}

// TestReserveRefuses checks that a failed bind gives no reservation and reports its cause, that an occupied fixed
// port is refused rather than shared, that a listener the listen function returns for another endpoint than the
// requested one (any address other than exactly 127.0.0.1, also another loopback address, not the requested fixed
// port, or port zero) is refused and closed, and that a reservation whose drain channel pair cannot be created is
// rolled back: its listener is closed and a Reserve of the same generation then succeeds. A refusal leaves nothing
// eligible. The error of a wrong-endpoint refusal and of a pair failure is not decided here.
func TestReserveRefuses(t *testing.T) {
	errListen := errors.New("listen failed")
	// wrongEndpoint returns a listen function that hands over a fake listener at addr for any call.
	wrongEndpoint := func(addr net.Addr) func(t *testing.T) (workerlink.ListenFunc, *fakeListener) {
		return func(*testing.T) (workerlink.ListenFunc, *fakeListener) {
			l := newFakeListenerAt(addr)
			return func(context.Context, string, string) (net.Listener, error) { return l, nil }, l
		}
	}
	tests := map[string]struct {
		port uint16
		// listen returns the listen function and the fake listener it hands over, if any; nil binds fake listeners
		// on the requested ports.
		listen func(t *testing.T) (workerlink.ListenFunc, *fakeListener)
		// occupied binds the requested port first and keeps it.
		occupied bool
		// pairFails makes the creation of the reservation's drain channel pair fail.
		pairFails bool
		want      error
	}{
		"error: listen fails": {
			listen: func(*testing.T) (workerlink.ListenFunc, *fakeListener) {
				return func(context.Context, string, string) (net.Listener, error) { return nil, errListen }, nil
			},
			want: errListen,
		},
		"error: occupied fixed port": {
			listen:   func(*testing.T) (workerlink.ListenFunc, *fakeListener) { return realListen, nil },
			occupied: true,
			want:     syscall.EADDRINUSE,
		},
		"error: listener on the wildcard address": {
			port:   41001,
			listen: wrongEndpoint(&net.TCPAddr{IP: net.IPv4zero, Port: 41001}),
		},
		"error: listener on the wildcard address for a selected port": {
			listen: wrongEndpoint(&net.TCPAddr{IP: net.IPv4zero, Port: 41001}),
		},
		"error: listener on another loopback address": {
			port:   41001,
			listen: wrongEndpoint(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 41001}),
		},
		"error: listener on another loopback address for a selected port": {
			listen: wrongEndpoint(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 41001}),
		},
		"error: listener that reports port zero for a selected port": {
			listen: wrongEndpoint(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}),
		},
		"error: drain channel pair cannot be created": {
			port:      41001,
			pairFails: true,
		},
		"error: listener on the IPv6 loopback": {
			port:   41001,
			listen: wrongEndpoint(&net.TCPAddr{IP: net.IPv6loopback, Port: 41001}),
		},
		"error: listener on another port than the fixed one": {
			port:   41001,
			listen: wrongEndpoint(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41002}),
		},
		"error: listener that is not TCP": {
			port:   41001,
			listen: wrongEndpoint(&net.UnixAddr{Name: "/nonexistent/socket", Net: "unix"}),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			listens := newFakeListens()
			var listen workerlink.ListenFunc = listens.listen
			var fake *fakeListener
			if tt.listen != nil {
				listen, fake = tt.listen(t)
			}
			port := tt.port
			if tt.occupied {
				port = tcpPort(t, listenTCP(t, "tcp4", "127.0.0.1:0").Addr())
			}
			reg := workerlink.NewRegistry(listen, realDial)
			fail := 0
			if tt.pairFails {
				fail = 1
			}
			pairs := newDrainPairs(t, reg, false, fail)
			res, err := reg.Reserve(t.Context(), "gen-a", port)
			if res != nil {
				_ = res.End()
				t.Error("Reserve returned a reservation for a refused bind")
			}
			if err == nil {
				t.Fatal("Reserve succeeded")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("Reserve error = %v, want %v", err, tt.want)
			}
			if fake != nil && !isClosed(fake.closed) {
				t.Error("the refused listener was left open")
			}
			if lease, err := reg.Acquire(t.Context(), ""); lease != nil || !errors.Is(err, workerlink.ErrUnavailable) {
				if lease != nil {
					_ = lease.Close()
				}
				t.Errorf("Acquire after a refused Reserve = %v, %v; want %v", lease, err, workerlink.ErrUnavailable)
			}
			if !tt.pairFails {
				return
			}
			if pairs.count() == 0 {
				t.Fatal("Reserve never created its drain channel pair through the registry's factory")
			}
			// Whether the listener was bound before the pair failed is the implementation's; one it got is closed.
			listens.mu.Lock()
			got := listens.listeners[loopback(port)]
			listens.mu.Unlock()
			if got != nil && !isClosed(got.closed) {
				t.Error("the listener of the rolled-back reservation was left open")
			}
			retry := reserve(t, reg, "gen-a", port)
			if retry.Port() != port {
				t.Errorf("Port() of the retried reservation = %d, want %d", retry.Port(), port)
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
// listening socket, sharing its accept queue, and that the two copies are independent in either close order:
// closing the duplicate leaves the supervisor's socket bound and listening, and a duplicate kept across End
// still accepts the connection that was queued before End.
func TestFileIsIndependentDuplicate(t *testing.T) {
	reg := workerlink.NewRegistry(realListen, realDial)
	res := reserve(t, reg, "gen-a", 0)
	port := res.Port()
	f, err := res.File()
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	want := socketKind{Family: "127.0.0.1", Type: unix.SOCK_STREAM, Listening: wantListening(true), Port: int(port)}
	if diff := gocmp.Diff(want, describeSocket(t, f)); diff != "" {
		t.Errorf("duplicate (-want +got):\n%s", diff)
	}
	checkCloseOnExec(t, f, "listener duplicate")
	if err := f.Close(); err != nil {
		t.Fatalf("closing the duplicate: %v", err)
	}
	// No other copy exists now; the connection completes only if the supervisor's socket still listens.
	c, err := realDial(t.Context(), "tcp4", loopback(port))
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	if err != nil {
		t.Fatalf("dial after the duplicate was closed: %v", err)
	}
	checkBindRefused(t, port, "after the duplicate was closed")

	// The other order: the supervisor's copy goes at End while the child's duplicate stays open.
	other := reserve(t, reg, "gen-b", 0)
	otherPort := other.Port()
	kept, err := other.File()
	if kept != nil {
		t.Cleanup(func() { _ = kept.Close() })
	}
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	queued, err := realDial(t.Context(), "tcp4", loopback(otherPort))
	if queued != nil {
		t.Cleanup(func() { _ = queued.Close() })
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	writeAll(t, queued, "queued before End\n")
	if err := other.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	checkBindRefused(t, otherPort, "after End with the child's duplicate open")
	l, err := net.FileListener(kept)
	if l != nil {
		t.Cleanup(func() { _ = l.Close() })
	}
	if err != nil {
		t.Fatalf("FileListener on the kept duplicate: %v", err)
	}
	tcp, ok := l.(*net.TCPListener)
	if !ok {
		t.Fatalf("the kept duplicate is a %T listener, want TCP", l)
	}
	if got := readExactly(t, acceptOne(t, tcp), 18); got != "queued before End\n" {
		t.Errorf("the kept duplicate accepted a connection that read %q, want the one queued before End", got)
	}
}

// TestLeaseReachesItsGeneration checks that every lease is a connection to the listener reserved for the
// generation it names; that publishing a second generation while the first is eligible never leaves both
// eligible (the second Publish may be refused or may replace the first; that choice is not decided here); that a
// retired generation is not offered; and that ending the older generation does not withdraw the newer one. The
// child side accepts every connection, so the reservation accepted none of them.
func TestLeaseReachesItsGeneration(t *testing.T) {
	dials := &recordingDials{}
	reg := workerlink.NewRegistry(realListen, dials.dial)
	a := reserve(t, reg, "gen-a", 0)
	b := reserve(t, reg, "gen-b", 0)
	childA, childB := childListener(t, a), childListener(t, b)
	publish(t, a, 4101)
	var wantDials []string
	// reach takes a lease, checks its fields and that the generation's own listener reads what it carries.
	reach := func(what string, exclude workerlink.Generation, want leaseFields, child *net.TCPListener) {
		t.Helper()
		lease := acquire(t, reg, exclude)
		if diff := gocmp.Diff(want, fieldsOf(lease)); diff != "" {
			t.Fatalf("%s (-want +got):\n%s", what, diff)
		}
		wantDials = append(wantDials, want.Authority)
		msg := what + "\n"
		writeAll(t, lease, msg)
		if got := readExactly(t, acceptOne(t, child), len(msg)); got != msg {
			t.Fatalf("%s: %s's listener read %q, want %q", what, want.Generation, got, msg)
		}
	}
	// only asserts that exactly one generation is eligible: the lease excluding it is refused.
	only := func(what string, eligible workerlink.Generation) {
		t.Helper()
		if lease, err := reg.Acquire(t.Context(), eligible); err == nil || lease != nil {
			if lease != nil {
				_ = lease.Close()
			}
			t.Fatalf("%s: Acquire excluding %s = %v, %v; no other generation may be eligible", what, eligible, lease, err)
		}
	}
	fieldsA := leaseFields{"gen-a", 4101, loopback(a.Port())}
	fieldsB := leaseFields{"gen-b", 4102, loopback(b.Port())}
	for i := range 3 {
		reach(fmt.Sprintf("lease %d to gen-a", i), "", fieldsA, childA)
	}
	only("with gen-a published", "gen-a")

	errPublish := b.Publish(4102)
	if errPublish != nil {
		// The second Publish was refused: gen-a is still the eligible one, until its drain retires it.
		reach("lease after a refused Publish of gen-b", "", fieldsA, childA)
		only("after a refused Publish of gen-b", "gen-a")
		a.Retire()
		publish(t, b, 4102)
	}
	reach("lease to gen-b", "", fieldsB, childB)
	only("with gen-b published", "gen-b")
	a.Retire()
	only("with gen-a retired", "gen-b")
	if err := a.End(); err != nil {
		t.Fatalf("End gen-a: %v", err)
	}
	reach("lease to gen-b after gen-a ended", "", fieldsB, childB)
	if err := b.End(); err != nil {
		t.Fatalf("End gen-b: %v", err)
	}
	if lease, err := reg.Acquire(t.Context(), ""); err == nil || lease != nil {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatalf("Acquire with both generations ended = %v, %v; want no lease", lease, err)
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
	listens.prepare(t, bindAndKeep(t, port, "after End"))
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
