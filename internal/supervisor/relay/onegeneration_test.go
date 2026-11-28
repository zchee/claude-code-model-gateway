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

package relay_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor/relay"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// oneGenTLegType is the error type of a worker leg broken by errPrivateLeg, which matches none of the relay's named
// failure classes.
const oneGenTLegType = "worker-leg-failure"

// oneGenTSwitch is a protocol switch, which fails a delivered attempt with an error type of its own.
const oneGenTSwitch = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: fixture\r\nConnection: Upgrade\r\n\r\n"

// oneGenTKey is the key of a value that the parent of a marked context carries.
type oneGenTKey struct{}

// TestOneGenerationDelivery decides the delivery attempts of a request whose context carries relay.OneGeneration: the
// public drain's relay, which must reach one worker generation at most.
//
// Before a lease was handed over and failed, a marked request is delivered exactly as an unmarked one: a refused
// Acquire, a source failure or a lease that comes with an error is retryable, and a worker that appears later gets the
// request, and a lease that came with an error gets no byte. After the first handed-over lease failed, also when it
// failed before it accepted a byte, the relay never calls Acquire again for the request. The exit of
// the worker that took it neither clears it as the taker nor shortens the remaining pauses: the relay waits out the
// schedule and then answers as taken, with one Lost report naming that first taker and the error type of its own
// failure; a stop reported by Wait answers the same at once, whether or not the taker has exited. A client that departs
// is owed nothing and causes no report. The loss is reported after the last pause and before the 503 with
// X-Should-Retry false is written, and every lease is closed when ServeHTTP ends. The mark keeps the values and the
// deadline of the context it marks. Every landed relay test decides the unmarked request, which this mark must leave
// as it is.
func TestOneGenerationDelivery(t *testing.T) {
	for name, tt := range oneGenTCases() {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) { oneGenTRun(t, w, tt) })
		})
	}
}

// oneGenTCase is one delivery of a request built on the context that mark returns.
type oneGenTCase struct {
	// mark derives the request's context from ctx; nil is relay.OneGeneration(ctx).
	mark func(t *testing.T, ctx context.Context) context.Context
	// setup arranges the world; depart ends the request's context, as a client that leaves does.
	setup func(t *testing.T, w *world, depart context.CancelFunc)
	// source, when set, is the worker source the relay uses instead of the world's counting source.
	source func(w *world) workerlink.Source
	// check runs after the answer with the case's own assertions; src is the worker source the relay used.
	check func(t *testing.T, w *world, src workerlink.Source)

	// departs is set when the client leaves: ServeHTTP may then end by returning or by http.ErrAbortHandler.
	departs bool
	// statuses are the statuses written to the client; retry is the X-Should-Retry value, empty when absent.
	statuses []int
	retry    string
	body     string
	// delivered is the generation of every lease the world's source handed out, in order.
	delivered []workerlink.Generation
	reads     map[workerlink.Generation]int32
	upstream  int32
	// waits and acquires are the exact numbers of Wait calls and of Acquire calls on the world's source.
	waits    int
	acquires int
	lost     []relay.Lost
	// errorTypes are the error types of the Lost reports, in order.
	errorTypes []string
}

func oneGenTCases() map[string]oneGenTCase {
	lostTo := func(g workerlink.Generation, pid int) []relay.Lost {
		return []relay.Lost{{Generation: g, WorkerPID: pid}}
	}
	return map[string]oneGenTCase{
		"success: a marked request reaches a worker that appears after refused attempts": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				g := w.reserve("gen-a", 41001, 4101, answering(okAnswer))
				w.setOnWait(func(n int) error {
					if n == 3 {
						g.publish()
					}
					return nil
				})
			},
			statuses:  []int{http.StatusOK},
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     3,
			acquires:  4,
		},
		"success: a marked request survives source failures before its first lease": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				w.start("gen-a", 41001, 4101, answering(okAnswer))
				w.src.setFail(func(n int) error {
					if n <= 2 {
						return errors.New("worker unavailable")
					}
					return nil
				})
			},
			statuses:  []int{http.StatusOK},
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     2,
			acquires:  3,
		},
		"success: a lease returned with an error does not make its generation the taker of a marked request": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				w.start("gen-a", 41001, 4101, answering(okAnswer))
			},
			source: func(w *world) workerlink.Source {
				return &strayLeaseSource{inner: w.src, stray: func(n int) bool { return n == 1 }}
			},
			check: func(t *testing.T, w *world, src workerlink.Source) {
				w.mu.Lock()
				a := w.byKey["gen-a"]
				w.mu.Unlock()
				if n := len(a.connections()); n != 2 {
					t.Errorf("gen-a got %d connections; want the one returned with an error and the one that delivered", n)
				}
				// The worker records a request line only once a whole head arrived, so the lease's own probe counts what
				// the relay wrote to the refused lease: a prefix of the request would pass every count above.
				strays := src.(*strayLeaseSource).strayProbes()
				if len(strays) != 1 {
					t.Fatalf("%d leases were returned with an error; want 1", len(strays))
				}
				if n := strays[0].written.Load(); n != 0 {
					t.Errorf("the relay wrote %d raw bytes to the lease that came with an error; want none", n)
				}
				if !strays[0].closed.Load() {
					t.Errorf("the lease that came with an error was not closed")
				}
			},
			statuses: []int{http.StatusOK},
			body:     "ok",
			// The world's source hands out both leases; the relay closes the first unused.
			delivered: []workerlink.Generation{"gen-a", "gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     1,
			acquires:  2,
		},
		"error: a marked request that no worker ever accepts is retryable": {
			setup:    func(*testing.T, *world, context.CancelFunc) {},
			statuses: []int{http.StatusServiceUnavailable},
			body:     undeliveredBody,
			waits:    attemptWaits,
			acquires: attemptWaits + 1,
		},
		"error: a stop before any lease was handed over leaves a marked request retryable": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				w.setOnWait(func(n int) error {
					if n == 2 {
						return errStopping
					}
					return nil
				})
			},
			statuses: []int{http.StatusServiceUnavailable},
			body:     undeliveredBody,
			waits:    2,
			acquires: 2,
		},
		"error: a marked request whose taker exits during an ordinary pause is never offered to its successor": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
				w.setOnWait(func(n int) error {
					if n == 10 {
						a.end()
						b.publish()
					}
					return nil
				})
			},
			check:      oneGenTUntouched("gen-b"),
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      attemptWaits,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"error: a marked request whose taker exits with no successor waits out the schedule and is lost to that taker": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
				w.setOnWait(func(n int) error {
					if n == 10 {
						a.end()
					}
					return nil
				})
			},
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      attemptWaits,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"error: a marked request whose taker exits as its attempt fails waits out the schedule": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				var a *generation
				a = w.start("gen-a", 41001, 4101, failing(true, func(*workerConn) {
					a.end()
					b.publish()
				}))
				w.src.setLegErr(errPrivateLeg)
			},
			check:      oneGenTUntouched("gen-b"),
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      attemptWaits,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"error: a marked request whose taker stays current is lost to it when the attempts run out": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
			},
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      attemptWaits,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"error: a taker that exits immediately before the wait reports the stop keeps the marked request": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
				w.setOnWait(func(n int) error {
					if n == 5 {
						a.end()
						b.publish()
						return errStopping
					}
					return nil
				})
			},
			check:      oneGenTUntouched("gen-b"),
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      5,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"error: a taker that exits in the middle of the wait that reports the stop keeps the marked request": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
				w.setMidWait(func(n int) error {
					if n == 4 {
						a.end()
						return errStopping
					}
					return nil
				})
			},
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      4,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"error: a taker that exited several pauses before the stop still holds the marked request": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
				w.setOnWait(func(n int) error {
					switch n {
					case 3:
						a.end()
						b.publish()
					case 10:
						return errStopping
					}
					return nil
				})
			},
			check:      oneGenTUntouched("gen-b"),
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      10,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"error: a stop in the pause right after the failure answers a marked request as taken by a current worker": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
				w.setOnWait(func(n int) error {
					if n == 1 {
						return errStopping
					}
					return nil
				})
			},
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      1,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"error: a late taker keeps the marked request for the rest of the schedule after it exits": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.reserve("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
				w.setOnWait(func(n int) error {
					switch n {
					case 3:
						a.publish()
					case 10:
						a.end()
						b.publish()
					}
					return nil
				})
			},
			check:      oneGenTUntouched("gen-b"),
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      attemptWaits,
			acquires:   4,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"error: a marked request taken on the last attempt is lost to that taker although it ended": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				var a *generation
				a = w.reserve("gen-a", 41001, 4101, failing(false, func(*workerConn) { a.end() }))
				w.src.setLegErr(errPrivateLeg)
				w.setOnWait(func(n int) error {
					if n == attemptWaits {
						a.publish()
					}
					return nil
				})
			},
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			waits:      attemptWaits,
			acquires:   attemptWaits + 1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"error: the loss of a marked request carries the error type of the taker's own failure after it exited": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, failing(true, func(c *workerConn) { c.send(oneGenTSwitch) }))
				w.setOnWait(func(n int) error {
					switch n {
					case 2:
						a.end()
						b.publish()
					case 6:
						return errStopping
					}
					return nil
				})
			},
			check:      oneGenTUntouched("gen-b"),
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      6,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{"protocol-switch"},
		},
		"error: a taker that failed with a protocol switch and exited during a pause is reported with that error type when the attempts run out": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, failing(true, func(c *workerConn) { c.send(oneGenTSwitch) }))
				w.setOnWait(func(n int) error {
					if n == 7 {
						a.end()
						b.publish()
					}
					return nil
				})
			},
			check:      oneGenTUntouched("gen-b"),
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      attemptWaits,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{"protocol-switch"},
		},
		"error: a marked request whose first handed-over lease fails before it accepts a byte is never offered to the successor": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, answering(okAnswer))
				w.setOnWait(func(n int) error {
					if n == 1 {
						a.end()
						b.publish()
					}
					return nil
				})
			},
			source: func(w *world) workerlink.Source {
				return &oneGenTDeadSource{inner: w.src, dead: func(n int) bool { return n == 1 }}
			},
			check:     oneGenTUntouched("gen-b"),
			statuses:  []int{http.StatusServiceUnavailable},
			retry:     "false",
			body:      lostBody,
			delivered: []workerlink.Generation{"gen-a"},
			// The handed-over lease failed its first read and its first write: gen-a never read the request.
			reads:      map[workerlink.Generation]int32{},
			waits:      attemptWaits,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{"connection-reset"},
		},
		"success: a source that answers with neither a lease nor an error before the first lease leaves a marked request deliverable": {
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				w.start("gen-a", 41001, 4101, answering(okAnswer))
			},
			source: func(w *world) workerlink.Source {
				return &oneGenTEmptySource{w: w, empty: func(n int) bool { return n <= 2 }}
			},
			check: func(t *testing.T, w *world, _ workerlink.Source) {
				if diff := gocmp.Diff([]string{"acquire", "wait", "acquire", "wait", "acquire"}, w.callOrder()); diff != "" {
					t.Errorf("order of the relay's lease requests and waits (-want +got):\n%s", diff)
				}
			},
			statuses:  []int{http.StatusOK},
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     2,
			// The world's source sees only the third request; the first two are answered before it.
			acquires: 1,
		},
		"success: a client that departs during a pause after the taker exited is owed nothing": {
			setup: func(t *testing.T, w *world, depart context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
				w.setOnWait(func(n int) error {
					switch n {
					case 3:
						a.end()
						b.publish()
					case 6:
						depart()
					}
					return nil
				})
			},
			check:     oneGenTUntouched("gen-b"),
			departs:   true,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     6,
			acquires:  1,
		},
		"success: a client that departs while the taker of its marked request is current is owed nothing": {
			setup: func(t *testing.T, w *world, depart context.CancelFunc) {
				w.start("gen-a", 41001, 4101, failing(true, nil))
				w.setOnWait(func(n int) error {
					if n == 2 {
						depart()
					}
					return nil
				})
			},
			departs:   true,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     2,
			acquires:  1,
		},
		"error: a marked context keeps its parent's value and deadline, and a context derived from it keeps the mark": {
			mark: func(t *testing.T, ctx context.Context) context.Context {
				deadline := time.Now().Add(24 * time.Hour)
				parent, cancelParent := context.WithDeadline(context.WithValue(ctx, oneGenTKey{}, "parent"), deadline)
				// The request's own context ends with the test; the derived cancels are only released with it.
				context.AfterFunc(ctx, cancelParent)
				marked := relay.OneGeneration(parent)
				oneGenTKeepsParent(t, "the marked context", marked, deadline)
				derived, cancel := context.WithCancel(context.WithValue(marked, oneGenTOtherKey{}, "unrelated"))
				context.AfterFunc(ctx, cancel)
				oneGenTKeepsParent(t, "the context derived from the marked one", derived, deadline)
				return derived
			},
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
				w.setOnWait(func(n int) error {
					if n == 10 {
						a.end()
						b.publish()
					}
					return nil
				})
			},
			check:      oneGenTUntouched("gen-b"),
			statuses:   []int{http.StatusServiceUnavailable},
			retry:      "false",
			body:       lostBody,
			delivered:  []workerlink.Generation{"gen-a"},
			reads:      map[workerlink.Generation]int32{"gen-a": 1},
			upstream:   1,
			waits:      attemptWaits,
			acquires:   1,
			lost:       lostTo("gen-a", 4101),
			errorTypes: []string{oneGenTLegType},
		},
		"success: the context given to OneGeneration stays unmarked and its request goes to the successor": {
			mark: func(_ *testing.T, ctx context.Context) context.Context {
				_ = relay.OneGeneration(ctx)
				return ctx
			},
			setup: func(t *testing.T, w *world, _ context.CancelFunc) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
				w.setOnWait(func(n int) error {
					if n == 10 {
						a.end()
						b.publish()
					}
					return nil
				})
			},
			statuses:  []int{http.StatusOK},
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a", "gen-b"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1, "gen-b": 1},
			upstream:  2,
			waits:     10,
			acquires:  2,
		},
	}
}

// oneGenTEmptySource is a worker source that answers the requests empty selects with neither a lease nor an error,
// noting each in the world's call order, and passes every other request to the world's counting source.
type oneGenTEmptySource struct {
	w     *world
	empty func(n int) bool
	calls atomic.Int32
}

func (s *oneGenTEmptySource) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	if s.empty(int(s.calls.Add(1))) {
		s.w.note("acquire")
		return nil, nil
	}
	return s.w.src.Acquire(ctx, exclude)
}

// oneGenTDeadSource is a worker source that, on the requests dead selects, hands over the lease of inner wrapped so
// that it accepts no byte: its first read and its first write fail with a connection reset, before the worker could
// read anything. Every other request is passed to inner as it is.
type oneGenTDeadSource struct {
	inner workerlink.Source
	dead  func(n int) bool
	calls atomic.Int32
}

func (s *oneGenTDeadSource) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	n := int(s.calls.Add(1))
	l, err := s.inner.Acquire(ctx, exclude)
	if err == nil && s.dead(n) {
		return &oneGenTDeadLease{Lease: l}, nil
	}
	return l, err
}

// oneGenTDeadLease is a lease whose reads and writes fail at once with a connection reset; Close closes the lease.
type oneGenTDeadLease struct {
	workerlink.Lease
}

func (*oneGenTDeadLease) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
}

func (*oneGenTDeadLease) Write([]byte) (int, error) {
	return 0, &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.ECONNRESET)}
}

// oneGenTOtherKey is the key of an unrelated value that a test adds on top of a marked context.
type oneGenTOtherKey struct{}

// oneGenTKeepsParent fails unless ctx still carries the value its parent stored under oneGenTKey and the parent's
// deadline. It asserts nothing about the type of ctx.
func oneGenTKeepsParent(t *testing.T, what string, ctx context.Context, deadline time.Time) {
	t.Helper()
	if got := ctx.Value(oneGenTKey{}); got != "parent" {
		t.Errorf("%s carries %v under the parent's key; want the parent's value %q", what, got, "parent")
	}
	got, ok := ctx.Deadline()
	if !ok || !got.Equal(deadline) {
		t.Errorf("%s has the deadline %v (set %t); want the parent's deadline %v", what, got, ok, deadline)
	}
}

// oneGenTUntouched returns a check that fails when any connection reached the named generation.
func oneGenTUntouched(name workerlink.Generation) func(t *testing.T, w *world, src workerlink.Source) {
	return func(t *testing.T, w *world, _ workerlink.Source) {
		t.Helper()
		w.mu.Lock()
		g := w.byKey[name]
		w.mu.Unlock()
		if n := len(g.connections()); n != 0 {
			t.Errorf("%q got %d connections; a request that a generation took is never offered to another", name, n)
		}
	}
}

// oneGenTMark returns the request context of tt built on ctx. A panic of the mark fails the case alone, before any
// worker or handler of the case exists.
func oneGenTMark(t *testing.T, ctx context.Context, tt oneGenTCase) (marked context.Context, ok bool) {
	t.Helper()
	defer func() {
		if v := recover(); v != nil {
			t.Errorf("marking the request context panicked: %v", v)
			marked, ok = nil, false
		}
	}()
	if tt.mark == nil {
		return relay.OneGeneration(ctx), true
	}
	return tt.mark(t, ctx), true
}

// oneGenTRun runs one case in w.
func oneGenTRun(t *testing.T, w *world, tt oneGenTCase) {
	ctx, depart := context.WithCancel(t.Context())
	defer depart()
	reqCtx, ok := oneGenTMark(t, ctx, tt)
	if !ok {
		return
	}
	tt.setup(t, w, depart)
	cw := newClientWriter()
	// reportMu guards atReport, the client's state at each Lost call, so that a relay that calls Lost twice at once
	// shows as two reports instead of a race in the recorder.
	var reportMu sync.Mutex
	var atReport []snapshot
	o := w.options()
	o.Lost = func(l relay.Lost) {
		w.recordLost(l)
		reportMu.Lock()
		defer reportMu.Unlock()
		atReport = append(atReport, cw.snapshot())
	}
	if tt.source != nil {
		o.Workers = tt.source(w)
	}
	// The last Wait the case expects is held after its pause has run out and before it returns its result, until the
	// test has seen that nothing was reported or written yet: the answer follows the whole schedule, not a report made
	// before or during its last pause.
	final, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	defer free()
	var waitCalls atomic.Int32
	o.Wait = func(ctx context.Context, d time.Duration) error {
		n := int(waitCalls.Add(1))
		err := w.wait(ctx, d)
		if n == tt.waits {
			close(final)
			<-release
		}
		return err
	}
	h := newRelay(t, o)
	answer := serve(h, cw, postMessages(reqCtx, requestBody))
	var out outcome
	select {
	case <-final:
		synctest.Wait()
		early, ended := ready(answer)
		if ended {
			t.Errorf("ServeHTTP ended while Wait call %d had not returned; the relay joins every Wait it starts", tt.waits)
		}
		if lost := w.lostReports(); len(lost) != 0 {
			t.Errorf("Lost was called with %+v before the last pause of the schedule had returned", lost)
		}
		if s := cw.snapshot(); len(s.statuses) != 0 || s.body != "" || len(s.flushes) != 0 {
			t.Errorf("the client held %v %q before the last pause of the schedule had returned", s.statuses, s.body)
		}
		free()
		out = early
		if !ended {
			out = await(t, answer, "the relay's answer")
		}
	case out = <-answer:
		if tt.waits > 0 {
			t.Errorf("ServeHTTP ended before Wait call %d had returned; the answer follows the whole schedule", tt.waits)
		}
	case <-time.After(watchdog):
		t.Fatalf("the relay neither answered nor reached Wait call %d within %v", tt.waits, watchdog)
	}
	// Every lease is closed by the time ServeHTTP has ended, before any later timer could close it.
	checkLeasesClosed(t, w)
	// Let any timer of a relay that kept trying run; synctest.Wait shows that nothing is left to run.
	time.Sleep(time.Minute)
	synctest.Wait()
	if tt.departs {
		mustEnd(t, out)
	} else {
		mustReturn(t, out)
	}
	got := cw.snapshot()
	if diff := gocmp.Diff(tt.statuses, got.statuses, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("statuses written (-want +got):\n%s", diff)
	}
	if got.body != tt.body {
		t.Errorf("body = %q; want %q", got.body, tt.body)
	}
	if len(tt.statuses) == 1 {
		checkAnswerHead(t, got.head, tt.statuses[0], tt.retry)
	}
	if diff := gocmp.Diff(tt.delivered, w.src.delivered(), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("leases handed out by generation (-want +got):\n%s", diff)
	}
	w.mu.Lock()
	gens := slices.Clone(w.gens)
	w.mu.Unlock()
	for _, g := range gens {
		if got, want := g.reads.Load(), tt.reads[g.name]; got != want {
			t.Errorf("requests read by %q = %d; want %d", g.name, got, want)
		}
		checkSameRequest(t, g, "POST /v1/messages HTTP/1.1", requestBody)
	}
	if got := w.upstream.Load(); got != tt.upstream {
		t.Errorf("upstream submissions = %d; want %d", got, tt.upstream)
	}
	waits := w.waitCalls()
	if len(waits) != tt.waits {
		t.Errorf("Wait was called %d times; want %d", len(waits), tt.waits)
	}
	for i, d := range waits {
		if d != attemptWait {
			t.Errorf("Wait call %d paused %v; want %v", i+1, d, attemptWait)
		}
	}
	if calls := w.src.acquireCalls(); len(calls) != tt.acquires {
		t.Errorf("Acquire was called %d times (%+v); want %d", len(calls), calls, tt.acquires)
	}
	checkAlternation(t, w.callOrder())
	lost := w.lostReports()
	checkLost(t, lost, tt.lost)
	var errorTypes []string
	for _, l := range lost {
		errorTypes = append(errorTypes, l.ErrorType)
	}
	if diff := gocmp.Diff(tt.errorTypes, errorTypes, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("error types of the Lost reports (-want +got):\n%s", diff)
	}
	reportMu.Lock()
	reports := slices.Clone(atReport)
	reportMu.Unlock()
	for i, s := range reports {
		if len(s.statuses) != 0 || s.body != "" || len(s.flushes) != 0 {
			t.Errorf("at loss report %d the client already held %v %q; the loss is reported before the answer", i+1, s.statuses, s.body)
		}
	}
	checkLeasesClosed(t, w)
	if tt.check != nil {
		tt.check(t, w, o.Workers)
	}
}

// oneGenTFirstBody and oneGenTSecondBody are the bodies of the two requests one relay serves in
// TestOneGenerationPerRequest; they differ, so that each worker shows which request it received.
const (
	oneGenTFirstBody  = `{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[{"role":"user","content":"the first request"}]}`
	oneGenTSecondBody = `{"model":"claude-sonnet-4-6","max_tokens":2,"messages":[{"role":"user","content":"the second request"}]}`
)

// oneGenTRequestKey is the key of the name of a request ("first" or "second") in its context, by which the Wait of
// TestOneGenerationPerRequest counts the pauses of each request.
type oneGenTRequestKey struct{}

// TestOneGenerationPerRequest decides that the taker of a marked request belongs to that request, not to the relay or
// to the mark: one handler serves two requests whose contexts derive from one marked context. The first request's
// worker fails it with a protocol switch; in the first request's third pause that worker exits, its successor is
// published and the second request is served, while the first request is held in that pause until the second one
// has finished. The second request is delivered to the successor, which receives its bytes and nothing else, and its
// answer, its loss report and its own pauses are decided while the first request is still held. Released, the first
// request waits out its own schedule and is lost to its own taker with its own error type. When the successor fails
// the second request too, each request is lost to its own taker. One row passes the same marked context to both
// requests: they are still two requests, and only the first one waits. A relay that keeps the taker on the handler,
// on the mark or under the request's context, that sends one request's bytes for another or after it, or that shares
// the pause schedule between requests fails.
func TestOneGenerationPerRequest(t *testing.T) {
	tests := map[string]struct {
		// successor is the behaviour of gen-b, the generation published while the first request is held.
		successor func(c *workerConn)
		status    int
		retry     string
		body      string
		// secondWaits and secondLost are the second request's pauses and the reports made when it has finished.
		secondWaits int
		secondLost  []relay.Lost
		secondTypes []string
		upstream    int32
		// lost and errorTypes are all reports, in order, once the first request has finished.
		lost       []relay.Lost
		errorTypes []string
		// sameContext passes the marked context unchanged to both requests; every Wait then counts as the first
		// request's, since only the first request may wait.
		sameContext bool
	}{
		"success: a second marked request reaches the successor while the first is held by its exited taker": {
			successor:   answering(okAnswer),
			status:      http.StatusOK,
			body:        "ok",
			secondWaits: 0,
			upstream:    2,
			lost:        []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
			errorTypes:  []string{"protocol-switch"},
		},
		"success: a second request given the same marked context reaches the successor while the first is held": {
			successor:   answering(okAnswer),
			status:      http.StatusOK,
			body:        "ok",
			secondWaits: 0,
			upstream:    2,
			lost:        []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
			errorTypes:  []string{"protocol-switch"},
			sameContext: true,
		},
		"error: two marked requests on one relay are each lost to their own taker": {
			successor:   failing(true, nil),
			status:      http.StatusServiceUnavailable,
			retry:       "false",
			body:        lostBody,
			secondWaits: attemptWaits,
			secondLost:  []relay.Lost{{Generation: "gen-b", WorkerPID: 4102}},
			secondTypes: []string{oneGenTLegType},
			upstream:    2,
			lost:        []relay.Lost{{Generation: "gen-b", WorkerPID: 4102}, {Generation: "gen-a", WorkerPID: 4101}},
			errorTypes:  []string{oneGenTLegType, "protocol-switch"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				marked, ok := oneGenTMark(t, t.Context(), oneGenTCase{})
				if !ok {
					return
				}
				firstCtx := context.WithValue(marked, oneGenTRequestKey{}, "first")
				secondCtx := context.WithValue(marked, oneGenTRequestKey{}, "second")
				if tt.sameContext {
					firstCtx, secondCtx = marked, marked
				}
				b := w.reserve("gen-b", 41002, 4102, tt.successor)
				a := w.start("gen-a", 41001, 4101, failing(true, func(c *workerConn) { c.send(oneGenTSwitch) }))
				// A failed read on a lease is reported as the private leg failure; the protocol switch is not a read
				// failure, so the two takers fail with different error types.
				w.src.setLegErr(errPrivateLeg)

				firstCW, secondCW := newClientWriter(), newClientWriter()
				release := make(chan struct{})
				var releaseOnce sync.Once
				free := func() { releaseOnce.Do(func() { close(release) }) }
				// A failing row still ends the hold, so that the bubble can be torn down.
				defer free()
				second := make(chan (<-chan outcome), 1)
				var h http.Handler
				var waitMu sync.Mutex
				waits := map[string]int{}
				o := w.options()
				o.Wait = func(ctx context.Context, d time.Duration) error {
					who, _ := ctx.Value(oneGenTRequestKey{}).(string)
					if tt.sameContext {
						who = "first"
					}
					waitMu.Lock()
					waits[who]++
					n := waits[who]
					waitMu.Unlock()
					if who == "first" && n == 3 {
						a.end()
						b.publish()
						second <- serve(h, secondCW, postMessages(secondCtx, oneGenTSecondBody))
						<-release
					}
					return w.wait(ctx, d)
				}
				waitsOf := func(who string) int {
					waitMu.Lock()
					defer waitMu.Unlock()
					return waits[who]
				}
				h = newRelay(t, o)
				first := serve(h, firstCW, postMessages(firstCtx, oneGenTFirstBody))

				// The second request, decided while the first one is held in its third pause.
				secondOut := await(t, await(t, second, "the second request's start"), "the second request's answer")
				// The second request's lease is closed by the time its ServeHTTP has ended; the first request's
				// lease is not decided here.
				for i, l := range w.src.handed() {
					if l.Generation() == "gen-b" && !closed(l.closed) {
						t.Errorf("lease %d (generation gen-b) is still open when the second request's ServeHTTP ended", i)
					}
				}
				synctest.Wait()
				mustReturn(t, secondOut)
				gotSecond := secondCW.snapshot()
				if diff := gocmp.Diff([]int{tt.status}, gotSecond.statuses); diff != "" {
					t.Errorf("statuses written to the second client (-want +got):\n%s", diff)
				}
				if gotSecond.body != tt.body {
					t.Errorf("second body = %q; want %q", gotSecond.body, tt.body)
				}
				checkAnswerHead(t, gotSecond.head, tt.status, tt.retry)
				if got := waitsOf("second"); got != tt.secondWaits {
					t.Errorf("the second request paused %d times; want %d", got, tt.secondWaits)
				}
				if got := waitsOf("first"); got != 3 {
					t.Errorf("%d pauses were counted for the first request while it was held in its third; want 3", got)
				}
				lost := w.lostReports()
				checkLost(t, lost, tt.secondLost)
				oneGenTCheckTypes(t, "when the second request had finished", lost, tt.secondTypes)
				oneGenTCheckOnly(t, b, oneGenTSecondBody)
				firstOut, answered := ready(first)
				if answered {
					t.Errorf("the first request was answered while it was held in its third pause")
				}
				if s := firstCW.snapshot(); len(s.statuses) != 0 || s.body != "" {
					t.Errorf("the first client held %v %q while its request was held in its third pause", s.statuses, s.body)
				}

				// The first request, released.
				free()
				if !answered {
					firstOut = await(t, first, "the first request's answer")
				}
				checkLeasesClosed(t, w)
				time.Sleep(time.Minute)
				synctest.Wait()
				mustReturn(t, firstOut)
				gotFirst := firstCW.snapshot()
				if diff := gocmp.Diff([]int{http.StatusServiceUnavailable}, gotFirst.statuses); diff != "" {
					t.Errorf("statuses written to the first client (-want +got):\n%s", diff)
				}
				if gotFirst.body != lostBody {
					t.Errorf("first body = %q; want %q", gotFirst.body, lostBody)
				}
				checkAnswerHead(t, gotFirst.head, http.StatusServiceUnavailable, "false")
				if got := waitsOf("first"); got != attemptWaits {
					t.Errorf("the first request paused %d times; want %d", got, attemptWaits)
				}
				if got := waitsOf("second"); got != tt.secondWaits {
					t.Errorf("the second request paused %d times by the end; want %d", got, tt.secondWaits)
				}
				if got := waitsOf(""); got != 0 {
					t.Errorf("%d pauses came with a context that names neither request", got)
				}
				for i, d := range w.waitCalls() {
					if d != attemptWait {
						t.Errorf("Wait call %d paused %v; want %v", i+1, d, attemptWait)
					}
				}
				wantCalls := []acquireCall{{Got: "gen-a"}, {Got: "gen-b"}}
				if diff := gocmp.Diff(wantCalls, w.src.acquireCalls()); diff != "" {
					t.Errorf("Acquire calls, one per request (-want +got):\n%s", diff)
				}
				oneGenTCheckOnly(t, a, oneGenTFirstBody)
				oneGenTCheckOnly(t, b, oneGenTSecondBody)
				if got := w.upstream.Load(); got != tt.upstream {
					t.Errorf("upstream submissions = %d; want %d", got, tt.upstream)
				}
				lost = w.lostReports()
				checkLost(t, lost, tt.lost)
				oneGenTCheckTypes(t, "when both requests had finished", lost, tt.errorTypes)
				checkLeasesClosed(t, w)
			})
		})
	}
}

// oneGenTCheckOnly fails unless g received exactly one connection, which carried one complete POST /v1/messages
// with body and no byte after it.
func oneGenTCheckOnly(t *testing.T, g *generation, body string) {
	t.Helper()
	conns := g.connections()
	if len(conns) != 1 || g.reads.Load() != 1 {
		t.Errorf("%q got %d connections and read %d requests; want exactly the one request it took", g.name, len(conns), g.reads.Load())
		return
	}
	if c := conns[0]; c.line != "POST /v1/messages HTTP/1.1" || string(c.body) != body {
		t.Errorf("%q received %q with body %q; want POST /v1/messages HTTP/1.1 with body %q", g.name, c.line, c.body, body)
	}
	if n := conns[0].bytesAfter.Load(); n != 0 {
		t.Errorf("%q received %d bytes after the request it parsed; the leg carries that one request", g.name, n)
	}
}

// oneGenTCheckTypes compares the error types of the Lost reports, in order.
func oneGenTCheckTypes(t *testing.T, when string, lost []relay.Lost, want []string) {
	t.Helper()
	var got []string
	for _, l := range lost {
		got = append(got, l.ErrorType)
	}
	if diff := gocmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("error types of the Lost reports %s (-want +got):\n%s", when, diff)
	}
}
