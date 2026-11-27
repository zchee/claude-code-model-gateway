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

// Package workerlink owns the supervisor's per-generation worker listener and the connection leases the public
// request path takes to the current worker generation.
//
// One mutex per Registry guards the state of the registry and of all its reservations. It is held only to read or
// change that state, never across a listen, a dial, a Close or a Write, so a slow peer of one generation cannot
// stall the others. Every wait (for pinned dials, for an acknowledgment's writer, for a concurrent End) is a wait
// on a channel.
package workerlink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Generation identifies one attempted worker process and the listener reserved for it.
type Generation string

// ListenFunc binds a listener for a network and address, as net.ListenConfig.Listen does.
type ListenFunc func(context.Context, string, string) (net.Listener, error)

// DialFunc opens a connection to a network and address, as net.Dialer.DialContext does.
type DialFunc func(context.Context, string, string) (net.Conn, error)

// Lease is an already connected socket to one worker generation. Close releases the lease.
type Lease interface {
	net.Conn
	// Generation is the generation the connection reached.
	Generation() Generation
	// PID is the process identifier published for that generation.
	PID() int
	// Authority is the value for the HTTP Host field, never an address to open another connection to.
	Authority() string
	// Exited is closed when the generation dies or is finally stopped, not when its admission is withdrawn.
	Exited() <-chan struct{}
}

// Source gives connection leases to the current eligible worker generation.
type Source interface {
	// Acquire returns a lease to a current eligible generation other than the excluded one; an empty exclusion
	// admits any current eligible generation.
	Acquire(context.Context, Generation) (Lease, error)
}

// ackWriteBudget bounds the write of every drain acknowledgment of a Registry. The worker allows five seconds for
// its whole drain exchange, of which the supervisor may already have spent some waiting for pinned dials; a write
// still blocked after two seconds means the worker has stopped reading its channel, and failing it then lets the
// manager stop that worker while the worker itself still waits.
const ackWriteBudget = 2 * time.Second

// loopbackHost is the only address a worker listener is bound to and dialled at.
const loopbackHost = "127.0.0.1"

// loopbackIP is loopbackHost parsed once for the address checks.
var loopbackIP = net.IPv4(127, 0, 0, 1)

// Registry holds the reservations of the worker generations and gives leases to the eligible one.
type Registry struct {
	// drainPair creates the drain channel pair of each reservation: the parent's connection and the child's file.
	// It is newDrainPair unless the package's own tests replace it, so that they can make one pair fail or watch
	// the parent's writes.
	drainPair func() (parent net.Conn, child *os.File, err error)

	listen ListenFunc
	dial   DialFunc
	// ackBudget is the write budget of every acknowledgment of this registry, fixed here so that all its
	// reservations share one value.
	ackBudget time.Duration

	// mu guards the fields below and the mutable fields of every reservation of this registry.
	mu sync.Mutex
	// live holds the reservations that have not ended.
	live []*Reservation
	// eligible is the one published reservation whose admission is open, or nil.
	eligible *Reservation
}

// ackState is how far a reservation's drain acknowledgment has got.
type ackState int

const (
	// ackNone means no acknowledgment holds the claim or has entered a write; one may try.
	ackNone ackState = iota
	// ackWriting means one acknowledgment holds the claim to write the line; others join it.
	ackWriting
	// ackSent means the line was written completely and the listener was released.
	ackSent
	// ackFailed means a write was entered and failed; the line may be partial, so it is never written again.
	ackFailed
)

// Reservation is the supervisor's retained listener and drain channel for one worker generation.
type Reservation struct {
	reg        *Registry
	generation Generation
	port       uint16
	authority  string
	listener   net.Listener
	parent     net.Conn
	// endCtx ends when End begins. Its Done channel is every lease's Exited channel, and it cancels the dials that
	// pinned this generation.
	endCtx    context.Context
	endCancel context.CancelFunc

	// The fields below are guarded by reg.mu.
	child        *os.File
	channelTaken bool
	pid          int
	retired      bool
	ended        bool
	// pins counts the dials admitted to this generation that have not settled; pinsSettled is closed when the count
	// falls to zero and replaced when it rises from zero.
	pins        int
	pinsSettled chan struct{}
	// listenerReleased records that the acknowledgment or End has taken the listener to close it.
	listenerReleased bool
	ack              ackState
	// ackDone is closed when the claimed writer has finished: its outcome is recorded in ackErr, or its claim was
	// released before any Write.
	ackDone chan struct{}
	ackErr  error
	// ackCause is the first interruption of the claimed write, recorded by the cancellation callback or by End at
	// ackCauseAt; ackInterruptErr is a failure to end that write through its deadline.
	ackCause        error
	ackCauseAt      time.Time
	ackInterruptErr error
	// endDone is closed when the first End has finished; endErr is its result.
	endDone chan struct{}
	endErr  error
}

// ReportFD is the production child's inherited listening-report descriptor.
const ReportFD = 3

// LivenessFD is the production child's inherited parent-liveness descriptor.
const LivenessFD = 4

// ListenerFD is the production child's inherited TCP listener descriptor.
const ListenerFD = 5

// DrainFD is the production child's inherited duplex drain descriptor.
const DrainFD = 6

// ListenerFDEnv names the variable carrying the inherited TCP listener descriptor.
const ListenerFDEnv = "MODEL_GATEWAY_WORKER_LISTENER_FD"

// DrainFDEnv names the variable carrying the inherited local drain descriptor.
const DrainFDEnv = "MODEL_GATEWAY_WORKER_DRAIN_FD"

// ErrUnavailable denotes that no eligible worker generation can take a lease.
var ErrUnavailable = errors.New("worker unavailable")

// ErrRetired denotes that the worker generation's admission has been withdrawn.
var ErrRetired = errors.New("worker generation retired")

// NewRegistry returns a registry that binds reservations with listen and opens leases with dial.
func NewRegistry(listen ListenFunc, dial DialFunc) *Registry {
	return &Registry{drainPair: newDrainPair, listen: listen, dial: dial, ackBudget: ackWriteBudget}
}

// Reserve binds the generation's loopback TCP listener on port, zero selecting one, and its drain channel.
//
// Setup is all or nothing: a listener at any other endpoint than 127.0.0.1 and the requested port, a drain pair that
// cannot be made, or a generation that is empty, cannot be framed on the drain channel or is already reserved
// leaves nothing open. The reservation never accepts on its listener; it only keeps the address and the accept queue
// for the child.
func (r *Registry) Reserve(ctx context.Context, generation Generation, port uint16) (*Reservation, error) {
	if _, err := drainLine(DrainMessage{Type: Draining, Generation: generation}); err != nil {
		// The length, not the value: a generation that cannot be framed may be arbitrarily long.
		return nil, fmt.Errorf("workerlink: a generation of %d bytes cannot be reserved: %w", len(generation), err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	address := net.JoinHostPort(loopbackHost, strconv.Itoa(int(port)))
	l, err := r.listen(ctx, "tcp4", address)
	if err != nil {
		return nil, fmt.Errorf("workerlink: bind %s for generation %q: %w", address, generation, err)
	}
	if l == nil {
		return nil, fmt.Errorf("workerlink: bind %s for generation %q returned no listener", address, generation)
	}
	bound, err := loopbackPort(l.Addr(), port)
	if err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("workerlink: listener for generation %q refused: %w", generation, err)
	}
	parent, child, err := r.drainPair()
	if err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("workerlink: drain channel for generation %q: %w", generation, err)
	}
	endCtx, endCancel := context.WithCancel(context.Background())
	res := &Reservation{
		reg:        r,
		generation: generation,
		port:       bound,
		authority:  net.JoinHostPort(loopbackHost, strconv.Itoa(int(bound))),
		listener:   l,
		parent:     parent,
		endCtx:     endCtx,
		endCancel:  endCancel,
		child:      child,
	}
	r.mu.Lock()
	duplicate := slices.ContainsFunc(r.live, func(other *Reservation) bool { return other.generation == generation })
	if !duplicate {
		r.live = append(r.live, res)
	}
	r.mu.Unlock()
	if duplicate {
		endCancel()
		_ = parent.Close()
		_ = child.Close()
		_ = l.Close()
		return nil, fmt.Errorf("workerlink: generation %q is already reserved", generation)
	}
	return res, nil
}

// loopbackPort returns the port of addr when addr is exactly 127.0.0.1 at a nonzero port, and at want unless want is
// zero. Reserve and the listener import share it, so the supervisor and the worker accept the same endpoints.
func loopbackPort(addr net.Addr, want uint16) (uint16, error) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("address %v is not a TCP address", addr)
	}
	if ip := tcp.IP.To4(); ip == nil || !ip.Equal(loopbackIP) {
		return 0, fmt.Errorf("address %v is not %s", addr, loopbackHost)
	}
	if tcp.Port <= 0 || tcp.Port > 65535 {
		return 0, fmt.Errorf("address %v has no usable port", addr)
	}
	if want != 0 && tcp.Port != int(want) {
		return 0, fmt.Errorf("address %v is not at port %d", addr, want)
	}
	return uint16(tcp.Port), nil
}

// Acquire returns a connected lease to a current eligible generation other than exclude.
//
// It answers at once when no generation is eligible, and the relay does any waiting. An admitted dial pins its
// generation: Retire leaves the pin and hands a successful dial over, End cancels the dial and closes a connection
// that completes anyway. A dial that fails leaves the generation eligible.
func (r *Registry) Acquire(ctx context.Context, exclude Generation) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	res := r.eligible
	if res == nil || res.generation == exclude {
		err := r.refusalLocked(exclude)
		r.mu.Unlock()
		return nil, err
	}
	res.pinLocked()
	r.mu.Unlock()

	dialCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(res.endCtx, cancel)
	conn, err := r.dial(dialCtx, "tcp4", res.authority)
	stop()
	cancel()

	r.mu.Lock()
	if err == nil && !res.ended {
		l := &lease{Conn: conn, res: res, pid: res.pid}
		res.unpinLocked()
		r.mu.Unlock()
		return l, nil
	}
	r.mu.Unlock()
	// A connection that completed after End is closed before the pin is released, so that End, which waits for the
	// pin, never returns with it still open.
	if err == nil {
		_ = conn.Close()
	}
	r.mu.Lock()
	res.unpinLocked()
	r.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("workerlink: connect to generation %q at %s: %w", res.generation, res.authority, err)
	}
	return nil, fmt.Errorf("workerlink: generation %q ended while its connection was opened: %w", res.generation, ErrUnavailable)
}

// refusalLocked returns ErrRetired while a retired reservation other than exclude still exists, and ErrUnavailable
// otherwise.
func (r *Registry) refusalLocked(exclude Generation) error {
	for _, res := range r.live {
		if res.retired && res.generation != exclude {
			return ErrRetired
		}
	}
	return ErrUnavailable
}

// pinLocked counts one more dial admitted to the generation.
func (r *Reservation) pinLocked() {
	if r.pins == 0 {
		r.pinsSettled = make(chan struct{})
	}
	r.pins++
}

// unpinLocked settles one admitted dial and wakes the waiters when it was the last.
func (r *Reservation) unpinLocked() {
	r.pins--
	if r.pins == 0 {
		close(r.pinsSettled)
	}
}

// awaitPins returns when no admitted dial pins the generation, when ctx ends (with its error) or when abort is
// closed (with nil).
func (r *Reservation) awaitPins(ctx context.Context, abort <-chan struct{}) error {
	for {
		r.reg.mu.Lock()
		if r.pins == 0 {
			r.reg.mu.Unlock()
			return nil
		}
		settled := r.pinsSettled
		r.reg.mu.Unlock()
		select {
		case <-settled:
		case <-ctx.Done():
			return ctx.Err()
		case <-abort:
			return nil
		}
	}
}

// Port returns the reserved port the listener is bound to.
func (r *Reservation) Port() uint16 { return r.port }

// File returns a duplicate of the reserved listener for the child; the caller owns and closes it.
//
// The duplicate is close-on-exec, so that only the child the caller passes it to inherits it. After End or after the
// acknowledgment released the listener there is nothing left to duplicate.
func (r *Reservation) File() (*os.File, error) {
	r.reg.mu.Lock()
	gone := r.ended || r.listenerReleased
	r.reg.mu.Unlock()
	if gone {
		return nil, fmt.Errorf("workerlink: the listener of generation %q has been released", r.generation)
	}
	dup, ok := r.listener.(interface{ File() (*os.File, error) })
	if !ok {
		return nil, fmt.Errorf("workerlink: the listener of generation %q (%T) cannot be duplicated", r.generation, r.listener)
	}
	f, err := dup.File()
	if err != nil {
		return nil, fmt.Errorf("workerlink: duplicate the listener of generation %q: %w", r.generation, err)
	}
	return f, nil
}

// Publish makes the generation eligible for leases on behalf of the process pid.
//
// A retired or ended reservation is never published again, so a late publication cannot reopen admission. While
// another generation is eligible the publication is refused; that generation has to be retired or ended first, so
// that at most one generation is eligible at a time.
func (r *Reservation) Publish(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("workerlink: generation %q cannot be published for process %d", r.generation, pid)
	}
	reg := r.reg
	reg.mu.Lock()
	defer reg.mu.Unlock()
	switch {
	case r.ended:
		return fmt.Errorf("workerlink: generation %q has ended: %w", r.generation, ErrUnavailable)
	case r.retired:
		return fmt.Errorf("workerlink: generation %q cannot be published: %w", r.generation, ErrRetired)
	case r.pid != 0 && r.pid != pid:
		return fmt.Errorf("workerlink: generation %q is already published for process %d", r.generation, r.pid)
	case reg.eligible != nil && reg.eligible != r:
		return fmt.Errorf("workerlink: generation %q cannot be published while generation %q is eligible", r.generation, reg.eligible.generation)
	}
	r.pid = pid
	reg.eligible = r
	return nil
}

// Retire withdraws the generation's admission: no new lease is given for it.
//
// Dials that pinned the generation before are neither cancelled nor marked exited; each is handed over or fails by
// its own outcome. Retire is idempotent and has no effect after End.
func (r *Reservation) Retire() {
	reg := r.reg
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if r.ended {
		return
	}
	r.retired = true
	if reg.eligible == r {
		reg.eligible = nil
	}
}

// End finally stops the generation: it cancels unfinished dials, wakes Exited waiters and releases its files.
//
// It denies admission first and records itself as the interruption of a claimed acknowledgment write, then closes
// every lease's Exited channel, cancels the pinned dials and closes the drain channel's parent end, which also ends an
// acknowledgment write in progress. It keeps the listener until every pinned dial has settled and the acknowledgment's
// writer has finished; only then does it take the listener, unless that writer already released it. A repeated End
// waits for the first and returns its result.
func (r *Reservation) End() error {
	reg := r.reg
	reg.mu.Lock()
	if r.endDone != nil {
		done := r.endDone
		reg.mu.Unlock()
		<-done
		return r.endErr
	}
	r.endDone = make(chan struct{})
	r.ended = true
	r.interruptAckLocked(r.endedAckError())
	if reg.eligible == r {
		reg.eligible = nil
	}
	reg.live = slices.DeleteFunc(reg.live, func(other *Reservation) bool { return other == r })
	child := r.child
	r.child = nil
	reg.mu.Unlock()

	r.endCancel()
	var errs []error
	// The parent end is lent to the manager; a manager that already closed it leaves nothing to report.
	if err := r.parent.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		errs = append(errs, err)
	}
	// The context never ends here: only the pins themselves release this wait.
	_ = r.awaitPins(context.Background(), nil)
	// No acknowledgment claims the write after ended is set, so one wait covers every writer. The listener is taken
	// only after that writer has finished, so a writer whose line went out closes it itself and reports that Close.
	reg.mu.Lock()
	writing := r.ack == ackWriting
	ackDone := r.ackDone
	reg.mu.Unlock()
	if writing {
		<-ackDone
	}
	reg.mu.Lock()
	release := !r.listenerReleased
	r.listenerReleased = true
	reg.mu.Unlock()
	if release {
		if err := r.listener.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if child != nil {
		if err := child.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		r.endErr = fmt.Errorf("workerlink: end generation %q: %w", r.generation, err)
	}
	close(r.endDone)
	return r.endErr
}

// lease is a connection handed over for one generation.
type lease struct {
	net.Conn
	res *Reservation
	pid int
}

func (l *lease) Generation() Generation { return l.res.generation }

func (l *lease) PID() int { return l.pid }

func (l *lease) Authority() string { return l.res.authority }

func (l *lease) Exited() <-chan struct{} { return l.res.endCtx.Done() }

// ImportListener consumes an inherited listener descriptor and returns the listener when it is a listening
// loopback IPv4 TCP socket on the expected port.
//
// The file is closed on success and on failure; it is marked close-on-exec before anything else is checked, and the
// returned listener holds its own close-on-exec duplicate. A refused descriptor gives no listener and there is no
// fallback bind. Linux reports the listening state, so a socket that is not listening is refused there. macOS does
// not report it: there a bound loopback TCP socket that is not listening is returned too, and its first Accept
// fails, which the caller treats as a failed start.
func ImportListener(file *os.File, expected uint16) (net.Listener, error) {
	if file == nil {
		return nil, errors.New("workerlink: no inherited listener descriptor")
	}
	l, err := importListener(file, expected)
	if err != nil {
		return nil, fmt.Errorf("workerlink: inherited listener refused: %w", err)
	}
	return l, nil
}
