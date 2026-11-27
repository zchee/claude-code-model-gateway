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

package ownership_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	goleak.VerifyTestMain(m)
}

// The native probe is the production source of evidence; the authority must accept it unchanged.
var _ ownership.Processes = procinfo.Probe{}

// noPanic runs call and turns a panic into a test failure, so that one unimplemented function cannot abort the
// binary and with it every other test of the package.
func noPanic[T any](t testing.TB, what string, call func() T) (result T) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("%s panicked: %v", what, p)
		}
	}()
	return call()
}

// guarded runs call and returns a panic as an error, for goroutines in which t.Fatal must not be called.
func guarded(what string, call func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%s panicked: %v", what, p)
		}
	}()
	return call()
}

type resolved struct {
	owner ownership.Owner
	err   error
}

func resolve(t testing.TB, a ownership.Authority, ctx context.Context, name string, port uint16) (ownership.Owner, error) {
	t.Helper()
	r := noPanic(t, "Authority.Resolve", func() resolved {
		owner, err := a.Resolve(ctx, name, port)
		return resolved{owner, err}
	})
	return r.owner, r.err
}

func stop(t testing.TB, a ownership.Authority, ctx context.Context, name string, expected procinfo.Record) error {
	t.Helper()
	return noPanic(t, "Authority.Stop", func() error { return a.Stop(ctx, name, expected) })
}

// stateRoot returns a clean absolute state root with symbolic links resolved, as an instance's state root is.
func stateRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// testBirth is a whole-second local time, so that its legacy start text names it exactly.
var testBirth = time.Date(2026, time.September, 9, 23, 24, 12, 0, time.Local)

const (
	gatewayExe = "/opt/model-gateway/bin/model-gateway"
	proxyExe   = "/home/user/.local/share/model-gateway/bin/claude-code-proxy"
	// The roles the supervisor's publishers write (internal/cli/serveshim/workerproc.go:200,346) and the proxy's.
	roleSupervisor = "supervisor"
	roleWorker     = "worker"
	roleProxy      = "proxy"
	gatewayDevice  = 64769
	gatewayInode   = 4242
	proxyInode     = 5151
)

// sampleOf returns a complete live sample of pid running argv from image, as the native probe returns it on Linux.
func sampleOf(pid int, image procinfo.Image, argv ...string) procinfo.Identity {
	return procinfo.Identity{
		PID:        pid,
		ParentPID:  1,
		Argv:       argv,
		Birth:      testBirth,
		Image:      procinfo.ImageEvidence{Image: image, DirectFromExecutable: true},
		StartToken: procinfo.StartToken(fmt.Sprintf("2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/%d", 880000+pid)),
	}
}

var (
	gatewayImage = procinfo.Image{Path: gatewayExe, Device: gatewayDevice, Inode: gatewayInode}
	proxyImage   = procinfo.Image{Path: proxyExe, Device: gatewayDevice, Inode: proxyInode}
)

// supervisorSample is the supervisor as the launch starts it: the gateway executable with the serve-shim command.
func supervisorSample(pid int) procinfo.Identity {
	return sampleOf(pid, gatewayImage, gatewayExe, "serve-shim")
}

// workerSample is a request worker: the gateway executable with the serve-worker command.
func workerSample(pid int) procinfo.Identity {
	return sampleOf(pid, gatewayImage, gatewayExe, "serve-worker")
}

// proxySample is the owned proxy: the installed claude-code-proxy binary serving without its own monitor.
func proxySample(pid int) procinfo.Identity {
	return sampleOf(pid, proxyImage, proxyExe, "serve", "--no-monitor")
}

// recordFor returns the record a publisher writes for the sampled process in root.
func recordFor(root string, id procinfo.Identity, role string, port uint16) procinfo.Record {
	return procinfo.Record{
		PID:        id.PID,
		Command:    strings.Join(id.Argv, " "),
		StartedAt:  procinfo.LegacyStart(id.Birth),
		Role:       role,
		StateRoot:  root,
		Generation: "generation-1",
		Port:       port,
		Version:    "1.2.3",
		Image:      id.Image.Image,
		StartToken: id.StartToken,
	}
}

// seed writes a record with the existing record writer, independently of the store under test.
func seed(t testing.TB, root, name string, r procinfo.Record) {
	t.Helper()
	if err := procinfo.WriteRecord(root, name, r); err != nil {
		t.Fatalf("seed record %s: %v", name, err)
	}
}

// rawState is a record's two files as they are on disk, read without validation or repair.
type rawState struct {
	JSON, Mirror       string
	HasJSON, HasMirror bool
}

func readRaw(t testing.TB, root, name string) rawState {
	t.Helper()
	var s rawState
	for _, f := range []struct {
		suffix  string
		content *string
		present *bool
	}{{".pid.json", &s.JSON, &s.HasJSON}, {".pid", &s.Mirror, &s.HasMirror}} {
		data, err := os.ReadFile(filepath.Join(root, name+f.suffix))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			t.Fatalf("read %s%s: %v", name, f.suffix, err)
		default:
			*f.content, *f.present = string(data), true
		}
	}
	return s
}

// decoded returns the record the raw JSON holds, failing the test if it does not hold one.
func (s rawState) decoded(t testing.TB) procinfo.Record {
	t.Helper()
	var r procinfo.Record
	if !s.HasJSON {
		t.Fatal("no record file")
	}
	if err := json.Unmarshal([]byte(s.JSON), &r); err != nil {
		t.Fatalf("record file %q: %v", s.JSON, err)
	}
	return r
}

// fakeProcess is the state of one simulated process.
type fakeProcess struct {
	id         procinfo.Identity
	exited     bool
	unreadable error
	// hiddenArgv makes the identity inconclusive and lists the process in the table without arguments, as a
	// protected process looks.
	hiddenArgv bool
	// survives lists the signals the process ignores.
	survives map[os.Signal]bool
	// exitAfterTerm, when positive, makes the process exit by itself that long after a terminate it survives.
	exitAfterTerm time.Duration
	termAt        time.Time
}

func (p *fakeProcess) gone(now time.Time) bool {
	return p.exited || p.exitAfterTerm > 0 && !p.termAt.IsZero() && !now.Before(p.termAt.Add(p.exitAfterTerm))
}

// event is one call the authority made, in order.
type event struct {
	kind   string // "identity", "table", "alive", "listener" or "signal"
	pid    int
	signal os.Signal
	at     time.Time
	// sample is the identity a sample returned, or the one a signal was asked to deliver to.
	sample procinfo.Identity
}

// eventLimit aborts a runaway implementation: no correct stop or resolve comes near it.
const eventLimit = 100000

// fakeHost is a stateful process table: a process stays alive until a signal it does not survive arrives. Every
// call is logged with its time, so that the tests can judge the order of samples and signals.
type fakeHost struct {
	mu        sync.Mutex
	processes map[int]*fakeProcess
	// ports, when set, answers a listener query with the live holder of the port instead of the script; a negative
	// holder makes the port ambiguous.
	ports map[uint16]int
	// listener answers each listener query in order; the last answer repeats.
	listener []listenerAnswer
	queries  int
	events   []event
	// afterListener runs after a listener query answered from ports, with that answer and the lock held.
	afterListener func(h *fakeHost, port uint16, answer procinfo.Listener)
	// beforeIdentity runs before the n-th identity sample of pid is answered (n counts from 1), with the lock held.
	beforeIdentity func(h *fakeHost, pid, n int)
	identities     map[int]int
	// stamp writes a distinct, authority-irrelevant parent number into every identity sample, so that the sample
	// handed to a signal can be told from the others.
	stamp   bool
	samples int
	// signalErr is returned by every signal call, whatever its effect.
	signalErr error
	// delay is slept, in the caller's goroutine, before a listener or identity answer.
	listenerDelay, identityDelay time.Duration
	// confirmDelay is slept before every listener answer after the first.
	confirmDelay time.Duration
}

type listenerAnswer struct {
	listener procinfo.Listener
	err      error
}

func newHost(processes ...procinfo.Identity) *fakeHost {
	h := &fakeHost{processes: map[int]*fakeProcess{}, identities: map[int]int{}}
	for _, id := range processes {
		h.add(id)
	}
	return h
}

func (h *fakeHost) add(id procinfo.Identity) *fakeProcess {
	p := &fakeProcess{id: id, survives: map[os.Signal]bool{}}
	h.processes[id.PID] = p
	return p
}

func (h *fakeHost) log(e event) {
	e.at = time.Now()
	h.events = append(h.events, e)
	if len(h.events) > eventLimit {
		panic(fmt.Sprintf("more than %d evidence calls; the implementation does not converge", eventLimit))
	}
}

func (h *fakeHost) Identity(ctx context.Context, pid int) (procinfo.Identity, error) {
	if h.identityDelay > 0 {
		time.Sleep(h.identityDelay)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.identities[pid]++
	if h.beforeIdentity != nil {
		h.beforeIdentity(h, pid, h.identities[pid])
	}
	p, ok := h.processes[pid]
	switch {
	case !ok || p.gone(time.Now()):
		h.log(event{kind: "identity", pid: pid})
		return procinfo.Identity{}, os.ErrNotExist
	case p.unreadable != nil:
		h.log(event{kind: "identity", pid: pid})
		return procinfo.Identity{}, p.unreadable
	case p.hiddenArgv:
		h.log(event{kind: "identity", pid: pid})
		return procinfo.Identity{}, errInconclusive
	}
	id := p.id
	id.Argv = append([]string(nil), p.id.Argv...)
	if h.stamp {
		h.samples++
		id.ParentPID = 100000 + h.samples
	}
	h.log(event{kind: "identity", pid: pid, sample: id})
	return id, nil
}

func (h *fakeHost) Table(ctx context.Context) (map[int]procinfo.Identity, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log(event{kind: "table"})
	table := map[int]procinfo.Identity{}
	for pid, p := range h.processes {
		switch {
		case p.gone(time.Now()) || p.unreadable != nil:
		case p.hiddenArgv:
			row := p.id
			row.Argv = nil
			table[pid] = row
		default:
			table[pid] = p.id
		}
	}
	return table, nil
}

func (h *fakeHost) Alive(ctx context.Context, pid int) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log(event{kind: "alive", pid: pid})
	p, ok := h.processes[pid]
	return ok && !p.gone(time.Now()), nil
}

func (h *fakeHost) ListenerEvidence(ctx context.Context, port uint16) (procinfo.Listener, error) {
	h.mu.Lock()
	first := h.queries == 0
	h.mu.Unlock()
	switch {
	case first && h.listenerDelay > 0:
		time.Sleep(h.listenerDelay)
	case !first && h.confirmDelay > 0:
		time.Sleep(h.confirmDelay)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log(event{kind: "listener", pid: int(port)})
	h.queries++
	if pid, ok := h.ports[port]; ok {
		answer := procinfo.Listener{State: procinfo.ListenerAbsent}
		switch p, live := h.processes[pid]; {
		case pid < 0:
			answer = procinfo.Listener{State: procinfo.ListenerAmbiguous}
		case live && !p.gone(time.Now()):
			answer = procinfo.Listener{State: procinfo.ListenerIdentified, PID: pid}
		}
		if h.afterListener != nil {
			h.afterListener(h, port, answer)
		}
		return answer, nil
	}
	if len(h.listener) == 0 {
		return procinfo.Listener{State: procinfo.ListenerAbsent}, nil
	}
	answer := h.listener[min(h.queries-1, len(h.listener)-1)]
	return answer.listener, answer.err
}

func (h *fakeHost) signal(ctx context.Context, id procinfo.Identity, sig os.Signal) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log(event{kind: "signal", pid: id.PID, signal: sig, sample: id})
	if p, ok := h.processes[id.PID]; ok && !p.gone(time.Now()) {
		if p.survives[sig] {
			if sig == syscall.SIGTERM && p.termAt.IsZero() {
				p.termAt = time.Now()
			}
		} else {
			p.exited = true
		}
	}
	return h.signalErr
}

func (h *fakeHost) snapshot() []event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]event(nil), h.events...)
}

func (h *fakeHost) count(kind string) int {
	n := 0
	for _, e := range h.snapshot() {
		if e.kind == kind {
			n++
		}
	}
	return n
}

type sent struct {
	PID    int
	Signal os.Signal
}

func (h *fakeHost) signals() []sent {
	var out []sent
	for _, e := range h.snapshot() {
		if e.kind == "signal" {
			out = append(out, sent{e.pid, e.signal})
		}
	}
	return out
}

// terminateGrace is how long a terminated process has to exit before it is killed (lib/process-supervision.js:655).
const terminateGrace = 3 * time.Second

// checkSamples fails unless every signal follows the rule for its decision, counting only successful identity
// samples of the signalled process:
//   - the identity handed to the signal is, in full, the last sample of that process taken before it (every sample
//     is stamped with a distinct parent number, so a stale one is told apart); other evidence calls may come in
//     between, since a second sample that also opens a process handle leaves no trace of its own here;
//   - the terminate was decided on exactly two samples: one to decide, one immediately before acting;
//   - the kill comes exactly when the grace has ended and was decided on the samples taken at that instant: two
//     decisions, and at most one more observation there, a final exit poll; samples taken earlier, during the
//     grace, were exit polls and decide nothing.
func checkSamples(t testing.TB, events []event) {
	t.Helper()
	var termAt time.Time
	from := map[int]int{}
	for i, e := range events {
		if e.kind != "signal" {
			continue
		}
		var decision []event
		for _, s := range events[from[e.pid]:i] {
			if s.kind != "identity" || s.pid != e.pid || s.sample.PID == 0 {
				continue
			}
			if e.signal == syscall.SIGKILL && !s.at.Equal(e.at) {
				continue
			}
			decision = append(decision, s)
		}
		switch {
		case len(decision) == 0:
			t.Errorf("signal %v to %d followed no sample of it; events: %s", e.signal, e.pid, describe(events))
		default:
			if diff := gocmp.Diff(decision[len(decision)-1].sample, e.sample); diff != "" {
				t.Errorf("signal %v was handed another identity than the last sample of that process (-sample +handed):\n%s", e.signal, diff)
			}
		}
		switch e.signal {
		case syscall.SIGTERM:
			if len(decision) != 2 {
				t.Errorf("the terminate to %d was decided on %d samples of it; want exactly 2; events: %s", e.pid, len(decision), describe(events))
			}
			termAt = e.at
		case syscall.SIGKILL:
			if len(decision) < 2 || len(decision) > 3 {
				t.Errorf("the kill to %d was decided on %d samples taken when the grace ended; want 2, or 3 with a final exit poll; events: %s", e.pid, len(decision), describe(events))
			}
			if !e.at.Equal(termAt.Add(terminateGrace)) {
				t.Errorf("kill to %d came %v after the terminate; want exactly %v", e.pid, e.at.Sub(termAt), terminateGrace)
			}
		}
		from[e.pid] = i + 1
	}
}

func describe(events []event) string {
	var b strings.Builder
	start := time.Time{}
	if len(events) > 0 {
		start = events[0].at
	}
	for _, e := range events {
		fmt.Fprintf(&b, "[%v %s %d", e.at.Sub(start), e.kind, e.pid)
		if e.signal != nil {
			fmt.Fprintf(&b, " %v", e.signal)
		}
		b.WriteString("] ")
	}
	return b.String()
}

// noLegacy reads no command line as a Node gateway's.
func noLegacy([]string) (string, string, bool) { return "", "", false }

// authorityOn returns an authority over root whose evidence and signals come from host and whose pauses are timers.
func authorityOn(root string, h *fakeHost) ownership.Authority {
	return ownership.Authority{
		Records:   ownership.Store{Root: root},
		Processes: h,
		Signal:    h.signal,
		Wait:      timerWait,
		Legacy:    noLegacy,
	}
}

// timerWait pauses for d with a timer, so that a synctest bubble advances its clock instead of sleeping.
func timerWait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// errInconclusive is the shape of the native probe's inconclusive answer.
var errInconclusive = fmt.Errorf("%w: pid sample incomplete", procinfo.ErrUnknown)

// errProbeTimeout is the native probe's answer when its budget ended during a sample.
var errProbeTimeout = fmt.Errorf("%w: %w", procinfo.ErrUnknown, context.DeadlineExceeded)

// errVanished is what a signal call returns for a process that has already gone.
var errVanished = syscall.ESRCH
