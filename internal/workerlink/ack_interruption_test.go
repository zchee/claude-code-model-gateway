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
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// wrappedRegistry is a registry with fake listeners, dials that succeed at once and gated drain channel pairs, whose
// parent ends the test may wrap once more, per reservation, before the registry receives them. A wrapper sits on top
// of the gated end, so write deadlines stay on the bubble's clock.
type wrappedRegistry struct {
	reg     *workerlink.Registry
	pairs   *drainPairs
	listens *fakeListens

	mu   sync.Mutex
	wrap func(*gatedConn) net.Conn
}

// newWrappedRegistry must run inside a bubble. With listen nil the registry binds fake listeners on the requested
// ports; otherwise listen receives that listen function and returns the one the registry uses.
func newWrappedRegistry(t *testing.T, listen func(inner workerlink.ListenFunc) workerlink.ListenFunc) *wrappedRegistry {
	t.Helper()
	r := &wrappedRegistry{listens: newFakeListens()}
	fn := workerlink.ListenFunc(r.listens.listen)
	if listen != nil {
		fn = listen(fn)
	}
	r.reg = workerlink.NewRegistry(fn, (&immediateDials{t: t}).dial)
	r.pairs = newDrainPairs(t, r.reg, true, 0)
	workerlink.ReplaceDrainPair(r.reg, func() (net.Conn, *os.File, error) {
		parent, child, err := r.pairs.pair()
		if err != nil {
			return nil, nil, err
		}
		r.mu.Lock()
		wrap := r.wrap
		r.mu.Unlock()
		if wrap == nil {
			return parent, child, nil
		}
		return wrap(parent.(*gatedConn)), child, nil
	})
	t.Cleanup(func() { r.listens.checkNeverAccepted(t) })
	return r
}

// reserveOptions shapes one reservation of a wrappedRegistry.
type reserveOptions struct {
	// park makes every interrupted Write of the gated end wait for releaseReturns before it returns.
	park bool
	// wrap wraps the gated end; nil hands the gated end itself to the registry.
	wrap func(*gatedConn) net.Conn
}

// wrappedReservation is a published, retired reservation of a wrappedRegistry. Its gatedReservation holds the gated
// end; conn is what the registry holds as the parent end.
type wrappedReservation struct {
	gatedReservation
	conn net.Conn
}

// releaser is a wrapper that parks something until the test releases it.
type releaser interface{ release() }

// reserve reserves generation on port, takes its drain channel, publishes it for pid and retires it. Whatever the
// wrapper parks and every parked return of the gated end are released at cleanup before End runs, so nothing the
// test holds outlives a failed assertion.
func (r *wrappedRegistry) reserve(t *testing.T, generation workerlink.Generation, port uint16, pid int, opts reserveOptions) wrappedReservation {
	t.Helper()
	var conn net.Conn
	r.mu.Lock()
	r.wrap = func(g *gatedConn) net.Conn {
		conn = g
		if opts.wrap != nil {
			conn = opts.wrap(g)
		}
		return conn
	}
	r.mu.Unlock()
	res := reserve(t, r.reg, generation, port)
	r.mu.Lock()
	r.wrap = nil
	r.mu.Unlock()
	parent := r.pairs.gatedAt(t, r.pairs.count()-1)
	// Registered after End's cleanup, so it runs first.
	t.Cleanup(func() {
		if rel, ok := conn.(releaser); ok {
			rel.release()
		}
		parent.releaseReturns()
		_ = parent.Close()
	})
	if opts.park {
		parent.parkOnReturn()
	}
	_, child := drainChannel(t, res)
	publish(t, res, pid)
	res.Retire()
	return wrappedReservation{res: res, parent: parent, child: child, listener: r.listens.at(t, port), conn: conn}
}

// parentClosed reports whether the gated end has been closed.
func parentClosed(g *gatedConn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// pausingDeadlineConn parks, once armed, the next call that sets a deadline on the parent end: the call announces
// itself on inside and waits for release; then it fails with failPaused when that is set, and otherwise sets the
// deadline on the gated end. The acknowledgment's claimed writer arms its write deadline before it enters Write, so a
// test that acts while the call is parked acts between the claim and the Write. With rejectPast, every later call that
// asks for a deadline that is not in the future is refused and counted, as by a connection that cannot be interrupted
// through its deadline; the zero time, which clears the deadline, is passed on.
type pausingDeadlineConn struct {
	*gatedConn
	failPaused error
	rejectPast bool
	inside     chan struct{}
	resume     chan struct{}
	once       sync.Once
	// pastAsked counts the calls refused for asking a deadline that is not in the future.
	pastAsked atomic.Int32

	mu    sync.Mutex
	armed bool
}

func newPausingDeadlineConn(g *gatedConn) *pausingDeadlineConn {
	return &pausingDeadlineConn{gatedConn: g, inside: make(chan struct{}), resume: make(chan struct{})}
}

// arm makes the next deadline call park.
func (c *pausingDeadlineConn) arm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = true
}

// pause parks the call if it is the armed one, and returns the error the call is to return instead of setting t.
func (c *pausingDeadlineConn) pause(t time.Time) error {
	c.mu.Lock()
	armed := c.armed
	c.armed = false
	c.mu.Unlock()
	if armed {
		close(c.inside)
		<-c.resume
		return c.failPaused
	}
	if c.rejectPast && !t.IsZero() && !t.After(time.Now()) {
		c.pastAsked.Add(1)
		return errPastDeadline
	}
	return nil
}

func (c *pausingDeadlineConn) release() { c.once.Do(func() { close(c.resume) }) }

func (c *pausingDeadlineConn) SetWriteDeadline(t time.Time) error {
	if err := c.pause(t); err != nil {
		return err
	}
	return c.gatedConn.SetWriteDeadline(t)
}

func (c *pausingDeadlineConn) SetDeadline(t time.Time) error {
	if err := c.pause(t); err != nil {
		return err
	}
	return c.gatedConn.SetDeadline(t)
}

// errPastDeadline is the refusal of a deadline that is not in the future.
var errPastDeadline = errors.New("a deadline that has passed is refused by the connection")

// errArm is the refusal of the deadline call the test parks.
var errArm = errors.New("the write deadline is refused by the connection")

// TestAcknowledgeDrainInterruptedBeforeWrite checks an interruption that arrives after an acknowledgment claimed the
// write and before it entered Write. The test parks the acknowledgment inside the first call that sets a deadline on
// the parent end, which the claimed writer makes before its Write, and there cancels the context or starts End; it
// lets the bubble settle before the call goes on. That is an interruption before the write: no Write call at all. A
// cancellation returns the context's error and keeps the listener, and the acknowledgment stays retryable: a second
// call on the same reservation writes the one line, releases the listener and succeeds. That holds also on a
// connection that accepts the initial arm and refuses a deadline in the past: the cancellation, which came before the
// write was admitted, neither sets a deadline on the parent end nor closes it, so the second call succeeds on the same
// live connection. When the parked call is an arm that fails, the cancellation that coincides with it is still what
// the call returns, and the second call succeeds. After End the call fails, and End returns and releases the listener.
// The End case relies on the rule that the deadline is set outside the registry's lock.
func TestAcknowledgeDrainInterruptedBeforeWrite(t *testing.T) {
	tests := map[string]struct {
		// end interrupts with End instead of the caller's cancellation.
		end bool
		// rejectPast refuses every later call that asks for a deadline in the past.
		rejectPast bool
		// failArm makes the parked arm itself fail.
		failArm bool
	}{
		"error: the caller cancels between the claim and the Write, and a second acknowledgment succeeds": {},
		"error: the generation ends between the claim and the Write":                                      {end: true},
		"error: the caller cancels during the initial arm on a connection that refuses a past deadline, and a second acknowledgment succeeds on it": {
			rejectPast: true,
		},
		"error: the caller cancels during an arm that fails, and a second acknowledgment succeeds": {failArm: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			guardedBubble(t, func(t *testing.T, at func(string)) {
				at("setting up the reservation")
				r := newWrappedRegistry(t, nil)
				var pausing *pausingDeadlineConn
				g := r.reserve(t, "gen-a", 41001, 4101, reserveOptions{wrap: func(gc *gatedConn) net.Conn {
					pausing = newPausingDeadlineConn(gc)
					pausing.rejectPast = tt.rejectPast
					if tt.failArm {
						pausing.failPaused = errArm
					}
					return pausing
				}})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				pausing.arm()
				ack := startAck(ctx, g.res)
				at("waiting for the claimed writer's first deadline call")
				await(t, pausing.inside, "the acknowledgment's first deadline call on the parent end")
				var ended chan error
				if tt.end {
					ended = make(chan error, 1)
					go func() { ended <- g.res.End() }()
				} else {
					cancel()
				}
				at("letting the interruption settle while the deadline call is parked")
				synctest.Wait()
				pausing.release()
				at("the interrupted acknowledgment")
				err := ack.wait(t, "the interrupted AcknowledgeDrain")
				if offered, writes := g.parent.attempts(); writes != 0 {
					t.Errorf("an acknowledgment interrupted before its Write made %d Write calls (%q); it must make none", writes, offered)
				}
				if err == nil {
					t.Fatal("the interrupted acknowledgment reported success")
				}
				if tt.end {
					at("End after the interrupted acknowledgment")
					if err := await(t, ended, "End"); err != nil {
						t.Errorf("End: %v", err)
					}
					if !isClosed(g.listener.closed) {
						t.Error("End did not release the listener")
					}
					return
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("AcknowledgeDrain cancelled before its Write = %v, want %v", err, context.Canceled)
				}
				if n := pausing.pastAsked.Load(); n != 0 {
					t.Errorf("a deadline in the past was asked of the parent end %d times after a cancellation that came before the write was admitted; it must not set a deadline on the parent end", n)
				}
				if parentClosed(g.parent) {
					t.Error("the parent end was closed by a cancellation that came before the write was admitted; it must stay live")
				}
				if isClosed(g.listener.closed) {
					t.Fatal("the acknowledgment cancelled before its Write released the listener")
				}

				at("the second acknowledgment")
				g.parent.openGate()
				if err := startAck(t.Context(), g.res).wait(t, "the second AcknowledgeDrain"); err != nil {
					t.Fatalf("second AcknowledgeDrain after a cancellation before the write = %v; the acknowledgment must stay retryable on the same live connection", err)
				}
				want := frame("retired", "gen-a")
				if offered, writes := g.parent.attempts(); offered != want {
					t.Errorf("bytes offered to the drain channel %q in %d Write calls, want the one line %q", offered, writes, want)
				}
				if got := readExactly(t, g.child, len(want)); got != want {
					t.Errorf("acknowledgment %q, want %q", got, want)
				}
				if !isClosed(g.listener.closed) {
					t.Error("the second acknowledgment did not release the listener")
				}
			})
		})
	}
}

// completedWriteConn holds, after it went through, the Write that completes the acknowledgment's line: its bytes are
// on the channel, and the Write returns only when the test calls release. Open the gated end below it, so that every
// Write goes through to the real end.
type completedWriteConn struct {
	*gatedConn
	line      int
	completed chan struct{}
	proceed   chan struct{}
	once      sync.Once

	mu     sync.Mutex
	sent   int
	parked bool
}

func newCompletedWriteConn(g *gatedConn, line int) *completedWriteConn {
	return &completedWriteConn{gatedConn: g, line: line, completed: make(chan struct{}), proceed: make(chan struct{})}
}

func (c *completedWriteConn) Write(p []byte) (int, error) {
	n, err := c.gatedConn.Write(p)
	c.mu.Lock()
	c.sent += n
	park := err == nil && !c.parked && c.sent >= c.line
	if park {
		c.parked = true
		close(c.completed)
	}
	c.mu.Unlock()
	if park {
		<-c.proceed
	}
	return n, err
}

func (c *completedWriteConn) release() { c.once.Do(func() { close(c.proceed) }) }

// parkedCloseListener is a reservation's listener whose first Close waits for the test, then closes the fake listener
// below it and returns err. It records whether that first Close has returned. A later Close neither waits nor fails.
type parkedCloseListener struct {
	*fakeListener
	err     error
	closing chan struct{}
	allow   chan struct{}

	first, allowed sync.Once
	returned       atomic.Bool
}

func (l *parkedCloseListener) Close() error {
	first := false
	l.first.Do(func() {
		first = true
		close(l.closing)
	})
	if !first {
		return l.fakeListener.Close()
	}
	<-l.allow
	_ = l.fakeListener.Close()
	l.returned.Store(true)
	return l.err
}

func (l *parkedCloseListener) release() { l.allowed.Do(func() { close(l.allow) }) }

// TestAcknowledgeDrainReleasesListenerBeforeSuccess checks End racing an acknowledgment whose Write went through
// completely. The test holds that Write after its bytes reached the channel and before it returns, starts End, lets
// the bubble settle, and only then lets the Write return; it also holds the listener's first Close. Whoever closes the
// listener, the acknowledgment must not return nil before that Close has returned, and when the Close fails, the
// acknowledgment's error carries that failure. The listener is closed once and the channel carries the one line.
// What End returns is not decided here.
func TestAcknowledgeDrainReleasesListenerBeforeSuccess(t *testing.T) {
	errClose := errors.New("listener close failed")
	tests := map[string]struct {
		closeErr error
	}{
		"success: End races a complete Write and the listener's Close succeeds": {},
		"error: End races a complete Write and the listener's Close fails":      {closeErr: errClose},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			guardedBubble(t, func(t *testing.T, at func(string)) {
				at("setting up the reservation")
				var listener *parkedCloseListener
				r := newWrappedRegistry(t, func(inner workerlink.ListenFunc) workerlink.ListenFunc {
					return func(ctx context.Context, network, address string) (net.Listener, error) {
						l, err := inner(ctx, network, address)
						if err != nil {
							return nil, err
						}
						listener = &parkedCloseListener{fakeListener: l.(*fakeListener), err: tt.closeErr, closing: make(chan struct{}), allow: make(chan struct{})}
						return listener, nil
					}
				})
				want := frame("retired", "gen-a")
				var conn *completedWriteConn
				g := r.reserve(t, "gen-a", 41001, 4101, reserveOptions{wrap: func(gc *gatedConn) net.Conn {
					conn = newCompletedWriteConn(gc, len(want))
					return conn
				}})
				// Registered after End's cleanup, so it runs first.
				t.Cleanup(listener.release)
				g.parent.openGate()
				ack := startAck(t.Context(), g.res)
				at("waiting for the Write that completes the line")
				await(t, conn.completed, "the Write that completes the acknowledgment's line")

				at("End while the completed Write has not returned")
				ended := make(chan error, 1)
				go func() { ended <- g.res.End() }()
				synctest.Wait()
				if ack.poll() {
					t.Fatalf("AcknowledgeDrain returned %v before its Write returned", ack.err)
				}
				at("the completed Write returns")
				conn.release()
				synctest.Wait()
				if ack.poll() && ack.err == nil && !listener.returned.Load() {
					t.Fatalf("AcknowledgeDrain returned nil before the listener's Close returned (Close begun: %t); an acknowledgment succeeds only once the listener is released", isClosed(listener.closing))
				}

				at("the listener's Close returns")
				listener.release()
				err := ack.wait(t, "AcknowledgeDrain")
				endErr := await(t, ended, "End")
				t.Logf("AcknowledgeDrain = %v; End = %v", err, endErr)
				switch {
				case tt.closeErr == nil && err != nil:
					t.Errorf("AcknowledgeDrain = %v; want nil, the line went out and the listener was closed", err)
				case tt.closeErr != nil && !errors.Is(err, tt.closeErr):
					t.Errorf("AcknowledgeDrain = %v; want an error that carries the listener's Close failure %v", err, tt.closeErr)
				}
				if !listener.returned.Load() {
					t.Error("the listener's Close never returned")
				}
				if n := g.listener.closings.Load(); n != 1 {
					t.Errorf("the listener was closed %d times, want once", n)
				}
				if got := readExactly(t, g.child, len(want)); got != want {
					t.Errorf("acknowledgment %q, want %q", got, want)
				}
			})
		})
	}
}

// interruptionSentinels are the errors an acknowledgment's failure may match; which of them it matches is its
// classification.
var interruptionSentinels = map[string]error{
	"context.Canceled":           context.Canceled,
	"context.DeadlineExceeded":   context.DeadlineExceeded,
	"os.ErrDeadlineExceeded":     os.ErrDeadlineExceeded,
	"net.ErrClosed":              net.ErrClosed,
	"io.ErrShortWrite":           io.ErrShortWrite,
	"workerlink.ErrUnavailable":  workerlink.ErrUnavailable,
	"workerlink.ErrRetired":      workerlink.ErrRetired,
	"workerlink.ErrDrainMessage": workerlink.ErrDrainMessage,
}

// classification reports which interruptionSentinels err matches.
func classification(err error) map[string]bool {
	got := make(map[string]bool, len(interruptionSentinels))
	for name, sentinel := range interruptionSentinels {
		got[name] = errors.Is(err, sentinel)
	}
	return got
}

// TestAcknowledgeDrainFirstInterruptionDecides checks that the first interruption of a write decides its error. The
// budget's own error is measured first on a sibling reservation of the same registry, with nothing else interrupting
// its Write. Then the target's Write is interrupted by the budget and held after it chose its error and before it
// returns; while it is held the caller cancels, or End begins, and only then does the Write return. The target's error
// must match exactly the sentinels the budget's own error matches: the later cancellation or End neither replaces nor
// reclassifies it. Which error the budget gives is the implementation's; only that it stays the budget's is checked.
func TestAcknowledgeDrainFirstInterruptionDecides(t *testing.T) {
	tests := map[string]struct {
		// end interrupts with End instead of the caller's cancellation.
		end bool
	}{
		"error: a cancellation after the budget expired keeps the budget's error": {},
		"error: an End after the budget expired keeps the budget's error":         {end: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			guardedBubble(t, func(t *testing.T, at func(string)) {
				at("setting up the reservations")
				r := newWrappedRegistry(t, nil)
				g := r.reserve(t, "gen-a", 41001, 4101, reserveOptions{park: true})
				sibling := r.reserve(t, "gen-sibling", 41003, 4103, reserveOptions{park: true})

				at("the budget's own error on the sibling")
				sibAck := startAck(t.Context(), sibling.res)
				await(t, sibling.parent.entered, "the sibling acknowledgment's write")
				budgetErr := awaitLeftWrite(t, sibling.gatedReservation, sibAck)
				if budgetErr == nil {
					t.Fatal("the sibling AcknowledgeDrain reported success for a line the worker never read")
				}
				if err := sibling.res.End(); err != nil {
					t.Fatalf("End of the sibling: %v", err)
				}

				at("the target's Write interrupted by the budget")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ack := startAck(ctx, g.res)
				await(t, g.parent.entered, "the target acknowledgment's write")
				await(t, g.parent.returning, "the budget's interruption of the target's Write")
				later := "cancellation"
				var ended chan error
				if tt.end {
					later = "End"
					ended = make(chan error, 1)
					go func() { ended <- g.res.End() }()
				} else {
					cancel()
				}
				at("the later interruption while the interrupted Write is held")
				synctest.Wait()
				if ack.poll() {
					t.Fatalf("AcknowledgeDrain returned %v while its interrupted Write had not returned yet", ack.err)
				}
				g.parent.releaseReturns()
				err := ack.wait(t, "AcknowledgeDrain")
				await(t, g.parent.exited, "the return of the interrupted Write")
				if tt.end {
					if endErr := await(t, ended, "End"); endErr != nil {
						t.Errorf("End: %v", endErr)
					}
				}
				if err == nil {
					t.Fatal("AcknowledgeDrain reported success for a Write the budget interrupted")
				}
				if diff := gocmp.Diff(classification(budgetErr), classification(err)); diff != "" {
					t.Errorf("the error of a Write the budget interrupted before the later %s is classified differently from the budget's own error (-budget +after the %s):\n%s\nbudget's own error: %v\nerror: %v", later, later, diff, budgetErr, err)
				}
			})
		})
	}
}

// failedWriteConn fails every Write with err, an error the Write chooses itself before anything else can happen, and
// holds the first such Write after that choice and before it returns, until release. returning is closed when the
// first Write has chosen its error.
type failedWriteConn struct {
	*gatedConn
	err       error
	returning chan struct{}
	proceed   chan struct{}
	writes    atomic.Int32

	first, once sync.Once
}

func newFailedWriteConn(g *gatedConn, err error) *failedWriteConn {
	return &failedWriteConn{gatedConn: g, err: err, returning: make(chan struct{}), proceed: make(chan struct{})}
}

func (c *failedWriteConn) Write([]byte) (int, error) {
	c.writes.Add(1)
	park := false
	c.first.Do(func() {
		park = true
		close(c.returning)
	})
	if park {
		<-c.proceed
	}
	return 0, c.err
}

func (c *failedWriteConn) release() { c.once.Do(func() { close(c.proceed) }) }

// TestAcknowledgeDrainKeepsWriteError checks a Write that fails by itself: the connection's Write has already chosen
// EPIPE, the error of a peer that is gone, and is held after that choice and before it returns. While it is held the
// caller cancels, or End begins, and only then does the Write return. Neither the cancellation nor End caused that
// error, so the acknowledgment returns an error that matches EPIPE and not the later cancellation or End. Only a
// timeout of a deadline the acknowledgment armed, or the close error of its own interruption, may be translated.
func TestAcknowledgeDrainKeepsWriteError(t *testing.T) {
	errPipe := &net.OpError{Op: "write", Net: "unix", Err: os.NewSyscallError("write", syscall.EPIPE)}
	tests := map[string]struct {
		// end interrupts with End instead of the caller's cancellation.
		end bool
		// later is the error of the later interruption, which the result must not match.
		later error
	}{
		"error: a cancellation after the Write chose EPIPE keeps EPIPE": {later: context.Canceled},
		"error: an End after the Write chose EPIPE keeps EPIPE":         {end: true, later: workerlink.ErrUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			guardedBubble(t, func(t *testing.T, at func(string)) {
				at("setting up the reservation")
				r := newWrappedRegistry(t, nil)
				var failing *failedWriteConn
				g := r.reserve(t, "gen-a", 41001, 4101, reserveOptions{wrap: func(gc *gatedConn) net.Conn {
					failing = newFailedWriteConn(gc, errPipe)
					return failing
				}})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ack := startAck(ctx, g.res)
				at("waiting for the Write to choose its error")
				await(t, failing.returning, "the Write that chose EPIPE")
				var ended chan error
				if tt.end {
					ended = make(chan error, 1)
					go func() { ended <- g.res.End() }()
				} else {
					cancel()
				}
				at("the later interruption while the failed Write is held")
				synctest.Wait()
				if ack.poll() {
					t.Fatalf("AcknowledgeDrain returned %v while its Write had not returned yet", ack.err)
				}
				failing.release()
				err := ack.wait(t, "AcknowledgeDrain")
				if tt.end {
					if endErr := await(t, ended, "End"); endErr != nil {
						t.Errorf("End: %v", endErr)
					}
				}
				if n := failing.writes.Load(); n != 1 {
					t.Errorf("%d Write calls, want the one that failed", n)
				}
				if !errors.Is(err, syscall.EPIPE) {
					t.Errorf("AcknowledgeDrain = %v; want an error that matches the Write's own %v", err, syscall.EPIPE)
				}
				if errors.Is(err, tt.later) {
					t.Errorf("AcknowledgeDrain = %v matches %v, which came after the Write had chosen its error and did not cause it", err, tt.later)
				}
				if !tt.end {
					checkFailedAckKeeps(t, r.reg, g.gatedReservation)
				}
			})
		})
	}
}

// entryDeadlineConn records the write deadline the gated end below it holds when the first Write begins.
type entryDeadlineConn struct {
	*gatedConn

	mu      sync.Mutex
	seen    bool
	atEntry time.Time
}

func (c *entryDeadlineConn) Write(p []byte) (int, error) {
	c.gatedConn.mu.Lock()
	deadline := c.deadline
	c.gatedConn.mu.Unlock()
	c.mu.Lock()
	if !c.seen {
		c.seen, c.atEntry = true, deadline
	}
	c.mu.Unlock()
	return c.gatedConn.Write(p)
}

func (c *entryDeadlineConn) deadlineAtEntry() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.atEntry
}

// TestAcknowledgeDrainArmsCallerDeadline checks that a caller's deadline earlier than the budget is armed as the write
// deadline itself before the Write begins, so that it ends the write at that deadline whenever a callback of the
// context happens to run. The target reservation is prepared first, the budget is measured on a sibling reservation
// of the same registry, and the caller's context gets half of it, created just before the call. When the Write
// begins, the parent end must already hold exactly the caller's deadline as its write deadline. An acknowledgment that
// returns context.DeadlineExceeded before any Write is entered is also correct; then nothing about the armed deadline
// is observed. When the measured budget leaves no room for a shorter deadline, the case logs that it could not be
// constructed and asserts nothing.
func TestAcknowledgeDrainArmsCallerDeadline(t *testing.T) {
	tests := map[string]struct {
		target, sibling workerlink.Generation
	}{
		"error: the caller's deadline before the budget is the armed write deadline": {target: "gen-a", sibling: "gen-sibling"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			guardedBubble(t, func(t *testing.T, at func(string)) {
				r := newWrappedRegistry(t, nil)
				at("preparing the target reservation")
				var recorder *entryDeadlineConn
				g := r.reserve(t, tt.target, 41001, 4101, reserveOptions{park: true, wrap: func(gc *gatedConn) net.Conn {
					recorder = &entryDeadlineConn{gatedConn: gc}
					return recorder
				}})

				at("measuring the budget on a sibling reservation of the same registry")
				sibling := r.reserve(t, tt.sibling, 41003, 4103, reserveOptions{park: true})
				sibAck := startAck(t.Context(), sibling.res)
				await(t, sibling.parent.entered, "the sibling acknowledgment's write")
				if err := awaitLeftWrite(t, sibling.gatedReservation, sibAck); err == nil {
					t.Fatal("the sibling AcknowledgeDrain reported success for a line the worker never read")
				}
				sibEntry, _ := sibling.parent.heldSpan()
				budget := sibling.parent.interruption().Sub(sibEntry)
				if err := sibling.res.End(); err != nil {
					t.Fatalf("End of the sibling: %v", err)
				}
				if budget <= 0 {
					t.Logf("the budget measured on the sibling is %v; no deadline strictly shorter than it can be placed, so the case was not constructed and nothing is checked", budget)
					return
				}

				ctx, cancel := context.WithTimeout(t.Context(), budget/2)
				defer cancel()
				deadline, _ := ctx.Deadline()
				ack := startAck(ctx, g.res)
				at("waiting for the target's Write to begin or its acknowledgment to return")
				select {
				case err := <-ack.done:
					ack.returned, ack.err = true, err
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("AcknowledgeDrain before any Write = %v, want %v", err, context.DeadlineExceeded)
					}
					if offered, writes := g.parent.attempts(); writes != 0 {
						t.Errorf("AcknowledgeDrain returned before its Write was held, yet %d Write calls were made (%q)", writes, offered)
					}
					t.Log("the acknowledgment ended before its Write; the armed deadline was not observed")
				case <-g.parent.entered:
					entry, _ := g.parent.heldSpan()
					switch armed := recorder.deadlineAtEntry(); {
					case armed.IsZero():
						t.Errorf("no write deadline was armed when the Write began; the caller's deadline, %v later, must be", deadline.Sub(entry))
					case !armed.Equal(deadline):
						t.Errorf("the write deadline armed when the Write began is %v from the caller's deadline (%v after the Write began, the caller's deadline %v after it); the earlier of the caller's deadline and the budget must be the armed deadline", armed.Sub(deadline), armed.Sub(entry), deadline.Sub(entry))
					}
					at("the target's Write interrupted and held before it returns")
					err := awaitLeftWrite(t, g.gatedReservation, ack)
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("AcknowledgeDrain whose Write the caller's deadline ended = %v, want %v", err, context.DeadlineExceeded)
					}
				case <-time.After(watchdog):
					t.Fatalf("neither the Write began nor AcknowledgeDrain returned within %v", watchdog)
				}
				checkFailedAckKeeps(t, r.reg, g.gatedReservation)
			})
		})
	}
}

// refusingDeadlineConn refuses deadline calls with err instead of passing them to the gated end below it: every call;
// with afterEntry, every call made once the held Write has begun; with once, only the first call. It counts the
// refusals.
type refusingDeadlineConn struct {
	*gatedConn
	err        error
	afterEntry bool
	once       bool
	refusals   atomic.Int32
}

func (c *refusingDeadlineConn) refuse() bool {
	switch {
	case c.afterEntry && !isClosed(c.entered):
		return false
	case c.once:
		return c.refusals.CompareAndSwap(0, 1)
	}
	c.refusals.Add(1)
	return true
}

func (c *refusingDeadlineConn) SetWriteDeadline(t time.Time) error {
	if c.refuse() {
		return c.err
	}
	return c.gatedConn.SetWriteDeadline(t)
}

func (c *refusingDeadlineConn) SetDeadline(t time.Time) error {
	if c.refuse() {
		return c.err
	}
	return c.gatedConn.SetDeadline(t)
}

// TestAcknowledgeDrainRefusedDeadline checks a parent end that refuses to set a deadline. When no deadline can be set
// at all, the budget cannot be armed: the acknowledgment fails before it enters Write, its error carries the refusal,
// and the listener and the retirement are kept. When the arm is refused only once, nothing was written, so a second
// acknowledgment on the same reservation arms the deadline, writes the one line and succeeds. When the deadline is
// armed but every deadline call after the Write began is refused, the caller's cancellation still ends the Write at
// once, by closing the parent end, and the acknowledgment returns, at the instant of the cancellation and after its
// Write has left, an error that matches both the context's error and the refusal.
func TestAcknowledgeDrainRefusedDeadline(t *testing.T) {
	errDeadline := errors.New("deadline refused by the connection")
	tests := map[string]struct {
		// afterEntry refuses only the deadline calls made once the Write began.
		afterEntry bool
		// once refuses only the first deadline call, and a second acknowledgment follows.
		once bool
	}{
		"error: the budget cannot be armed":                                        {},
		"error: the arm is refused once, and a second acknowledgment succeeds":     {once: true},
		"error: the cancellation cannot set the deadline on the Write in progress": {afterEntry: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			guardedBubble(t, func(t *testing.T, at func(string)) {
				at("setting up the reservation")
				r := newWrappedRegistry(t, nil)
				var refusing *refusingDeadlineConn
				g := r.reserve(t, "gen-a", 41001, 4101, reserveOptions{park: tt.afterEntry, wrap: func(gc *gatedConn) net.Conn {
					refusing = &refusingDeadlineConn{gatedConn: gc, err: errDeadline, afterEntry: tt.afterEntry, once: tt.once}
					return refusing
				}})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ack := startAck(ctx, g.res)

				if !tt.afterEntry {
					at("waiting for the acknowledgment to fail or to enter Write")
					select {
					case err := <-ack.done:
						ack.returned, ack.err = true, err
					case <-g.parent.entered:
						t.Fatalf("the acknowledgment entered Write although the parent end refused every deadline (%d refusals); with no budget armed it must fail before the Write", refusing.refusals.Load())
					case <-time.After(watchdog):
						t.Fatalf("AcknowledgeDrain neither returned nor entered Write within %v", watchdog)
					}
					if offered, writes := g.parent.attempts(); writes != 0 {
						t.Errorf("an acknowledgment whose budget could not be armed made %d Write calls (%q); it must make none", writes, offered)
					}
					if ack.err == nil {
						t.Fatal("AcknowledgeDrain reported success although no deadline could be armed")
					}
					if !errors.Is(ack.err, errDeadline) {
						t.Errorf("AcknowledgeDrain = %v; want an error that carries the refusal %v", ack.err, errDeadline)
					}
					checkFailedAckKeeps(t, r.reg, g.gatedReservation)
					if !tt.once {
						return
					}
					at("the second acknowledgment after the refused arm")
					g.parent.openGate()
					if err := startAck(t.Context(), g.res).wait(t, "the second AcknowledgeDrain"); err != nil {
						t.Fatalf("second AcknowledgeDrain after an arm refused once = %v; a refused arm writes nothing, so the acknowledgment must stay retryable", err)
					}
					want := frame("retired", "gen-a")
					if offered, writes := g.parent.attempts(); offered != want {
						t.Errorf("bytes offered to the drain channel %q in %d Write calls, want the one line %q", offered, writes, want)
					}
					if got := readExactly(t, g.child, len(want)); got != want {
						t.Errorf("acknowledgment %q, want %q", got, want)
					}
					if !isClosed(g.listener.closed) {
						t.Error("the second acknowledgment did not release the listener")
					}
					return
				}

				at("waiting for the acknowledgment's Write")
				await(t, g.parent.entered, "the acknowledgment's write")
				synctest.Wait()
				if ack.poll() {
					t.Fatalf("AcknowledgeDrain returned %v before anything ended its write", ack.err)
				}
				cancelled := time.Now()
				cancel()
				at("the Write interrupted after the cancellation and held before it returns")
				err := awaitLeftWrite(t, g.gatedReservation, ack)
				if waited := g.parent.interruption().Sub(cancelled); waited != 0 {
					t.Errorf("the Write was interrupted %v of the bubble's time after the cancellation, want at once; a cancellation that cannot set the deadline must close the parent end", waited)
				}
				if waited := ack.at.Sub(cancelled); waited != 0 {
					t.Errorf("AcknowledgeDrain returned %v of the bubble's time after the cancellation, want at once", waited)
				}
				if !parentClosed(g.parent) {
					t.Error("the parent end was not closed although the cancellation could not set its deadline")
				}
				if !errors.Is(err, context.Canceled) || !errors.Is(err, errDeadline) {
					t.Errorf("AcknowledgeDrain = %v; want an error that matches both %v and the refusal %v (%d deadline calls refused after the Write began)", err, context.Canceled, errDeadline, refusing.refusals.Load())
				}
				checkFailedAckKeeps(t, r.reg, g.gatedReservation)
			})
		})
	}
}
