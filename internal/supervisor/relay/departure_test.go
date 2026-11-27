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
	"slices"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// The public listener reports a departed client to the relay only by ending the request context, and a handler cannot
// tell from that whether the client half-closed in order, reset its connection, or the server is shutting down. These
// tests therefore decide only what the three share: after the departure the relay makes no new attempt and writes
// nothing more on the public response, and ServeHTTP ends by returning or by http.ErrAbortHandler. What the measured
// half-close fixes beyond that (the public side's own orderly end, the worker's provider request running to its end)
// and what a reset fixes (the measured disconnect table, the existing public-listener tests) are decided at the public
// listener, not here.

const (
	sseHead   = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n"
	sseFirst  = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	sseSecond = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	sseLast   = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// gatedWorker sends head, waits for release and sends rest; it records whether its whole answer could be written.
func gatedWorker(head, rest string, release <-chan struct{}) func(c *workerConn) {
	return func(c *workerConn) {
		defer close(c.finished)
		defer c.close()
		if !c.readRequest() {
			return
		}
		c.submit()
		if head != "" && !c.send(head) {
			return
		}
		select {
		case <-release:
		case <-c.g.w.done:
			return
		}
		c.send(rest)
	}
}

// TestClientDeparture decides what a departed client does to the relay's attempts and to the public response.
func TestClientDeparture(t *testing.T) {
	tests := map[string]struct {
		run func(t *testing.T, w *world)
	}{
		"success: a client that leaves before any worker is eligible ends the attempts": {
			run: func(t *testing.T, w *world) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				w.setOnWait(func(n int) error {
					if n == 3 {
						cancel()
					}
					return nil
				})
				cw := newClientWriter()
				o := await(t, serve(w.handler(t), cw, postMessages(ctx, `{}`)), "the relay to give up")
				mustEnd(t, o)
				// Let any timer of a relay that kept trying run; synctest.Wait shows that nothing is left to run.
				time.Sleep(time.Minute)
				synctest.Wait()
				if calls := w.src.acquireCalls(); len(calls) != 3 {
					t.Errorf("Acquire was called %d times; the departure during the third wait ends the attempts at 3", len(calls))
				}
				if got := cw.snapshot(); len(got.statuses) != 0 || got.body != "" {
					t.Errorf("the relay wrote %v %q to a departed client; want nothing", got.statuses, got.body)
				}
				checkLost(t, w.lostReports(), nil)
			},
		},
		"success: a client that leaves while its answer is pending gets nothing and causes no new attempt": {
			run: func(t *testing.T, w *world) {
				release := make(chan struct{})
				g := w.start("gen-a", 41001, 4101, gatedWorker("", okAnswer, release))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cw := newClientWriter()
				done := serve(w.handler(t), cw, postMessages(ctx, `{}`))
				synctest.Wait()
				onlyConnection(t, g)
				cw.mark()
				cancel()
				synctest.Wait()
				close(release)
				mustEnd(t, await(t, done, "the relay to finish"))
				time.Sleep(time.Minute)
				synctest.Wait()
				checkNothingAfterDeparture(t, w, cw, "")
				checkReleased(t, w)
			},
		},
		"success: a client that leaves and then loses the worker that took its request causes no new attempt": {
			run: func(t *testing.T, w *world) {
				b := w.reserve("gen-b", 41002, 4102, answering(okAnswer))
				release := make(chan struct{})
				a := w.start("gen-a", 41001, 4101, gatedWorker("", okAnswer, release))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cw := newClientWriter()
				done := serve(w.handler(t), cw, postMessages(ctx, `{}`))
				synctest.Wait()
				onlyConnection(t, a)
				cw.mark()
				cancel()
				synctest.Wait()
				a.end()
				b.publish()
				mustEnd(t, await(t, done, "the relay to finish"))
				time.Sleep(time.Minute)
				synctest.Wait()
				if n := b.reads.Load(); n != 0 {
					t.Errorf("the successor read %d requests of a departed client; want none", n)
				}
				checkNothingAfterDeparture(t, w, cw, "")
				close(release)
				checkReleased(t, w)
			},
		},
		"success: a client that leaves during a stream gets nothing more": {
			run: func(t *testing.T, w *world) {
				release := make(chan struct{})
				g := w.start("gen-a", 41001, 4101, gatedWorker(sseHead+chunk(sseFirst), chunk(sseSecond)+chunk(sseLast)+"0\r\n\r\n", release))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cw := newClientWriter()
				done := serve(w.handler(t), cw, postMessages(ctx, `{"stream":true}`))
				synctest.Wait()
				onlyConnection(t, g)
				if got := cw.snapshot(); got.body != sseFirst || len(got.flushes) == 0 || got.flushes[len(got.flushes)-1].bodyLen != len(sseFirst) {
					t.Fatalf("before the departure the client has %q with flushes %+v; want the first event flushed", got.body, got.flushes)
				}
				cw.mark()
				cancel()
				synctest.Wait()
				close(release)
				mustEnd(t, await(t, done, "the relay to finish"))
				time.Sleep(time.Minute)
				synctest.Wait()
				checkNothingAfterDeparture(t, w, cw, sseFirst)
				checkReleased(t, w)
			},
		},
		"success: a failed client write ends the relayed stream": {
			run: func(t *testing.T, w *world) {
				cw := newClientWriter()
				w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					defer c.close()
					if !c.readRequest() {
						return
					}
					c.submit()
					if !c.send(sseHead + chunk(sseFirst)) {
						return
					}
					// One event at a time: the next is sent once the relay has made the Write of the previous one. The
					// limit only keeps a relay that stops writing early from parking the worker.
					for i, e := range []string{chunk(sseSecond), chunk(sseLast) + "0\r\n\r\n"} {
						if !cw.until(func(s snapshot) bool { return s.writes >= i+1 }, time.Minute) {
							t.Errorf("the relay did not write event %d while the worker was alive", i+1)
						}
						if !c.send(e) {
							return
						}
					}
				})
				cw.failWritesFrom(2)
				mustEnd(t, await(t, serve(w.handler(t), cw, postMessages(t.Context(), `{"stream":true}`)), "the relay to finish"))
				time.Sleep(time.Minute)
				synctest.Wait()
				cw.mu.Lock()
				writes := cw.writes
				cw.mu.Unlock()
				if writes != 2 {
					t.Errorf("the relay called Write %d times; after the failed second write it writes nothing more", writes)
				}
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("deliveries by generation (-want +got):\n%s", diff)
				}
				if n := len(w.src.acquireCalls()); n != 1 {
					t.Errorf("Acquire was called %d times; a departed client causes no new attempt", n)
				}
				checkLost(t, w.lostReports(), nil)
				checkReleased(t, w)
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, tt.run)
		})
	}
}

// checkReleased waits until every fake worker has finished its connection and then fails for every lease the relay
// still holds: after a departure the relay may close the lease early or once the worker is done, but it closes it.
func checkReleased(t *testing.T, w *world) {
	t.Helper()
	w.mu.Lock()
	gens := slices.Clone(w.gens)
	w.mu.Unlock()
	for _, g := range gens {
		for _, c := range g.connections() {
			await(t, c.finished, "the fake worker of "+string(g.name)+" to finish its connection")
		}
	}
	synctest.Wait()
	checkLeasesClosed(t, w)
}

// onlyConnection returns the one connection that reached g after it read its request.
func onlyConnection(t *testing.T, g *generation) *workerConn {
	t.Helper()
	conns := g.connections()
	if len(conns) != 1 || g.reads.Load() != 1 {
		t.Fatalf("the worker has %d connections and read %d requests; want one request", len(conns), g.reads.Load())
	}
	return conns[0]
}

// checkNothingAfterDeparture fails when the relay touched the public response after the mark, when the client holds
// more than before, when the request went anywhere after its one delivery to gen-a, or when a loss was reported.
func checkNothingAfterDeparture(t testing.TB, w *world, cw *clientWriter, before string) {
	t.Helper()
	if n := cw.callsAfterMark(); n != 0 {
		t.Errorf("the relay made %d calls on the response after the client departed; want none", n)
	}
	got := cw.snapshot()
	if got.body != before || (before == "" && len(got.statuses) != 0) {
		t.Errorf("the client holds %v %q; want only what it had before it departed (%q)", got.statuses, got.body, before)
	}
	if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
		t.Errorf("deliveries by generation (-want +got):\n%s", diff)
	}
	if n := len(w.src.acquireCalls()); n != 1 {
		t.Errorf("Acquire was called %d times; a departed client causes no new attempt", n)
	}
	if got := w.upstream.Load(); got != 1 {
		t.Errorf("upstream submissions = %d; want 1", got)
	}
	// A departure is not a lost request: the worker was not the one that failed.
	checkLost(t, w.lostReports(), nil)
}
