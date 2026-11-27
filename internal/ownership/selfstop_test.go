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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/ownership"
)

const (
	ssToken      = "73656c662d73746f702d636f6e74726f6c2d746f6b656e2d6162636465663031"
	ssOtherToken = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	ssGeneration = "supervisor-startup-5d21"
	ssHealthPID  = 4242
	// ssWatchdog only turns a hang into a failure; no assertion depends on it.
	ssWatchdog = 20 * time.Second
	// ssProofCap is the largest identity answer body the client reads.
	ssProofCap = 1 << 20
)

var ssNonce = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ssRequest is one request the fake listener parsed.
type ssRequest struct {
	Method, Target, Host, Proto string
	TransferEncoding            []string
	Header                      http.Header
	Body                        string
}

// ssConn is everything one accepted connection carried.
type ssConn struct {
	received bytes.Buffer
	requests []ssRequest
	// leftOpen is true when the client neither closed nor finished the connection before the watchdog.
	leftOpen bool
}

// ssIdentity writes the answer to an identity request and reports whether the connection stays open for another
// request. It may block until the client leaves.
type ssIdentity func(w io.Writer, req ssRequest, c net.Conn, rest *bufio.Reader) bool

// ssFake is a raw TCP listener the test holds. It answers the identity path with the case's behaviour, the stop
// path with an acknowledgement or a failure, and /healthz with a health document; it records every byte.
type ssFake struct {
	ln       net.Listener
	port     uint16
	identity ssIdentity
	// stopAnswer is written for a stop request; empty means the connection is closed without an answer.
	stopAnswer string
	// health is the health document's body; empty means the health connection is closed without an answer.
	health string
	mu     sync.Mutex
	conns  []*ssConn
	wg     sync.WaitGroup
}

type ssTee struct {
	f *ssFake
	c *ssConn
	r io.Reader
}

func (t ssTee) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.f.mu.Lock()
	t.c.received.Write(p[:n])
	t.f.mu.Unlock()
	return n, err
}

// ssOption configures the fake before its accept goroutine starts.
type ssOption func(*ssFake)

func ssWithHealth(body string) ssOption { return func(f *ssFake) { f.health = body } }

func ssWithStopAnswer(answer string) ssOption { return func(f *ssFake) { f.stopAnswer = answer } }

func ssStart(t *testing.T, identity ssIdentity, options ...ssOption) *ssFake {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &ssFake{
		ln: ln, port: uint16(ln.Addr().(*net.TCPAddr).Port), identity: identity,
		stopAnswer: ssAnswer("202 Accepted", "application/json", `{"ok":true,"stopping":true}`),
		health:     fmt.Sprintf(`{"ok":true,"supervisorPid":%d}`, ssHealthPID),
	}
	for _, option := range options {
		option(f)
	}
	f.wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Go(func() { f.serve(conn) })
		}
	})
	t.Cleanup(func() { _ = ln.Close(); f.wg.Wait() })
	return f
}

func ssAnswer(status, contentType, body string, extra ...string) string {
	head := []string{"HTTP/1.1 " + status, "Content-Length: " + strconv.Itoa(len(body))}
	if contentType != "" {
		head = append(head, "Content-Type: "+contentType)
	}
	return strings.Join(append(head, extra...), "\r\n") + "\r\n\r\n" + body
}

func (f *ssFake) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	c := &ssConn{}
	f.mu.Lock()
	f.conns = append(f.conns, c)
	f.mu.Unlock()
	_ = conn.SetDeadline(time.Now().Add(ssWatchdog))
	br := bufio.NewReader(ssTee{f, c, conn})
	defer func() {
		// Record whatever else the client sends until it closes the connection.
		if _, err := io.Copy(io.Discard, br); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				f.mu.Lock()
				c.leftOpen = true
				f.mu.Unlock()
			}
		}
	}()
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		body, _ := io.ReadAll(io.LimitReader(req.Body, ssProofCap))
		r := ssRequest{Method: req.Method, Target: req.RequestURI, Host: req.Host, Proto: req.Proto, TransferEncoding: req.TransferEncoding, Header: req.Header, Body: string(body)}
		f.mu.Lock()
		c.requests = append(c.requests, r)
		f.mu.Unlock()
		target, _, _ := strings.Cut(r.Target, "?")
		switch target {
		case "/internal/identity":
			if !f.identity(conn, r, conn, br) {
				ssHalfClose(conn)
				return
			}
		case "/internal/stop":
			if f.stopAnswer == "" {
				ssHalfClose(conn)
				return
			}
			_, _ = io.WriteString(conn, f.stopAnswer)
		case "/healthz":
			if f.health == "" {
				ssHalfClose(conn)
				return
			}
			_, _ = io.WriteString(conn, ssAnswer("200 OK", "application/json", f.health))
		default:
			_, _ = io.WriteString(conn, ssAnswer("404 Not Found", "text/plain", "not found"))
		}
	}
}

func ssHalfClose(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

// record waits until every connection has ended and returns what they carried.
func (f *ssFake) record() []*ssConn {
	_ = f.ln.Close()
	f.wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func ssSign(t testing.TB, token, nonce, generation string) string {
	t.Helper()
	proof, err := controlauth.SignSupervisorIdentity(token, nonce, generation)
	if err != nil {
		t.Errorf("sign: %v", err)
	}
	return proof
}

// ssProven answers like the supervisor: a proof of this root's token over the client's nonce.
func ssProven(t testing.TB, extra ...string) ssIdentity {
	return func(w io.Writer, req ssRequest, _ net.Conn, _ *bufio.Reader) bool {
		nonce := req.Header.Get(controlauth.IdentityNonceHeader)
		body := fmt.Sprintf(`{"generation":%q,"proof":%q}`, ssGeneration, ssSign(t, ssToken, nonce, ssGeneration))
		_, _ = io.WriteString(w, ssAnswer("200 OK", "application/json", body, append([]string{"Cache-Control: no-store"}, extra...)...))
		return true
	}
}

// ssBody answers 200 JSON with a body built from the client's nonce.
func ssBody(build func(nonce string) string) ssIdentity {
	return func(w io.Writer, req ssRequest, _ net.Conn, _ *bufio.Reader) bool {
		_, _ = io.WriteString(w, ssAnswer("200 OK", "application/json", build(req.Header.Get(controlauth.IdentityNonceHeader))))
		return true
	}
}

// ssProofBody is a valid proof body for the request's nonce and generation.
func ssProofBody(t testing.TB, req ssRequest, generation string) string {
	nonce := req.Header.Get(controlauth.IdentityNonceHeader)
	return fmt.Sprintf(`{"generation":%q,"proof":%q}`, generation, ssSign(t, ssToken, nonce, generation))
}

// ssProofOfSize is a valid proof body of exactly size bytes, its generation padded to fit.
func ssProofOfSize(t testing.TB, req ssRequest, size int) string {
	frame := len(ssProofBody(t, req, "g"))
	return ssProofBody(t, req, strings.Repeat("g", size-frame+1))
}

// ssChunks frames body as a complete chunked entity of chunks of at most size bytes.
func ssChunks(body string, size int) string {
	var b strings.Builder
	for len(body) > 0 {
		n := min(size, len(body))
		fmt.Fprintf(&b, "%x\r\n%s\r\n", n, body[:n])
		body = body[n:]
	}
	return b.String() + "0\r\n\r\n"
}

func ssRaw(answer string, keep bool) ssIdentity {
	return func(w io.Writer, _ ssRequest, _ net.Conn, _ *bufio.Reader) bool {
		_, _ = io.WriteString(w, answer)
		return keep
	}
}

func stopSelf(t testing.TB, ctx context.Context, port uint16, token string) (int, error) {
	t.Helper()
	type result struct {
		pid int
		err error
	}
	r := noPanic(t, "StopSelf", func() result {
		pid, err := ownership.StopSelf(ctx, port, token)
		return result{pid, err}
	})
	return r.pid, r.err
}

// ssRefusal is the pinned refusal for a listener that could not be identified (lib/process-supervision.js:613).
func ssRefusal(port uint16, pid int) string {
	observed := "unknown"
	if pid != 0 {
		observed = strconv.Itoa(pid)
	}
	return fmt.Sprintf("could not confirm the owner of :%d (last observed PID %s); the owning process could not be identified as this model-gateway install; left the listener untouched", port, observed)
}

// ssCheckChallenge decides that a request is only the identity challenge: exact numeric Host, no Origin, no token,
// one canonical nonce and no entity.
func ssCheckChallenge(t *testing.T, port uint16, r ssRequest) {
	t.Helper()
	if r.Method != http.MethodGet || r.Target != "/internal/identity" {
		t.Errorf("challenge request %s %s; want GET /internal/identity", r.Method, r.Target)
	}
	if want := "127.0.0.1:" + strconv.Itoa(int(port)); r.Host != want {
		t.Errorf("challenge Host %q; want %q", r.Host, want)
	}
	if got := r.Header.Values("Host"); len(got) > 0 {
		t.Errorf("Host parsed as a header %v", got)
	}
	nonces := r.Header.Values(controlauth.IdentityNonceHeader)
	if len(nonces) != 1 || !ssNonce.MatchString(nonces[0]) {
		t.Errorf("challenge nonces %q; want exactly one of 64 lowercase hexadecimal characters", nonces)
	}
	for _, forbidden := range []string{"Origin", "X-Model-Gateway-Control-Token", "Authorization", "Cookie"} {
		if v, ok := r.Header[forbidden]; ok {
			t.Errorf("challenge carries %s: %q", forbidden, v)
		}
	}
	if r.Body != "" || len(r.TransferEncoding) > 0 {
		t.Errorf("challenge carries an entity %q (transfer coding %q)", r.Body, r.TransferEncoding)
	}
	if r.Proto != "HTTP/1.1" {
		t.Errorf("challenge protocol %q; want HTTP/1.1", r.Proto)
	}
}

// ssTranscripts lists, per accepted connection in order, the requests it carried.
func ssTranscripts(conns []*ssConn) [][]string {
	var out [][]string
	for _, c := range conns {
		var requests []string
		for _, r := range c.requests {
			requests = append(requests, r.Method+" "+r.Target)
		}
		out = append(out, requests)
	}
	return out
}

// ssCheckNothingButChallenge decides the negative guarantees of a refusal on everything the listener received.
func ssCheckNothingButChallenge(t *testing.T, f *ssFake, conns []*ssConn) {
	t.Helper()
	identity := 0
	for i, c := range conns {
		raw := c.received.String()
		lower := strings.ToLower(raw)
		if strings.Contains(lower, ssToken) {
			t.Errorf("connection %d received the control token: %q", i, raw)
		}
		if strings.Contains(raw, "POST ") || strings.Contains(raw, "/internal/stop") {
			t.Errorf("connection %d received a stop: %q", i, raw)
		}
		if c.leftOpen {
			t.Errorf("connection %d was left open by the client", i)
		}
		for _, r := range c.requests {
			switch r.Target {
			case "/internal/identity":
				identity++
				if i != 0 {
					t.Errorf("an identity challenge arrived on connection %d; a stop attempt uses one connection", i)
				}
				ssCheckChallenge(t, f.port, r)
			case "/healthz":
				if r.Method != http.MethodGet || r.Body != "" || r.Proto != "HTTP/1.1" {
					t.Errorf("health request %s %s with entity %q", r.Method, r.Proto, r.Body)
				}
				for _, forbidden := range []string{"X-Model-Gateway-Control-Token", controlauth.IdentityNonceHeader, "Authorization"} {
					if v, ok := r.Header[forbidden]; ok {
						t.Errorf("the health request carries %s: %q", forbidden, v)
					}
				}
			default:
				t.Errorf("unexpected request %s %s on connection %d", r.Method, r.Target, i)
			}
		}
		for _, r := range c.requests {
			if r.Host != "127.0.0.1:"+strconv.Itoa(int(f.port)) {
				t.Errorf("connection %d named Host %q", i, r.Host)
			}
		}
	}
	if identity != 1 {
		t.Errorf("%d identity challenges were sent; want exactly 1 (no retry, no redirect followed)", identity)
	}
	if diff := gocmp.Diff([][]string{{"GET /internal/identity"}, {"GET /healthz"}}, ssTranscripts(conns)); diff != "" {
		t.Errorf("requests per connection (-want +got): one challenge, then one health request on a connection of its own:\n%s", diff)
	}
}

func TestStopSelfRefusesAnUnprovenListener(t *testing.T) {
	longGeneration := strings.Repeat("g", ssProofCap)
	tests := map[string]struct {
		identity ssIdentity
		// noHealth closes the health connection without an answer.
		noHealth bool
		health   string
		wantPID  int
	}{
		"error: end of stream instead of an answer": {
			identity: func(io.Writer, ssRequest, net.Conn, *bufio.Reader) bool { return false },
			noHealth: true,
		},
		"error: a redirect to the identity path": {
			identity: func(w io.Writer, _ ssRequest, c net.Conn, _ *bufio.Reader) bool {
				_, _ = io.WriteString(w, ssAnswer("302 Found", "", "", "Location: http://"+c.LocalAddr().String()+"/internal/identity"))
				return true
			},
			wantPID: ssHealthPID,
		},
		"error: not found": {
			identity: ssRaw(ssAnswer("404 Not Found", "text/plain", "404 page not found"), true),
			wantPID:  ssHealthPID,
		},
		"error: an HTML page": {
			identity: ssRaw(ssAnswer("200 OK", "text/html", "<!doctype html><title>dev server</title>"), true),
			wantPID:  ssHealthPID,
		},
		"error: a proof under another root's token": {
			identity: ssBody(func(nonce string) string {
				return fmt.Sprintf(`{"generation":%q,"proof":%q}`, ssGeneration, ssSign(t, ssOtherToken, nonce, ssGeneration))
			}),
			wantPID: ssHealthPID,
		},
		"error: a proof for another nonce": {
			identity: ssBody(func(string) string {
				other := strings.Repeat("ab", 32)
				return fmt.Sprintf(`{"generation":%q,"proof":%q}`, ssGeneration, ssSign(t, ssToken, other, ssGeneration))
			}),
			wantPID: ssHealthPID,
		},
		"error: a proof for another generation than the one answered": {
			identity: ssBody(func(nonce string) string {
				return fmt.Sprintf(`{"generation":%q,"proof":%q}`, ssGeneration, ssSign(t, ssToken, nonce, "another-generation"))
			}),
			wantPID: ssHealthPID,
		},
		"error: a proof in uppercase": {
			identity: ssBody(func(nonce string) string {
				return fmt.Sprintf(`{"generation":%q,"proof":%q}`, ssGeneration, strings.ToUpper(ssSign(t, ssToken, nonce, ssGeneration)))
			}),
			wantPID: ssHealthPID,
		},
		"error: a truncated proof": {
			identity: ssBody(func(nonce string) string {
				return fmt.Sprintf(`{"generation":%q,"proof":%q}`, ssGeneration, ssSign(t, ssToken, nonce, ssGeneration)[:62])
			}),
			wantPID: ssHealthPID,
		},
		"error: an answer with another member": {
			identity: ssBody(func(nonce string) string {
				return fmt.Sprintf(`{"generation":%q,"proof":%q,"pid":1}`, ssGeneration, ssSign(t, ssToken, nonce, ssGeneration))
			}),
			wantPID: ssHealthPID,
		},
		"error: an answer without the generation": {
			identity: ssBody(func(nonce string) string {
				return fmt.Sprintf(`{"proof":%q}`, ssSign(t, ssToken, nonce, ssGeneration))
			}),
			wantPID: ssHealthPID,
		},
		"error: an answer that is not JSON": {
			identity: ssBody(func(string) string { return "generation=x&proof=y" }),
			wantPID:  ssHealthPID,
		},
		"error: a valid proof with a status other than 200": {
			identity: func(w io.Writer, req ssRequest, _ net.Conn, _ *bufio.Reader) bool {
				nonce := req.Header.Get(controlauth.IdentityNonceHeader)
				body := fmt.Sprintf(`{"generation":%q,"proof":%q}`, ssGeneration, ssSign(t, ssToken, nonce, ssGeneration))
				_, _ = io.WriteString(w, ssAnswer("500 Internal Server Error", "application/json", body))
				return true
			},
			wantPID: ssHealthPID,
		},
		"error: a valid proof that ends the connection": {
			identity: func(w io.Writer, req ssRequest, c net.Conn, r *bufio.Reader) bool {
				ssProven(t, "Connection: close")(w, req, c, r)
				return false
			},
			wantPID: ssHealthPID,
		},
		"error: a valid proof over the body cap": {
			identity: ssBody(func(nonce string) string {
				return fmt.Sprintf(`{"generation":%q,"proof":%q}`, longGeneration, ssSign(t, ssToken, nonce, longGeneration))
			}),
			wantPID: ssHealthPID,
		},
		"error: a valid proof whose announced length is never completed": {
			identity: func(w io.Writer, req ssRequest, c net.Conn, _ *bufio.Reader) bool {
				body := ssProofBody(t, req, ssGeneration)
				_, _ = fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body)+10, body)
				return false
			},
			wantPID: ssHealthPID,
		},
		"error: a valid chunked proof without its terminal chunk": {
			identity: func(w io.Writer, req ssRequest, _ net.Conn, _ *bufio.Reader) bool {
				body := ssProofBody(t, req, ssGeneration)
				_, _ = fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(body), body)
				return false
			},
			wantPID: ssHealthPID,
		},
		"error: a proof with a duplicate member": {
			identity: ssBody(func(nonce string) string {
				return fmt.Sprintf(`{"generation":%q,"generation":%q,"proof":%q}`, ssGeneration, ssGeneration, ssSign(t, ssToken, nonce, ssGeneration))
			}),
			wantPID: ssHealthPID,
		},
		"error: a proof followed by a trailing value": {
			identity: ssBody(func(nonce string) string {
				return fmt.Sprintf(`{"generation":%q,"proof":%q} {}`, ssGeneration, ssSign(t, ssToken, nonce, ssGeneration))
			}),
			wantPID: ssHealthPID,
		},
		"error: a protocol switch instead of an answer": {
			identity: func(w io.Writer, req ssRequest, _ net.Conn, _ *bufio.Reader) bool {
				_, _ = fmt.Fprintf(w, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n%s", ssProofBody(t, req, ssGeneration))
				return false
			},
			wantPID: ssHealthPID,
		},
		"error: a health document without a process number": {
			identity: ssRaw(ssAnswer("404 Not Found", "text/plain", "404 page not found"), true),
			health:   `{"ok":true}`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var options []ssOption
			switch {
			case tt.noHealth:
				options = append(options, ssWithHealth(""))
			case tt.health != "":
				options = append(options, ssWithHealth(tt.health))
			}
			f := ssStart(t, tt.identity, options...)
			ctx, cancel := context.WithTimeout(t.Context(), ssWatchdog)
			defer cancel()
			pid, err := stopSelf(t, ctx, f.port, ssToken)
			if !errors.Is(err, ownership.ErrUnproven) {
				t.Fatalf("StopSelf = %d, %v; want an error matching ErrUnproven", pid, err)
			}
			if diff := gocmp.Diff(ssRefusal(f.port, tt.wantPID), err.Error()); diff != "" {
				t.Errorf("refusal text (-want +got):\n%s", diff)
			}
			if pid != tt.wantPID {
				t.Errorf("health process number %d; want %d", pid, tt.wantPID)
			}
			ssCheckNothingButChallenge(t, f, f.record())
		})
	}
}

func TestStopSelfStopsAProvenSupervisor(t *testing.T) {
	tests := map[string]ssIdentity{
		"success: a proof framed by length": ssProven(t),
		"success: a chunked proof is read in full": func(w io.Writer, req ssRequest, _ net.Conn, _ *bufio.Reader) bool {
			nonce := req.Header.Get(controlauth.IdentityNonceHeader)
			body := fmt.Sprintf(`{"generation":%q,"proof":%q}`, ssGeneration, ssSign(t, ssToken, nonce, ssGeneration))
			half := len(body) / 2
			_, _ = fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", half, body[:half])
			_, _ = fmt.Fprintf(w, "%x\r\n%s\r\n0\r\n\r\n", len(body)-half, body[half:])
			return true
		},
		"success: a proof body of exactly the cap": func(w io.Writer, req ssRequest, _ net.Conn, _ *bufio.Reader) bool {
			_, _ = io.WriteString(w, ssAnswer("200 OK", "application/json", ssProofOfSize(t, req, ssProofCap)))
			return true
		},
		"success: a long generation below the cap": func(w io.Writer, req ssRequest, _ net.Conn, _ *bufio.Reader) bool {
			nonce := req.Header.Get(controlauth.IdentityNonceHeader)
			long := strings.Repeat("g", ssProofCap/2)
			body := fmt.Sprintf(`{"generation":%q,"proof":%q}`, long, ssSign(t, ssToken, nonce, long))
			_, _ = io.WriteString(w, ssAnswer("200 OK", "application/json", body))
			return true
		},
	}
	for name, identity := range tests {
		t.Run(name, func(t *testing.T) {
			f := ssStart(t, identity)
			ctx, cancel := context.WithTimeout(t.Context(), ssWatchdog)
			defer cancel()
			if _, err := stopSelf(t, ctx, f.port, ssToken); err != nil {
				t.Fatalf("StopSelf: %v", err)
			}
			conns := f.record()
			if len(conns) != 1 {
				t.Fatalf("%d connections; want both exchanges on one connection", len(conns))
			}
			c := conns[0]
			if c.leftOpen {
				t.Error("the client left the connection open after the acknowledgement")
			}
			if len(c.requests) != 2 {
				t.Fatalf("requests %+v; want the challenge and then the stop", c.requests)
			}
			ssCheckChallenge(t, f.port, c.requests[0])
			stop := c.requests[1]
			if stop.Method != http.MethodPost || stop.Target != "/internal/stop" {
				t.Errorf("second request %s %s; want POST /internal/stop", stop.Method, stop.Target)
			}
			if diff := gocmp.Diff([]string{ssToken}, stop.Header.Values("X-Model-Gateway-Control-Token")); diff != "" {
				t.Errorf("stop token headers (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff([]string{"application/json"}, stop.Header.Values("Content-Type")); diff != "" {
				t.Errorf("stop content type (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff([]string{"2"}, stop.Header.Values("Content-Length")); diff != "" {
				t.Errorf("stop Content-Length (-want +got):\n%s", diff)
			}
			if stop.Body != "{}" {
				t.Errorf("stop body %q; want {}", stop.Body)
			}
			for _, forbidden := range []string{"Origin", "Authorization", controlauth.IdentityNonceHeader} {
				if v, ok := stop.Header[forbidden]; ok {
					t.Errorf("stop carries %s: %q", forbidden, v)
				}
			}
			want := "127.0.0.1:" + strconv.Itoa(int(f.port))
			if diff := gocmp.Diff([][2]string{{want, "HTTP/1.1"}, {want, "HTTP/1.1"}}, [][2]string{{c.requests[0].Host, c.requests[0].Proto}, {stop.Host, stop.Proto}}); diff != "" {
				t.Errorf("Host and protocol of the two requests (-want +got):\n%s", diff)
			}
			if n := strings.Count(strings.ToLower(c.received.String()), ssToken); n != 1 {
				t.Errorf("the token appeared %d times on the wire; want once, in the stop", n)
			}
		})
	}
}

// TestStopSelfDoesNotRepeatAnUnacknowledgedStop decides that a proven supervisor that does not acknowledge gets
// exactly one stop, on the connection of its proof, and no new attempt on any connection.
func TestStopSelfDoesNotRepeatAnUnacknowledgedStop(t *testing.T) {
	tests := map[string]string{
		"error: a failed stop answer":                  ssAnswer("500 Internal Server Error", "", ""),
		"error: an acknowledgement of other shape":     ssAnswer("202 Accepted", "application/json", `{"ok":false}`),
		"error: the connection ends without an answer": "",
	}
	for name, answer := range tests {
		t.Run(name, func(t *testing.T) {
			f := ssStart(t, ssProven(t), ssWithStopAnswer(answer))
			ctx, cancel := context.WithTimeout(t.Context(), ssWatchdog)
			defer cancel()
			if _, err := stopSelf(t, ctx, f.port, ssToken); !errors.Is(err, ownership.ErrStopUnacknowledged) {
				t.Errorf("StopSelf = %v; want an error matching ErrStopUnacknowledged", err)
			}
			if diff := gocmp.Diff([][]string{{"GET /internal/identity", "POST /internal/stop"}}, ssTranscripts(f.record())); diff != "" {
				t.Errorf("requests per connection (-want +got): one connection with the challenge and the stop, nothing else:\n%s", diff)
			}
		})
	}
}

func TestStopSelfFreshChallenge(t *testing.T) {
	f := ssStart(t, ssProven(t))
	for range 2 {
		if _, err := stopSelf(t, t.Context(), f.port, ssToken); err != nil {
			t.Fatalf("StopSelf: %v", err)
		}
	}
	var nonces []string
	for _, c := range f.record() {
		for _, r := range c.requests {
			if r.Target == "/internal/identity" {
				nonces = append(nonces, r.Header.Get(controlauth.IdentityNonceHeader))
			}
		}
	}
	if len(nonces) != 2 || nonces[0] == nonces[1] {
		t.Errorf("challenges %q; want two different nonces", nonces)
	}
}

// TestStopSelfInvalidRequest decides that a request the client cannot make safely opens no connection at all.
func TestStopSelfInvalidRequest(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	tests := map[string]struct {
		ctx      context.Context
		token    string
		zeroPort bool
		wantErr  error
	}{
		"error: a context that has already ended": {ctx: canceled, token: ssToken, wantErr: context.Canceled},
		"error: a token that is not this format":  {ctx: t.Context(), token: strings.ToUpper(ssToken)},
		"error: no token":                         {ctx: t.Context(), token: ""},
		"error: port zero":                        {ctx: t.Context(), token: ssToken, zeroPort: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := ssStart(t, ssProven(t))
			port := f.port
			if tt.zeroPort {
				port = 0
			}
			_, err := stopSelf(t, tt.ctx, port, tt.token)
			if err == nil {
				t.Fatal("StopSelf succeeded")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("StopSelf = %v; want an error matching %v", err, tt.wantErr)
			}
			if strings.Contains(strings.ToLower(err.Error()), ssToken) {
				t.Errorf("the error repeats the token: %v", err)
			}
			if conns := f.record(); len(conns) != 0 {
				t.Errorf("%d connections were opened", len(conns))
			}
		})
	}
}
