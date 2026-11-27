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
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor/relay"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// unchecked marks a count a case does not decide.
const unchecked = -1

// TestDeliveryAttempts decides the relay's delivery state machine (lib/commands.js:2345-2391,2420-2441) with the
// generation lease as the delivery identity.
//
// A lease handed to the relay is a delivery to its generation: the relay latches it before writing, and never sends
// the request to that generation again. Before the worker's final response head, the request is sent again to every
// newer generation that accepts it once the one that took it is no longer current, bounded only by the attempts
// (lib/commands.js:2363-2389). A generation is current until it exits or is finally stopped, also while it drains: the
// source's current worker is the child process, replaced only when the next one starts after the exit
// (:2613,2625,2651,2682,2695,2709-2711). When the attempts run out, the answer depends on the generation that took
// the request: none took it, a retryable 503; the generation that took it is still alive, 503 with
// X-Should-Retry: false and one Lost report (lib/commands.js:2363-2364); the generation that took it has ended and no
// newer one accepted, the source's retryable 503, because a later attempt that finds no worker or another one does not
// carry the acceptance forward (:2367-2370, :2385, :2438; :2651 sets the current worker to null at its exit). The
// exception is the last attempt: when the connection of the last attempt reached a generation and then failed, the
// request is answered as taken whether or not that generation is still current (:2384).
func TestDeliveryAttempts(t *testing.T) {
	for name, tt := range deliveryCases() {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) { runDeliveryCase(t, w, tt) })
		})
	}
}

// deliveryCase is one case of the delivery state machine.
type deliveryCase struct {
	setup func(t *testing.T, w *world)
	// check runs after the answer with the case's own assertions.
	check func(t *testing.T, w *world)

	status int
	// retry is the X-Should-Retry value; empty means the field must be absent.
	retry     string
	body      string
	delivered []workerlink.Generation
	reads     map[workerlink.Generation]int32
	upstream  int32
	// waits and acquires are the number of Wait and Acquire calls, or unchecked.
	waits    int
	acquires int
	lost     []relay.Lost
}

func deliveryCases() map[string]deliveryCase {
	return map[string]deliveryCase{
		"success: a worker that appears after failures before delivery gets the request once": {
			setup: func(t *testing.T, w *world) {
				g := w.reserve("gen-a", 41001, 4101, answering(okAnswer))
				w.setOnWait(func(n int) error {
					if n == 3 {
						g.publish()
					}
					return nil
				})
			},
			status:    http.StatusOK,
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     3,
			acquires:  4,
		},
		"success: a worker that appears during the last wait gets the request on the last attempt": {
			setup: func(t *testing.T, w *world) {
				g := w.reserve("gen-a", 41001, 4101, answering(okAnswer))
				w.setOnWait(func(n int) error {
					if n == attemptWaits {
						g.publish()
					}
					return nil
				})
			},
			status:    http.StatusOK,
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     attemptWaits,
			acquires:  attemptWaits + 1,
		},
		"error: no eligible worker in any attempt leaves the failure retryable": {
			setup:    func(*testing.T, *world) {},
			status:   http.StatusServiceUnavailable,
			body:     undeliveredBody,
			waits:    attemptWaits,
			acquires: attemptWaits + 1,
		},
		"error: a worker source failure in every attempt leaves the failure retryable": {
			setup: func(t *testing.T, w *world) {
				w.start("gen-a", 41001, 4101, answering(okAnswer))
				w.src.setFail(func(int) error { return errors.New("worker unavailable") })
			},
			status:   http.StatusServiceUnavailable,
			body:     undeliveredBody,
			waits:    attemptWaits,
			acquires: attemptWaits + 1,
		},
		"error: refused connections in every attempt leave the failure retryable": {
			setup: func(t *testing.T, w *world) {
				g := w.reserve("gen-a", 41001, 4101, answering(okAnswer))
				g.dialErr = errors.New("refused")
				g.publish()
			},
			status:   http.StatusServiceUnavailable,
			body:     undeliveredBody,
			waits:    attemptWaits,
			acquires: attemptWaits + 1,
		},
		"success: the request is sent again after the generation that took it ended": {
			setup: func(t *testing.T, w *world) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				var a *generation
				a = w.start("gen-a", 41001, 4101, failing(false, func(*workerConn) {
					a.end()
					b.publish()
				}))
			},
			status:    http.StatusOK,
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a", "gen-b"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1, "gen-b": 1},
			upstream:  1,
			waits:     unchecked,
			acquires:  unchecked,
		},
		"success: a generation that submitted upstream and ended is followed by a second submission": {
			setup: func(t *testing.T, w *world) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				var a *generation
				a = w.start("gen-a", 41001, 4101, failing(true, func(*workerConn) {
					a.end()
					b.publish()
				}))
			},
			status:    http.StatusOK,
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a", "gen-b"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1, "gen-b": 1},
			upstream:  2,
			waits:     unchecked,
			acquires:  unchecked,
		},
		"error: a generation that is still current is not sent the request again": {
			setup: func(t *testing.T, w *world) {
				w.start("gen-a", 41001, 4101, failing(true, nil))
				w.src.setLegErr(errPrivateLeg)
			},
			status:    http.StatusServiceUnavailable,
			retry:     "false",
			body:      lostBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     attemptWaits,
			acquires:  unchecked,
			lost:      []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
		},
		"error: a worker that closes without reading has still been delivered the request": {
			setup: func(t *testing.T, w *world) {
				w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					c.close()
				})
			},
			status:    http.StatusServiceUnavailable,
			retry:     "false",
			body:      lostBody,
			delivered: []workerlink.Generation{"gen-a"},
			waits:     attemptWaits,
			acquires:  unchecked,
			lost:      []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
		},
		"error: a stop between attempts answers at once for a generation that is still current": {
			setup: func(t *testing.T, w *world) {
				w.start("gen-a", 41001, 4101, failing(true, nil))
				w.setOnWait(func(n int) error {
					if n == 1 {
						return errStopping
					}
					return nil
				})
			},
			status:    http.StatusServiceUnavailable,
			retry:     "false",
			body:      lostBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     1,
			acquires:  unchecked,
			lost:      []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
		},
		"error: a stop right after a connection that took the request failed is answered as taken although its generation ended": {
			setup: func(t *testing.T, w *world) {
				var a *generation
				a = w.start("gen-a", 41001, 4101, failing(true, func(*workerConn) { a.end() }))
				w.setOnWait(func(n int) error {
					if n == 1 {
						return errStopping
					}
					return nil
				})
			},
			status:    http.StatusServiceUnavailable,
			retry:     "false",
			body:      lostBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     1,
			acquires:  1,
			lost:      []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
		},
		"error: a stop that arrives during the wait after a failed taken connection is answered as taken although its generation ended in that wait": {
			setup: func(t *testing.T, w *world) {
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.setMidWait(func(n int) error {
					if n == 1 {
						a.end()
						return errStopping
					}
					return nil
				})
			},
			status:    http.StatusServiceUnavailable,
			retry:     "false",
			body:      lostBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     1,
			acquires:  1,
			lost:      []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
		},
		"error: a stop after a later attempt that delivered nothing leaves the failure retryable": {
			setup: func(t *testing.T, w *world) {
				var a *generation
				a = w.start("gen-a", 41001, 4101, failing(true, func(*workerConn) { a.end() }))
				w.setOnWait(func(n int) error {
					if n == 2 {
						return errStopping
					}
					return nil
				})
			},
			status:    http.StatusServiceUnavailable,
			body:      undeliveredBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     2,
			acquires:  2,
		},
		"error: a stop between attempts answers at once before any delivery": {
			setup: func(t *testing.T, w *world) {
				w.setOnWait(func(n int) error {
					if n == 2 {
						return errStopping
					}
					return nil
				})
			},
			status:   http.StatusServiceUnavailable,
			body:     undeliveredBody,
			waits:    2,
			acquires: 2,
		},
		"error: a generation that ended after taking the request and has no successor is answered as retryable": {
			setup: func(t *testing.T, w *world) {
				var a *generation
				a = w.start("gen-a", 41001, 4101, failing(false, func(*workerConn) { a.end() }))
			},
			status:    http.StatusServiceUnavailable,
			body:      undeliveredBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			waits:     attemptWaits,
			acquires:  unchecked,
		},
		"error: a connection that took the request on the last attempt and failed is answered as taken although its generation ended": {
			setup: func(t *testing.T, w *world) {
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
			status:    http.StatusServiceUnavailable,
			retry:     "false",
			body:      lostBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			waits:     attemptWaits,
			acquires:  attemptWaits + 1,
			lost:      []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
		},
		"error: a generation that took the request on the attempt before the last and ended is answered as retryable": {
			setup: func(t *testing.T, w *world) {
				var a *generation
				a = w.reserve("gen-a", 41001, 4101, failing(false, func(*workerConn) { a.end() }))
				w.setOnWait(func(n int) error {
					if n == attemptWaits-1 {
						a.publish()
					}
					return nil
				})
			},
			status:    http.StatusServiceUnavailable,
			body:      undeliveredBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			waits:     attemptWaits,
			acquires:  attemptWaits + 1,
		},
		"error: a generation that took the request on the last attempt and is still current is answered as taken": {
			setup: func(t *testing.T, w *world) {
				a := w.reserve("gen-a", 41001, 4101, failing(true, nil))
				w.setOnWait(func(n int) error {
					if n == attemptWaits {
						a.publish()
					}
					return nil
				})
			},
			status:    http.StatusServiceUnavailable,
			retry:     "false",
			body:      lostBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     attemptWaits,
			acquires:  attemptWaits + 1,
			lost:      []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
		},
		"error: a generation that ends during the waits is forgotten as the taker": {
			setup: func(t *testing.T, w *world) {
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.setOnWait(func(n int) error {
					if n == 10 {
						a.end()
					}
					return nil
				})
			},
			status:    http.StatusServiceUnavailable,
			body:      undeliveredBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     attemptWaits,
			acquires:  unchecked,
		},
		"error: a retired generation that is still alive keeps the request as taken": {
			setup: func(t *testing.T, w *world) {
				var a *generation
				a = w.start("gen-a", 41001, 4101, failing(true, func(*workerConn) { a.retire() }))
			},
			status:    http.StatusServiceUnavailable,
			retry:     "false",
			body:      lostBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     attemptWaits,
			acquires:  unchecked,
			lost:      []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
		},
		"success: a successor on the same port is a new generation and gets the request": {
			setup: func(t *testing.T, w *world) {
				b := w.reserve("gen-b", 41001, 4102, answering(okAnswer))
				var a *generation
				a = w.start("gen-a", 41001, 4101, failing(false, func(*workerConn) {
					a.end()
					b.publish()
				}))
			},
			check: func(t *testing.T, w *world) {
				for _, l := range w.src.handed() {
					if got := l.Authority(); got != "127.0.0.1:41001" {
						t.Errorf("lease of %q has authority %q; both generations are at 127.0.0.1:41001", l.Generation(), got)
					}
				}
			},
			status:    http.StatusOK,
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a", "gen-b"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1, "gen-b": 1},
			upstream:  1,
			waits:     unchecked,
			acquires:  unchecked,
		},
		"success: each newer generation gets the request after the one that took it ended": {
			setup: func(t *testing.T, w *world) {
				c := w.reserve("gen-c", 41003, 4103, answering(okAnswer))
				var a, b *generation
				b = w.reserve("gen-b", 41002, 4102, failing(true, func(*workerConn) {
					b.end()
					c.publish()
				}))
				a = w.start("gen-a", 41001, 4101, failing(true, func(*workerConn) {
					a.end()
					b.publish()
				}))
			},
			status:    http.StatusOK,
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a", "gen-b", "gen-c"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1, "gen-b": 1, "gen-c": 1},
			upstream:  3,
			waits:     unchecked,
			acquires:  unchecked,
		},
		"error: three generations that each took the request and ended leave the failure retryable": {
			setup: func(t *testing.T, w *world) {
				var a, b, c *generation
				c = w.reserve("gen-c", 41003, 4103, failing(true, func(*workerConn) { c.end() }))
				b = w.reserve("gen-b", 41002, 4102, failing(true, func(*workerConn) {
					b.end()
					c.publish()
				}))
				a = w.start("gen-a", 41001, 4101, failing(true, func(*workerConn) {
					a.end()
					b.publish()
				}))
			},
			status:    http.StatusServiceUnavailable,
			body:      undeliveredBody,
			delivered: []workerlink.Generation{"gen-a", "gen-b", "gen-c"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1, "gen-b": 1, "gen-c": 1},
			upstream:  3,
			waits:     attemptWaits,
			acquires:  unchecked,
		},
		"success: a dial that completes after its generation was retired is a delivery to it": {
			setup: func(t *testing.T, w *world) {
				a := w.start("gen-a", 41001, 4101, answering(okAnswer))
				w.setDialGate(func(_ context.Context, g *generation) error {
					if g == a {
						a.retire()
					}
					return nil
				})
			},
			status:    http.StatusOK,
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     0,
			acquires:  1,
		},
		"error: a request handed over after retirement and then lost is answered as taken": {
			setup: func(t *testing.T, w *world) {
				a := w.start("gen-a", 41001, 4101, failing(true, nil))
				w.setDialGate(func(_ context.Context, g *generation) error {
					if g == a {
						a.retire()
					}
					return nil
				})
			},
			status:    http.StatusServiceUnavailable,
			retry:     "false",
			body:      lostBody,
			delivered: []workerlink.Generation{"gen-a"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1},
			upstream:  1,
			waits:     attemptWaits,
			acquires:  unchecked,
			lost:      []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}},
		},
		"success: a dial cancelled by the abrupt end of its generation is not a delivery": {
			setup: func(t *testing.T, w *world) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, answering(okAnswer))
				parked := make(chan struct{})
				w.setDialGate(func(ctx context.Context, g *generation) error {
					if g != a {
						return nil
					}
					close(parked)
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-w.done:
						return net.ErrClosed
					}
				})
				go func() {
					select {
					case <-parked:
					case <-w.done:
						return
					}
					a.end()
					b.publish()
				}()
			},
			status:    http.StatusOK,
			body:      "ok",
			delivered: []workerlink.Generation{"gen-b"},
			reads:     map[workerlink.Generation]int32{"gen-b": 1},
			upstream:  1,
			waits:     unchecked,
			acquires:  unchecked,
		},
		"success: a dial that completes after the abrupt end of its generation is not a delivery": {
			setup: func(t *testing.T, w *world) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				a := w.start("gen-a", 41001, 4101, answering(okAnswer))
				parked := make(chan struct{})
				w.setDialGate(func(ctx context.Context, g *generation) error {
					if g != a {
						return nil
					}
					close(parked)
					// The end cancels the dial; this one ignores that and completes anyway.
					select {
					case <-ctx.Done():
					case <-w.done:
					}
					return nil
				})
				go func() {
					select {
					case <-parked:
					case <-w.done:
						return
					}
					a.end()
					b.publish()
				}()
			},
			check: func(t *testing.T, w *world) {
				w.mu.Lock()
				a := w.byKey["gen-a"]
				w.mu.Unlock()
				if n := len(a.connections()); n != 1 {
					t.Errorf("gen-a got %d connections; want the one late dial, closed before any request", n)
				}
			},
			status:    http.StatusOK,
			body:      "ok",
			delivered: []workerlink.Generation{"gen-b"},
			reads:     map[workerlink.Generation]int32{"gen-b": 1},
			upstream:  1,
			waits:     unchecked,
			acquires:  unchecked,
		},
		"success: a successor on the same port answers after interim responses failed the delivered attempt": {
			setup: func(t *testing.T, w *world) {
				b := w.reserve("gen-b", 41001, 4102, answering(okAnswer))
				var a *generation
				a = w.start("gen-a", 41001, 4101, failing(false, func(c *workerConn) {
					c.send(strings.Repeat("HTTP/1.1 100 Continue\r\n\r\n", 6))
					select {
					case <-c.relayClosed:
					case <-c.g.w.done:
					}
					a.end()
					b.publish()
				}))
			},
			status:    http.StatusOK,
			body:      "ok",
			delivered: []workerlink.Generation{"gen-a", "gen-b"},
			reads:     map[workerlink.Generation]int32{"gen-a": 1, "gen-b": 1},
			upstream:  1,
			waits:     unchecked,
			acquires:  unchecked,
		},
	}
}

// runDeliveryCase runs one delivery case in w.
func runDeliveryCase(t *testing.T, w *world, tt deliveryCase) {
	tt.setup(t, w)
	h := w.handler(t)
	cw := newClientWriter()
	o := await(t, serve(h, cw, postMessages(t.Context(), requestBody)), "the relay's answer")
	synctest.Wait()
	mustReturn(t, o)
	got := cw.snapshot()
	if diff := gocmp.Diff([]int{tt.status}, got.statuses); diff != "" {
		t.Errorf("statuses written (-want +got):\n%s", diff)
	}
	if got.body != tt.body {
		t.Errorf("body = %q; want %q", got.body, tt.body)
	}
	checkAnswerHead(t, got.head, tt.status, tt.retry)
	if diff := gocmp.Diff(tt.delivered, w.src.delivered()); diff != "" {
		t.Errorf("deliveries by generation (-want +got):\n%s", diff)
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
	if tt.waits != unchecked && len(waits) != tt.waits {
		t.Errorf("Wait was called %d times; want %d", len(waits), tt.waits)
	}
	for i, d := range waits {
		if d != attemptWait {
			t.Errorf("Wait call %d paused %v; want %v", i+1, d, attemptWait)
		}
	}
	if calls := w.src.acquireCalls(); tt.acquires != unchecked && len(calls) != tt.acquires {
		t.Errorf("Acquire was called %d times (%+v); want %d", len(calls), calls, tt.acquires)
	}
	checkAlternation(t, w.callOrder())
	checkLost(t, w.lostReports(), tt.lost)
	checkLeasesClosed(t, w)
	if tt.check != nil {
		tt.check(t, w)
	}
}

// checkAlternation fails unless the relay starts with a lease request and never makes two lease requests without a
// wait between them: the source schedules every later attempt 50 ms after the previous one
// (lib/commands.js:2360-2362,2365,2370,2385). Several waits in a row are allowed: while the generation that took the
// request is still current the source waits without a new request (:2363-2365). That this relay asks for a lease at
// all on such an iteration is a property of the lease seam, not of the source; the number of waits is checked by the
// case itself.
func checkAlternation(t testing.TB, order []string) {
	t.Helper()
	for i, e := range order {
		switch {
		case i == 0 && e != "acquire":
			t.Errorf("the relay's first call was %s; want a lease request (order %v)", e, order)
			return
		case i > 0 && e == "acquire" && order[i-1] == "acquire":
			t.Errorf("calls %d and %d of the relay are lease requests without a wait between them (order %v)", i, i+1, order)
			return
		}
	}
}

// requestBody is the client's request in the delivery cases.
const requestBody = `{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[{"role":"user","content":"雪\u0000"}]}`

// checkSameRequest fails unless every request g read completely is the client's: the same request line and the same
// body bytes, also on a delivery after another generation took the request.
func checkSameRequest(t *testing.T, g *generation, line, body string) {
	t.Helper()
	for i, c := range g.connections() {
		if c.line == "" {
			continue
		}
		if c.line != line || string(c.body) != body {
			t.Errorf("connection %d of %q carried %q with body %q; want %q with the client's body %q", i, g.name, c.line, c.body, line, body)
		}
	}
}

// checkAnswerHead checks the head of an answer: a 503 the relay wrote itself is JSON, carries X-Should-Retry only
// when a worker took the request and keeps the client connection (lib/commands.js:2428-2431); a relayed worker answer
// ends it (the worker leg's Connection: close is copied, as the measured source does).
func checkAnswerHead(t *testing.T, head http.Header, status int, retry string) {
	t.Helper()
	if status == http.StatusServiceUnavailable {
		if got := head.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q; want application/json", got)
		}
		if _, ok := head["Connection"]; ok {
			t.Errorf("the relay's own 503 carries Connection %q; the source sends none", head.Values("Connection"))
		}
	} else if diff := gocmp.Diff([]string{"close"}, head.Values("Connection")); diff != "" {
		t.Errorf("Connection of the relayed answer (-want +got):\n%s", diff)
	}
	values, ok := head["X-Should-Retry"]
	switch {
	case retry == "" && ok:
		t.Errorf("X-Should-Retry = %q; want the field absent", values)
	case retry != "" && !slices.Equal(values, []string{retry}):
		t.Errorf("X-Should-Retry = %q; want %q", values, retry)
	}
}

// TestCommittedFailure decides a worker failure after its final response head was relayed: the client connection is
// broken without a rewritten status (lib/commands.js:2393-2401), no generation is sent the request again even when a
// newer one is eligible, and no Lost report is made.
func TestCommittedFailure(t *testing.T) {
	const streamHead = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n"
	const event = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	tests := map[string]struct {
		// answer is what the worker sends and waits to see relayed; tail is what it sends after that, just before it
		// dies. A cut chunk is sent as tail: a chunked reader may hold the end of a complete event together with the
		// start of a cut chunk inside one Read, so the event can only be required while the worker lives if it
		// arrived alone.
		answer, tail string
		// atLeast and atMost bound the body the client received: every complete event the worker sent reaches it,
		// and nothing it did not send.
		atLeast, atMost string
		// replace ends the worker's generation and publishes a successor before it dies.
		replace bool
	}{
		"error: the worker dies after its head and first event while a successor is eligible": {
			answer:  streamHead + chunk(event),
			atLeast: event,
			atMost:  event,
			replace: true,
		},
		"error: the worker dies after its head and first event and stays current": {
			answer:  streamHead + chunk(event),
			atLeast: event,
			atMost:  event,
		},
		"error: the worker dies right after its final head": {
			answer:  streamHead,
			replace: true,
		},
		"error: the stream is cut inside a chunk": {
			answer:  streamHead + chunk(event),
			tail:    "64\r\n" + "event: content",
			atLeast: event,
			atMost:  event + "event: content",
		},
		"error: the body ends before its declared length": {
			answer:  "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\nConnection: close\r\n\r\n{\"partial\":",
			atLeast: `{"partial":`,
			atMost:  `{"partial":`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				cw := newClientWriter()
				var a *generation
				a = w.start("gen-a", 41001, 4101, failing(true, func(c *workerConn) {
					c.send(tt.answer)
					// Die only once the client has the head and what the relay must pass on before the failure; the
					// limit only keeps a relay that never passes it on from parking the worker.
					if !cw.until(func(s snapshot) bool {
						return len(s.statuses) > 0 && (tt.atLeast == "" || flushedBody(len(tt.atLeast))(s))
					}, time.Minute) {
						t.Errorf("while the worker was alive the client got no head or not %q flushed; the relay must pass on what it received before the failure", tt.atLeast)
					}
					if tt.tail != "" {
						c.send(tt.tail)
					}
					if tt.replace {
						a.end()
						b.publish()
					}
				}))
				o := await(t, serve(w.handler(t), cw, postMessages(t.Context(), `{"stream":true}`)), "the end of the relayed answer")
				synctest.Wait()
				mustAbort(t, o)
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{http.StatusOK}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if !strings.HasPrefix(got.body, tt.atLeast) || !strings.HasPrefix(tt.atMost, got.body) {
					t.Errorf("relayed body = %q; want at least %q and at most %q", got.body, tt.atLeast, tt.atMost)
				}
				if got.body != "" && (len(got.flushes) == 0 || got.flushes[len(got.flushes)-1].bodyLen != len(got.body)) {
					t.Errorf("flushes %+v; the bytes relayed before the failure must have been flushed", got.flushes)
				}
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("deliveries by generation (-want +got):\n%s", diff)
				}
				if n := b.reads.Load(); n != 0 {
					t.Errorf("the successor read %d requests; a committed answer is never sent again", n)
				}
				if got := [2]int32{a.reads.Load(), w.upstream.Load()}; got != [2]int32{1, 1} {
					t.Errorf("the worker read %d requests and submitted %d upstream; want one of each", got[0], got[1])
				}
				checkLost(t, w.lostReports(), nil)
				checkLeasesClosed(t, w)
			})
		})
	}
}

// chunk frames s as one HTTP/1.1 chunk.
func chunk(s string) string {
	return strconv.FormatInt(int64(len(s)), 16) + "\r\n" + s + "\r\n"
}

// TestInterimResponses decides the interim heads before a worker's final head: up to five are passed over and the
// final answer is relayed; a sixth, a protocol switch or a status below 100 fails the delivered attempt, which for a
// generation that is still current is the taken-request 503. The bound of five is the port's own, against a worker
// that never sends its final head; the source waits without a count. No interim head reaches the client.
func TestInterimResponses(t *testing.T) {
	const cont = "HTTP/1.1 100 Continue\r\n\r\n"
	const hints = "HTTP/1.1 103 Early Hints\r\nLink: </style.css>; rel=preload\r\n\r\n"
	// final carries a field of its own, so that the copied head can be told from an interim one.
	const final = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nX-Final-Head: yes\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	tests := map[string]struct {
		heads  string
		status int
		retry  string
		body   string
		lost   bool
	}{
		"success: one interim head is passed over": {
			heads:  cont,
			status: http.StatusOK,
			body:   "ok",
		},
		"success: five interim heads are passed over": {
			heads:  cont + hints + cont + hints + cont,
			status: http.StatusOK,
			body:   "ok",
		},
		"error: a sixth interim head fails the attempt": {
			heads:  strings.Repeat(cont, 6),
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
		"error: a protocol switch fails the attempt": {
			heads:  "HTTP/1.1 101 Switching Protocols\r\nUpgrade: fixture\r\nConnection: Upgrade\r\n\r\n",
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
		"error: a status below 100 fails the attempt": {
			heads:  "HTTP/1.1 099 Odd\r\n\r\n",
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					defer c.close()
					if !c.readRequest() {
						return
					}
					c.submit()
					if !c.send(tt.heads) {
						return
					}
					c.send(final)
				})
				cw := newClientWriter()
				o := await(t, serve(w.handler(t), cw, postMessages(t.Context(), requestBody)), "the relay's answer")
				synctest.Wait()
				mustReturn(t, o)
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{tt.status}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if got.body != tt.body {
					t.Errorf("body = %q; want %q", got.body, tt.body)
				}
				checkAnswerHead(t, got.head, tt.status, tt.retry)
				if tt.status == http.StatusOK {
					if got.head.Get("X-Final-Head") != "yes" {
						t.Errorf("the relayed head %v is not the final head", got.head)
					}
					if v, ok := got.head["Link"]; ok {
						t.Errorf("the relayed head carries the interim head's Link %q", v)
					}
				}
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("deliveries by generation (-want +got):\n%s", diff)
				}
				var want []relay.Lost
				if tt.lost {
					want = []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}}
				}
				checkLost(t, w.lostReports(), want)
				checkLeasesClosed(t, w)
			})
		})
	}
}
