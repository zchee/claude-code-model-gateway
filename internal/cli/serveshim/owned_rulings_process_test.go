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
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveshim"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

// rulingServingChild returns the supervisor's first worker, serving and reported, after one request it answered.
func rulingServingChild(t *testing.T, s *ownedShim) *ownedChild {
	t.Helper()
	s.awaitReady(t)
	child := s.nextChild(t)
	child.command(t, "serve", "serving")
	child.command(t, "report", "reported")
	if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: child.pid}, s.probe(t, rand.Text())); diff != "" {
		t.Fatalf("request to the reporting child (-want +got):\n%s", diff)
	}
	return child
}

// TestNoLeaseAfterStopInhibition checks that once a stop has recorded that no further worker may start, the relay
// acquires no lease: with the stop held right after that point, while the worker is still alive and was eligible, an
// authorized request gets the supervisor's own answer and never reaches the worker. It rejects a relay that keeps
// handing requests to the eligible generation until the stop disconnects the worker. The control is the first request,
// which reached the worker before the stop.
func TestNoLeaseAfterStopInhibition(t *testing.T) {
	tests := map[string]struct{}{
		"error: a request sent after stop inhibition gets the supervisor's answer and the live worker receives nothing": {},
	}
	for name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inhibited, release := make(chan struct{}), make(chan struct{})
			signal := sync.OnceFunc(func() { close(inhibited) })
			letGo := sync.OnceFunc(func() { close(release) })
			s := startOwnedShim(t, nil, withHooks(serveshim.Hooks{StopInhibited: func() { signal(); <-release }}))
			t.Cleanup(letGo)
			child := rulingServingChild(t, s)
			s.cancel()
			select {
			case <-inhibited:
			case <-time.After(10 * time.Second):
				t.Fatal("the stop did not reach its inhibition within 10s")
			}
			answer := make(chan ownedAnswer, 1)
			go func() {
				got, err := s.send(t.Context(), rand.Text(), nil)
				answer <- ownedAnswer{got, err}
			}()
			// The worker is alive and has received only the request before the stop.
			if diff := gocmp.Diff("requests 0 1 0 0", child.command(t, "requests", "requests ")); diff != "" {
				t.Errorf("requests of the live worker while the stop is held (-want +got):\n%s", diff)
			}
			var a ownedAnswer
			select {
			case a = <-answer:
			case <-time.After(10 * time.Second):
				// The relay may wait for the stop to proceed; the request still must not reach the worker.
				letGo()
				a = <-answer
			}
			letGo()
			if a.err == nil && (a.result.ChildPID != 0 || a.result.Status == http.StatusOK) {
				t.Errorf("the request after stop inhibition was answered %+v; want the supervisor's own answer, not the worker's", a.result)
			}
			if diff := gocmp.Diff("0 1 0 0", child.counts(t)); diff != "" {
				t.Errorf("requests the worker received in total (-want +got):\n%s", diff)
			}
		})
	}
}

// TestStopInterruptsContestedStartFence checks that a supervisor whose first worker launch waits for the state root's
// start fence, which another holder keeps, does not wait for that holder when it is told to stop or when its report
// deadline expires. Run returns its normal result within a bound: nil after the stop, the report timeout after the
// deadline. It starts no worker. While the other holder still keeps the fence, the public port and then the reserved
// worker port must each be bound by this test no later than fenceReleaseBound after Run returned: one deadline for
// both ports, anchored at the fixture's record of Run's return.
//
// The contested wait is established before the stop or the deadline. The test holds the fence for the whole case, and
// it requires the supervisor's own reservation of the configured worker port to be in place while no worker has
// started. The reservation is seen as a connection that the port accepts. Before the supervisor started, a connection
// to the port must have been refused with ECONNREFUSED; any other outcome of that connect, a success, a reset, a
// timeout or another error, does not show that nobody listens, and the case fails as undecided. A worker starts only inside the fence, and the case relies on the launch reserving the worker's
// listener before it waits for the fence; a launch whose reservation listens while no worker has started has then
// reached its fence wait or is about to. A launch that reserves only inside the fence never shows its reservation here,
// and the case fails as not staged. The Launching hook alone cannot show the wait, since it runs before anything is
// reserved.
//
// It rejects a launch that waits on the fence without a way out, so that the stop or the deadline joins it; a stop
// that ends with another result; and a supervisor whose public listener or reservation cannot be bound by that
// deadline. A bind that first succeeds after the deadline is undecided and fails; it never passes. The control row has
// a free fence and starts its worker.
func TestStopInterruptsContestedStartFence(t *testing.T) {
	tests := map[string]struct {
		// contested keeps the start fence held by this test for the whole case.
		contested bool
		// deadline waits for the report deadline instead of stopping the supervisor.
		deadline bool
	}{
		"error: a stop while the launch waits for a contested start fence ends the supervisor within the bound": {contested: true},
		"error: the report deadline while the launch waits for a contested start fence ends the supervisor":     {contested: true, deadline: true},
		"success: with the start fence free the launch starts its worker":                                       {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workerPort := configuredPort(t)
			var starts atomic.Int32
			launching := make(chan struct{})
			signal := sync.OnceFunc(func() { close(launching) })
			prepare := func(state string) {
				if !tc.contested {
					return
				}
				fence, err := lifecycle.NewStartFence(state)
				if err != nil {
					t.Fatal(err)
				}
				release, err := fence.Acquire(t.Context())
				if err != nil {
					t.Fatalf("hold the start fence: %v", err)
				}
				t.Cleanup(func() { _ = release() })
			}
			worker := net.JoinHostPort("127.0.0.1", strconv.Itoa(workerPort))
			if tc.contested {
				// A listener found on the port later can then only be one that came after this point. Only a refusal shows that
				// nobody listens: a reset can follow a completed handshake, and a timeout shows nothing.
				conn, err := fenceDial(t, worker)
				switch {
				case err == nil:
					_ = conn.Close()
					t.Fatalf("test infrastructure: the configured worker port %d accepts connections before the supervisor started; undecided; not a product failure", workerPort)
				case !errors.Is(err, syscall.ECONNREFUSED):
					t.Fatalf("test infrastructure: a connection to the configured worker port %d before the supervisor started ended with %v, not a refusal, so it does not show that nobody listens; undecided; not a product failure", workerPort, err)
				}
			}
			s := startOwnedShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(workerPort)}, withHooks(serveshim.Hooks{
				Launching: signal,
				Starting:  func() { starts.Add(1) },
			}), prepare)
			s.awaitReady(t)
			select {
			case <-launching:
			case <-time.After(10 * time.Second):
				t.Fatal("the supervisor did not begin its first launch within 10s")
			}
			if !tc.contested {
				child := s.nextChild(t)
				if child.port != workerPort {
					t.Errorf("the worker inherited port %d; want the configured %d", child.port, workerPort)
				}
				if n := starts.Load(); n != 1 {
					t.Errorf("%d worker starts with a free fence; want 1", n)
				}
				s.cancel()
				return
			}
			awaitFenceReservation(t, s, worker, &starts)
			// A supervisor that stops at once needs moments; the margin is for a loaded venue.
			const margin = 4 * time.Second
			bound := time.Now().Add(margin)
			if tc.deadline {
				bound = s.startedAt.Add(ownedReportDeadline + margin)
			} else {
				s.cancel()
			}
			if !s.awaitStop(bound) {
				t.Fatalf("the supervisor was still running at its bound while another holder kept the start fence; stderr: %s", s.stderr)
			}
			switch {
			case tc.deadline && (s.result == nil || s.result.Error() != ownedTimeoutText):
				t.Errorf("the supervisor stopped with %v; want the report timeout %q", s.result, ownedTimeoutText)
			case !tc.deadline && s.result != nil:
				t.Errorf("the supervisor stopped with %v after its stop; want nil, the orderly stop", s.result)
			}
			if n := starts.Load(); n != 0 {
				t.Errorf("%d worker processes were started while another holder kept the start fence; want none", n)
			}
			_, public, err := net.SplitHostPort(s.public)
			if err != nil {
				t.Fatal(err)
			}
			requireFencePortsReleased(t, s.doneAt.Add(fenceReleaseBound), []fencePort{{"public", public}, {"worker", strconv.Itoa(workerPort)}})
		})
	}
}

// fenceDial connects to address once, within a short timeout.
func fenceDial(t *testing.T, address string) (net.Conn, error) {
	t.Helper()
	dialer := net.Dialer{Timeout: time.Second}
	return dialer.DialContext(t.Context(), "tcp4", address)
}

// awaitFenceReservation returns once worker, the configured worker port, accepts a connection while the supervisor runs
// and no worker has started: the supervisor's reservation of the worker's listener is in place, and the launch is at or
// before its wait for the contested fence. The connection stays queued, unaccepted, until the case ends. It fails the
// case when the supervisor stops or a worker starts first, or when the reservation is not seen within ten seconds. The
// polling only finds the moment; each attempt is a connect that cannot disturb the supervisor's own bind.
func awaitFenceReservation(t *testing.T, s *ownedShim, worker string, starts *atomic.Int32) {
	t.Helper()
	const bound = 10 * time.Second
	deadline := time.Now().Add(bound)
	for {
		conn, err := fenceDial(t, worker)
		if err == nil {
			t.Cleanup(func() { _ = conn.Close() })
			if n := starts.Load(); n != 0 {
				t.Fatalf("%d worker processes were started while another holder kept the start fence; want none", n)
			}
			select {
			case <-s.done:
				t.Fatalf("the supervisor stopped before its stop or deadline with the worker port reserved: %v; stderr: %s", s.result, s.stderr)
			default:
			}
			return
		}
		select {
		case <-s.done:
			t.Fatalf("the supervisor stopped before it reserved the worker port %s: %v; stderr: %s", worker, s.result, s.stderr)
		case <-time.After(10 * time.Millisecond):
		}
		if n := starts.Load(); n != 0 {
			t.Fatalf("%d worker processes were started while another holder kept the start fence; want none", n)
		}
		if time.Now().After(deadline) {
			t.Fatalf("test infrastructure: the worker port %s did not accept a connection within %v of the launch (last: %v), so the launch's wait at the contested fence could not be established; nothing was judged; not a product failure", worker, bound, err)
		}
	}
}

// fenceReleaseBound is how long the ports may still be in use once Run returned. A correct supervisor closed its
// listeners before returning. A copy of a socket that a fork elsewhere in this test binary holds until its exec lasts
// moments, and the bound leaves room for that on a loaded venue.
const fenceReleaseBound = 2 * time.Second

// fencePort is a port the supervisor must have released, with the name the failure messages use for it.
type fencePort struct{ what, port string }

// requireFencePortsReleased checks that each of ports, in the given order, is bound by this test no later than
// deadline, which the caller anchors at Run's return. Only "address already in use" is retried, since a short-lived
// copy of the socket in a forked process gives exactly that; any other error fails at once. A conflict that lasts to
// the deadline fails, as the release was not observed, and a first successful bind that completes after the deadline
// is undecided and fails too. Neither failure decides between a reservation the supervisor kept and another holder of
// the port: the port lock excludes cooperating test processes only.
func requireFencePortsReleased(t *testing.T, deadline time.Time, ports []fencePort) {
	t.Helper()
	for _, p := range ports {
		requireFencePortReleased(t, deadline, p)
	}
}

// requireFencePortReleased is requireFencePortsReleased for one port.
func requireFencePortReleased(t *testing.T, deadline time.Time, p fencePort) {
	t.Helper()
	address := net.JoinHostPort("127.0.0.1", p.port)
	for {
		var cfg net.ListenConfig
		ln, err := cfg.Listen(t.Context(), "tcp4", address)
		observed := time.Now()
		if err == nil {
			_ = ln.Close()
			if observed.After(deadline) {
				t.Errorf("the %s port %s was first bound %v after the release deadline, %v after the supervisor returned: its release by the deadline was not observed; undecided", p.what, p.port, observed.Sub(deadline), fenceReleaseBound)
			}
			return
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("bind the %s port %s after the supervisor returned: %v; want it released", p.what, p.port, err)
			return
		}
		if observed.After(deadline) {
			t.Errorf("the %s port %s was still in use %v after the supervisor returned (%v): its release was not observed; this does not tell a reservation the supervisor kept from another holder of the port", p.what, p.port, fenceReleaseBound, err)
			return
		}
		select {
		case <-t.Context().Done():
			t.Errorf("the %s port %s: the test ended before its release was observed", p.what, p.port)
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestFailedPublicDrainStopsRetiredWorker checks a public drain whose delivery to the worker fails after its generation
// was retired: the worker's listener accepts the supervisor's drain connection and never reads it. The supervisor
// stops that worker by the immediate path, its parent's end of file, not before ten seconds after the retirement,
// which leave room for the relay's attempts and the worker's own drain exchange, and within a generous bound after
// them; it observes the exit and ends the reservation, so the port is free for another program. Admission is never
// reopened for the retired generation and the drain is not sent again: the worker's farewell counts only the request
// it served before the drain, and no drain request at all.
//
// It rejects a supervisor that stops the worker at once when the delivery fails, one that never stops it, one that
// relays to it again or resends the drain on a new connection, and one that keeps its reservation. The failed
// delivery is staged with the worker fixture's held accept, a listener that holds the next connection unread; no
// supervisor hook is used. The lower bound counts from the moment the public drain was sent, which precedes the
// retirement; the upper bound counts from the first refused request after it, which follows the retirement.
func TestFailedPublicDrainStopsRetiredWorker(t *testing.T) {
	tests := map[string]struct{}{
		"error: a public drain whose delivery fails stops the retired worker after ten seconds and never relays to it": {},
	}
	for name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const (
				// recovery is the ruled time after the retirement before which the worker must not be stopped.
				recovery = 10 * time.Second
				// early is how far before recovery the worker must still be running, a margin for the retirement's
				// lag behind the moment the drain was sent.
				early = time.Second
				// late is the venue margin after recovery.
				late = 8 * time.Second
			)
			exits := make(chan int, 4)
			s := startOwnedShim(t, nil, withHooks(serveshim.Hooks{ExitObserved: func(pid int) {
				select {
				case exits <- pid:
				default:
				}
			}}))
			child := rulingServingChild(t, s)
			port := child.port
			child.command(t, "hold-accept", "armed")
			drained := make(chan error, 1)
			sentAt := time.Now()
			go func() {
				_, err := s.publicDrain(context.WithoutCancel(t.Context()))
				drained <- err
			}()
			t.Cleanup(func() { <-drained })
			child.command(t, "await-accept", "accept held")
			// The generation is retired before the drain is sent; the first refused request bounds it from above.
			if diff := gocmp.Diff(ownedRetryable, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request after the public drain (-want +got):\n%s", diff)
			}
			retiredBy := time.Now()
			farewell := child.farewell(t)
			goneAt := time.Now()
			if diff := gocmp.Diff("parent gone 0 1 0 0", farewell); diff != "" {
				t.Errorf("the retired worker's farewell (-want +got):\n%s", diff)
			}
			child.exited(t)
			if goneAt.Before(sentAt.Add(recovery - early)) {
				t.Errorf("the retired worker was stopped %v after the public drain was sent; want no stop before %v after its retirement", goneAt.Sub(sentAt), recovery)
			}
			if goneAt.After(retiredBy.Add(recovery + late)) {
				t.Errorf("the retired worker was stopped %v after its retirement was observed; want within %v", goneAt.Sub(retiredBy), recovery+late)
			}
			select {
			case pid := <-exits:
				if pid != child.pid {
					t.Errorf("the supervisor observed the exit of %d; want the retired worker %d", pid, child.pid)
				}
			case <-time.After(10 * time.Second):
				t.Error("the supervisor did not observe the retired worker's exit within 10s")
			}
			// The reservation ended: another program can take the port.
			foreign := awaitForeign(t, port, 5*time.Second, "after the retired worker was stopped")
			foreign.stop()
		})
	}
}
