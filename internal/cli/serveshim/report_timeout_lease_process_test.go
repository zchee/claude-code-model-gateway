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

//go:build process && (darwin || linux)

package serveshim

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// TestReportTimeoutDecisionIsFinal checks, in virtual time and without a race against the scheduler, that a decided
// report timeout is final until the supervisor stops. After the deadline passed without an eligible worker, a worker
// whose report arrives later never makes its generation obtainable: the relay's lease source gives no lease for it,
// and a respawn attempt arms no new report timer, so no second timeout is ever decided. Whether the late report still
// writes the worker's pid record is not part of that contract and is not checked. The control case delivers the same
// report before the deadline: the relay obtains a lease to the worker's generation, its pid record carries the
// reported port, and no timeout follows. TestReportTimeoutIsFinal shows the same contract through a running
// supervisor, and TestReportTimeoutAgainstStop the warning a timeout that loses to a stop leaves.
//
// The worker's launch is the one step replaced: the state directory has no logs directory, so every launch fails
// before a process starts, and the test installs the worker a successful launch would have made current, with a
// real reservation of the supervisor's registry, whose port is the one the worker reports. Its pid is this process,
// whose birth time and executable the pid record can carry. The test reaches the supervisor through these unexported
// names, which a rework of the supervisor must carry along or replace: the shim fields it constructs, fatal (Run's
// inbox), mu, current and reportTimer, the methods startWorker, publish, registry and stop, the lease source
// relayWorkers, and the workerProc fields pid, generation, res and exited.
func TestReportTimeoutDecisionIsFinal(t *testing.T) {
	// reportWait restates reportTimeout, the 5000 ms the supervisor gives a worker to report its listener.
	const reportWait = 5 * time.Second
	const timeoutText = "model-gateway: shim worker did not report its listener port within 5000ms"
	const launchFailure = "model-gateway: could not start the shim worker: "
	const generation = "fixture-generation"
	tests := map[string]struct {
		expire bool
		want   string // The decided timeout's text, or "" when none is decided.
	}{
		"success: report published before the deadline makes the generation obtainable and no timeout follows":                  {},
		"error: report published after the deadline leaves the generation unobtainable and a respawn decides no second timeout": {expire: true, want: timeoutText},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				s := &shim{lifetime: t.Context(), state: state, executable: exe, version: "1.2.3", stderr: &stderr, fatal: make(chan error, 1)}
				// takeFatal returns the failure handed to Run, or "" when none is pending.
				takeFatal := func() string {
					select {
					case err := <-s.fatal:
						return err.Error()
					default:
						return ""
					}
				}

				// The first launch arms the report deadline; its failure is the one handed to Run so far.
				s.startWorker()
				if got := takeFatal(); !strings.HasPrefix(got, launchFailure) {
					t.Fatalf("first launch handed Run %q; want the launch failure starting with %q", got, launchFailure)
				}
				res, err := s.registry().Reserve(t.Context(), generation, 0)
				if err != nil {
					t.Fatalf("reserve the worker's generation: %v", err)
				}
				// Ended last, after the stop, as the exit watcher ends the generation of a worker that exited.
				defer func() { _ = res.End() }()
				reportedPort := int(res.Port())
				// leased asks the relay's lease source for a lease, as one relay attempt does, and returns the generation
				// it reached and the port its connection is connected to.
				leased := func() (workerlink.Generation, int, error) {
					lease, err := relayWorkers{s}.Acquire(t.Context(), "")
					if err != nil {
						return "", 0, err
					}
					defer func() { _ = lease.Close() }()
					remote, ok := lease.RemoteAddr().(*net.TCPAddr)
					if !ok {
						return lease.Generation(), 0, fmt.Errorf("the lease is connected to %v, not a TCP address", lease.RemoteAddr())
					}
					return lease.Generation(), remote.Port, nil
				}
				// checkLease compares the lease the relay obtains with the report's verdict: the worker's generation
				// after a timely report, connected to the port the worker reported, so that a lease reaching the right
				// generation through another endpoint is refused as well; no eligible generation after a late one.
				checkLease := func(when string) {
					t.Helper()
					got, port, err := leased()
					if tc.expire {
						if !errors.Is(err, workerlink.ErrUnavailable) {
							t.Errorf("lease %s: generation %q, %v; want none, %v", when, got, err, workerlink.ErrUnavailable)
						}
						return
					}
					if err != nil || got != generation || port != reportedPort {
						t.Errorf("lease %s: generation %q on port %d, %v; want generation %q on the reported port %d", when, got, port, err, generation, reportedPort)
					}
				}
				w := &workerProc{pid: os.Getpid(), generation: generation, res: res, exited: make(chan struct{})}
				s.mu.Lock()
				s.current = w
				s.mu.Unlock()

				if tc.expire {
					// The deadline passes with no eligible worker; its expiry is decided.
					time.Sleep(reportWait + time.Millisecond)
					synctest.Wait()
				}
				// The worker's exit is what releases its stop below: a stop waits for the current worker to exit.
				defer func() {
					close(w.exited)
					s.stop("")
				}()
				if err := s.publish(w, reportedPort, true); err != nil {
					t.Fatalf("deliver the worker's report: %v", err)
				}
				if !tc.expire {
					// The report reached the current worker's publication: its pid record carries the reported port.
					record, err := procinfo.ReadRecord(state, "shim")
					if err != nil {
						t.Fatalf("read the worker pid record: %v", err)
					}
					if diff := gocmp.Diff([]int{os.Getpid(), reportedPort}, []int{record.PID, int(record.Port)}); diff != "" {
						t.Errorf("published worker pid record pid and port (-want +got):\n%s", diff)
					}
				}
				checkLease("after the report")

				if tc.expire {
					// The worker exits and its replacement's launch is attempted, as the exit watcher does it.
					s.mu.Lock()
					s.current = nil
					s.mu.Unlock()
					s.startWorker()
					synctest.Wait()
					// Checked before any virtual time passes: an armed timer would otherwise have fired and cleared itself.
					s.mu.Lock()
					armed := s.reportTimer != nil
					s.mu.Unlock()
					if armed {
						t.Error("the respawn attempt after the decided timeout armed a report timer")
					}
				}
				// Two whole report deadlines pass: a deadline still armed, or one the respawn armed, would expire.
				time.Sleep(2 * reportWait)
				synctest.Wait()

				if diff := gocmp.Diff(tc.want, takeFatal()); diff != "" {
					t.Errorf("failure handed to Run (-want +got):\n%s", diff)
				}
				// While the timeout is pending, the respawn's launch failure is printed as a warning, and it is the only
				// line: a second timeout would add another. Without a timeout nothing is printed.
				var wantStderr []string
				if tc.expire {
					wantStderr = []string{launchFailure}
				}
				var gotStderr []string
				for line := range strings.Lines(stderr.String()) {
					if strings.HasPrefix(line, launchFailure) {
						line = launchFailure
					}
					gotStderr = append(gotStderr, line)
				}
				if diff := gocmp.Diff(wantStderr, gotStderr); diff != "" {
					t.Errorf("stderr lines, a launch failure shortened to its prefix (-want +got):\n%s\nstderr: %q", diff, stderr.String())
				}
				// Two deadlines later the lease is still the one the report decided: a late report did not become eligible.
				checkLease("after two report deadlines")
			})
		})
	}
}
