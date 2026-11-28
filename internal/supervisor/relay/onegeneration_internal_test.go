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

package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// TestOneGenerationDeliverDeparture decides what deliver returns for a marked request whose client departs: no
// exchange, a failure that says the client departed and names no taker, and no further Acquire or Wait. Through
// ServeHTTP these returns cannot be told apart, because a departed client is answered nothing whatever the failure
// says; a relay that returned the taker instead would report a loss the moment a caller reads the failure without
// looking at the context. The client departs inside an Acquire that refuses the request, during the handed-over
// attempt, and while the request waits with a taker that is current or has exited.
func TestOneGenerationDeliverDeparture(t *testing.T) {
	tests := map[string]struct {
		// acquire answers the n-th Acquire; depart ends the request's context, as a client that leaves does.
		acquire func(t *testing.T, n int, depart context.CancelFunc) (workerlink.Lease, error)
		// onWait runs at the start of the n-th Wait.
		onWait func(n int, depart context.CancelFunc)
		want   oneGenTDeparture
	}{
		"success: a client that departs inside a refused Acquire has departed": {
			acquire: func(_ *testing.T, _ int, depart context.CancelFunc) (workerlink.Lease, error) {
				depart()
				return nil, workerlink.ErrUnavailable
			},
			want: oneGenTDeparture{Departed: true, Acquires: 1, Waits: 0},
		},
		"success: a client that departs during the handed-over attempt has departed": {
			acquire: func(t *testing.T, _ int, depart context.CancelFunc) (workerlink.Lease, error) {
				return newOneGenTLease(t, depart), nil
			},
			want: oneGenTDeparture{Departed: true, Acquires: 1, Waits: 0},
		},
		"success: a client that departs while the request waits with its current taker has departed": {
			acquire: func(t *testing.T, _ int, _ context.CancelFunc) (workerlink.Lease, error) {
				return newOneGenTLease(t, nil), nil
			},
			onWait: func(n int, depart context.CancelFunc) {
				if n == 2 {
					depart()
				}
			},
			want: oneGenTDeparture{Departed: true, Acquires: 1, Waits: 2},
		},
		"success: a client that departs while the request waits with its exited taker has departed": {
			acquire: func(t *testing.T, _ int, _ context.CancelFunc) (workerlink.Lease, error) {
				l := newOneGenTLease(t, nil)
				close(l.exited)
				return l, nil
			},
			onWait: func(n int, depart context.CancelFunc) {
				if n == 3 {
					depart()
				}
			},
			want: oneGenTDeparture{Departed: true, Acquires: 1, Waits: 3},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, depart := context.WithCancel(t.Context())
				defer depart()
				marked, ok := oneGenTMarkContext(t, ctx)
				if !ok {
					return
				}
				src := &oneGenTSource{}
				src.acquire = func(n int) (workerlink.Lease, error) { return tt.acquire(t, n, depart) }
				var waits atomic.Int32
				built, err := New(Options{
					Workers: src,
					Lost:    func(Lost) { t.Errorf("deliver reported a loss; only answerFailure reports one") },
					Wait: func(ctx context.Context, d time.Duration) error {
						n := int(waits.Add(1))
						if tt.onWait != nil {
							tt.onWait(n, depart)
						}
						timer := time.NewTimer(d)
						defer timer.Stop()
						select {
						case <-timer.C:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				// Only deliver, its result and the cleanup of an unexpected exchange reach into the handler.
				h, isHandler := built.(*handler)
				if !isHandler {
					t.Fatalf("New returned %T; want *handler", built)
				}
				r, err := http.NewRequestWithContext(marked, http.MethodPost, "http://127.0.0.1:18764/v1/messages", strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				ex, f, ok := oneGenTDeliver(t, h, marked, r)
				if !ok {
					return
				}
				if ex != nil {
					ex.finish()
				}
				// Every lease is closed by the time deliver has returned without an exchange.
				for i, l := range src.handed() {
					if !oneGenTClosed(l.closed) {
						t.Errorf("lease %d is still open when deliver returned", i+1)
					}
				}
				// Nothing may follow the departure: let any timer of a relay that kept trying run first.
				time.Sleep(time.Minute)
				synctest.Wait()
				got := oneGenTDeparture{Exchange: ex != nil, Departed: f.departed, Taker: f.taker != nil, Acquires: src.calls(), Waits: int(waits.Load())}
				if diff := gocmp.Diff(tt.want, got); diff != "" {
					t.Errorf("what deliver returned and the calls it made (-want +got):\n%s", diff)
				}
				for i, l := range src.handed() {
					if !oneGenTClosed(l.closed) {
						t.Errorf("lease %d is still open a minute after deliver returned", i+1)
					}
				}
			})
		})
	}
}

// oneGenTDeparture is what deliver returned and the calls it made: whether it returned an exchange, whether the
// failure says the client departed and whether it names a taker, and the numbers of Acquire and Wait calls.
type oneGenTDeparture struct {
	Exchange bool
	Departed bool
	Taker    bool
	Acquires int
	Waits    int
}

// oneGenTMarkContext returns relay.OneGeneration(ctx). A panic fails the case alone.
func oneGenTMarkContext(t *testing.T, ctx context.Context) (marked context.Context, ok bool) {
	t.Helper()
	defer func() {
		if v := recover(); v != nil {
			t.Errorf("marking the request context panicked: %v", v)
			marked, ok = nil, false
		}
	}()
	return OneGeneration(ctx), true
}

// oneGenTDeliver runs deliver for r. A panic fails the case alone.
func oneGenTDeliver(t *testing.T, h *handler, ctx context.Context, r *http.Request) (ex *exchange, f failure, ok bool) {
	t.Helper()
	defer func() {
		if v := recover(); v != nil {
			t.Errorf("deliver panicked: %v", v)
			ex, f, ok = nil, failure{}, false
		}
	}()
	ex, f = h.deliver(ctx, r, "/v1/messages", []byte("{}"))
	return ex, f, true
}

// oneGenTSource is a worker source whose Acquire calls acquire with their one-based number and keeps every lease it
// handed out.
type oneGenTSource struct {
	acquire func(n int) (workerlink.Lease, error)

	mu     sync.Mutex
	n      int
	leases []*oneGenTLease
}

func (s *oneGenTSource) Acquire(context.Context, workerlink.Generation) (workerlink.Lease, error) {
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	l, err := s.acquire(n)
	if tracked, ok := l.(*oneGenTLease); ok {
		s.mu.Lock()
		s.leases = append(s.leases, tracked)
		s.mu.Unlock()
	}
	return l, err
}

func (s *oneGenTSource) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *oneGenTSource) handed() []*oneGenTLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.leases)
}

// oneGenTReadWatchdog bounds, in virtual time, how long a read of oneGenTLease waits for the lease to be closed after
// the client departed.
const oneGenTReadWatchdog = time.Hour

// errOneGenTWatchdog is the read error of oneGenTLease once its watchdog ended the read.
var errOneGenTWatchdog = errors.New("the test lease's read watchdog ended the read")

// oneGenTLease is an in-memory lease to generation gen-a that takes every byte written to it, never answers and takes
// no deadlines. When it has a depart function, its first read calls it and then waits until the lease is closed, as a
// connection does whose client left during the exchange, for at most oneGenTReadWatchdog; otherwise every read ends
// the stream at once, as a worker that dropped the connection after taking the request.
type oneGenTLease struct {
	t         *testing.T
	depart    context.CancelFunc
	exited    chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func newOneGenTLease(t *testing.T, depart context.CancelFunc) *oneGenTLease {
	return &oneGenTLease{t: t, depart: depart, exited: make(chan struct{}), closed: make(chan struct{})}
}

// oneGenTClosed reports whether ch is closed.
func oneGenTClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (l *oneGenTLease) Read([]byte) (int, error) {
	if l.depart == nil {
		return 0, io.EOF
	}
	l.depart()
	select {
	case <-l.closed:
		return 0, net.ErrClosed
	case <-time.After(oneGenTReadWatchdog):
		// A relay that does not end the attempt at the departure must fail this case, not hold the bubble.
		l.t.Errorf("the handed-over lease was not closed within %v of the client's departure", oneGenTReadWatchdog)
		return 0, errOneGenTWatchdog
	}
}

func (l *oneGenTLease) Write(p []byte) (int, error) {
	if oneGenTClosed(l.closed) {
		return 0, net.ErrClosed
	}
	return len(p), nil
}

func (l *oneGenTLease) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (*oneGenTLease) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50001}
}

func (*oneGenTLease) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41001}
}
func (*oneGenTLease) SetDeadline(time.Time) error       { return os.ErrNoDeadline }
func (*oneGenTLease) SetReadDeadline(time.Time) error   { return os.ErrNoDeadline }
func (*oneGenTLease) SetWriteDeadline(time.Time) error  { return os.ErrNoDeadline }
func (*oneGenTLease) Generation() workerlink.Generation { return "gen-a" }
func (*oneGenTLease) PID() int                          { return 4101 }
func (*oneGenTLease) Authority() string                 { return "127.0.0.1:41001" }
func (l *oneGenTLease) Exited() <-chan struct{}         { return l.exited }
