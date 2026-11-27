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

package supervisor_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

const (
	ctlToken      = "6d6f64656c2d676174657761792d636f6e74726f6c2d746f6b656e2d31323334"
	ctlOtherToken = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	// ctlGeneration is the startup generation the supervisor published in its own record.
	ctlGeneration = "supervisor-startup-3f9c2a71"
	ctlNonceName  = "X-Model-Gateway-Identity-Nonce"
	ctlTokenName  = "X-Model-Gateway-Control-Token"
	// ctlWatchdog only turns a hang into a failure; no assertion depends on it.
	ctlWatchdog = 30 * time.Second
)

// ctlNoPanic runs call and turns a panic into a test failure, so that the unimplemented dispatcher cannot abort the
// package's other tests.
func ctlNoPanic[T any](t testing.TB, what string, call func() T) (result T) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("%s panicked: %v", what, p)
		}
	}()
	return call()
}

type ctlGateway struct {
	handler http.Handler
	err     error
}

func ctlNewGateway(t testing.TB, o supervisor.Options) (http.Handler, error) {
	t.Helper()
	g := ctlNoPanic(t, "NewGateway", func() ctlGateway {
		h, err := supervisor.NewGateway(o)
		return ctlGateway{h, err}
	})
	return g.handler, g.err
}

type ctlSigned struct{ Token, Nonce, Generation string }

// ctlFixture is a dispatcher served on a loopback listener of its own, with counters for every action it may take.
type ctlFixture struct {
	port   int
	relays atomic.Int64
	stops  atomic.Int64
	mu     sync.Mutex
	signed []ctlSigned
	// stopHook runs inside Stop, when set.
	stopHook func()
}

func (f *ctlFixture) signatures() []ctlSigned {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ctlSigned(nil), f.signed...)
}

// ctlOptions returns complete options whose callbacks count into f.
func ctlOptions(f *ctlFixture, port int) supervisor.Options {
	return supervisor.Options{
		PublicPort: port,
		Token:      ctlToken,
		Relay: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.relays.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "relayed")
		}),
		Restart: func(context.Context, []byte) error { return errors.New("restart is not part of these tests") },
		Stop: func() {
			f.stops.Add(1)
			if f.stopHook != nil {
				f.stopHook()
			}
		},
		StartupGeneration: ctlGeneration,
		SignIdentity: func(token, nonce, generation string) (string, error) {
			f.mu.Lock()
			f.signed = append(f.signed, ctlSigned{token, nonce, generation})
			f.mu.Unlock()
			return controlauth.SignSupervisorIdentity(token, nonce, generation)
		},
	}
}

// ctlServe binds a loopback listener, builds the dispatcher for its actual port and serves it until cleanup.
func ctlServe(t *testing.T, mutate func(*ctlFixture, *supervisor.Options)) *ctlFixture {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &ctlFixture{port: ln.Addr().(*net.TCPAddr).Port}
	o := ctlOptions(f, f.port)
	if mutate != nil {
		mutate(f, &o)
	}
	handler, err := ctlNewGateway(t, o)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	served := make(chan error, 1)
	go func() { served <- supervisor.Serve(ctx, ln, handler) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("public listener: %v", err)
		}
	})
	return f
}

// ctlConn is one client connection whose received bytes are all kept.
type ctlConn struct {
	conn     net.Conn
	br       *bufio.Reader
	received bytes.Buffer
}

func ctlDial(t *testing.T, port int) *ctlConn {
	t.Helper()
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(ctlWatchdog)); err != nil {
		t.Fatal(err)
	}
	c := &ctlConn{conn: conn}
	c.br = bufio.NewReader(io.TeeReader(conn, &c.received))
	return c
}

// exchange writes one raw request and reads one complete response with its body.
// ctlAnswer is a response whose body has been read and closed.
type ctlAnswer struct {
	StatusCode int
	Header     http.Header
	Close      bool
}

func (c *ctlConn) exchange(t *testing.T, raw string) (ctlAnswer, []byte) {
	t.Helper()
	if _, err := io.WriteString(c.conn, raw); err != nil {
		t.Fatalf("write request: %v", err)
	}
	method, _, _ := strings.Cut(raw, " ")
	return c.readFor(t, method)
}

// readFor reads the response to a request of the given method; a HEAD answer has no body whatever its framing says.
func (c *ctlConn) readFor(t *testing.T, method string) (ctlAnswer, []byte) {
	t.Helper()
	resp, err := http.ReadResponse(c.br, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read response: %v; received so far %q", err, c.received.String())
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return ctlAnswer{StatusCode: resp.StatusCode, Header: resp.Header, Close: resp.Close}, body
}

func ctlNonce(t testing.TB) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

// ctlHead joins a request line and header lines into a request head.
func ctlHead(requestLine string, headers ...string) string {
	return requestLine + "\r\n" + strings.Join(append(headers, ""), "\r\n") + "\r\n"
}

func ctlHost(port int) string { return "Host: 127.0.0.1:" + strconv.Itoa(port) }

// ctlIdentityRequest is the proof challenge a stop client sends: exact numeric Host, no Origin, no token.
func ctlIdentityRequest(port int, nonce string) string {
	return ctlHead("GET /internal/identity HTTP/1.1", ctlHost(port), ctlNonceName+": "+nonce)
}

// ctlIdentityAnswer is the body of a successful proof.
type ctlIdentityAnswer struct {
	Generation string `json:"generation"`
	Proof      string `json:"proof"`
}

// ctlCheckProof decides a successful identity answer: its framing, its exact members and a proof that verifies for
// this root's token, the client's nonce and the supervisor's startup generation.
func ctlCheckProof(t *testing.T, resp ctlAnswer, body []byte, nonce string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("identity status %d, body %q; want 200", resp.StatusCode, body)
	}
	if diff := gocmp.Diff([]string{"application/json"}, resp.Header.Values("Content-Type")); diff != "" {
		t.Errorf("Content-Type (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff([]string{"no-store"}, resp.Header.Values("Cache-Control")); diff != "" {
		t.Errorf("Cache-Control (-want +got):\n%s", diff)
	}
	if resp.Close {
		t.Error("a successful proof closed the connection; the stop must be able to follow on it")
	}
	var members map[string]any
	if err := json.Unmarshal(body, &members); err != nil {
		t.Fatalf("identity body %q is not one JSON object: %v", body, err)
	}
	if len(members) != 2 {
		t.Errorf("identity body members %v; want exactly generation and proof", members)
	}
	var answer ctlIdentityAnswer
	if err := json.Unmarshal(body, &answer, json.RejectUnknownMembers(true)); err != nil {
		t.Fatalf("identity body %q: %v", body, err)
	}
	if diff := gocmp.Diff(ctlGeneration, answer.Generation); diff != "" {
		t.Errorf("signed generation (-want +got):\n%s", diff)
	}
	if !controlauth.VerifySupervisorIdentity(ctlToken, nonce, ctlGeneration, answer.Proof) {
		t.Errorf("proof %q does not verify for this root's token, the client's nonce and the startup generation", answer.Proof)
	}
	if controlauth.VerifySupervisorIdentity(ctlOtherToken, nonce, ctlGeneration, answer.Proof) {
		t.Errorf("proof %q verifies for another root's token", answer.Proof)
	}
}

// ctlNoEcho fails if the received bytes repeat the token or the client's nonce.
func ctlNoEcho(t *testing.T, received, nonce string) {
	t.Helper()
	for what, secret := range map[string]string{"control token": ctlToken, "nonce": nonce} {
		if secret != "" && strings.Contains(strings.ToLower(received), secret) {
			t.Errorf("the response repeats the %s: %q", what, received)
		}
	}
}

func TestControlIdentityProof(t *testing.T) {
	f := ctlServe(t, nil)
	c := ctlDial(t, f.port)
	nonce := ctlNonce(t)
	resp, body := c.exchange(t, ctlIdentityRequest(f.port, nonce))
	ctlCheckProof(t, resp, body, nonce)
	ctlNoEcho(t, c.received.String(), nonce)
	if diff := gocmp.Diff([]ctlSigned{{ctlToken, nonce, ctlGeneration}}, f.signatures()); diff != "" {
		t.Errorf("signer calls (-want +got):\n%s", diff)
	}
	// The same connection stays usable, and a fresh nonce gets a fresh proof.
	second := ctlNonce(t)
	resp, body = c.exchange(t, ctlIdentityRequest(f.port, second))
	ctlCheckProof(t, resp, body, second)
	if n := f.relays.Load(); n != 0 {
		t.Errorf("identity requests reached the relay %d times", n)
	}
	if n := f.stops.Load(); n != 0 {
		t.Errorf("identity requests invoked Stop %d times", n)
	}
}

// TestControlIdentitySignsOnlyTheStartupGeneration decides that nothing the client sends chooses what is signed:
// a generation in the query or in a header is ignored, and the nonce never comes from the query.
func TestControlIdentitySignsOnlyTheStartupGeneration(t *testing.T) {
	f := ctlServe(t, nil)
	nonce := ctlNonce(t)
	tests := map[string]struct {
		raw        string
		wantStatus int
	}{
		"success: a query string is not part of the path": {
			raw:        ctlHead("GET /internal/identity?generation=client-chosen HTTP/1.1", ctlHost(f.port), ctlNonceName+": "+nonce, "X-Model-Gateway-Generation: client-chosen"),
			wantStatus: http.StatusOK,
		},
		"success: an empty entity announced by length": {
			raw:        ctlHead("GET /internal/identity HTTP/1.1", ctlHost(f.port), ctlNonceName+": "+nonce, "Content-Length: 0"),
			wantStatus: http.StatusOK,
		},
		"success: an empty chunked entity": {
			raw:        ctlHead("GET /internal/identity HTTP/1.1", ctlHost(f.port), ctlNonceName+": "+nonce, "Transfer-Encoding: chunked") + "0\r\n\r\n",
			wantStatus: http.StatusOK,
		},
		"error: a nonce in the query does not count": {
			raw:        ctlHead("GET /internal/identity?nonce="+nonce+" HTTP/1.1", ctlHost(f.port)),
			wantStatus: http.StatusBadRequest,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := ctlDial(t, f.port)
			resp, body := c.exchange(t, tt.raw)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status %d, body %q; want %d", resp.StatusCode, body, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusOK {
				ctlCheckProof(t, resp, body, nonce)
			}
		})
	}
	for _, s := range f.signatures() {
		if s.Generation != ctlGeneration {
			t.Errorf("the signer was asked to sign generation %q; only the startup generation may be signed", s.Generation)
		}
	}
	if n := f.relays.Load(); n != 0 {
		t.Errorf("identity requests reached the relay %d times", n)
	}
}

func TestControlIdentityRefusals(t *testing.T) {
	f := ctlServe(t, nil)
	nonce := ctlNonce(t)
	host := ctlHost(f.port)
	challenge := ctlNonceName + ": " + nonce
	tests := map[string]struct {
		raw        string
		wantStatus int
	}{
		"error: the name localhost is not the control Host": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", "Host: localhost:"+strconv.Itoa(f.port), challenge), wantStatus: http.StatusForbidden,
		},
		"error: another port in the Host": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", "Host: 127.0.0.1:"+strconv.Itoa(f.port+1), challenge), wantStatus: http.StatusForbidden,
		},
		"error: an Origin": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, "Origin: http://127.0.0.1:"+strconv.Itoa(f.port), challenge), wantStatus: http.StatusForbidden,
		},
		"error: an empty Origin": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, "Origin:", challenge), wantStatus: http.StatusForbidden,
		},
		"error: a control token offered to the proof": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, challenge, ctlTokenName+": "+ctlToken), wantStatus: http.StatusForbidden,
		},
		"error: an empty control token header offered to the proof": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, challenge, ctlTokenName+":"), wantStatus: http.StatusForbidden,
		},
		"error: POST is not the proof's method": {
			raw: ctlHead("POST /internal/identity HTTP/1.1", host, challenge, "Content-Length: 0"), wantStatus: http.StatusForbidden,
		},
		"error: HEAD is not the proof's method": {
			raw: ctlHead("HEAD /internal/identity HTTP/1.1", host, challenge), wantStatus: http.StatusForbidden,
		},
		"error: a forbidden Origin is refused before a malformed challenge": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, "Origin: http://example.test"), wantStatus: http.StatusForbidden,
		},
		"error: no challenge": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host), wantStatus: http.StatusBadRequest,
		},
		"error: two challenges": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, challenge, ctlNonceName+": "+ctlNonce(t)), wantStatus: http.StatusBadRequest,
		},
		"error: a short challenge": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, ctlNonceName+": "+nonce[:62]), wantStatus: http.StatusBadRequest,
		},
		"error: an uppercase challenge": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, ctlNonceName+": "+strings.ToUpper(nonce)), wantStatus: http.StatusBadRequest,
		},
		"error: an empty challenge": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, ctlNonceName+":"), wantStatus: http.StatusBadRequest,
		},
		"error: a request body": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, challenge, "Content-Length: 2") + "{}", wantStatus: http.StatusBadRequest,
		},
		"error: a chunked request body": {
			raw: ctlHead("GET /internal/identity HTTP/1.1", host, challenge, "Transfer-Encoding: chunked") + "2\r\n{}\r\n0\r\n\r\n", wantStatus: http.StatusBadRequest,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := ctlDial(t, f.port)
			resp, body := c.exchange(t, tt.raw)
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status %d; want %d", resp.StatusCode, tt.wantStatus)
			}
			if len(body) != 0 {
				t.Errorf("refusal body %q; want empty", body)
			}
			ctlNoEcho(t, c.received.String(), nonce)
		})
	}
	if diff := gocmp.Diff([]ctlSigned(nil), f.signatures()); diff != "" {
		t.Errorf("refused requests reached the signer (-want +got):\n%s", diff)
	}
	if n := f.relays.Load(); n != 0 {
		t.Errorf("refused identity requests reached the relay %d times", n)
	}
	if n := f.stops.Load(); n != 0 {
		t.Errorf("refused identity requests invoked Stop %d times", n)
	}
}

func TestControlIdentitySignerFailure(t *testing.T) {
	f := ctlServe(t, func(_ *ctlFixture, o *supervisor.Options) {
		o.SignIdentity = func(string, string, string) (string, error) {
			return "0000000000000000000000000000000000000000000000000000000000000000", errors.New("signer unavailable")
		}
	})
	c := ctlDial(t, f.port)
	nonce := ctlNonce(t)
	resp, body := c.exchange(t, ctlIdentityRequest(f.port, nonce))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status %d; want 500", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Errorf("a failed signer produced body %q; want empty, never a partial proof", body)
	}
	if strings.Contains(c.received.String(), "signer unavailable") {
		t.Errorf("the response repeats the signer's error: %q", c.received.String())
	}
	if n := f.relays.Load(); n != 0 {
		t.Errorf("the request reached the relay %d times", n)
	}
}

// TestControlOtherTrafficIsRelayed is the positive control for the counts above: an ordinary request reaches the
// relay, so a zero count for the control paths means they were dispatched before it.
func TestControlOtherTrafficIsRelayed(t *testing.T) {
	f := ctlServe(t, nil)
	c := ctlDial(t, f.port)
	resp, body := c.exchange(t, ctlHead("GET /v1/models HTTP/1.1", ctlHost(f.port)))
	if resp.StatusCode != http.StatusOK || string(body) != "relayed" {
		t.Errorf("ordinary request got %d %q; want the relay's answer", resp.StatusCode, body)
	}
	if n := f.relays.Load(); n != 1 {
		t.Errorf("relay reached %d times; want 1", n)
	}
}

func TestNewGatewayRefusesIncompleteOptions(t *testing.T) {
	var f ctlFixture
	tests := map[string]func(*supervisor.Options){
		"error: no signer":                      func(o *supervisor.Options) { o.SignIdentity = nil },
		"error: no token":                       func(o *supervisor.Options) { o.Token = "" },
		"error: a short token":                  func(o *supervisor.Options) { o.Token = ctlToken[:62] },
		"error: an uppercase token":             func(o *supervisor.Options) { o.Token = strings.ToUpper(ctlToken) },
		"error: no startup generation":          func(o *supervisor.Options) { o.StartupGeneration = "" },
		"error: a startup generation not UTF-8": func(o *supervisor.Options) { o.StartupGeneration = "gen-\xff" },
		"error: no stop callback":               func(o *supervisor.Options) { o.Stop = nil },
		"error: port zero":                      func(o *supervisor.Options) { o.PublicPort = 0 },
		"error: a negative port":                func(o *supervisor.Options) { o.PublicPort = -1 },
		"error: a port above the TCP range":     func(o *supervisor.Options) { o.PublicPort = 65536 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			o := ctlOptions(&f, 18764)
			mutate(&o)
			handler, err := ctlNewGateway(t, o)
			if err == nil || handler != nil {
				t.Errorf("NewGateway = %v, %v; want no handler and an error", handler, err)
			}
			if err != nil && strings.Contains(err.Error(), ctlToken[:16]) {
				t.Errorf("the error repeats the token: %v", err)
			}
		})
	}
	t.Run("success: complete options give a dispatcher", func(t *testing.T) {
		handler, err := ctlNewGateway(t, ctlOptions(&f, 18764))
		if err != nil || handler == nil {
			t.Errorf("NewGateway = %v, %v; want a dispatcher", handler, err)
		}
	})
	if f.stops.Load() != 0 || f.relays.Load() != 0 {
		t.Error("constructing a dispatcher invoked a callback")
	}
}
