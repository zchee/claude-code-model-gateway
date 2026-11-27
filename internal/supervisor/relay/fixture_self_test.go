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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor/relay"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// TestFixture checks the fake world without the relay. The happy paths: a lease reaches the fake worker of the
// published generation, the counters move separately, the waits are virtual, and an ended generation is excluded from
// later leases. Each failure detector catches a deliberate violation: a connection without a request, a lease left
// open, bytes after the request, an answer nobody reads, the relay's close against the worker's own, a call after the
// departure mark, a pause skipped between attempts, a leaked error text, and a parked upload read that Close or a read
// deadline must end.
func TestFixture(t *testing.T) {
	tests := map[string]struct {
		run func(t *testing.T, w *world)
	}{
		"success: a lease reaches the published worker and every counter moves once": {
			run: func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				lease, err := w.src.Acquire(t.Context(), "")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				if got, want := lease.Authority(), "127.0.0.1:41001"; got != want {
					t.Errorf("Authority() = %q; want %q", got, want)
				}
				if got := lease.PID(); got != 4101 {
					t.Errorf("PID() = %d; want 4101", got)
				}
				if _, err := io.WriteString(lease, "POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1:41001\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi"); err != nil {
					t.Fatalf("write the request: %v", err)
				}
				resp, err := http.ReadResponse(bufio.NewReader(lease), &http.Request{Method: http.MethodPost})
				if err != nil {
					t.Fatalf("read the answer: %v", err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatalf("read the answer body: %v", err)
				}
				if resp.StatusCode != http.StatusOK || string(body) != "ok" {
					t.Errorf("answer = %d %q; want 200 \"ok\"", resp.StatusCode, body)
				}
				if err := lease.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
				synctest.Wait()
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("delivered (-want +got):\n%s", diff)
				}
				if got := g.reads.Load(); got != 1 {
					t.Errorf("worker reads = %d; want 1", got)
				}
				if got := w.upstream.Load(); got != 1 {
					t.Errorf("upstream submissions = %d; want 1", got)
				}
				conns := g.connections()
				if len(conns) != 1 || conns[0].line != "POST /v1/messages HTTP/1.1" || string(conns[0].body) != "hi" {
					t.Errorf("the worker saw %d connections; want one carrying the request", len(conns))
				}
				checkLeasesClosed(t, w)
			},
		},
		"success: a worker that fails before submitting counts a read but no submission": {
			run: func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, failing(false, nil))
				lease, err := w.src.Acquire(t.Context(), "")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				defer func() { _ = lease.Close() }()
				if _, err := io.WriteString(lease, "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:41001\r\nConnection: close\r\n\r\n"); err != nil {
					t.Fatalf("write the request: %v", err)
				}
				if resp, err := http.ReadResponse(bufio.NewReader(lease), nil); err == nil {
					_ = resp.Body.Close()
					t.Error("the failing worker answered")
				}
				if got, want := [2]int32{g.reads.Load(), w.upstream.Load()}, [2]int32{1, 0}; got != want {
					t.Errorf("reads, submissions = %v; want %v", got, want)
				}
			},
		},
		"success: waits are virtual and recorded": {
			run: func(t *testing.T, w *world) {
				start := time.Now()
				for range 3 {
					if err := w.wait(t.Context(), attemptWait); err != nil {
						t.Fatalf("wait: %v", err)
					}
				}
				if got := time.Since(start); got != 3*attemptWait {
					t.Errorf("three waits took %v of virtual time; want %v", got, 3*attemptWait)
				}
				if diff := gocmp.Diff([]time.Duration{attemptWait, attemptWait, attemptWait}, w.waitCalls()); diff != "" {
					t.Errorf("wait calls (-want +got):\n%s", diff)
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := w.wait(ctx, time.Hour); err == nil {
					t.Error("a wait with an ended context returned nil")
				}
			},
		},
		"success: an ended generation gives no lease and its successor on the same port does": {
			run: func(t *testing.T, w *world) {
				a := w.start("gen-a", 41001, 4101, answering(okAnswer))
				a.end()
				if _, err := w.src.Acquire(t.Context(), ""); err == nil {
					t.Error("Acquire succeeded with only an ended generation")
				}
				w.start("gen-b", 41001, 4102, answering(okAnswer))
				lease, err := w.src.Acquire(t.Context(), "gen-a")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				defer func() { _ = lease.Close() }()
				if got := lease.Generation(); got != "gen-b" {
					t.Errorf("lease generation = %q; want gen-b", got)
				}
			},
		},
		"success: a connection closed before any request counts as a connection and not as a read": {
			run: func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				lease, err := w.src.Acquire(t.Context(), "")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				_ = lease.Close()
				synctest.Wait()
				if n, reads, up := len(g.connections()), g.reads.Load(), w.upstream.Load(); n != 1 || reads != 0 || up != 0 {
					t.Errorf("connections, reads, submissions = %d, %d, %d; want 1, 0, 0", n, reads, up)
				}
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("delivered (-want +got):\n%s", diff)
				}
			},
		},
		"success: the lease check catches a lease left open": {
			run: func(t *testing.T, w *world) {
				w.start("gen-a", 41001, 4101, failing(false, nil))
				lease, err := w.src.Acquire(t.Context(), "")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				rec := &recorder{}
				checkLeasesClosed(rec, w)
				if !rec.failed() {
					t.Error("checkLeasesClosed accepted a lease that is still open")
				}
				_ = lease.Close()
				rec = &recorder{}
				checkLeasesClosed(rec, w)
				if rec.failed() {
					t.Errorf("checkLeasesClosed refused closed leases: %v", rec.errs)
				}
			},
		},
		"success: the worker counts bytes sent after the request": {
			run: func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				lease, err := w.src.Acquire(t.Context(), "")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				defer func() { _ = lease.Close() }()
				const extra = "GET /second HTTP/1.1\r\n\r\n"
				go func() {
					_, _ = io.WriteString(lease, "POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi"+extra)
				}()
				if resp, err := http.ReadResponse(bufio.NewReader(lease), nil); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				synctest.Wait()
				if n := g.connections()[0].bytesAfter.Load(); n != int64(len(extra)) {
					t.Errorf("bytes after the request = %d; want %d", n, len(extra))
				}
			},
		},
		"success: the worker records an answer nobody reads as a failed write": {
			run: func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				lease, err := w.src.Acquire(t.Context(), "")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				defer func() { _ = lease.Close() }()
				if _, err := io.WriteString(lease, "GET /healthz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
					t.Fatalf("write the request: %v", err)
				}
				time.Sleep(workerWriteBudget + time.Second)
				synctest.Wait()
				if !g.connections()[0].writeFailed.Load() {
					t.Error("the worker's unread answer is not recorded as a failed write")
				}
			},
		},
		"success: the worker tells the relay's close from its own": {
			run: func(t *testing.T, w *world) {
				release := make(chan struct{})
				defer close(release)
				own := w.start("gen-a", 41001, 4101, failing(false, nil))
				ownLease, err := w.src.Acquire(t.Context(), "")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				if _, err := io.WriteString(ownLease, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatalf("write the request: %v", err)
				}
				synctest.Wait()
				_ = ownLease.Close()
				synctest.Wait()
				if closed(own.connections()[0].relayClosed) {
					t.Error("the worker's own close was taken for the relay's")
				}
				own.end()
				peer := w.start("gen-b", 41002, 4102, gatedWorker("", okAnswer, release))
				peerLease, err := w.src.Acquire(t.Context(), "")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				if _, err := io.WriteString(peerLease, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatalf("write the request: %v", err)
				}
				synctest.Wait()
				_ = peerLease.Close()
				synctest.Wait()
				if !closed(peer.connections()[0].relayClosed) {
					t.Error("the relay's close was not seen")
				}
			},
		},
		"success: the departure check catches a call after the mark and nothing else": {
			run: func(t *testing.T, w *world) {
				// A baseline that passes: one delivery, one lease request, one submission, nothing written.
				w.start("gen-a", 41001, 4101, answering(okAnswer))
				lease, err := w.src.Acquire(t.Context(), "")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				go func() {
					_, _ = io.WriteString(lease, "POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
				}()
				if resp, err := http.ReadResponse(bufio.NewReader(lease), nil); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				_ = lease.Close()
				synctest.Wait()
				cw := newClientWriter()
				cw.mark()
				rec := &recorder{}
				checkNothingAfterDeparture(rec, w, cw, "")
				if rec.failed() {
					t.Fatalf("the baseline fails already: %v", rec.errs)
				}
				// Only the count of calls after the mark changes.
				cw.mu.Lock()
				cw.afterMark++
				cw.mu.Unlock()
				rec = &recorder{}
				checkNothingAfterDeparture(rec, w, cw, "")
				if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "after the client departed") {
					t.Errorf("diagnostics %q; want exactly the one about calls after the departure", rec.errs)
				}
			},
		},
		"success: the departure check catches a lost report and nothing else": {
			run: func(t *testing.T, w *world) {
				w.start("gen-a", 41001, 4101, answering(okAnswer))
				lease, err := w.src.Acquire(t.Context(), "")
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				go func() {
					_, _ = io.WriteString(lease, "POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
				}()
				if resp, err := http.ReadResponse(bufio.NewReader(lease), nil); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				_ = lease.Close()
				synctest.Wait()
				cw := newClientWriter()
				cw.mark()
				rec := &recorder{}
				checkNothingAfterDeparture(rec, w, cw, "")
				if rec.failed() {
					t.Fatalf("the baseline fails already: %v", rec.errs)
				}
				w.recordLost(relay.Lost{Generation: "gen-a", WorkerPID: 4101, ErrorType: "Error"})
				rec = &recorder{}
				checkNothingAfterDeparture(rec, w, cw, "")
				if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "Lost was called 1 times") {
					t.Errorf("diagnostics %q; want exactly the one about the lost report", rec.errs)
				}
			},
		},
		"success: a flush without a status commits 200": {
			run: func(t *testing.T, w *world) {
				cw := newClientWriter()
				cw.Flush()
				if got := cw.snapshot().statuses; len(got) != 1 || got[0] != http.StatusOK {
					t.Errorf("a flush without a status committed %v; want [200]", got)
				}
			},
		},
		"success: the in-memory links refuse another process for a published generation": {
			run: func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				if err := g.res.Publish(4101); err != nil {
					t.Errorf("publishing the same process again: %v", err)
				}
				if err := g.res.Publish(4199); err == nil {
					t.Error("a published generation was published again for another process")
				}
			},
		},
		"success: the call order check catches two attempts without a pause": {
			run: func(t *testing.T, w *world) {
				for order, bad := range map[string]bool{"acquire acquire": true, "wait acquire": true, "acquire wait acquire acquire": true, "acquire wait acquire": false, "acquire wait wait acquire": false, "": false} {
					rec := &recorder{}
					checkAlternation(rec, strings.Fields(order))
					if rec.failed() != bad {
						t.Errorf("order %q: refused = %v; want %v", order, rec.failed(), bad)
					}
				}
			},
		},
		"success: the private markers catch a leaked error text": {
			run: func(t *testing.T, w *world) {
				rec := &recorder{}
				checkLost(rec, []relay.Lost{{Generation: "gen-a", WorkerPID: 4101, ErrorType: errPrivateLeg.Error()}}, []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}})
				if !rec.failed() {
					t.Error("checkLost accepted an error type that carries the failure's text")
				}
				rec = &recorder{}
				checkLost(rec, []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}}, []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}})
				if !rec.failed() {
					t.Error("checkLost accepted an empty error type")
				}
			},
		},
		"success: a parked upload read ends at Close and at its read deadline": {
			run: func(t *testing.T, w *world) {
				for name, end := range map[string]func(u *upload){
					"close":    func(u *upload) { _ = u.Close() },
					"deadline": func(u *upload) { _ = u.SetReadDeadline(time.Now().Add(time.Second)) },
				} {
					u := newUpload(0, 'x')
					u.open = true
					done := make(chan error, 1)
					go func() {
						_, err := u.Read(make([]byte, 1))
						done <- err
					}()
					synctest.Wait()
					if _, ok := ready(done); ok {
						t.Fatalf("%s: the open upload's read returned before its end", name)
					}
					end(u)
					err := await(t, done, name+": the end of the parked read")
					if err == nil || errors.Is(err, io.EOF) {
						t.Errorf("%s: the parked read ended with %v; want a failure", name, err)
					}
					_ = u.Close()
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, tt.run)
		})
	}
}

// recorder stands in for a test to show that a check fails: it records Errorf calls. Only Helper and Errorf may be
// called on it.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.errs) > 0
}
