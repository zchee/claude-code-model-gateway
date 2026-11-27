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

package supervisor_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// retryableBody is the answer to a request that no current generation kept: a resend is free, so it carries no retry
// veto.
const retryableBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway could not deliver this request to the shim worker; retry it shortly"}}`

// leaseAttempts is one attempt plus the relay's 80 waits of 50 milliseconds (lib/commands.js:2357-2389).
const leaseAttempts = 1 + 80

// generationAction is what a worker generation does with a request whose body it has read to the end.
type generationAction int

const (
	// dropBody closes the connection without a response; the generation stays current.
	dropBody generationAction = iota + 1
	// dropBodyAndEnd ends the generation, as an observed exit does, and then closes the connection without a response.
	dropBodyAndEnd
	// answerOK writes a complete 200 response.
	answerOK
	// commitThenFail writes the head of a 200 response and a prefix of its body, then waits until the test lets it
	// fail: it ends the generation and resets the connection.
	commitThenFail
	// dropBodyAndRetire withdraws the generation's admission, as a drain does, and closes the connection without a
	// response; the generation keeps running and is not ended.
	dropBodyAndRetire
	// handOff reserves the next generation of the fixture's plan, ends its own generation, publishes the next one and
	// closes the connection without a response.
	handOff
)

const (
	answerOKBytes = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	// committedPrefix is the part of the committed response that a commitThenFail generation sends.
	committedPrefix = "partial"
)

// leaseFixture is a registry whose generations the test plays itself. A generation's worker logic serves on a
// private lease listener that the registry's dial function alone reaches: a dial to the generation's authority is
// routed there. The reservation's own listener, which owns the authority's port, accepts and counts whatever reaches
// it; anything there connected by port, not through a lease.
type leaseFixture struct {
	ctx      context.Context
	reg      *workerlink.Registry
	acquires atomic.Int32
	// afterAcquire, when set, runs after the relay's n-th request for a lease was answered, before the relay sees the
	// answer.
	afterAcquire func(n int32)
	// leasedAt records, per generation, the number of the first lease request that returned a lease to it.
	leasedAt map[workerlink.Generation]int32
	// parkDial, when set, runs at the start of every dial the registry makes, before it connects.
	parkDial func()
	chained  atomic.Int32
	chainErr atomic.Pointer[error]
	mu       sync.Mutex
	// plan holds the actions of the generations handOff starts, in order.
	plan    []generationAction
	routes  map[string]string
	refused map[string]bool
	lost    []workerlink.Generation
	workers []*generationWorker
}

func newLeaseFixture(t *testing.T) *leaseFixture {
	t.Helper()
	f := &leaseFixture{ctx: t.Context(), routes: make(map[string]string), refused: make(map[string]bool), leasedAt: make(map[workerlink.Generation]int32)}
	var cfg net.ListenConfig
	f.reg = workerlink.NewRegistry(cfg.Listen, func(ctx context.Context, network, address string) (net.Conn, error) {
		f.mu.Lock()
		refused, route := f.refused[address], f.routes[address]
		f.mu.Unlock()
		if refused {
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
		}
		if f.parkDial != nil {
			f.parkDial()
		}
		if route == "" {
			return nil, fmt.Errorf("the fixture has no generation at %s", address)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, route)
	})
	t.Cleanup(func() {
		for _, w := range f.generations() {
			w.end()
			w.wg.Wait()
		}
	})
	return f
}

// Acquire counts the relay's requests for a lease and passes them to the registry.
func (f *leaseFixture) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	n := f.acquires.Add(1)
	lease, err := f.reg.Acquire(ctx, exclude)
	if err == nil {
		f.mu.Lock()
		if _, seen := f.leasedAt[lease.Generation()]; !seen {
			f.leasedAt[lease.Generation()] = n
		}
		f.mu.Unlock()
	}
	if f.afterAcquire != nil {
		f.afterAcquire(n)
	}
	return lease, err
}

func (f *leaseFixture) recordLost(g workerlink.Generation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lost = append(f.lost, g)
}

func (f *leaseFixture) lostGenerations() []workerlink.Generation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lost)
}

// generations returns every generation the fixture started, in order.
func (f *leaseFixture) generations() []*generationWorker {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.workers)
}

// checkLeaseUse fails the test when a connection reached a generation's port directly instead of through a lease, or
// when a relayed request did not carry its generation's authority in its Host field.
func (f *leaseFixture) checkLeaseUse(t *testing.T) {
	t.Helper()
	if n := f.byPort(); n != 0 {
		t.Errorf("%d connections reached a generation's port directly instead of through a lease", n)
	}
	for _, w := range f.generations() {
		if wrong := w.wrongHosts(); len(wrong) != 0 {
			t.Errorf("generation %q received requests with Host %q; want its authority 127.0.0.1:%d", w.gen, wrong, w.port)
		}
	}
}

// byPort counts the connections that reached any generation's authority port directly.
func (f *leaseFixture) byPort() int32 {
	var n int32
	for _, w := range f.generations() {
		n += w.byPort.Load()
	}
	return n
}

// generationWorker is the worker side of one reserved generation.
type generationWorker struct {
	gen       workerlink.Generation
	port      uint16
	res       *workerlink.Reservation
	authority net.Listener
	lease     net.Listener
	action    generationAction
	f         *leaseFixture
	byPort    atomic.Int32
	mu        sync.Mutex
	bodies    []string
	hosts     []string
	// publishedAfter is the number of lease requests the relay had made when the generation was published.
	publishedAfter int32
	wg             sync.WaitGroup
	once           sync.Once
	// ended is closed once end has run; committed is closed when a commitThenFail generation has sent its prefix;
	// fail lets it fail.
	ended     chan struct{}
	committed chan struct{}
	fail      chan struct{}
	failOnce  sync.Once
}

// letFail lets a commitThenFail generation fail; it is safe to call more than once.
func (w *generationWorker) letFail() { w.failOnce.Do(func() { close(w.fail) }) }

// start reserves generation gen on port (zero selects one), serves it with action, and publishes it unless publish is
// false. A dial to the generation is refused by the registry when refuse is set.
func (f *leaseFixture) start(t *testing.T, gen workerlink.Generation, port uint16, action generationAction, publish, refuse bool) *generationWorker {
	t.Helper()
	w, err := f.reserveWorker(gen, port, action, refuse)
	if err == nil && publish {
		err = w.publish()
	}
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// reserveWorker reserves a generation without publishing it, for use outside the test goroutine too. The fixture's
// cleanup ends every generation it started.
func (f *leaseFixture) reserveWorker(gen workerlink.Generation, port uint16, action generationAction, refuse bool) (*generationWorker, error) {
	res, err := f.reg.Reserve(f.ctx, gen, port)
	if err != nil {
		return nil, fmt.Errorf("reserve generation %q on port %d: %w", gen, port, err)
	}
	file, err := res.File()
	if err != nil {
		_ = res.End()
		return nil, fmt.Errorf("take the worker's copy of generation %q's listener: %w", gen, err)
	}
	authority, err := workerlink.ImportListener(file, res.Port())
	if err != nil {
		_ = res.End()
		return nil, fmt.Errorf("import generation %q's listener: %w", gen, err)
	}
	var cfg net.ListenConfig
	lease, err := cfg.Listen(f.ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		_, _ = res.End(), authority.Close()
		return nil, err
	}
	w := &generationWorker{
		gen: gen, port: res.Port(), res: res, authority: authority, lease: lease, action: action, f: f,
		ended: make(chan struct{}), committed: make(chan struct{}), fail: make(chan struct{}),
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(w.port)))
	f.mu.Lock()
	f.workers = append(f.workers, w)
	f.routes[address] = lease.Addr().String()
	f.refused[address] = refuse
	f.mu.Unlock()
	w.wg.Go(func() {
		for {
			conn, err := authority.Accept()
			if err != nil {
				return
			}
			w.byPort.Add(1)
			_ = conn.Close()
		}
	})
	w.wg.Go(func() {
		for {
			conn, err := lease.Accept()
			if err != nil {
				return
			}
			w.wg.Go(func() { w.serve(conn) })
		}
	})
	return w, nil
}

func (w *generationWorker) publish() error {
	if err := w.res.Publish(os.Getpid()); err != nil {
		return fmt.Errorf("publish generation %q: %w", w.gen, err)
	}
	w.mu.Lock()
	w.publishedAfter = w.f.acquires.Load()
	w.mu.Unlock()
	return nil
}

// end ends the generation in the registry and closes the worker's listeners, so that its port is free.
func (w *generationWorker) end() {
	w.once.Do(func() {
		_ = w.res.End()
		_, _ = w.authority.Close(), w.lease.Close()
		close(w.ended)
	})
}

// wrongHosts returns the Host fields of the generation's requests that were not its own authority.
func (w *generationWorker) wrongHosts() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	want := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(w.port)))
	var wrong []string
	for _, h := range w.hosts {
		if h != want {
			wrong = append(wrong, h)
		}
	}
	return wrong
}

// received returns the request bodies the generation read to the end.
func (w *generationWorker) received() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.bodies)
}

func (w *generationWorker) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(hangGuard))
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return
	}
	w.mu.Lock()
	w.bodies = append(w.bodies, string(body))
	w.hosts = append(w.hosts, req.Host)
	w.mu.Unlock()
	switch w.action {
	case dropBody:
	case dropBodyAndEnd:
		w.end()
	case dropBodyAndRetire:
		w.res.Retire()
	case handOff:
		w.f.mu.Lock()
		var action generationAction
		if len(w.f.plan) > 0 {
			action, w.f.plan = w.f.plan[0], w.f.plan[1:]
		}
		w.f.mu.Unlock()
		next, err := w.f.reserveWorker(workerlink.Generation(fmt.Sprintf("handed-generation-%03d", w.f.chained.Add(1))), 0, action, false)
		w.end()
		if err == nil {
			err = next.publish()
		}
		if err != nil {
			w.f.chainErr.CompareAndSwap(nil, &err)
		}
	case answerOK:
		_, _ = io.WriteString(conn, answerOKBytes)
	case commitThenFail:
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n"+committedPrefix)
		close(w.committed)
		<-w.fail
		w.end()
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
	}
}

// servePublicLeases runs supervisor.Serve with the handler that reaches workers through f, and returns the public
// address. A handler constructor that panics fails the test instead of ending the test binary.
func servePublicLeases(t *testing.T, f *leaseFixture) string {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, err := func() (h http.Handler, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("the lease handler's constructor panicked: %v", r)
			}
		}()
		return supervisor.NewLeaseHandler(ln.Addr().(*net.TCPAddr).Port, f, strings.Repeat("5d", 32), "1.2.3", 37, f.recordLost)
	}()
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	served := make(chan error, 1)
	go func() { served <- supervisor.Serve(ctx, ln, h) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("public listener: %v", err)
		}
		_ = ln.Close()
	})
	return ln.Addr().String()
}

// answered is a response whose body was read and closed.
type answered struct {
	StatusCode int
	Header     http.Header
}

// post sends one request with body to the public listener and returns the response head and its body.
func post(t *testing.T, public, body string) (answered, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), hangGuard)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+public+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return answered{StatusCode: resp.StatusCode, Header: resp.Header}, string(got), readErr
}

// postQuiet is post for use outside the test goroutine: it returns every failure instead of failing the test.
func postQuiet(ctx context.Context, public, body string) (answered, string, error) {
	ctx, cancel := context.WithTimeout(ctx, hangGuard)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+public+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return answered{}, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return answered{}, "", err
	}
	got, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return answered{StatusCode: resp.StatusCode, Header: resp.Header}, string(got), readErr
}

// probeBody returns a request body that no other request of the test carries.
func probeBody() string { return `{"model":"m","probe":"` + rand.Text() + `"}` }

// generationAnswer is what the client sees, with the X-Should-Retry values exactly as sent (nil when the field is
// absent), the exact bodies each generation received, and the generations reported as having lost a request.
type generationAnswer struct {
	Status       int
	ShouldRetry  []string
	ContentType  string
	Body         string
	FirstBodies  []string
	SecondBodies []string
	Lost         []workerlink.Generation
}

// TestRelayDeliveryFollowsGeneration checks that the recipient of a relayed request is the worker generation a lease
// reached, not a port number, and the source's two ordinary exhaustion cases with generations in place of workers:
//
//   - The generation that took the body is still current when the attempts run out: the 503 with X-Should-Retry: false,
//     and the loss is reported for that generation once, after the relay has waited through its attempts
//     (lib/commands.js:2353-2356,2363-2365,2427-2437).
//   - A later attempt finds no generation, or another one: the acceptance is not carried forward, and when nothing newer
//     accepts before the attempts run out the answer is the retryable 503 without the header (lib/commands.js:
//     2367-2370,2384,2427-2431).
//
// A newer generation that is eligible on a later attempt receives the body once and answers at that attempt. The third
// case, a last attempt whose connection reached a generation that is already gone, is TestRelayLastAttemptException;
// a committed response is TestRelayCommittedResponseIsNeverResent.
func TestRelayDeliveryFollowsGeneration(t *testing.T) {
	const (
		first  = workerlink.Generation("first-generation")
		second = workerlink.Generation("second-generation")
	)
	tests := map[string]struct {
		firstAction  generationAction
		firstRefuses bool
		// second, when set, is published once the first generation has taken the body and ended.
		second        generationAction
		secondRefuses bool
		secondOnSame  bool
		// publishNothing leaves every generation unpublished.
		publishNothing bool
		// exhausted marks a case whose answer comes only once the relay has stopped retrying. Unless takerLive is set,
		// the relay seeks an eligible generation on every retry, so its first retry, the first lease request after one
		// that led to no answer, is held: the answer must not exist while it is held, so the relay retried instead of
		// answering at its first failure. How many attempts the relay makes is decided by
		// TestRelayLastAttemptException, which controls its last one.
		exhausted bool
		// takerLive marks a case whose generation that took the body stays current until the attempts run out. The
		// relay may then wait without asking for another lease, as the source waits without looking up a worker while
		// the accepting one is current (lib/commands.js:2363-2366), so the case asserts only the one delivered body, the
		// one reported loss and the veto; how the relay waits is the relay's own tests' matter.
		takerLive bool
		// firstTaken and secondTaken say whether the generation must have received the body exactly once.
		firstTaken, secondTaken bool
		want                    generationAnswer
	}{
		"error: the generation that took the body is still current, so the body is not sent again and the loss is reported": {
			firstAction: dropBody, exhausted: true, takerLive: true, firstTaken: true,
			want: generationAnswer{Status: http.StatusServiceUnavailable, ShouldRetry: []string{"false"}, ContentType: "application/json", Body: lostWorkerBody, Lost: []workerlink.Generation{first}},
		},
		"error: the generation that took the body is retired but still running, so it stays current and the loss is reported": {
			// The source's current worker changes only when a worker is started, which needs the previous one gone, and
			// at its exit (lib/commands.js:2613,2625,2651): a draining worker stays current until it exits.
			firstAction: dropBodyAndRetire, exhausted: true, takerLive: true, firstTaken: true,
			want: generationAnswer{Status: http.StatusServiceUnavailable, ShouldRetry: []string{"false"}, ContentType: "application/json", Body: lostWorkerBody, Lost: []workerlink.Generation{first}},
		},
		"error: the generation that took the body ended and no generation is published afterwards": {
			firstAction: dropBodyAndEnd, exhausted: true, firstTaken: true,
			want: generationAnswer{Status: http.StatusServiceUnavailable, ContentType: "application/json", Body: retryableBody},
		},
		"error: the generation that took the body ended and a newer generation on another port refuses connections": {
			firstAction: dropBodyAndEnd, second: answerOK, secondRefuses: true, exhausted: true, firstTaken: true,
			want: generationAnswer{Status: http.StatusServiceUnavailable, ContentType: "application/json", Body: retryableBody},
		},
		"error: no generation took the body because the only one refuses connections from the start": {
			firstAction: answerOK, firstRefuses: true, exhausted: true,
			want: generationAnswer{Status: http.StatusServiceUnavailable, ContentType: "application/json", Body: retryableBody},
		},
		"success: request before the first report waits and gets the retryable 503": {
			firstAction: answerOK, publishNothing: true, exhausted: true,
			want: generationAnswer{Status: http.StatusServiceUnavailable, ContentType: "application/json", Body: retryableBody},
		},
		"success: a newer generation on another port receives the body again and answers": {
			firstAction: dropBodyAndEnd, second: answerOK, firstTaken: true, secondTaken: true,
			want: generationAnswer{Status: http.StatusOK, ContentType: "text/plain", Body: "ok"},
		},
		"success: a newer generation on the same port is another recipient and receives the body again": {
			firstAction: dropBodyAndEnd, second: answerOK, secondOnSame: true, firstTaken: true, secondTaken: true,
			want: generationAnswer{Status: http.StatusOK, ContentType: "text/plain", Body: "ok"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// The relay's attempts last about four seconds; the cases need not wait in turn.
			t.Parallel()
			f := newLeaseFixture(t)
			body := probeBody()
			held, letRetry := make(chan struct{}), make(chan struct{})
			freeRetry := sync.OnceFunc(func() { close(letRetry) })
			// Released before the cleanups run, among them the server's, which waits for the request parked here.
			defer freeRetry()
			seeksReplacement := tc.exhausted && !tc.takerLive
			if seeksReplacement {
				var retried atomic.Bool
				f.afterAcquire = func(n int32) {
					if n > 1 && retried.CompareAndSwap(false, true) {
						close(held)
						<-letRetry
					}
				}
			}
			one := f.start(t, first, 0, tc.firstAction, !tc.publishNothing, tc.firstRefuses)
			public := servePublicLeases(t, f)
			var two *generationWorker
			var secondErr error
			secondStarted := make(chan struct{})
			// abandonSecond lets the goroutine below return when the case ends before the first generation did: the
			// fixture ends the first only in a cleanup that runs after this case's own.
			abandonSecond := make(chan struct{})
			if tc.second != 0 {
				// The second generation is published once the first has taken the body and ended: an explicit barrier,
				// so the second is first available to a later attempt.
				go func() {
					defer close(secondStarted)
					select {
					case <-one.ended:
					case <-abandonSecond:
						return
					case <-time.After(hangGuard):
						return
					}
					port := uint16(0)
					if tc.secondOnSame {
						port = one.port
					}
					two, secondErr = f.reserveWorker(second, port, tc.second, tc.secondRefuses)
					if secondErr == nil {
						secondErr = two.publish()
					}
				}()
				t.Cleanup(func() {
					close(abandonSecond)
					<-secondStarted
				})
			} else {
				close(secondStarted)
			}
			type result struct {
				resp    answered
				got     string
				readErr error
			}
			results := make(chan result, 1)
			go func() {
				resp, got, readErr := postQuiet(t.Context(), public, body)
				results <- result{resp, got, readErr}
			}()
			if seeksReplacement {
				select {
				case <-held:
				case r := <-results:
					t.Fatalf("answered %d before the relay retried; want it to retry before it answers", r.resp.StatusCode)
				case <-time.After(hangGuard):
					t.Fatal("the relay never retried its lease request")
				}
				select {
				case r := <-results:
					t.Fatalf("answered %d while the relay's first retry was held", r.resp.StatusCode)
				default:
				}
				freeRetry()
			}
			var r result
			select {
			case r = <-results:
			case <-time.After(hangGuard):
				t.Fatal("no answer within the hang guard")
			}
			resp, got, readErr := r.resp, r.got, r.readErr
			<-secondStarted
			if secondErr != nil {
				t.Fatalf("start the second generation: %v", secondErr)
			}
			if readErr != nil {
				t.Errorf("read the answer: %v", readErr)
			}
			want := tc.want
			if tc.firstTaken {
				want.FirstBodies = []string{body}
			}
			if tc.secondTaken {
				want.SecondBodies = []string{body}
			}
			answer := generationAnswer{
				Status: resp.StatusCode, ShouldRetry: resp.Header.Values("X-Should-Retry"), ContentType: resp.Header.Get("Content-Type"),
				Body: got, FirstBodies: one.received(), Lost: f.lostGenerations(),
			}
			if two != nil {
				answer.SecondBodies = two.received()
			}
			if diff := gocmp.Diff(want, answer); diff != "" {
				t.Errorf("client-visible answer, exact bodies each generation received and reported losses (-want +got):\n%s", diff)
			}
			if tc.secondTaken && two != nil {
				two.mu.Lock()
				published := two.publishedAfter
				two.mu.Unlock()
				f.mu.Lock()
				leased := f.leasedAt[second]
				f.mu.Unlock()
				// The first lease request after the second generation became eligible reaches it; one already under
				// way when it was published may too.
				if leased != published && leased != published+1 {
					t.Errorf("the second generation, published after lease request %d, was first leased by request %d; want the next one", published, leased)
				}
			}
			f.checkLeaseUse(t)
		})
	}
}

// TestRelayCommittedResponseIsNeverResent checks that once a generation's response head was relayed, the relay sends
// the body to no other generation when that generation fails: the client keeps the status it received, reads the
// part of the body that was sent, and then sees its response end before its declared length (lib/commands.js:
// 2393-2401,2416-2417). The failure is held until the client has read the committed head and prefix, and a newer
// generation is eligible when it happens.
func TestRelayCommittedResponseIsNeverResent(t *testing.T) {
	t.Parallel()
	f := newLeaseFixture(t)
	public := servePublicLeases(t, f)
	body := probeBody()
	one := f.start(t, "committing-generation", 0, commitThenFail, true, false)
	// The generation's failure is let go on every path, so that a failed step never leaves the fixture waiting.
	t.Cleanup(one.letFail)
	ctx, cancel := context.WithTimeout(t.Context(), hangGuard)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+public+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	prefix := make([]byte, len(committedPrefix))
	if _, err := io.ReadFull(resp.Body, prefix); err != nil {
		t.Fatalf("read the committed prefix: %v", err)
	}
	select {
	case <-one.committed:
	case <-ctx.Done():
		t.Fatal("the generation never committed its response")
	}
	// A newer generation is eligible before the committed one fails.
	one.res.Retire()
	two := f.start(t, "eligible-generation", 0, answerOK, true, false)
	one.letFail()
	rest, readErr := io.ReadAll(resp.Body)
	// The end must come from the other side, never from this client's own deadline or cancellation.
	peerEnded := errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, io.EOF) || errors.Is(readErr, syscall.ECONNRESET)
	if readErr != nil && (errors.Is(readErr, context.DeadlineExceeded) || errors.Is(readErr, context.Canceled) || os.IsTimeout(readErr)) {
		peerEnded = false
	}
	type committed struct {
		Status       int
		ContentType  string
		Body         string
		Ended        bool
		FirstBodies  []string
		SecondBodies []string
	}
	want := committed{Status: http.StatusOK, ContentType: "text/plain", Body: committedPrefix, Ended: true, FirstBodies: []string{body}}
	got := committed{
		Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: string(prefix) + string(rest),
		Ended: peerEnded, FirstBodies: one.received(), SecondBodies: two.received(),
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("committed response after its generation failed (-want +got):\n%s\nread error: %v", diff, readErr)
	}
	f.checkLeaseUse(t)
}

// TestRelayLastAttemptException pins the one case in which the pinned source answers a request as lost although the
// generation that took its body has already gone. The relay's error handler marks the body as taken whenever the
// connection of the LAST attempt reached a worker, without asking whether that worker is still current
// (lib/commands.js:2382-2385, 2353-2356); the answer is then the 503 with X-Should-Retry: false (lib/commands.js:
// 2427-2437), and the loss is reported for that generation.
//
// No generation is eligible for the first 80 attempts; once the relay's 80th request for a lease was refused, one
// generation is published, so the last attempt is the first to reach it. That generation takes the body, ends, and
// only then closes the connection.
func TestRelayLastAttemptException(t *testing.T) {
	t.Parallel()
	f := newLeaseFixture(t)
	last, err := f.reserveWorker("last-attempt-generation", 0, dropBodyAndEnd, false)
	if err != nil {
		t.Fatal(err)
	}
	published := make(chan error, 1)
	f.afterAcquire = func(n int32) {
		if n == leaseAttempts-1 {
			published <- last.publish()
		}
	}
	public := servePublicLeases(t, f)
	body := probeBody()
	resp, got, readErr := post(t, public, body)
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("publish the generation of the last attempt: %v", err)
		}
	default:
		t.Fatalf("the relay asked for %d leases; the generation of the last attempt was never published", f.acquires.Load())
	}
	type answer struct {
		Status      int
		ShouldRetry []string
		ContentType string
		Body        string
		Complete    bool
		Bodies      []string
		Lost        []workerlink.Generation
	}
	want := answer{
		Status: http.StatusServiceUnavailable, ShouldRetry: []string{"false"}, ContentType: "application/json",
		Body: lostWorkerBody, Complete: true, Bodies: []string{body}, Lost: []workerlink.Generation{last.gen},
	}
	gotAnswer := answer{
		Status: resp.StatusCode, ShouldRetry: resp.Header.Values("X-Should-Retry"), ContentType: resp.Header.Get("Content-Type"),
		Body: got, Complete: readErr == nil, Bodies: last.received(), Lost: f.lostGenerations(),
	}
	if diff := gocmp.Diff(want, gotAnswer); diff != "" {
		t.Errorf("answer after a last attempt whose generation took the body and was gone (-want +got):\n%s", diff)
	}
	select {
	case <-last.ended:
	default:
		t.Errorf("generation %q, which took the body on the last attempt, had not ended when the relay answered", last.gen)
	}
	f.checkLeaseUse(t)
}

// TestRelayResendsToEveryNewerGeneration checks that resends are not capped: each time the generation that took the
// body is no longer current and a newer generation accepts, the body is sent to that newer generation, never twice to
// one generation, bounded only by the attempts (lib/commands.js:2357-2389). Three generations in a row take the body:
// the first two end after taking it, each making the next one eligible, and the third answers.
func TestRelayResendsToEveryNewerGeneration(t *testing.T) {
	t.Parallel()
	f := newLeaseFixture(t)
	public := servePublicLeases(t, f)
	f.plan = []generationAction{handOff, answerOK}
	f.start(t, "handed-generation-000", 0, handOff, true, false)
	body := probeBody()
	resp, got, readErr := post(t, public, body)
	if errp := f.chainErr.Load(); errp != nil {
		t.Fatalf("a generation could not start the next one: %v", *errp)
	}
	type delivery struct {
		Generation workerlink.Generation
		Bodies     []string
	}
	var deliveries []delivery
	for _, w := range f.generations() {
		deliveries = append(deliveries, delivery{w.gen, w.received()})
	}
	want := []delivery{
		{"handed-generation-000", []string{body}},
		{"handed-generation-001", []string{body}},
		{"handed-generation-002", []string{body}},
	}
	if diff := gocmp.Diff(want, deliveries); diff != "" {
		t.Errorf("bodies each generation received (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff([]any{http.StatusOK, "ok", nil, []workerlink.Generation(nil)}, []any{resp.StatusCode, got, readErr, f.lostGenerations()}); diff != "" {
		t.Errorf("answer from the third generation, read error and reported losses (-want +got):\n%s", diff)
	}
	f.checkLeaseUse(t)
}

// TestRelayHandsOverADialPinnedBeforeRetire checks that a dial the relay began while the generation was eligible and
// that completes after the generation was retired, as a drain does, is handed over: the request is delivered to that
// generation and answered by it, not treated as undelivered and not sent anywhere else.
func TestRelayHandsOverADialPinnedBeforeRetire(t *testing.T) {
	t.Parallel()
	f := newLeaseFixture(t)
	entered, letDial := make(chan struct{}, 1), make(chan struct{})
	freeDial := sync.OnceFunc(func() { close(letDial) })
	// Released before the cleanups run, among them the server's, which waits for the dial parked here.
	defer freeDial()
	f.parkDial = func() {
		select {
		case entered <- struct{}{}:
			<-letDial
		default:
		}
	}
	one := f.start(t, "pinned-generation", 0, answerOK, true, false)
	public := servePublicLeases(t, f)
	body := probeBody()
	type result struct {
		resp answered
		got  string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		resp, got, err := postQuiet(t.Context(), public, body)
		results <- result{resp, got, err}
	}()
	select {
	case <-entered:
	case r := <-results:
		t.Fatalf("answered %d before the relay dialled", r.resp.StatusCode)
	case <-time.After(hangGuard):
		t.Fatal("the relay never dialled the generation")
	}
	one.res.Retire()
	freeDial()
	var r result
	select {
	case r = <-results:
	case <-time.After(hangGuard):
		t.Fatal("no answer within the hang guard")
	}
	if diff := gocmp.Diff([]any{http.StatusOK, "ok", nil, []string{body}}, []any{r.resp.StatusCode, r.got, r.err, one.received()}); diff != "" {
		t.Errorf("answer to a request whose dial completed after its generation was retired, and the bodies it received (-want +got):\n%s", diff)
	}
	f.checkLeaseUse(t)
}
