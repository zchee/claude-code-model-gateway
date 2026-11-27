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

package serveshim_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// ownedTimeoutText is the supervisor's diagnostic for a worker that did not report its listener in time.
const ownedTimeoutText = "model-gateway: shim worker did not report its listener port within 5000ms"

// ownedFollow delivers every further line the child sends on its control connection, and closes the channel when the
// connection ends, which happens when the child has exited.
func ownedFollow(c *ownedChild) <-chan string {
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		for c.lines.Scan() {
			lines <- c.lines.Text()
		}
	}()
	return lines
}

// untilOrStopped waits until at, or until the supervisor stops, whichever comes first.
func (s *ownedShim) untilOrStopped(at time.Time) {
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-s.done:
	}
}

// TestReportTimeoutIsFinal carries the report timeout's finality test with every worker's listener
// received from the supervisor. Once the report deadline has passed without a report, it is final until the supervisor
// stops: a replacement worker that reports afterwards never becomes eligible, so it receives no request, and the
// timeout is reported once, as the error Run returns, with nothing printed beside it. A replacement whose report is
// accepted before the deadline is eligible and the deadline no longer applies.
//
// The first worker exits without a report, so the supervisor starts a replacement, which inherits the first worker's
// deadline. The replacement reports at a time around the deadline, or starts around it and reports at once, while
// requests wait at the public listener for an eligible generation. Which of the report and the deadline is decided
// first is up to the scheduler; whichever it is, the outcome must be one of the two described above.
func TestReportTimeoutIsFinal(t *testing.T) {
	type schedule struct {
		// exitAt is when the first worker exits without a report, counted from the report deadline.
		exitAt time.Duration
		// reportAt is when the replacement reports its listener, counted from the report deadline, unless onArrival
		// makes it report as soon as it starts.
		reportAt  time.Duration
		onArrival bool
	}
	tests := map[string]schedule{}
	for _, at := range []time.Duration{-20 * time.Millisecond, -10 * time.Millisecond, -5 * time.Millisecond, -2 * time.Millisecond, -time.Millisecond, 0, time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 50 * time.Millisecond, 500 * time.Millisecond} {
		tests[fmt.Sprintf("success: replacement reporting %v from the report deadline is never eligible after the timeout", at)] = schedule{exitAt: -ownedReportDeadline, reportAt: at}
	}
	// The replacement starts about the respawn delay plus a process start after the first worker exits.
	for _, at := range []time.Duration{-400 * time.Millisecond, -300 * time.Millisecond, -200 * time.Millisecond, -150 * time.Millisecond, -120 * time.Millisecond, -100 * time.Millisecond, -80 * time.Millisecond, -60 * time.Millisecond} {
		tests[fmt.Sprintf("success: replacement started after a worker exit %v from the report deadline is never eligible after the timeout", at)] = schedule{exitAt: at, onArrival: true}
	}
	for name, sc := range tests {
		t.Run(name, func(t *testing.T) {
			// Each case waits for its own report deadline; the cases need not wait in turn.
			t.Parallel()
			// A configured worker port, as in the original: the replacement reserves the same number after the first
			// worker's generation ended. The real bind of the configured port stays under test, so a collision in the
			// moment before the supervisor's start fails the case instead of passing it.
			s := startOwnedShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(configuredPort(t))}, nil)
			s.awaitReady(t)
			deadline := s.readyAt.Add(ownedReportDeadline)

			first := s.nextChild(t)
			s.untilOrStopped(deadline.Add(sc.exitAt))
			// The first worker may already be disconnected when the supervisor stopped first; its reply is not needed.
			_, _ = io.WriteString(first.conn, "exit\n")

			var second *ownedChild
			var lines <-chan string
			select {
			case second = <-s.children:
				if second.refusal != "" {
					t.Fatalf("the replacement did not receive its listener and drain channel from the supervisor: %s", second.refusal)
				}
				lines = ownedFollow(second)
			case <-s.done:
			case <-time.After(20 * time.Second):
				t.Fatal("supervisor neither started a replacement nor stopped within 20s of its worker's exit")
			}

			// farewell is the replacement's last line when its supervisor went away, with its request counts.
			farewell := ""
			report := func() {
				if farewell == "" {
					_, _ = io.WriteString(second.conn, "report\n")
				}
			}
			if second != nil {
				_, _ = io.WriteString(second.conn, "serve\n")
				switch reply := <-lines; {
				case reply == "serving":
				case strings.HasPrefix(reply, "parent gone"):
					farewell = reply
				default:
					t.Fatalf("replacement could not serve its inherited listener: %q", reply)
				}
				if sc.onArrival {
					report()
				}
			}

			// Requests wait at the public listener for an eligible generation from shortly before the deadline until
			// shortly after it, and record which worker answered them.
			var probes sync.WaitGroup
			var servedMu sync.Mutex
			var servedBySecond int
			probeCtx, stopProbes := context.WithCancel(t.Context())
			defer stopProbes()
			s.untilOrStopped(deadline.Add(-100 * time.Millisecond))
			for range 2 {
				probes.Go(func() {
					for probeCtx.Err() == nil {
						got, err := s.send(probeCtx, rand.Text(), nil)
						if err == nil && second != nil && got.Status == http.StatusOK && got.ChildPID == second.pid {
							servedMu.Lock()
							servedBySecond++
							servedMu.Unlock()
						}
						if err != nil {
							// The supervisor stopped, or the client gave up; a short pause avoids a busy loop.
							time.Sleep(10 * time.Millisecond)
						}
					}
				})
			}

			if second != nil && !sc.onArrival {
				s.untilOrStopped(deadline.Add(sc.reportAt))
				report()
			}

			s.untilOrStopped(deadline.Add(500 * time.Millisecond))
			stopProbes()
			probes.Wait()

			// A supervisor that kept its deadline has stopped long before this; one whose replacement reported in time
			// is still running.
			if !s.awaitStop(deadline.Add(8 * time.Second)) {
				if second == nil {
					t.Fatal("supervisor still running without a replacement worker, after its report deadline")
				}
				got := s.probe(t, rand.Text())
				if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: second.pid}, got); diff != "" {
					t.Errorf("request to a supervisor whose replacement reported before the deadline (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("", s.stderr.String()); diff != "" {
					t.Errorf("stderr mismatch (-want +got):\n%s", diff)
				}
				return
			}
			if s.result == nil || s.result.Error() != ownedTimeoutText {
				t.Errorf("supervisor stopped with %v; want exactly the report timeout %q", s.result, ownedTimeoutText)
			}
			// The timeout is reported once, by Run's caller; nothing else is printed, a second timeout least of all.
			if diff := gocmp.Diff("", s.stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
			if second == nil {
				return
			}
			for line := range lines {
				if strings.HasPrefix(line, "parent gone") {
					farewell = line
				}
			}
			// The replacement counts the requests it received before and after its report.
			if diff := gocmp.Diff("parent gone 0 0 0 0", farewell); diff != "" {
				t.Errorf("replacement of a supervisor that timed out received requests (-want +got):\n%s", diff)
			}
			if servedBySecond != 0 {
				t.Errorf("%d request(s) were answered by the replacement of a supervisor that timed out; want 0", servedBySecond)
			}
		})
	}
}
