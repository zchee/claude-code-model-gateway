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
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStartupErrorsWithLiveParent checks that only the peer's end of file on the liveness pipe is parent loss. A
// supervised worker with a valid handoff and a live parent stops with status 1 and its own diagnostic when a startup
// input is invalid or its port report cannot be written, never with status 0. Only once both the report reader and
// the liveness pipe are gone is a failing report the parent-loss stop: a clean exit, with no readiness line after it.
// Every row ends within lossBound and prints no readiness line.
//
// Not staged: an invalid settings file. Which settings file the worker reads before its admission boundary is not
// fixed by the contract the test sees; the control token is the invalid input used.
func TestStartupErrorsWithLiveParent(t *testing.T) {
	tests := map[string]struct {
		// badToken writes a control token that is not 64 lowercase hexadecimal characters.
		badToken bool
		// closeReport closes the parent's read end of the report pipe before the launch.
		closeReport bool
		// endParent also closes the parent's write end of the liveness pipe before the launch.
		endParent bool
		// productionExit launches the production-exit child, whose Options.Exit is os.Exit as in the production binary,
		// instead of the worker helper, which leaves Exit nil.
		productionExit bool
		// wantStatus is the exit status; words is what the diagnostic must contain, lowercased, for status 1.
		wantStatus int
		words      []string
	}{
		// Rejects a worker that takes the close of its own liveness descriptor, made while it unwinds a failed start,
		// for its parent's end and ends the process through Options.Exit with status 0. The production-exit child wires
		// Exit as the production binary does, so such a call is a status 0 here; with Exit nil it could not be seen.
		"error: a malformed control token with a live parent exits with status 1 and a token diagnostic": {
			badToken: true, productionExit: true, wantStatus: 1, words: []string{"token"},
		},
		// Rejects a worker that takes a failed report write for parent loss while its liveness pipe is still open, whether
		// it then returns cleanly or ends the process through Options.Exit with status 0, which the production-exit child
		// makes visible.
		"error: a closed report reader with a live parent is a failed report with status 1": {
			closeReport: true, productionExit: true, wantStatus: 1, words: []string{"report"},
		},
		// Rejects a worker that reports an error, or prints its readiness line, when the report reader and the
		// liveness pipe are both gone: that is the parent-loss stop.
		"success: a closed report reader and an ended parent are the clean parent-loss stop": {
			closeReport: true, endParent: true, wantStatus: 0,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newLossHandoff(t)
			if tc.badToken {
				if err := os.WriteFile(h.token, []byte("not-hex\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.closeReport {
				if err := h.reportR.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.endParent {
				if err := h.liveW.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var c *lossChild
			if tc.productionExit {
				c = h.launchProductionExit(t, "")
			} else {
				c = h.launch(t)
			}
			if !awaitLoss(c.done, c.startedAt.Add(lossBound)) {
				t.Fatalf("the worker was still running %v after its launch\nstdout:\n%s\nstderr:\n%s", lossBound, c.stdout(), c.stderr)
			}
			<-c.outputEnd
			text := strings.ToLower(c.stdout() + "\n" + c.stderr.String())
			if code := c.exitCode(); code != tc.wantStatus {
				t.Errorf("the worker ended with %v (status %d); want status %d\nstdout:\n%s\nstderr:\n%s", c.runErr, code, tc.wantStatus, c.stdout(), c.stderr)
			}
			for _, w := range tc.words {
				if !strings.Contains(text, w) {
					t.Errorf("the worker's diagnostic does not name the %s: %s", w, text)
				}
			}
			if tc.wantStatus == 0 && !strings.Contains(c.stdout(), "worker returned cleanly") {
				t.Errorf("the worker did not return cleanly\nstdout:\n%s\nstderr:\n%s", c.stdout(), c.stderr)
			}
			if port, ok := c.readiness(); ok {
				t.Errorf("the worker printed its readiness line on port %s", port)
			}
			h.closeTransfer()
			if !tc.closeReport {
				if data := lossReportBytes(t, h.reportR); len(data) != 0 {
					t.Errorf("the worker wrote %q on its report pipe despite the failed start; want zero bytes", data)
				}
			}
			requireLossNoNotice(t, h.drain)
		})
	}
}

// TestParentEOFAfterSocketBindRemovesPath checks a parent that ends after the worker bound its Unix socket pathname
// and before its admission boundary. It runs the production-exit child, whose Options.Exit is os.Exit as in the
// production binary, and parks the child's settings lookup at its first call that finds the socket bound: the worker
// looks up settings while it builds its handlers, after the bind and before its boundary. The parent ends while the
// lookup is still parked. The worker must then exit with status 0 within lossBound, with zero report bytes, no
// readiness line, no answer to the requests queued on its listener and no drain notice, and its socket pathname must
// be gone once it exited; the lock file beside it may stay.
//
// It rejects a worker that leaves its socket file behind on this hard exit, for the next worker to reclaim; one that
// waits for the parked startup step instead of ending through its production exit; and one that reports, prints
// readiness, answers or exits with another status after its parent's end. The control parks the same lookup with a
// live parent, lets it go, and is served over the socket, so that the park itself cannot make the parent-loss row pass.
//
// A worker that reports although its lookup was parked looked the setting up off its startup path; the case then
// cannot decide the rule and fails as the test's own infrastructure failure, with the report it saw. A bind still in
// flight at the parent's end is not tested.
func TestParentEOFAfterSocketBindRemovesPath(t *testing.T) {
	tests := map[string]struct{ parentEnds bool }{
		"error: a parent that ends after the socket bind gets a clean exit and the socket path is gone": {parentEnds: true},
		"success: with the parent alive the worker parked after its socket bind serves the socket":      {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newLossHandoff(t)
			up := newLossUpstream(t, h.env)
			fifo, release := makeLossParkFIFO(t, h)
			var probes []lossProbe
			if tc.parentEnds {
				probes = sendLossProbes(t, "127.0.0.1:"+strconv.Itoa(int(h.port)), "queued before the launch")
			}
			c := h.launchProductionExit(t, fifo)
			// Registered after the launch, so a child still parked is let go before the launch's cleanup waits for it.
			t.Cleanup(release)
			parkTimer := time.NewTimer(time.Until(c.startedAt.Add(lossBound)))
			defer parkTimer.Stop()
			select {
			case <-c.parked:
			case <-c.done:
				<-c.outputEnd
				t.Fatalf("the worker exited before it looked up a setting with its socket bound: %v\nstdout:\n%s\nstderr:\n%s", c.runErr, c.stdout(), c.stderr)
			case <-parkTimer.C:
				if port, ok := c.readiness(); ok {
					t.Fatalf("test infrastructure: the worker printed readiness on port %s without looking up a setting after its socket bind; the stage cannot be staged; not a product failure", port)
				}
				t.Fatalf("the worker neither bound its socket and looked up a setting nor exited within %v of its launch (a wait at a startup step)\nstdout:\n%s\nstderr:\n%s", lossBound, c.stdout(), c.stderr)
			}
			if info, err := os.Lstat(h.socket); err != nil || info.Mode().Type() != fs.ModeSocket {
				t.Fatalf("the worker's lookup parked with its socket bound, but %s is %v, %v", h.socket, info, err)
			}
			if port, ok := c.readiness(); ok {
				t.Fatalf("test infrastructure: the lookup parked after the readiness line on port %s, past the admission boundary; not a product failure", port)
			}
			if !tc.parentEnds {
				release()
				port := c.awaitReady(t, time.Now().Add(lossBound))
				h.closeTransfer()
				if port != strconv.Itoa(int(h.port)) {
					t.Fatalf("readiness on port %s; want the inherited listener's port %d", port, h.port)
				}
				readLossReport(t, h.reportR, h.port)
				if status, err := lossUnixHealth(t.Context(), h.socket); err != nil || status != http.StatusOK {
					t.Errorf("health over the socket with the parent alive: %d, %v; want 200", status, err)
				}
				endedAt := time.Now()
				if err := h.liveW.Close(); err != nil {
					t.Fatal(err)
				}
				if !awaitLoss(c.done, endedAt.Add(lossBound)) {
					t.Fatalf("the worker was still running %v after its parent's end", lossBound)
				}
				<-c.outputEnd
				if code := c.exitCode(); code != 0 {
					t.Errorf("the worker ended with %v (status %d) after its parent's end; want status 0\nstdout:\n%s\nstderr:\n%s", c.runErr, code, c.stdout(), c.stderr)
				}
				requireLossNoNotice(t, h.drain)
				return
			}
			endedAt := time.Now()
			if err := h.liveW.Close(); err != nil {
				t.Fatal(err)
			}
			if !awaitLoss(c.done, endedAt.Add(lossBound)) {
				t.Errorf("the worker was still running %v after its parent's end, with its startup step parked before its admission boundary: the parent's end did not end it through its production exit\nstdout:\n%s\nstderr:\n%s", lossBound, c.stdout(), c.stderr)
				return
			}
			<-c.outputEnd
			h.closeTransfer()
			if report := lossReportBytes(t, h.reportR); len(report) != 0 {
				t.Fatalf("test infrastructure: the worker reported %q although its lookup was parked, so the parked lookup was off its startup path and this stage may be after its boundary; the rule is not decided here; not a product failure", report)
			}
			if code := c.exitCode(); code != 0 {
				t.Errorf("the worker ended with %v (status %d) after its parent's end before its admission boundary; want status 0\nstdout:\n%s\nstderr:\n%s", c.runErr, code, c.stdout(), c.stderr)
			}
			if port, ok := c.readiness(); ok {
				t.Errorf("the worker printed its readiness line on port %s after its parent's end before its admission boundary", port)
			}
			if _, err := os.Lstat(h.socket); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the socket path %s after the worker exited: %v; want it gone", h.socket, err)
			}
			if _, err := os.Lstat(filepath.Clean(h.socket + ".lock")); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("inspect the socket lock file: %v", err)
			}
			requireLossNoNotice(t, h.drain)
			if err := h.res.End(); err != nil {
				t.Fatal(err)
			}
			requireLossUnanswered(t, probes)
			if n := up.messages.Load(); n != 0 {
				t.Errorf("the fake upstream received %d message requests after the parent's end; want none", n)
			}
		})
	}
}
