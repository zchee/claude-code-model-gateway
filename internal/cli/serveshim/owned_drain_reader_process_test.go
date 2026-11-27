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

package serveshim_test

import (
	"crypto/rand"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestDrainReaderEndWithoutNoticeWithdrawsAdmission checks a worker whose drain channel delivers an unreadable frame
// instead of a valid notice while the worker lives: the supervisor's drain reader ends there. From then on no lease
// is acquired for that generation, so a request does not reach the worker, but the supervisor does not stop the
// worker and shuts nothing down: the worker is still alive and answering its test well past the ten seconds of the
// recovery after a failed public drain, and the supervisor still runs and answers on its public port.
//
// It rejects a supervisor that keeps leasing the generation after its drain reader ended, one that treats the ended
// reader as a crash or a failed drain and disconnects the worker, and one that stops itself. Requests that reached
// the worker before the supervisor read the frame are allowed; the frame is followed by probes until the first one
// gets the supervisor's retryable answer, within a bound, and the worker's count does not grow after that one. The
// control is the request before the frame, which reached the worker.
//
// Not staged: the worker closing its end of the drain channel without a notice. The frozen worker fixture has no
// command that closes the channel without sending a valid notice first.
func TestDrainReaderEndWithoutNoticeWithdrawsAdmission(t *testing.T) {
	tests := map[string]struct{ frame string }{
		"error: after an unreadable drain frame the live worker receives no request and is not stopped": {frame: "not a drain frame"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A supervisor that applied the recovery of a failed public drain would stop the worker ten seconds after;
			// the window watches past that with a margin.
			const watch = 13 * time.Second
			s := startOwnedShim(t, nil, nil)
			child := rulingServingChild(t, s)
			child.command(t, "notice-raw "+tc.frame, "sent")
			sentAt := time.Now()
			withdrawn := false
			for !withdrawn {
				got := s.probe(t, rand.Text())
				switch {
				case got == ownedRetryable:
					withdrawn = true
				case got.ChildPID != child.pid:
					t.Fatalf("request after the unreadable frame was answered %+v; want the worker's answer or the retryable one", got)
				case time.Since(sentAt) > 5*time.Second:
					t.Fatalf("requests still reached the worker %v after its unreadable drain frame; want admission withdrawn", time.Since(sentAt))
				}
			}
			counts := child.command(t, "requests", "requests ")
			if diff := gocmp.Diff(ownedRetryable, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request after admission was withdrawn (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(counts, child.command(t, "requests", "requests ")); diff != "" {
				t.Errorf("the worker's requests grew after admission was withdrawn (-want +got):\n%s", diff)
			}
			if s.awaitStop(sentAt.Add(watch)) {
				t.Fatalf("the supervisor stopped after the worker's unreadable drain frame: %v; stderr: %s", s.result, s.stderr)
			}
			// Still alive: a disconnected worker would have sent its farewell first, which is not a reply to this.
			reply := child.command(t, "requests", "")
			if !strings.HasPrefix(reply, "requests ") {
				t.Errorf("the worker after %v: %q; want it alive and answering, not disconnected", watch, reply)
			}
			if diff := gocmp.Diff(ownedRetryable, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("the supervisor's answer after %v (-want +got):\n%s", watch, diff)
			}
		})
	}
}
