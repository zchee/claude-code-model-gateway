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
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestRelayedResponse decides how a worker's answer reaches the client: its status, fields and body unchanged, a
// stream flushed as it arrives and ended cleanly, and Connection: close on every relayed answer, which ends the client
// connection as the measured source does when it copies the worker's own Connection: close (lib/commands.js:2416-2417).
func TestRelayedResponse(t *testing.T) {
	tests := map[string]struct {
		method, target string
		answer         string
		status         int
		fields         http.Header
		body           string
	}{
		"success: status, fields and body of the worker answer reach the client": {
			method: http.MethodPost,
			target: "/v1/messages",
			answer: "HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nRequest-Id: req_1\r\nContent-Length: 13\r\nConnection: close\r\n\r\n{\"error\":\"x\"}",
			status: http.StatusNotFound,
			fields: http.Header{"Content-Type": {"application/json"}, "Request-Id": {"req_1"}, "Connection": {"close"}},
			body:   `{"error":"x"}`,
		},
		"success: an answer whose head has no connection field still ends the client connection": {
			method: http.MethodPost,
			target: "/v1/messages",
			answer: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\n\r\nok",
			status: http.StatusOK,
			fields: http.Header{"Content-Type": {"text/plain"}, "Connection": {"close"}},
			body:   "ok",
		},
		"success: a redirect is relayed and never followed": {
			method: http.MethodPost,
			target: "/v1/messages",
			answer: "HTTP/1.1 307 Temporary Redirect\r\nLocation: /v1/elsewhere\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
			status: http.StatusTemporaryRedirect,
			fields: http.Header{"Location": {"/v1/elsewhere"}, "Connection": {"close"}},
		},
		"success: a bodyless answer to an unexpected path is relayed bodyless": {
			method: http.MethodGet,
			target: "/unexpected-probe",
			answer: "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n",
			status: http.StatusNoContent,
			fields: http.Header{"Connection": {"close"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				w.start("gen-a", 41001, 4101, answering(tt.answer))
				cw := newClientWriter()
				o := await(t, serve(w.handler(t), cw, clientRequest(t.Context(), tt.method, tt.target, nil, nil, 0)), "the relayed answer")
				synctest.Wait()
				mustReturn(t, o)
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{tt.status}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				names := make([]string, 0, len(tt.fields))
				for name := range tt.fields {
					names = append(names, name)
				}
				if diff := gocmp.Diff(tt.fields, pick(got.head, names...)); diff != "" {
					t.Errorf("relayed fields (-want +got):\n%s", diff)
				}
				if got.body != tt.body {
					t.Errorf("body = %q; want %q", got.body, tt.body)
				}
				if n := len(w.src.acquireCalls()); n != 1 {
					t.Errorf("Acquire was called %d times; one answer needs one lease", n)
				}
				checkOneDelivery(t, w)
				checkLost(t, w.lostReports(), nil)
			})
		})
	}
}

// TestFreshLeasePerRequest decides that every request gets a lease of its own, even when the worker leaves the
// connection of an earlier answer open: no connection is pooled and no request is written on an earlier one.
func TestFreshLeasePerRequest(t *testing.T) {
	tests := map[string]struct {
		requests int
	}{
		"success: two requests on one handler use two leases": {requests: 2},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					defer c.close()
					if !c.readRequest() {
						return
					}
					c.submit()
					if !c.send("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\n\r\nok") {
						return
					}
					// Keep the connection open until the relay closes it, as a worker that would reuse it does.
					select {
					case <-c.relayClosed:
					case <-w.done:
					}
				})
				h := w.handler(t)
				for i := range tt.requests {
					cw := newClientWriter()
					o := await(t, serve(h, cw, postMessages(t.Context(), requestBody)), "the relayed answer")
					synctest.Wait()
					mustReturn(t, o)
					if got := cw.snapshot(); got.body != "ok" {
						t.Errorf("answer %d = %q; want \"ok\"", i+1, got.body)
					}
				}
				conns := g.connections()
				if len(conns) != tt.requests {
					t.Fatalf("the worker got %d connections for %d requests; want one each", len(conns), tt.requests)
				}
				for i, c := range conns {
					if n := c.bytesAfter.Load(); n != 0 {
						t.Errorf("the relay wrote %d more bytes on connection %d after its request", n, i)
					}
					if !closed(c.relayClosed) {
						t.Errorf("the relay kept connection %d open after its answer", i)
					}
				}
				if got := [2]int32{g.reads.Load(), w.upstream.Load()}; got != [2]int32{int32(tt.requests), int32(tt.requests)} {
					t.Errorf("reads and submissions = %v; want %d of each", got, tt.requests)
				}
				checkLeasesClosed(t, w)
			})
		})
	}
}

// TestRelayedStream decides that each event of a stream is flushed to the client before the worker sends the next,
// and that a stream the worker ends cleanly ends cleanly for the client.
func TestRelayedStream(t *testing.T) {
	tests := map[string]struct {
		events []string
	}{
		"success: three events are flushed one by one and the stream ends cleanly": {
			events: []string{sseFirst, sseSecond, sseLast},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				next := make(chan struct{})
				g := w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					defer c.close()
					if !c.readRequest() {
						return
					}
					c.submit()
					if !c.send(sseHead) {
						return
					}
					for _, e := range tt.events {
						if !c.send(chunk(e)) {
							return
						}
						select {
						case <-next:
						case <-w.done:
							return
						}
					}
					c.send("0\r\n\r\n")
				})
				cw := newClientWriter()
				done := serve(w.handler(t), cw, postMessages(t.Context(), `{"stream":true}`))
				var sent string
				for i, e := range tt.events {
					sent += e
					synctest.Wait()
					got := cw.snapshot()
					if got.body != sent || len(got.flushes) == 0 || got.flushes[len(got.flushes)-1].bodyLen != len(sent) {
						t.Fatalf("after event %d the client has %q with flushes %+v; want %q flushed", i+1, got.body, got.flushes, sent)
					}
					next <- struct{}{}
				}
				o := await(t, done, "the end of the stream")
				synctest.Wait()
				mustReturn(t, o)
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{http.StatusOK}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(http.Header{"Content-Type": {"text/event-stream"}, "Connection": {"close"}}, pick(got.head, "Content-Type", "Connection")); diff != "" {
					t.Errorf("relayed fields (-want +got):\n%s", diff)
				}
				if got.body != sent {
					t.Errorf("body = %q; want %q", got.body, sent)
				}
				if n := g.reads.Load(); n != 1 {
					t.Errorf("the worker read %d requests; want 1", n)
				}
				checkOneDelivery(t, w)
			})
		})
	}
}

// TestHealth decides the supervisor's additions to the worker's health document (lib/commands.js:2402-2414): the
// supervisor version, its process identifier and proxyRecovery, assigned as JavaScript assigns properties, with the
// worker's status kept and the length of the new document. The source's compat member belongs to the remote control
// feature, which is not ported: the relay does not add it, and leaves one the worker sent as the worker sent it. A
// document that is not a JSON object, or is larger than 1 MiB, is answered 502; a bodyless answer (HEAD, 204, 304) is
// relayed without a document. Every relayed health answer ends the client connection.
func TestHealth(t *testing.T) {
	identity := `"supervisorVersion":"` + testVersion + `","supervisorPid":` + strconv.Itoa(testSupervisorPID) + `,"proxyRecovery":true`
	// exact is a document of exactly 1 MiB.
	exact := `{"pad":"` + strings.Repeat("x", 1<<20-10) + `"}`
	over := `{"pad":"` + strings.Repeat("x", 1<<20-9) + `"}`
	tests := map[string]struct {
		method, target string
		status         int
		document       string
		chunked        bool
		wantStatus     int
		wantBody       string
		// relayed is set for an answer relayed from the worker, which carries Connection: close.
		relayed bool
	}{
		"success: the health document of the worker gains the supervisor identity": {
			method: http.MethodGet, target: "/healthz?probe=1", status: http.StatusOK,
			document:   `{"ok":true,"models":2}`,
			wantStatus: http.StatusOK, wantBody: `{"ok":true,"models":2,` + identity + `}`, relayed: true,
		},
		"success: a failing health status is kept": {
			method: http.MethodGet, target: "/healthz", status: http.StatusServiceUnavailable,
			document:   `{"ok":false}`,
			wantStatus: http.StatusServiceUnavailable, wantBody: `{"ok":false,` + identity + `}`, relayed: true,
		},
		"success: members the worker already sent keep their place": {
			method: http.MethodGet, target: "/healthz", status: http.StatusOK,
			document:   `{"supervisorPid":1,"ok":true}`,
			wantStatus: http.StatusOK,
			wantBody:   `{"supervisorPid":` + strconv.Itoa(testSupervisorPID) + `,"ok":true,"supervisorVersion":"` + testVersion + `","proxyRecovery":true}`,
			relayed:    true,
		},
		"success: a compat member the worker sent is left as it is": {
			method: http.MethodGet, target: "/healthz", status: http.StatusOK,
			document:   `{"ok":true,"compat":{"hostsDetected":false}}`,
			wantStatus: http.StatusOK, wantBody: `{"ok":true,"compat":{"hostsDetected":false},` + identity + `}`, relayed: true,
		},
		"success: a document of exactly 1 MiB is augmented": {
			method: http.MethodGet, target: "/healthz", status: http.StatusOK,
			document:   exact,
			wantStatus: http.StatusOK, wantBody: exact[:len(exact)-1] + "," + identity + "}", relayed: true,
		},
		"success: a chunked document of exactly 1 MiB is augmented": {
			method: http.MethodGet, target: "/healthz", status: http.StatusOK,
			document: exact, chunked: true,
			wantStatus: http.StatusOK, wantBody: exact[:len(exact)-1] + "," + identity + "}", relayed: true,
		},
		"success: a bodyless HEAD answer is relayed without a document": {
			method: http.MethodHead, target: "/healthz", status: http.StatusOK,
			wantStatus: http.StatusOK, relayed: true,
		},
		"success: a 204 answer is relayed without a document": {
			method: http.MethodGet, target: "/healthz", status: http.StatusNoContent,
			wantStatus: http.StatusNoContent, relayed: true,
		},
		"success: a 304 answer is relayed without a document": {
			method: http.MethodGet, target: "/healthz", status: http.StatusNotModified,
			wantStatus: http.StatusNotModified, relayed: true,
		},
		"error: a health document that is not an object is answered 502": {
			method: http.MethodGet, target: "/healthz", status: http.StatusOK,
			document:   `[1]`,
			wantStatus: http.StatusBadGateway,
		},
		"error: a health document that is not JSON is answered 502": {
			method: http.MethodGet, target: "/healthz", status: http.StatusOK,
			document:   `{"ok":tru`,
			wantStatus: http.StatusBadGateway,
		},
		"error: a health document one byte over 1 MiB is answered 502": {
			method: http.MethodGet, target: "/healthz", status: http.StatusOK,
			document:   over,
			wantStatus: http.StatusBadGateway,
		},
		"error: a chunked health document one byte over 1 MiB is answered 502": {
			method: http.MethodGet, target: "/healthz", status: http.StatusOK,
			document: over, chunked: true,
			wantStatus: http.StatusBadGateway,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				answer := "HTTP/1.1 " + strconv.Itoa(tt.status) + " " + http.StatusText(tt.status) + "\r\nContent-Type: application/json\r\n"
				bodyless := tt.method == http.MethodHead || tt.status == http.StatusNoContent || tt.status == http.StatusNotModified
				switch {
				case tt.status == http.StatusNoContent:
					answer += "Connection: close\r\n\r\n"
				case bodyless:
					answer += "Content-Length: 22\r\nConnection: close\r\n\r\n"
				case tt.chunked:
					answer += "Transfer-Encoding: chunked\r\nConnection: close\r\n\r\n" + chunk(tt.document) + "0\r\n\r\n"
				default:
					answer += "Content-Length: " + strconv.Itoa(len(tt.document)) + "\r\nConnection: close\r\n\r\n" + tt.document
				}
				w.start("gen-a", 41001, 4101, answering(answer))
				cw := newClientWriter()
				o := await(t, serve(w.handler(t), cw, clientRequest(t.Context(), tt.method, tt.target, nil, nil, 0)), "the health answer")
				synctest.Wait()
				mustReturn(t, o)
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{tt.wantStatus}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if got.body != tt.wantBody {
					t.Errorf("body of %d bytes differs from the %d bytes wanted (first 80: %.80q)", len(got.body), len(tt.wantBody), got.body)
				}
				if tt.relayed {
					if diff := gocmp.Diff([]string{"close"}, got.head.Values("Connection")); diff != "" {
						t.Errorf("Connection of the relayed health answer (-want +got):\n%s", diff)
					}
				}
				if tt.wantBody != "" {
					want := http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(tt.wantBody))}}
					if diff := gocmp.Diff(want, pick(got.head, "Content-Type", "Content-Length")); diff != "" {
						t.Errorf("fields of the augmented document (-want +got):\n%s", diff)
					}
				}
				checkOneDelivery(t, w)
			})
		})
	}
}
