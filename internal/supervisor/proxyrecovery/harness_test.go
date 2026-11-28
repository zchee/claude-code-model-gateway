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

package proxyrecovery_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/proxyrecovery"
)

// proxyPort is the configured proxy port of every fake world; nothing binds it.
const proxyPort uint16 = 18765

// watchdog bounds every wait of a test inside its bubble. Fake time jumps to it only when every goroutine of the
// bubble is durably blocked, so it ends a hang with a message and never decides an expectation.
const watchdog = 24 * time.Hour

// wallBound bounds one bubble in real time. A goroutine that waits for a mutex is not durably blocked, so when a
// monitor holds a mutex across a collaborator call that a test holds open, fake time cannot advance, the watchdog
// never fires and synctest.Wait never returns. This bound runs outside the bubble and ends such a run with a message
// that names the test, well before the test binary's own timeout.
const wallBound = 2 * time.Minute

// clockOffset puts the injected clock far from the bubble's time.Now, so that a monitor that reads time.Now instead
// of Options.Now computes other retry times and fails.
const clockOffset = 1000*time.Hour + 17*time.Minute

// bubble runs f in a synctest bubble bounded by wallBound in real time.
func bubble(t *testing.T, f func(t *testing.T)) {
	t.Helper()
	name := t.Name()
	timer := time.AfterFunc(wallBound, func() {
		panic(fmt.Sprintf("%s: the bubble made no progress for %v of real time; a goroutine is probably waiting for a mutex that is held across a collaborator call the test holds open", name, wallBound))
	})
	defer timer.Stop()
	synctest.Test(t, f)
}

// errNoRecord is what a fake record read returns for a process number no record names.
var errNoRecord = errors.New("no proxy record names this process")

// event is one call of Options.Record.
type event struct {
	Name   string
	Fields map[string]any
}

// gate holds a collaborator call open until the test opens it or the call's context ends.
type gate struct {
	entered   chan struct{}
	release   chan struct{}
	ended     chan struct{}
	enterOnce sync.Once
	openOnce  sync.Once
	endOnce   sync.Once
	mu        sync.Mutex
	ctx       context.Context
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{})}
}

func (g *gate) enter(ctx context.Context) {
	g.enterOnce.Do(func() {
		g.mu.Lock()
		g.ctx = ctx
		g.mu.Unlock()
		close(g.entered)
	})
}

// hold blocks until the gate is opened or ctx ends, and reports which.
func (g *gate) hold(ctx context.Context) error {
	g.enter(ctx)
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		g.endOnce.Do(func() { close(g.ended) })
		return ctx.Err()
	}
}

// park blocks until the gate is opened. When ctx ends first it records that and keeps waiting, so that a caller can
// be seen waiting for a call that has been cancelled but has not returned; it then reports the context's error.
func (g *gate) park(ctx context.Context) error {
	g.enter(ctx)
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		g.endOnce.Do(func() { close(g.ended) })
		<-g.release
		return ctx.Err()
	}
}

// heldContext returns the context the first call that entered the gate received.
func (g *gate) heldContext() context.Context {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ctx
}

func (g *gate) open() { g.openOnce.Do(func() { close(g.release) }) }

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// child is a fake proxy process the world started.
type child struct {
	pid    int
	record procinfo.Record
	done   chan struct{}
	once   sync.Once
}

func (c *child) exit() { c.once.Do(func() { close(c.done) }) }

func (c *child) exited() bool { return closed(c.done) }

// proxyRecord is the record a publisher writes for a proxy with process number pid.
func proxyRecord(pid int) procinfo.Record {
	return procinfo.Record{
		PID:        pid,
		Command:    "/home/user/.claude/model-gateway/bin/claude-code-proxy serve --no-monitor",
		StartedAt:  procinfo.LegacyStart(time.Date(2026, time.November, 27, 9, 0, 0, 0, time.UTC)),
		Role:       "proxy",
		StateRoot:  "/home/user/.claude/model-gateway",
		Generation: fmt.Sprintf("proxy-generation-%d", pid),
		Port:       proxyPort,
		Version:    "1.2.3",
		Image:      procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/claude-code-proxy", Device: 64769, Inode: 5151},
		StartToken: procinfo.StartToken(fmt.Sprintf("2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/%d", 880000+pid)),
	}
}

// behaviour is what each collaborator of a world does when called. A test replaces the fields it cares about.
type behaviour struct {
	probe       func(context.Context) bool
	listening   func(context.Context) (bool, error)
	binary      func() bool
	resolve     func(context.Context) (ownership.Owner, error)
	start       func(context.Context) (proxyrecovery.Child, error)
	stopOwned   func(context.Context, procinfo.Record) error
	recorded    func(context.Context, int) (procinfo.Record, error)
	waitRelease func(context.Context) bool
	// record runs at the start of every Record call, outside the world's lock, so that a test can hold one.
	record func(name string)
}

// world is a fake system around one monitor. Every collaborator call is logged in order; the journal is kept apart.
// By default nothing answers, nothing listens, the binary exists, a holder is unknown, a start succeeds with a new
// child, a stop ends a child of this world, a record read finds a child of this world, and a port is released.
type world struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []string
	all      []string
	events   []event
	children []*child
	records  map[int]procinfo.Record
	// stopped lists the records StopOwned received, in order.
	stopped []procinfo.Record
	do      behaviour
	// offset is the injected clock's distance from the bubble's clock.
	offset time.Duration
	// clockReads counts the monitor's calls of Options.Now; the test's own reads of the clock are not counted.
	clockReads int
	// entered lists every Record call at its entry, before any hook a test placed can hold it.
	entered []string
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, records: map[int]procinfo.Record{}, offset: clockOffset}
	w.do = behaviour{
		probe:       func(context.Context) bool { return false },
		listening:   func(context.Context) (bool, error) { return false, nil },
		binary:      func() bool { return true },
		resolve:     func(context.Context) (ownership.Owner, error) { return ownership.Owner{Class: ownership.Unknown}, nil },
		start:       func(context.Context) (proxyrecovery.Child, error) { return w.spawn(), nil },
		stopOwned:   func(_ context.Context, r procinfo.Record) error { w.exit(r.PID); return nil },
		recorded:    w.lookup,
		waitRelease: func(context.Context) bool { return true },
	}
	// Whatever a test leaves running ends with it, so that the bubble can finish: every child exits.
	t.Cleanup(func() {
		w.mu.Lock()
		children := slices.Clone(w.children)
		w.mu.Unlock()
		for _, c := range children {
			c.exit()
		}
	})
	return w
}

// now reads the injected clock the monitor receives as Options.Now.
func (w *world) now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Now().Add(w.offset)
}

// jump moves the injected clock forward by d without any time passing in the bubble.
func (w *world) jump(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.offset += d
}

// until sleeps until the injected clock reads at.
func (w *world) until(at time.Time) { time.Sleep(at.Sub(w.now())) }

// set replaces collaborator behaviour under the world's lock.
func (w *world) set(change func(*behaviour)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	change(&w.do)
}

func (w *world) behaviour(call string) behaviour {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, call)
	w.all = append(w.all, call)
	return w.do
}

// spawn starts a fake child with the next process number, from 900 on.
func (w *world) spawn() proxyrecovery.Child {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := &child{pid: 900 + len(w.children), done: make(chan struct{})}
	c.record = proxyRecord(c.pid)
	w.children = append(w.children, c)
	return proxyrecovery.Child{Record: c.record, Done: c.done}
}

func (w *world) child(pid int) *child {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.children {
		if c.pid == pid {
			return c
		}
	}
	return nil
}

// exit ends the world's child pid, if there is one.
func (w *world) exit(pid int) {
	if c := w.child(pid); c != nil {
		c.exit()
	}
}

// lookup is the default record read: a child of this world, or a record the test placed.
func (w *world) lookup(_ context.Context, pid int) (procinfo.Record, error) {
	if c := w.child(pid); c != nil {
		return c.record, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if r, ok := w.records[pid]; ok {
		return r, nil
	}
	return procinfo.Record{}, errNoRecord
}

// stops returns the records StopOwned received so far.
func (w *world) stops() []procinfo.Record {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.stopped)
}

// owner makes every owner lookup answer o.
func (w *world) owner(o ownership.Owner, err error) {
	w.set(func(b *behaviour) { b.resolve = func(context.Context) (ownership.Owner, error) { return o, err } })
}

// place puts this root's record for a process the world did not start.
func (w *world) place(r procinfo.Record) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.records[r.PID] = r
}

func (w *world) startedPIDs() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	pids := []int{}
	for _, c := range w.children {
		pids = append(pids, c.pid)
	}
	return pids
}

// options wires every collaborator of the world into monitor options.
func (w *world) options(managed bool) proxyrecovery.Options {
	return proxyrecovery.Options{
		Managed: managed,
		Port:    proxyPort,
		Now: func() time.Time {
			w.mu.Lock()
			w.clockReads++
			w.mu.Unlock()
			return w.now()
		},
		Probe: func(ctx context.Context) bool { return w.behaviour("probe").probe(ctx) },
		Listening: func(ctx context.Context) (bool, error) {
			return w.behaviour("listening").listening(ctx)
		},
		BinaryExists: func() bool { return w.behaviour("binary").binary() },
		Resolve: func(ctx context.Context) (ownership.Owner, error) {
			return w.behaviour("resolve").resolve(ctx)
		},
		Start: func(ctx context.Context) (proxyrecovery.Child, error) {
			return w.behaviour("start").start(ctx)
		},
		StopOwned: func(ctx context.Context, r procinfo.Record) error {
			do := w.behaviour(fmt.Sprintf("stop-owned:%d", r.PID))
			w.mu.Lock()
			w.stopped = append(w.stopped, r)
			w.mu.Unlock()
			return do.stopOwned(ctx, r)
		},
		Recorded: func(ctx context.Context, pid int) (procinfo.Record, error) {
			return w.behaviour(fmt.Sprintf("recorded:%d", pid)).recorded(ctx, pid)
		},
		WaitRelease: func(ctx context.Context) bool { return w.behaviour("wait-release").waitRelease(ctx) },
		Record: func(name string, fields map[string]any) {
			w.mu.Lock()
			w.entered = append(w.entered, name)
			hook := w.do.record
			w.mu.Unlock()
			if hook != nil {
				hook(name)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			w.calls = append(w.calls, "record:"+name)
			w.all = append(w.all, "record:"+name)
			w.events = append(w.events, event{Name: name, Fields: deepCopy(fields)})
		},
	}
}

// deepCopy copies fields and every map nested in them, so that a monitor that reuses or changes a map it passed to
// Record cannot rewrite an event already recorded.
func deepCopy(fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		if nested, ok := v.(map[string]any); ok {
			v = deepCopy(nested)
		}
		out[k] = v
	}
	return out
}

// effects returns how many Record and StopOwned calls have been entered since the world was made.
func (w *world) effects() (records, stops int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.entered), len(w.stopped)
}

// reads returns how many times the monitor has read Options.Now.
func (w *world) reads() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.clockReads
}

// sequence returns the calls made so far and forgets them.
func (w *world) sequence() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	seq := append([]string{}, w.calls...)
	w.calls = nil
	return seq
}

// count returns how many calls since the world was made are name, or name with an argument.
func (w *world) count(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.all {
		if c == name || strings.HasPrefix(c, name+":") {
			n++
		}
	}
	return n
}

// journal returns the events recorded so far and forgets them.
func (w *world) journal() []event {
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := append([]event{}, w.events...)
	w.events = nil
	return kept
}

// The events the monitor journals, with the fields it owns (lib/process-supervision.js:1105-1195). The supervisor's
// own component, process number and start time are not among them: the Record function the integration supplies adds
// those, so a map that carries them at the top level is wrong.

func attemptEvent(n int) event {
	return event{Name: "proxy-recovery-started", Fields: map[string]any{"outcome": fmt.Sprintf("attempt-%d", n)}}
}

func finishedEvent(outcome proxyrecovery.State) event {
	return event{Name: "proxy-recovery-finished", Fields: map[string]any{"outcome": string(outcome)}}
}

func stopRequestedEvent(pid int) event {
	return event{Name: "proxy-stop-requested", Fields: map[string]any{"child": map[string]any{"component": "proxy", "pid": pid}, "signal": stopSignal}}
}

func startedEvent(pid int) event {
	return event{Name: "proxy-started", Fields: map[string]any{"child": map[string]any{"component": "proxy", "pid": pid}}}
}

// newMonitor constructs a monitor; a panic or an error fails the test before anything else runs.
func newMonitor(t *testing.T, o proxyrecovery.Options) *proxyrecovery.Monitor {
	t.Helper()
	m, err := construct(t, o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m == nil {
		t.Fatal("New returned no monitor and no error")
	}
	return m
}

// construct calls New; a panic fails the test, so that a declaration that is not implemented never counts as a
// refusal.
func construct(t *testing.T, o proxyrecovery.Options) (m *proxyrecovery.Monitor, err error) {
	t.Helper()
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		m, err = proxyrecovery.New(o)
	}()
	if panicked != nil {
		t.Fatalf("New panicked: %v", panicked)
	}
	return m, err
}

// pending is a Recover or Stop call running in its own goroutine.
type pending[T any] struct {
	done     chan struct{}
	val      T
	err      error
	panicked any
}

func (p *pending[T]) finished() bool { return closed(p.done) }

// result waits for the call, bounded by the watchdog, and fails the test if it panicked or hung.
func (p *pending[T]) result(t *testing.T, what string) (T, error) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(watchdog):
		t.Fatalf("%s did not return", what)
	}
	if p.panicked != nil {
		t.Fatalf("%s panicked: %v", what, p.panicked)
	}
	return p.val, p.err
}

func goRecover(m *proxyrecovery.Monitor, ctx context.Context) *pending[proxyrecovery.Result] {
	p := &pending[proxyrecovery.Result]{done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer func() { p.panicked = recover() }()
		p.val, p.err = m.Recover(ctx)
	}()
	return p
}

func goStop(m *proxyrecovery.Monitor, ctx context.Context) *pending[struct{}] {
	p := &pending[struct{}]{done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer func() { p.panicked = recover() }()
		p.err = m.Stop(ctx)
	}()
	return p
}

// tick runs one Recover call to its end.
func tick(t *testing.T, m *proxyrecovery.Monitor) proxyrecovery.Result {
	t.Helper()
	res, err := goRecover(m, t.Context()).result(t, "Recover")
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return res
}

// stop runs Stop to its end and returns its error.
func stop(t *testing.T, m *proxyrecovery.Monitor) error {
	t.Helper()
	_, err := goStop(m, t.Context()).result(t, "Stop")
	return err
}

// oneOf reports whether got equals one of the permitted call sequences, and the differences from each when not.
func oneOf(got []string, permitted ...[]string) (bool, string) {
	var diffs []string
	for _, want := range permitted {
		if slices.Equal(want, got) {
			return true, ""
		}
		diffs = append(diffs, fmt.Sprintf("want %q", want))
	}
	return false, fmt.Sprintf("got %q; %s", got, strings.Join(diffs, "; or "))
}

// reached waits, bounded by the watchdog, until a collaborator call entered g.
func reached(t *testing.T, g *gate, what string) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(watchdog):
		t.Fatalf("%s was never called", what)
	}
}
