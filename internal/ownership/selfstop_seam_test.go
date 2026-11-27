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

package ownership_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/ownership"
)

const seamPort uint16 = 18764

// seamPeer is the listener's end of one in-memory connection. Everything the client writes is captured as it
// arrives, whether or not the peer has parsed it yet, so that a write the client makes too early is visible.
type seamPeer struct {
	conn     net.Conn
	mu       sync.Mutex
	captured bytes.Buffer
	ended    chan struct{}
	br       *bufio.Reader
}

type seamCapture struct {
	p  *seamPeer
	pw *io.PipeWriter
}

func (c seamCapture) Write(b []byte) (int, error) {
	c.p.mu.Lock()
	c.p.captured.Write(b)
	c.p.mu.Unlock()
	return c.pw.Write(b)
}

func newSeamPeer(conn net.Conn) *seamPeer {
	pr, pw := io.Pipe()
	p := &seamPeer{conn: conn, ended: make(chan struct{}), br: bufio.NewReader(pr)}
	go func() {
		_, err := io.Copy(seamCapture{p, pw}, conn)
		_ = pw.CloseWithError(err)
		close(p.ended)
	}()
	return p
}

func (p *seamPeer) bytes() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.captured.String()
}

// next parses the next request the client sent.
func (p *seamPeer) next(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.ReadRequest(p.br)
	if err != nil {
		t.Fatalf("reading the client's request: %v; captured %q", err, p.bytes())
	}
	if _, err := io.Copy(io.Discard, req.Body); err != nil {
		t.Fatal(err)
	}
	return req
}

func (p *seamPeer) write(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(p.conn, s); err != nil {
		t.Fatalf("writing to the client: %v", err)
	}
}

// requests parses every complete request in the captured bytes.
func (p *seamPeer) requests(t *testing.T) []string {
	t.Helper()
	br := bufio.NewReader(strings.NewReader(p.bytes()))
	var out []string
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return out
		}
		_, _ = io.Copy(io.Discard, req.Body)
		out = append(out, req.Method+" "+req.RequestURI)
	}
}

// onlyRequest reports whether the captured bytes are exactly one complete request and nothing after it, so that a
// partial next request line counts as a write.
func (p *seamPeer) onlyRequest(t *testing.T) (string, bool) {
	t.Helper()
	captured := p.bytes()
	rest := strings.NewReader(captured)
	br := bufio.NewReader(rest)
	req, err := http.ReadRequest(br)
	if err != nil {
		return "", false
	}
	if _, err := io.Copy(io.Discard, req.Body); err != nil {
		return "", false
	}
	return req.Method + " " + req.RequestURI, br.Buffered() == 0 && rest.Len() == 0
}

type seamResult struct {
	pid int
	err error
}

// seamRun starts StopSelf over in-memory connections inside the caller's bubble and returns the peers it dials, in
// order, and its result.
func seamRun(t *testing.T, ctx context.Context) (<-chan *seamPeer, <-chan seamResult) {
	t.Helper()
	peers := make(chan *seamPeer, 4)
	restore := ownership.SetStopSelfDial(func(_ context.Context, network, address string) (net.Conn, error) {
		if want := fmt.Sprintf("127.0.0.1:%d", seamPort); address != want || (network != "tcp" && network != "tcp4") {
			return nil, fmt.Errorf("dial %s %s; want %s on tcp", network, address, want)
		}
		client, server := net.Pipe()
		peers <- newSeamPeer(server)
		return client, nil
	})
	t.Cleanup(restore)
	done := make(chan seamResult, 1)
	go func() {
		var r seamResult
		r.err = guarded("StopSelf", func() (err error) { r.pid, err = ownership.StopSelf(ctx, seamPort, ssToken); return err })
		done <- r
	}()
	return peers, done
}

// seamPeerOrResult waits for the next dial, failing with StopSelf's result if it returned first.
func seamPeerOrResult(t *testing.T, peers <-chan *seamPeer, done <-chan seamResult) *seamPeer {
	t.Helper()
	select {
	case p := <-peers:
		return p
	case r := <-done:
		t.Fatalf("StopSelf returned %d, %v without dialing", r.pid, r.err)
		return nil
	}
}

// seamProof returns a valid proof answer to the challenge, split before its last three bytes: the last character of
// the proof and the closing of the object.
func seamProof(t *testing.T, challenge *http.Request) (head, tail string) {
	t.Helper()
	nonce := challenge.Header.Get(controlauth.IdentityNonceHeader)
	body := fmt.Sprintf(`{"generation":%q,"proof":%q}`, ssGeneration, ssSign(t, ssToken, nonce, ssGeneration))
	answer := ssAnswer("200 OK", "application/json", body)
	return answer[:len(answer)-3], answer[len(answer)-3:]
}

// otherHex is a lowercase hexadecimal digit other than d.
func otherHex(d byte) string {
	if d == '0' {
		return "1"
	}
	return "0"
}

// TestStopSelfWritesNothingBeforeTheProof decides the order on the stop connection without timing: with the proof
// answer withheld three bytes before its end and every goroutine quiescent, the client has written exactly the
// challenge; once the valid tail arrives it writes the stop, once, on the same connection. The same answer with an
// invalid tail is the control: no stop, and only then the health request on a connection of its own.
func TestStopSelfWritesNothingBeforeTheProof(t *testing.T) {
	tests := map[string]struct {
		valid bool
	}{
		"success: a valid tail lets the stop follow on the same connection":     {valid: true},
		"error: an invalid tail is refused with nothing but the challenge sent": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peers, done := seamRun(t, t.Context())
				peer := seamPeerOrResult(t, peers, done)
				challenge := peer.next(t)
				head, tail := seamProof(t, challenge)
				peer.write(t, head)
				synctest.Wait()
				if got, only := peer.onlyRequest(t); got != "GET /internal/identity" || !only {
					t.Fatalf("before the proof was complete the client wrote more than the challenge: %q", peer.bytes())
				}
				if n := len(peers); n != 0 {
					t.Fatalf("%d more connections were opened before the proof was complete", n)
				}
				if strings.Contains(strings.ToLower(peer.bytes()), ssToken) {
					t.Fatalf("the token was written before the proof was complete: %q", peer.bytes())
				}
				if !tt.valid {
					tail = otherHex(tail[0]) + tail[1:]
				}
				peer.write(t, tail)
				if tt.valid {
					stop := peer.next(t)
					if stop.Method != http.MethodPost || stop.RequestURI != "/internal/stop" || stop.Header.Get("X-Model-Gateway-Control-Token") != ssToken {
						t.Errorf("after the proof the client sent %s %s", stop.Method, stop.RequestURI)
					}
					peer.write(t, ssAnswer("202 Accepted", "application/json", `{"ok":true,"stopping":true}`))
					r := <-done
					if r.err != nil {
						t.Fatalf("StopSelf: %v", r.err)
					}
				} else {
					synctest.Wait()
					select {
					case health := <-peers:
						req := health.next(t)
						if req.RequestURI != "/healthz" || req.Header.Get("X-Model-Gateway-Control-Token") != "" || req.Header.Get(controlauth.IdentityNonceHeader) != "" {
							t.Errorf("the second connection carried %s %s with headers %v", req.Method, req.RequestURI, req.Header)
						}
						health.write(t, ssAnswer("200 OK", "application/json", fmt.Sprintf(`{"ok":true,"supervisorPid":%d}`, ssHealthPID)))
					case r := <-done:
						t.Fatalf("StopSelf returned %d, %v without the health request", r.pid, r.err)
					}
					r := <-done
					if !errors.Is(r.err, ownership.ErrUnproven) || r.pid != ssHealthPID {
						t.Errorf("StopSelf = %d, %v; want %d and an error matching ErrUnproven", r.pid, r.err, ssHealthPID)
					}
				}
				synctest.Wait()
				<-peer.ended
				want := []string{"GET /internal/identity"}
				if tt.valid {
					want = append(want, "POST /internal/stop")
				}
				if diff := gocmp.Diff(want, peer.requests(t)); diff != "" {
					t.Errorf("requests on the stop connection (-want +got):\n%s", diff)
				}
				select {
				case extra := <-peers:
					t.Errorf("an unexpected connection was opened; it carried %q", extra.bytes())
				default:
				}
			})
		})
	}
}

// defaultBudget is the bound of an exchange whose context has no deadline, written here independently of the
// package's own constant.
const defaultBudget = 5 * time.Second

// TestStopSelfBudget decides the bounds of the exchange in virtual time. A context without a deadline gets five
// seconds for the whole exchange, not per request; a context with a deadline gets exactly that deadline, shorter or
// longer; a cancellation while the client waits for the challenge's answer, the stop's acknowledgment or the health
// document ends the exchange there. Every case requires the parked connection closed and the error for that point.
func TestStopSelfBudget(t *testing.T) {
	tests := map[string]struct {
		// deadline, when positive, is the caller's own.
		deadline time.Duration
		// delay is spent before the listener answers the challenge.
		delay time.Duration
		// park says where the listener stops answering: "challenge", "stop" or "health".
		park string
		// cancel cancels the context once the client is parked; otherwise a budget must end it.
		cancel  bool
		wantErr []error
		elapsed time.Duration
	}{
		"error: a silent listener and a context without a deadline": {
			park: "challenge", wantErr: []error{ownership.ErrUnproven}, elapsed: defaultBudget,
		},
		"error: the default budget spans the proof and the acknowledgment": {
			delay: 2 * time.Second, park: "stop", wantErr: []error{ownership.ErrStopUnacknowledged}, elapsed: defaultBudget,
		},
		"error: the default budget spans the refusal and the health document": {
			delay: 2 * time.Second, park: "health", wantErr: []error{ownership.ErrUnproven}, elapsed: defaultBudget,
		},
		"error: a shorter caller deadline ends a parked acknowledgment": {
			deadline: 1500 * time.Millisecond, park: "stop", wantErr: []error{ownership.ErrStopUnacknowledged, context.DeadlineExceeded}, elapsed: 1500 * time.Millisecond,
		},
		"error: a shorter caller deadline ends a parked health document": {
			deadline: 1500 * time.Millisecond, park: "health", wantErr: []error{ownership.ErrUnproven}, elapsed: 1500 * time.Millisecond,
		},
		"error: a longer caller deadline replaces the default": {
			deadline: 8 * time.Second, park: "challenge", wantErr: []error{ownership.ErrUnproven, context.DeadlineExceeded}, elapsed: 8 * time.Second,
		},
		"error: a cancellation while waiting for the challenge's answer": {
			park: "challenge", cancel: true, wantErr: []error{ownership.ErrUnproven, context.Canceled},
		},
		"error: a cancellation while waiting for the stop's acknowledgment": {
			park: "stop", cancel: true, wantErr: []error{ownership.ErrStopUnacknowledged, context.Canceled},
		},
		"error: a cancellation while waiting for the health document": {
			park: "health", cancel: true, wantErr: []error{ownership.ErrUnproven},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt.deadline > 0 {
					ctx, cancel = context.WithTimeout(ctx, tt.deadline)
					defer cancel()
				}
				start := time.Now()
				peers, done := seamRun(t, ctx)
				peer := seamPeerOrResult(t, peers, done)
				parked := peer
				challenge := peer.next(t)
				time.Sleep(tt.delay)
				switch tt.park {
				case "stop":
					head, tail := seamProof(t, challenge)
					peer.write(t, head+tail)
					_ = peer.next(t)
				case "health":
					head, tail := seamProof(t, challenge)
					peer.write(t, head+otherHex(tail[0])+tail[1:])
					parked = seamPeerOrResult(t, peers, done)
					if req := parked.next(t); req.RequestURI != "/healthz" {
						t.Fatalf("the second connection carried %s %s", req.Method, req.RequestURI)
					}
				}
				synctest.Wait()
				if tt.cancel {
					select {
					case r := <-done:
						t.Fatalf("StopSelf returned %d, %v before it was cancelled", r.pid, r.err)
					default:
					}
					cancel()
				}
				r := <-done
				for _, want := range tt.wantErr {
					if !errors.Is(r.err, want) {
						t.Errorf("StopSelf = %v; want an error matching %v", r.err, want)
					}
				}
				if tt.park == "health" && (r.pid != 0 || r.err == nil || r.err.Error() != ssRefusal(seamPort, 0)) {
					t.Errorf("StopSelf = %d, %v; want no health process number and the refusal %q", r.pid, r.err, ssRefusal(seamPort, 0))
				}
				if tt.elapsed > 0 {
					if got := time.Since(start); got != tt.elapsed {
						t.Errorf("the exchange ended after %v; want exactly %v", got, tt.elapsed)
					}
				}
				synctest.Wait()
				select {
				case <-parked.ended:
				default:
					t.Error("the client left the parked connection open")
				}
				if tt.park != "stop" && strings.Contains(strings.ToLower(peer.bytes()), ssToken) {
					t.Errorf("the token was written to an unproven listener: %q", peer.bytes())
				}
			})
		})
	}
}

// TestStopSelfRefusesAnOversizedProofEarly decides that an answer which cannot fit the 1 MiB cap is refused while
// the listener still holds the rest of it back: on its head when its announced length is over the cap, and once the
// cap is passed when it is chunked, before any terminal chunk or end of stream.
func TestStopSelfRefusesAnOversizedProofEarly(t *testing.T) {
	tests := map[string]func(challenge *http.Request) string{
		"error: an announced length one byte over the cap": func(*http.Request) string {
			return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", ssProofCap+1)
		},
		"error: an announced length far beyond the cap": func(*http.Request) string {
			return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", int64(1)<<62)
		},
		"error: chunks one byte over the cap, the terminal chunk held back": func(challenge *http.Request) string {
			req := ssRequest{Header: challenge.Header}
			chunks := ssChunks(ssProofOfSize(t, req, ssProofCap+1), 4096)
			return "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" + strings.TrimSuffix(chunks, "0\r\n\r\n")
		},
	}
	for name, answer := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peers, done := seamRun(t, t.Context())
				peer := seamPeerOrResult(t, peers, done)
				challenge := peer.next(t)
				written := make(chan struct{})
				go func() {
					defer close(written)
					// The client may stop reading once it has refused; the write then ends with an error.
					_, _ = io.WriteString(peer.conn, answer(challenge))
				}()
				// The rest of the answer is never written: a Content-Length body stays pending and the terminal chunk is
				// held back. Once every goroutine is blocked, the client must already have refused the proof and issued
				// its token-free diagnostic on another connection. When it closes the control connection is not
				// prescribed here.
				synctest.Wait()
				var health *seamPeer
				select {
				case health = <-peers:
				case r := <-done:
					t.Fatalf("StopSelf returned %d, %v without the health request", r.pid, r.err)
				default:
					t.Fatal("the client issued no health request while the oversized answer was still unfinished")
				}
				healthReq := health.next(t)
				if healthReq.Method != http.MethodGet || healthReq.URL.Path != "/healthz" {
					t.Errorf("diagnostic request = %s %s; want GET /healthz", healthReq.Method, healthReq.URL.Path)
				}
				health.write(t, ssAnswer("200 OK", "application/json", fmt.Sprintf(`{"ok":true,"supervisorPid":%d}`, ssHealthPID)))
				r := <-done
				if !errors.Is(r.err, ownership.ErrUnproven) || r.pid != ssHealthPID {
					t.Errorf("StopSelf = %d, %v; want %d and an error matching ErrUnproven", r.pid, r.err, ssHealthPID)
				}
				synctest.Wait()
				select {
				case <-peer.ended:
				default:
					t.Error("the control connection is still open after StopSelf returned")
				}
				<-written
				if strings.Contains(strings.ToLower(peer.bytes()), ssToken) {
					t.Errorf("the token was written to an unproven listener: %q", peer.bytes())
				}
			})
		})
	}
}
