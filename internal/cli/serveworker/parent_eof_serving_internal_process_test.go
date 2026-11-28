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

//go:build process && (linux || darwin)

package serveworker

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestParentEOFRemovesSocketPath checks a supervised worker that already serves its Unix socket when its parent's end
// of file arrives on the liveness pipe: the worker must stop with status 0, return from Run, and leave no file at the
// socket's pathname once it has exited. Before the parent ends, the report is read, the socket answers a health
// request and its pathname is a socket, so the worker had crossed its admission boundary and the file was there.
//
// The production-exit child ends the way the production binary does, with Options.Exit wired to os.Exit, so it rejects
// a worker that, as the source does, ends the process at once on its parent's loss and so leaves the bound socket file
// for the next worker to reclaim; the worker helper, whose Exit is nil, rejects a worker that returns without closing
// its Unix listener. Both reject a worker that ends with another status, and both require the child's line saying
// that Run returned, as a whole line of its output. The reclaim lock file beside the socket may stay and is not
// judged.
func TestParentEOFRemovesSocketPath(t *testing.T) {
	type exit struct {
		Status   int
		Returned bool
	}
	tests := map[string]struct {
		// productionExit launches the production-exit child instead of the worker helper.
		productionExit bool
		// returnedLine is the child's line that says Run returned.
		returnedLine string
		want         exit
	}{
		"success: the production-exit child serving its socket removes the socket path on its parent's end of file": {
			productionExit: true, returnedLine: "worker returned cleanly", want: exit{Status: 0, Returned: true},
		},
		"success: the worker helper serving its socket removes the socket path on its parent's end of file": {
			returnedLine: "worker returned cleanly", want: exit{Status: 0, Returned: true},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wrk3TRow(t, func(t *testing.T) {
				h := newLossHandoff(t)
				newLossUpstream(t, h.env)
				var c *lossChild
				if tc.productionExit {
					c = h.launchProductionExit(t, "")
				} else {
					c = h.launch(t)
				}
				port := c.awaitReady(t, c.startedAt.Add(lossBound))
				h.closeTransfer()
				if port != strconv.Itoa(int(h.port)) {
					t.Fatalf("readiness on port %s; want the inherited listener's port %d", port, h.port)
				}
				readLossReport(t, h.reportR, h.port)
				if status, err := lossUnixHealth(t.Context(), h.socket); err != nil || status != http.StatusOK {
					t.Fatalf("health over the socket with the parent alive: %d, %v; want 200\nstderr:\n%s", status, err, c.stderr)
				}
				if info, err := os.Lstat(h.socket); err != nil || info.Mode().Type() != fs.ModeSocket {
					t.Fatalf("the serving worker's socket path %s: %v; want a socket", h.socket, wrk3TDescribe(info, err))
				}

				endedAt := time.Now()
				if err := h.liveW.Close(); err != nil {
					t.Fatal(err)
				}
				if !c.awaitExit(t, endedAt.Add(lossBound)) {
					t.Fatalf("the worker was still running %v after its parent's end of file\nstdout:\n%s\nstderr:\n%s", lossBound, c.stdout(), c.stderr)
				}
				<-c.outputEnd
				got := exit{Status: c.exitCode(), Returned: slices.Contains(strings.Split(c.stdout(), "\n"), tc.returnedLine)}
				if diff := gocmp.Diff(tc.want, got); diff != "" {
					t.Errorf("the worker's end after its parent's end of file (-want +got):\n%s\nresult: %v\nstdout:\n%s\nstderr:\n%s", diff, c.runErr, c.stdout(), c.stderr)
				}
				if info, err := os.Lstat(h.socket); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the socket path %s after the worker exited: %v; want it gone", h.socket, wrk3TDescribe(info, err))
				}
				requireLossNoNotice(t, h.drain)
			})
		})
	}
}

// wrk3TDescribe names what an Lstat found: the error, or the file's mode.
func wrk3TDescribe(info fs.FileInfo, err error) string {
	if err != nil {
		return err.Error()
	}
	return "a file of mode " + info.Mode().String()
}

// TestParentEOFJoinsRequestHandlers checks that a supervised worker whose parent's end of file arrives once it serves
// stops at once and still joins its request handlers before Run returns, with a nil result, the in-process form of
// status 0. A native stream's handler is held inside its first response write: the test parks that write on the
// client's connection, and it stays parked until the test lets it go, also once the worker has closed that connection.
// Then the parent's end of file arrives on the liveness pipe. While the write is still held, the stop must end the
// service, which ends the stream's upstream request, close the client's connection, and complete its Close of the
// worker's copy of the TCP listener; Run must not return then, nor during wrk3TStage after it. Only then is the held
// write let go: Run must return nil, and the held write must have ended at the moment Run returned, as Run's goroutine
// sees it. That is the order observed in this run; the staging window only gives a worker that does not join its
// handlers the opportunity to return early. No drain notice is written.
//
// It rejects a worker that returns on its parent's end without joining a handler that is still running, and one that
// reports an error for that stop. The held write is the handler's own response write and takes none of the worker's
// locks, so no other part of the stop waits for it but the join of the handlers. Grok work, readiness, the model
// cache, the exporters and the transports are joined by the same stop and are not judged one by one.
func TestParentEOFJoinsRequestHandlers(t *testing.T) {
	tests := map[string]struct {
		wantErr error
	}{
		"success: a parent's end of file once serving returns only after a handler's held response write has ended": {
			wantErr: nil,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wrk3TRow(t, func(t *testing.T) {
				h := newInProcessHandoff(t)
				up := wrk3TNewHeldUpstream(t, h.env)
				tap := wrk3TNewTap()
				stderr := new(lossBuffer)
				r := wrk3TStartRun(t, context.WithValue(t.Context(), acceptGateKey{}, tap.gate), h.env, h.home, stderr)
				public := "127.0.0.1:" + r.awaitReady(t, stderr)

				stream := wrk3TDial(t, public)
				hold := tap.hold(stream.LocalAddr().String(), true)
				r.watched.Store(hold)
				// Registered after the start, so it runs before the cleanup that waits for Run, which joins the handler
				// this hold parks.
				t.Cleanup(hold.release.fire)
				wrk3TPost(t, stream, public, "/v1/messages", wrk3TStreamBody)
				wrk3TAwait(t, up.streamEntered.ch, time.Now().Add(wrk3TBound), "the native stream to reach the upstream")
				wrk3TAwait(t, hold.entered.ch, time.Now().Add(wrk3TBound), "the stream's handler to begin its response write")

				if err := h.liveW.Close(); err != nil {
					t.Fatal(err)
				}
				wrk3TAwait(t, up.streamEnded.ch, time.Now().Add(wrk3TBound), "the parent's end of file to end the stream's upstream request")
				wrk3TAwaitEnd(t, stream, time.Now().Add(wrk3TBound), "the worker closing the held handler's client connection")
				wrk3TAwait(t, tap.listenerClosed.ch, time.Now().Add(wrk3TBound), "the worker to close its copy of the TCP listener while a handler is held")
				select {
				case <-r.done:
					r.requireLifetime(t, "the early return was judged")
					t.Fatalf("Run returned (%v) while a request handler was still held in its response write\nstderr:\n%s", r.err, stderr)
				case <-time.After(wrk3TStage):
				}
				r.requireLifetime(t, "the held write was let go")
				if hold.ended.Load() {
					t.Fatal("test infrastructure: the held write ended before it was released; not a product failure")
				}

				hold.release.fire()
				err := r.awaitReturn(t, "the held write was let go", stderr)
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("Run returned %v after its parent's end of file; want %v\nstderr:\n%s", err, tc.wantErr, stderr)
				}
				if !r.heldEndedAtReturn {
					t.Error("Run returned while a request handler was still held in its response write")
				}
				requireLossNoNotice(t, h.drain)
			})
		})
	}
}
