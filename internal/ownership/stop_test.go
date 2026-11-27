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
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

const (
	workerPID  = 702
	decoyPID   = 703
	workerName = "shim"
	// workerPort is the worker's private listener, which the supervisor legitimately holds as well.
	workerPort uint16 = 39001
	// stopHorizon bounds a correct Stop in virtual time; it only turns a hang into a failure.
	stopHorizon = time.Minute
)

// stopCase is one stop of a recorded process against a simulated host.
type stopCase struct {
	// name, sample, role and port describe the recorded process; the zero value is the request worker.
	name   string
	sample func(pid int) procinfo.Identity
	role   string
	port   uint16
	// ports gives the listener holders; nil makes the worker's private port held by two processes.
	ports map[uint16]int
	// prepare changes the host, the stored record or the caller's copy before Stop runs.
	prepare func(t *testing.T, root string, h *fakeHost, expected *procinfo.Record)
	noStore bool
	wantErr error
	want    []sent
	// recordKept is true when the stored record files must be exactly as they were before Stop.
	recordKept bool
	// anyOutcome leaves the result, the record and the signals to the case itself.
	anyOutcome bool
	ctx        func(context.Context) context.Context
}

func runStop(t *testing.T, tt stopCase) *fakeHost {
	t.Helper()
	if tt.name == "" {
		tt.name, tt.sample, tt.role, tt.port = workerName, workerSample, roleWorker, workerPort
	}
	root := stateRoot(t)
	h := newHost(tt.sample(workerPID), workerSample(decoyPID))
	h.stamp = true
	h.ports = tt.ports
	if h.ports == nil {
		h.ports = map[uint16]int{workerPort: -1}
	}
	expected := recordFor(root, tt.sample(workerPID), tt.role, tt.port)
	if !tt.noStore {
		seed(t, root, tt.name, expected)
	}
	if tt.prepare != nil {
		tt.prepare(t, root, h, &expected)
	}
	before := readRaw(t, root, tt.name)
	ctx := t.Context()
	if tt.ctx != nil {
		ctx = tt.ctx(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, stopHorizon)
	defer cancel()
	err := stop(t, authorityOn(root, h), ctx, tt.name, expected)
	after := readRaw(t, root, tt.name)
	switch {
	case tt.anyOutcome:
	case tt.recordKept:
		if diff := gocmp.Diff(before, after); diff != "" {
			t.Errorf("the stored record files changed (-before +after):\n%s", diff)
		}
	case before.HasJSON && tt.wantErr == nil:
		if after.HasJSON || after.HasMirror {
			t.Errorf("the record of the stopped process is still stored: %+v", after)
		}
	}
	if h.processes[decoyPID].gone(time.Now()) {
		t.Error("the decoy process, which no record names, was ended")
	}
	for _, e := range h.snapshot() {
		if e.kind == "signal" && e.pid != workerPID {
			t.Errorf("a signal went to process %d; only the recorded process %d may be signalled", e.pid, workerPID)
		}
	}
	checkSamples(t, h.snapshot())
	if tt.anyOutcome {
		return h
	}
	if !errors.Is(err, tt.wantErr) {
		t.Errorf("Stop = %v; want %v\nevents: %s", err, tt.wantErr, describe(h.snapshot()))
	}
	// ErrStillRunning is reserved for a process that is still ours and outlived the kill; ErrNotOwned for a
	// process whose ownership the samples no longer prove. One error never claims both.
	if errors.Is(err, ownership.ErrNotOwned) && errors.Is(err, ownership.ErrStillRunning) {
		t.Errorf("Stop = %v matches both ErrNotOwned and ErrStillRunning", err)
	}
	if diff := gocmp.Diff(tt.want, h.signals()); diff != "" {
		t.Errorf("signals (-want +got):\n%s\nevents: %s", diff, describe(h.snapshot()))
	}
	return h
}

func survive(sigs ...os.Signal) func(*testing.T, string, *fakeHost, *procinfo.Record) {
	return func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) {
		for _, sig := range sigs {
			h.processes[workerPID].survives[sig] = true
		}
	}
}

// signalTimes returns the times of the first terminate and the first kill, zero when absent.
func signalTimes(events []event) (term, kill time.Time) {
	for _, e := range events {
		switch {
		case e.kind == "signal" && e.signal == syscall.SIGTERM && term.IsZero():
			term = e.at
		case e.kind == "signal" && e.signal == syscall.SIGKILL && kill.IsZero():
			kill = e.at
		}
	}
	return term, kill
}

func TestStopSignals(t *testing.T) {
	reused := procinfo.StartToken("2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/1")
	tests := map[string]stopCase{
		"success: one terminate to the exact recorded process, which then exits": {
			want: []sent{{workerPID, syscall.SIGTERM}},
		},
		"success: a signal call that fails while the process is gone is a stop": {
			prepare: func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) { h.signalErr = errVanished },
			want:    []sent{{workerPID, syscall.SIGTERM}},
		},
		"success: correlated image evidence that differs does not prevent the stop": {
			prepare: func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) {
				h.processes[workerPID].id.Image = procinfo.ImageEvidence{Path: foreignPath, Device: 1, Inode: 2}
			},
			want: []sent{{workerPID, syscall.SIGTERM}},
		},
		"success: another spelling of the recorded executable with its device and inode is the same image": {
			prepare: func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) {
				h.processes[workerPID].id.Image.Path = "/opt/model-gateway/bin/../bin/model-gateway"
			},
			want: []sent{{workerPID, syscall.SIGTERM}},
		},
		"success: the stored record decides, not a stale caller's copy of it": {
			prepare: func(t *testing.T, _ string, _ *fakeHost, expected *procinfo.Record) {
				expected.Command = gatewayExe + " serve-shim"
				expected.StartedAt = procinfo.LegacyStart(testBirth.Add(-time.Hour))
			},
			want: []sent{{workerPID, syscall.SIGTERM}},
		},
		"success: a process that ignores terminate is killed after the grace": {
			prepare: survive(syscall.SIGTERM),
			want:    []sent{{workerPID, syscall.SIGTERM}, {workerPID, syscall.SIGKILL}},
		},
		"success: a process that exits during the grace is not killed": {
			prepare: func(t *testing.T, root string, h *fakeHost, r *procinfo.Record) {
				survive(syscall.SIGTERM)(t, root, h, r)
				h.processes[workerPID].exitAfterTerm = time.Second
			},
			want: []sent{{workerPID, syscall.SIGTERM}},
		},
		"error: a process that survives terminate and kill is reported still running": {
			prepare:    survive(syscall.SIGTERM, syscall.SIGKILL),
			wantErr:    ownership.ErrStillRunning,
			want:       []sent{{workerPID, syscall.SIGTERM}, {workerPID, syscall.SIGKILL}},
			recordKept: true,
		},
		"error: signal calls that report success do not make a stop when the process stays": {
			prepare: func(t *testing.T, root string, h *fakeHost, r *procinfo.Record) {
				survive(syscall.SIGTERM, syscall.SIGKILL)(t, root, h, r)
				h.signalErr = nil
			},
			wantErr:    ownership.ErrStillRunning,
			want:       []sent{{workerPID, syscall.SIGTERM}, {workerPID, syscall.SIGKILL}},
			recordKept: true,
		},
		"success: an already exited process needs no signal": {
			prepare: func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) { h.processes[workerPID].exited = true },
		},
		"success: a number that already holds another lifetime means the recorded process is gone": {
			prepare: func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) {
				h.processes[workerPID].id.StartToken = reused
			},
		},
		"success: a number reused between the samples is not signalled": {
			prepare: func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) {
				h.beforeIdentity = func(h *fakeHost, pid, n int) {
					if pid == workerPID && n == 2 {
						h.processes[workerPID].id.StartToken = reused
					}
				}
			},
		},
		"success: a number reused while waiting for the exit is not killed": {
			prepare: func(t *testing.T, root string, h *fakeHost, r *procinfo.Record) {
				survive(syscall.SIGTERM)(t, root, h, r)
				h.beforeIdentity = func(h *fakeHost, pid, n int) {
					term, _ := signalTimes(h.events)
					if pid != workerPID || term.IsZero() || time.Since(term) < terminateGrace/2 {
						return
					}
					// The recorded process exits by itself after the terminate and a new process takes its number.
					next := h.add(procinfo.Identity{
						PID: workerPID, ParentPID: 1, Argv: []string{"/usr/bin/unrelated"}, Birth: testBirth.Add(time.Hour),
						StartToken: "2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/2",
					})
					next.survives[syscall.SIGTERM], next.survives[syscall.SIGKILL] = true, true
					h.beforeIdentity = nil
				}
			},
			want: []sent{{workerPID, syscall.SIGTERM}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// checkSamples, inside runStop, also decides that a kill comes exactly when the grace has ended.
			synctest.Test(t, func(t *testing.T) { _ = runStop(t, tt) })
		})
	}
}

// TestStopRefusals decides that every gap or contradiction in the evidence refuses the stop without a signal and
// keeps the record, so that the next attempt can still see it.
func TestStopRefusals(t *testing.T) {
	live := func(mutate func(*procinfo.Identity)) func(*testing.T, string, *fakeHost, *procinfo.Record) {
		return func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) { mutate(&h.processes[workerPID].id) }
	}
	store := func(mutate func(*procinfo.Record)) func(*testing.T, string, *fakeHost, *procinfo.Record) {
		return func(t *testing.T, root string, _ *fakeHost, expected *procinfo.Record) {
			r := *expected
			mutate(&r)
			seed(t, root, workerName, r)
		}
	}
	directImage := func(image procinfo.Image) func(*testing.T, string, *fakeHost, *procinfo.Record) {
		return live(func(id *procinfo.Identity) {
			id.Image = procinfo.ImageEvidence{Image: image, DirectFromExecutable: true}
		})
	}
	tests := map[string]stopCase{
		"error: an expected record without a start token grants nothing": {
			prepare: func(t *testing.T, root string, _ *fakeHost, expected *procinfo.Record) {
				expected.StartToken = ""
				seed(t, root, workerName, *expected)
			},
		},
		"error: no record is stored for the process": {noStore: true},
		"error: a newer record replaced the expected one": {
			prepare: store(func(r *procinfo.Record) { r.Generation = "generation-2" }),
		},
		"error: the stored record names another start token than the expected one": {
			prepare: store(func(r *procinfo.Record) { r.StartToken = "2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/9" }),
		},
		"error: the stored record contradicts the process although the caller's copy matches it": {
			prepare: store(func(r *procinfo.Record) { r.Command = gatewayExe + " serve-shim" }),
		},
		"error: an inconclusive sample keeps the record": {
			prepare: func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) {
				h.processes[workerPID].unreadable = errInconclusive
			},
		},
		"error: a live sample without a start token": {
			prepare: live(func(id *procinfo.Identity) { id.StartToken = "" }),
		},
		"error: a birth that differs from the record": {
			prepare: live(func(id *procinfo.Identity) { id.Birth = testBirth.Add(time.Second) }),
		},
		"error: a command that differs from the record": {
			prepare: live(func(id *procinfo.Identity) { id.Argv = []string{gatewayExe, "serve-shim"} }),
		},
		"error: a role that the command word contradicts although the command matches": {
			prepare: store(func(r *procinfo.Record) { r.Role = roleSupervisor }),
		},
		"error: a role word that is an operand after another command is not the role": {
			prepare: func(t *testing.T, root string, h *fakeHost, expected *procinfo.Record) {
				h.processes[workerPID].id.Argv = []string{gatewayExe, "serve-shim", "serve-worker"}
				r := *expected
				r.Command = gatewayExe + " serve-shim serve-worker"
				seed(t, root, workerName, r)
			},
		},
		"error: direct image evidence of another inode at the recorded path": {
			prepare: directImage(procinfo.Image{Path: gatewayExe, Device: gatewayDevice, Inode: gatewayInode + 1}),
		},
		"error: direct image evidence of another device with the recorded inode": {
			prepare: directImage(procinfo.Image{Path: gatewayExe, Device: gatewayDevice + 1, Inode: gatewayInode}),
		},
		"error: direct image evidence of another file elsewhere": {
			prepare: directImage(procinfo.Image{Path: foreignPath, Device: gatewayDevice, Inode: 9999}),
		},
		"error: correlated image evidence that matches does not rescue a contradicted birth": {
			prepare: live(func(id *procinfo.Identity) {
				id.Image.DirectFromExecutable = false
				id.Birth = testBirth.Add(time.Second)
			}),
		},
		"error: a second sample whose command changed cancels the signal": {
			prepare: func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) {
				h.beforeIdentity = func(h *fakeHost, pid, n int) {
					if pid == workerPID && n == 2 {
						h.processes[workerPID].id.Argv = []string{"/usr/bin/unrelated"}
					}
				}
			},
		},
		"error: an unreadable second sample cancels the signal": {
			prepare: func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) {
				h.beforeIdentity = func(h *fakeHost, pid, n int) {
					if pid == workerPID && n == 2 {
						h.processes[workerPID].unreadable = errInconclusive
					}
				}
			},
		},
	}
	for name, tt := range tests {
		tt.wantErr = ownership.ErrNotOwned
		tt.recordKept = !tt.noStore
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { _ = runStop(t, tt) })
		})
	}
	t.Run("success: a record replaced during the stop survives whole, whatever the stop decided", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var replacement procinfo.Record
			var publisher sync.WaitGroup
			var published error
			root := ""
			h := runStop(t, stopCase{
				prepare: func(t *testing.T, r string, h *fakeHost, expected *procinfo.Record) {
					root = r
					replacement = *expected
					replacement.Generation = "generation-2"
					h.beforeIdentity = func(h *fakeHost, pid, n int) {
						if pid == workerPID && n == 2 {
							// A publisher of the same store, which waits for the store's exclusion if Stop holds it.
							publisher.Go(func() {
								published = guarded("Store.Publish", func() error { return (ownership.Store{Root: r}).Publish(workerName, replacement) })
							})
						}
					}
				},
				anyOutcome: true,
			})
			publisher.Wait()
			if published != nil {
				t.Fatalf("publishing the replacement: %v", published)
			}
			checkStored(t, root, workerName, replacement)
			switch got := h.signals(); {
			case len(got) == 0, gocmp.Diff([]sent{{workerPID, syscall.SIGTERM}}, got) == "":
			default:
				t.Errorf("signals %v; want none (the replacement decided) or one terminate (the stop came first)", got)
			}
		})
	})
	t.Run("error: an ended context sends nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := runStop(t, stopCase{
				ctx: func(parent context.Context) context.Context {
					ctx, cancel := context.WithCancel(parent)
					cancel()
					return ctx
				},
				wantErr:    context.Canceled,
				recordKept: true,
			})
			if n := h.count("signal"); n != 0 {
				t.Errorf("%d signals with an ended context", n)
			}
		})
	})
}

// TestStopListenerRecords decides that a supervisor or proxy record also needs its listener: the port must be held
// by the recorded process alone, at every decision, while the worker's private port, which the supervisor holds
// too, decides nothing.
func TestStopListenerRecords(t *testing.T) {
	supervisor := stopCase{name: recordName, sample: supervisorSample, role: roleSupervisor, port: publicPort}
	proxy := stopCase{name: "proxy", sample: proxySample, role: roleProxy, port: proxyPort}
	with := func(c stopCase, ports map[uint16]int) stopCase {
		c.ports = ports
		return c
	}
	refused := func(c stopCase) stopCase {
		c.wantErr, c.recordKept = ownership.ErrNotOwned, true
		return c
	}
	// switchAfterMatch hands the port to the decoy right after the first observation that showed the recorded
	// holder for which ready reports true, so that only a later observation can see the change.
	switchAfterMatch := func(port uint16, ready func(*fakeHost) bool) func(*testing.T, string, *fakeHost, *procinfo.Record) {
		return func(t *testing.T, _ string, h *fakeHost, _ *procinfo.Record) {
			h.afterListener = func(h *fakeHost, p uint16, answer procinfo.Listener) {
				if p == port && answer.State == procinfo.ListenerIdentified && answer.PID == workerPID && ready(h) {
					h.ports = map[uint16]int{port: decoyPID}
					h.afterListener = nil
				}
			}
		}
	}
	terminated := func(h *fakeHost) bool { term, _ := signalTimes(h.events); return !term.IsZero() }
	escalation := refused(with(supervisor, map[uint16]int{publicPort: workerPID}))
	escalation.prepare = func(t *testing.T, root string, h *fakeHost, r *procinfo.Record) {
		survive(syscall.SIGTERM, syscall.SIGKILL)(t, root, h, r)
		switchAfterMatch(publicPort, terminated)(t, root, h, r)
	}
	escalation.want = []sent{{workerPID, syscall.SIGTERM}}
	between := refused(with(supervisor, map[uint16]int{publicPort: workerPID}))
	between.prepare = switchAfterMatch(publicPort, func(*fakeHost) bool { return true })
	proxyBetween := refused(with(proxy, map[uint16]int{proxyPort: workerPID}))
	proxyBetween.prepare = switchAfterMatch(proxyPort, func(*fakeHost) bool { return true })
	tests := map[string]stopCase{
		"success: the supervisor holding its recorded port alone is stopped": func() stopCase {
			c := with(supervisor, map[uint16]int{publicPort: workerPID})
			c.want = []sent{{workerPID, syscall.SIGTERM}}
			return c
		}(),
		"success: the proxy holding its recorded port alone is stopped": func() stopCase {
			c := with(proxy, map[uint16]int{proxyPort: workerPID})
			c.want = []sent{{workerPID, syscall.SIGTERM}}
			return c
		}(),
		"success: a worker is stopped although its private port has two holders": {
			ports: map[uint16]int{workerPort: -1},
			want:  []sent{{workerPID, syscall.SIGTERM}},
		},
		"error: the supervisor's port held by another process":                                                   refused(with(supervisor, map[uint16]int{publicPort: decoyPID})),
		"error: the supervisor's port held by several processes":                                                 refused(with(supervisor, map[uint16]int{publicPort: -1})),
		"error: the supervisor's port held by no process":                                                        refused(with(supervisor, map[uint16]int{})),
		"error: the proxy's port held by another process":                                                        refused(with(proxy, map[uint16]int{proxyPort: decoyPID})),
		"error: a holder change before the kill cancels the escalation":                                          escalation,
		"error: the proxy's port held by several processes":                                                      refused(with(proxy, map[uint16]int{proxyPort: -1})),
		"error: the proxy's port held by no process":                                                             refused(with(proxy, map[uint16]int{})),
		"error: the supervisor's port changing hands after its first matching observation cancels the terminate": between,
		"error: the proxy's port changing hands after its first matching observation cancels the terminate":      proxyBetween,
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { _ = runStop(t, tt) })
		})
	}
}

// TestStopEscalationNeedsFreshAuthority decides that the kill is its own decision: when the second of the samples
// taken after the grace shows the process running another program, it is not killed, and the refusal is about
// ownership, since the process may now be someone else's.
func TestStopEscalationNeedsFreshAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := runStop(t, stopCase{
			prepare: func(t *testing.T, root string, h *fakeHost, r *procinfo.Record) {
				survive(syscall.SIGTERM, syscall.SIGKILL)(t, root, h, r)
				afterGrace := 0
				h.beforeIdentity = func(h *fakeHost, pid, n int) {
					term, _ := signalTimes(h.events)
					if pid != workerPID || term.IsZero() || time.Since(term) < terminateGrace {
						return
					}
					if afterGrace++; afterGrace == 2 {
						h.processes[workerPID].id.Argv = []string{"/usr/bin/unrelated"}
					}
				}
			},
			wantErr:    ownership.ErrNotOwned,
			want:       []sent{{workerPID, syscall.SIGTERM}},
			recordKept: true,
		})
		_ = h
	})
}

// TestStopEachRecord decides that stopping several recorded processes judges each on its own record: a refusal
// for one neither stops nor protects the others.
func TestStopEachRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := stateRoot(t)
		guardian, worker, proxy := supervisorSample(holderPID), workerSample(workerPID), proxySample(decoyPID)
		h := newHost(guardian, worker, proxy)
		h.ports = map[uint16]int{publicPort: holderPID, proxyPort: decoyPID, workerPort: -1}
		h.processes[workerPID].unreadable = errInconclusive
		records := map[string]procinfo.Record{
			recordName: recordFor(root, guardian, roleSupervisor, publicPort),
			workerName: recordFor(root, worker, roleWorker, workerPort),
			"proxy":    recordFor(root, proxy, roleProxy, proxyPort),
		}
		for name, r := range records {
			seed(t, root, name, r)
		}
		a := authorityOn(root, h)
		got := map[string]bool{}
		for _, name := range []string{recordName, workerName, "proxy"} {
			got[name] = stop(t, a, t.Context(), name, records[name]) == nil
		}
		if diff := gocmp.Diff(map[string]bool{recordName: true, workerName: false, "proxy": true}, got); diff != "" {
			t.Errorf("stopped records (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff([]sent{{holderPID, syscall.SIGTERM}, {decoyPID, syscall.SIGTERM}}, h.signals()); diff != "" {
			t.Errorf("signals (-want +got):\n%s", diff)
		}
		if s := readRaw(t, root, workerName); !s.HasJSON || !s.HasMirror {
			t.Errorf("the record of the process that could not be identified was removed: %+v", s)
		}
		empty := stateRoot(t)
		none := newHost(guardian)
		if err := stop(t, authorityOn(empty, none), t.Context(), recordName, recordFor(empty, guardian, roleSupervisor, publicPort)); !errors.Is(err, ownership.ErrNotOwned) {
			t.Errorf("Stop without any stored record = %v; want ErrNotOwned", err)
		}
		if n := none.count("signal"); n != 0 {
			t.Errorf("Stop without any stored record sent %d signals", n)
		}
	})
}
