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

//go:build process

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// foreignHost is a Host value the public listener must never relay.
const foreignHost = "outside.invalid"

// verdict is what a request's own Host, and nothing else, permits.
type verdict int

const (
	// mustRelay: the Host is allowed and the framing is unambiguous, so the request reaches the worker. The connection
	// may already be closed when an earlier request was refused or was allowed to end the connection.
	mustRelay verdict = iota + 1
	// mustRefuse: the Host is not allowed or the request is incomplete; it never reaches the worker and is never
	// answered with a success status.
	mustRefuse
	// relayOrClose: the Host is allowed but a server may be unable to prove which Host belongs to the request. It is
	// relayed, or refused with the connection closed afterwards, or the connection is closed.
	relayOrClose
	// mayRelay: the Host is allowed for a request the listener may decline to relay (CONNECT, an upgrade, a request
	// still in flight when the peer half-closes). Any relay, refusal or close is acceptable; only another request's
	// answer or a local success is not.
	mayRelay
	// abandoned: the peer closed before the request was complete. It never reaches the worker; how the server
	// answers a request nobody finished is not a Host decision.
	abandoned
)

func (v verdict) String() string {
	switch v {
	case mustRelay:
		return "must relay"
	case mustRefuse:
		return "must refuse"
	case relayOrClose:
		return "relay or close"
	case mayRelay:
		return "may relay"
	case abandoned:
		return "abandoned"
	}
	return "unknown"
}

// wireRequest is one request exactly as the client writes it. In head and body, {own} is the listener's own
// authority, {foreign} a foreign Host, {probe} this request's marker, {nonce} the case's marker prefix and {len} the
// byte length of the rendered body.
type wireRequest struct {
	head string
	body string
	// chunk, when set, replaces body with one chunk carrying this text followed by the last chunk.
	chunk string
	// expect100 makes a sequential client wait for the interim 100 response before it writes the body.
	expect100 bool
	want      verdict
	// mayClose allows the server to close the connection after this request even if it was relayed.
	mayClose bool
}

// outcome is what the client observed for one request on the connection.
type outcome string

const (
	outRelayed   outcome = "relayed"
	outRefused   outcome = "refused"
	outAnswered  outcome = "answered locally with success"
	outMisrouted outcome = "answered with another request's relay"
	outClosed    outcome = "connection closed"
)

// recordingWorker is the loopback worker fixture. It records the X-Probe marker of every request the supervisor
// relays and echoes it in its answer, so the client can tell which request a relayed answer belongs to. The general
// OPTIONS handler is disabled so that a relayed OPTIONS * reaches the recorder.
type recordingWorker struct {
	port   int
	mu     sync.Mutex
	probes []string
}

func (w *recordingWorker) seen(nonce string) map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int)
	for _, p := range w.probes {
		if strings.HasPrefix(p, nonce) {
			out[p]++
		}
	}
	return out
}

func startRecordingWorker(t *testing.T, env gatewaytest.Environment) *recordingWorker {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := env.Listen(t.Context(), "tcp4", "127.0.0.1:0", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingWorker{port: ln.Addr().(*net.TCPAddr).Port}
	srv := &http.Server{
		DisableGeneralOptionsHandler: true,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			probe := r.Header.Get("X-Probe")
			rec.mu.Lock()
			rec.probes = append(rec.probes, probe)
			rec.mu.Unlock()
			w.Header().Set("X-Probe-Echo", probe)
			// Not a 2xx status: a 2xx answer to CONNECT or an upgrade would turn the connection into a tunnel.
			w.WriteHeader(http.StatusTeapot)
		}),
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(errors.Join(err, srv.Close()))
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return rec
}

// cappedListener hands the public listener connections whose reads return at most limit bytes, so a request head
// reaches the server in many reads whatever the kernel would have coalesced. A zero limit leaves reads unchanged.
type cappedListener struct {
	net.Listener
	limit int
}

func (l cappedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil || l.limit == 0 {
		return conn, err
	}
	return cappedConn{Conn: conn, limit: l.limit}, nil
}

type cappedConn struct {
	net.Conn
	limit int
}

func (c cappedConn) Read(b []byte) (int, error) {
	if len(b) > c.limit {
		b = b[:c.limit]
	}
	return c.Conn.Read(b)
}

// CloseWrite keeps the half-close net/http uses before closing a connection that still has unread input.
func (c cappedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// TestHostAssociationPerRequest writes several requests on one connection to the public listener and checks that
// each request is allowed or refused by the Host header that same request carried. A request whose own Host is not
// allowed never reaches the worker; where the server cannot prove which Host belongs to a request, it refuses the
// request and closes the connection rather than judge it by another request's Host.
func TestHostAssociationPerRequest(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	worker := startRecordingWorker(t, env)

	var pad strings.Builder
	for i := range 240 {
		fmt.Fprintf(&pad, "X-Pad-%03d: %s\r\n", i, strings.Repeat("p", 3986))
	}
	// About 960 KB: below the server's header limit, so net/http parses it as one request.
	largeHead := "GET /large HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\n" + pad.String() + "\r\n"

	ownPost := func(path string) wireRequest {
		return wireRequest{head: "POST " + path + " HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nContent-Length: {len}\r\n\r\n", body: "{}", want: mustRelay}
	}
	foreignPost := func(path string) wireRequest {
		return wireRequest{head: "POST " + path + " HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\nContent-Length: {len}\r\n\r\n", body: "{}", want: mustRefuse}
	}
	ownGet := wireRequest{head: "GET /own HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\n\r\n", want: mustRelay}
	foreignGet := wireRequest{head: "GET /foreign HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\n\r\n", want: mustRefuse}

	tests := map[string]struct {
		requests  []wireRequest
		pipelined bool
		// halfClose closes the client's write side after the last request; no closing request is appended.
		halfClose bool
		// readCaps lists the per-read byte limits to run the case with; nil means whole reads and single bytes.
		readCaps []int
	}{
		"success: keep-alive requests judged one by one": {
			// Two own requests in a row: closing after a relay is not a refusal.
			requests: []wireRequest{ownPost("/a"), ownGet, foreignPost("/b"), ownGet},
		},
		"success: pipelined requests judged one by one": {
			requests:  []wireRequest{foreignPost("/a"), ownPost("/b"), ownGet, foreignGet, ownGet},
			pipelined: true,
		},
		"success: refused request leaves a small body unread": {
			requests: []wireRequest{
				{head: "POST /unread HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\nContent-Length: {len}\r\n\r\n", body: strings.Repeat("u", 1024), want: mustRefuse},
				ownPost("/after"),
			},
			pipelined: true,
		},
		"success: refused request leaves a body larger than the server discards unread": {
			requests: []wireRequest{
				{head: "POST /unread HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\nContent-Length: {len}\r\n\r\n", body: strings.Repeat("u", 300<<10), want: mustRefuse},
				ownPost("/after"),
			},
			pipelined: true,
			readCaps:  []int{0},
		},
		"error: oversized body is read only in part and never relayed": {
			requests: []wireRequest{
				{head: "POST /oversized HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nContent-Length: {len}\r\n\r\n", body: strings.Repeat("o", 64<<20+1024), want: mustRefuse},
				foreignPost("/after"),
			},
			pipelined: true,
			readCaps:  []int{0},
		},
		"success: 100-continue answered before the body is sent": {
			requests: []wireRequest{
				{head: "POST /continue HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nExpect: 100-continue\r\nContent-Length: {len}\r\n\r\n", body: "{}", expect100: true, want: mustRelay},
				{head: "POST /continue HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\nExpect: 100-continue\r\nContent-Length: {len}\r\n\r\n", body: "{}", expect100: true, want: mustRefuse},
				ownGet,
			},
		},
		"success: 100-continue body sent without waiting": {
			requests: []wireRequest{
				{head: "POST /continue HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nExpect: 100-continue\r\nContent-Length: {len}\r\n\r\n", body: "{}", want: mustRelay},
				foreignPost("/b"),
				ownGet,
			},
			pipelined: true,
		},
		"success: chunked bodies with extensions and trailers": {
			requests: []wireRequest{
				{head: "POST /chunked HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nTrailer: X-Trailer\r\nTransfer-Encoding: chunked\r\n\r\n", body: "2;ext=1\r\n{}\r\n0\r\nX-Trailer: done\r\n\r\n", want: mustRelay},
				{head: "POST /chunked HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\nTransfer-Encoding: chunked\r\n\r\n", body: "3\r\nabc\r\n0\r\n\r\n", want: mustRefuse},
				ownGet,
			},
			pipelined: true,
		},
		"error: chunk data shaped like a request head stays body": {
			requests: []wireRequest{
				{head: "POST /chunked HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\nTransfer-Encoding: chunked\r\n\r\n", chunk: "GET /inner HTTP/1.1\r\nHost: {own}\r\nX-Probe: {nonce}-inner\r\n\r\n", want: mustRefuse},
				foreignGet,
			},
			pipelined: true,
		},
		"error: upgrade attempts judged by their own Host": {
			requests: []wireRequest{
				{head: "GET /upgrade HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", want: mustRefuse, mayClose: true},
				{head: "GET /upgrade HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", want: mayRelay, mayClose: true},
				foreignPost("/after"),
			},
		},
		"error: CONNECT attempts judged by their own Host": {
			requests: []wireRequest{
				{head: "CONNECT {own} HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\n\r\n", want: mustRefuse, mayClose: true},
				{head: "CONNECT {own} HTTP/1.1\r\nX-Probe: {probe}\r\n\r\n", want: mustRefuse, mayClose: true},
				{head: "CONNECT outside.invalid:443 HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\n\r\n", want: mayRelay, mayClose: true},
				foreignGet,
			},
		},
		"success: HTTP/1.0 keep-alive requests judged one by one": {
			requests: []wireRequest{
				{head: "POST /a HTTP/1.0\r\nHost: {own}\r\nConnection: keep-alive\r\nX-Probe: {probe}\r\nContent-Length: {len}\r\n\r\n", body: "{}", want: mustRelay},
				{head: "POST /b HTTP/1.0\r\nHost: {foreign}\r\nConnection: keep-alive\r\nX-Probe: {probe}\r\nContent-Length: {len}\r\n\r\n", body: "{}", want: mustRefuse},
				ownGet,
			},
			pipelined: true,
		},
		// net/http ignores Transfer-Encoding on an HTTP/1.0 request and frames its body by Content-Length. A reader
		// that frames it as chunked takes the rest of the body for a request with the own Host.
		"error: foreign request after an HTTP/1.0 body whose Transfer-Encoding is ignored": {
			requests: []wireRequest{
				{
					head:     "POST /a HTTP/1.0\r\nHost: {own}\r\nConnection: keep-alive\r\nX-Probe: {probe}\r\nTransfer-Encoding: chunked\r\nContent-Length: {len}\r\n\r\n",
					body:     "0\r\n\r\nGET /inner HTTP/1.1\r\nHost: {own}\r\nX-Probe: {nonce}-inner\r\n\r\n",
					want:     relayOrClose,
					mayClose: true,
				},
				foreignPost("/b"),
			},
			pipelined: true,
		},
		// Without a Content-Length net/http gives this HTTP/1.0 request an empty body; a reader that frames it as
		// chunked reads the next request head as a chunk size and loses track of every later request.
		"error: own request after an HTTP/1.0 request whose only framing is an ignored Transfer-Encoding": {
			requests: []wireRequest{
				{head: "POST /a HTTP/1.0\r\nHost: {own}\r\nConnection: keep-alive\r\nX-Probe: {probe}\r\nTransfer-Encoding: chunked\r\n\r\n", want: relayOrClose, mayClose: true},
				foreignGet,
				ownGet,
			},
			pipelined: true,
		},
		// net/http joins an obsolete folded line to the field above it.
		"error: own request whose Host value is folded": {
			requests: []wireRequest{
				{head: "GET /folded HTTP/1.1\r\nHost:\r\n {own}\r\nX-Probe: {probe}\r\n\r\n", want: relayOrClose, mayClose: true},
				ownGet,
				foreignGet,
			},
			pipelined: true,
		},
		"error: own request after a folded Content-Length": {
			requests: []wireRequest{
				{head: "POST /folded HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nContent-Length:\r\n {len}\r\n\r\n", body: "{}", want: relayOrClose, mayClose: true},
				ownGet,
				foreignGet,
			},
			pipelined: true,
		},
		"success: folded line that makes the own Host invalid": {
			requests: []wireRequest{
				{head: "GET /folded HTTP/1.1\r\nHost: {own}\r\n x\r\nX-Probe: {probe}\r\n\r\n", want: mustRefuse},
				ownGet,
			},
			pipelined: true,
		},
		// net/http answers OPTIONS * itself unless told otherwise; that answer must not leave the request's Host for
		// the next request to use.
		"error: foreign request after OPTIONS * with the own Host": {
			requests: []wireRequest{
				{head: "OPTIONS * HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\n\r\n", want: relayOrClose, mayClose: true},
				foreignPost("/b"),
			},
			pipelined: true,
		},
		"error: foreign request after OPTIONS * with the own Host on a keep-alive connection": {
			requests: []wireRequest{
				{head: "OPTIONS * HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\n\r\n", want: relayOrClose, mayClose: true},
				foreignPost("/b"),
			},
		},
		"error: own request after OPTIONS * with a foreign Host": {
			requests: []wireRequest{
				{head: "OPTIONS * HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\n\r\n", want: mustRefuse},
				ownPost("/b"),
			},
			pipelined: true,
		},
		"error: own request with a large head arriving in many reads": {
			requests: []wireRequest{
				{head: largeHead, want: mustRelay},
				foreignGet,
			},
			pipelined: true,
			readCaps:  []int{7},
		},
		// net/http treats the peer's half-close as its departure and cancels requests still in flight, so an
		// earlier request with the own Host may be answered without a relay; it is never judged by another Host.
		"success: peer closes in the middle of the next head": {
			requests: []wireRequest{
				{head: "POST /a HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nContent-Length: {len}\r\n\r\n", body: "{}", want: mayRelay},
				{head: "POST /b HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nContent-Le", want: abandoned},
			},
			pipelined: true,
			halfClose: true,
		},
		"success: peer closes in the middle of a body": {
			requests: []wireRequest{
				foreignPost("/a"),
				{head: "POST /b HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nContent-Length: 100\r\n\r\n", body: "0123456789", want: abandoned},
			},
			pipelined: true,
			halfClose: true,
		},
		"error: peer closes in the middle of a body with a foreign Host": {
			requests: []wireRequest{
				{head: "POST /a HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nContent-Length: {len}\r\n\r\n", body: "{}", want: mayRelay},
				{head: "POST /b HTTP/1.1\r\nHost: {foreign}\r\nX-Probe: {probe}\r\nContent-Length: 100\r\n\r\n", body: "0123456789", want: mustRefuse},
			},
			pipelined: true,
			halfClose: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			caps := tc.readCaps
			if caps == nil {
				caps = []int{0, 1}
			}
			for _, readCap := range caps {
				label := "whole reads"
				if readCap > 0 {
					label = "reads of at most " + strconv.Itoa(readCap) + " bytes"
				}
				t.Run(label, func(t *testing.T) {
					requests := tc.requests
					if !tc.halfClose {
						// A closing request shows whether the connection stayed open after the last case request.
						requests = append(slices.Clip(requests), wireRequest{
							head: "GET /closing HTTP/1.1\r\nHost: {own}\r\nX-Probe: {probe}\r\nConnection: close\r\n\r\n", want: mustRelay,
						})
					}
					checkAssociation(t, worker, requests, tc.pipelined, tc.halfClose, readCap)
				})
			}
		})
	}
}

func checkAssociation(t *testing.T, worker *recordingWorker, requests []wireRequest, pipelined, halfClose bool, readCap int) {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	own := ln.Addr().String()
	h, err := supervisor.NewHandler(ln.Addr().(*net.TCPAddr).Port, func() int { return worker.port }, strings.Repeat("9f", 32), "1.2.3", 29)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- supervisor.Serve(ctx, cappedListener{Listener: ln, limit: readCap}, h) }()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("public listener: %v", err)
		}
		_ = ln.Close()
	}()

	nonce := rand.Text()
	probes := make([]string, len(requests))
	heads := make([]string, len(requests))
	bodies := make([]string, len(requests))
	for i, r := range requests {
		probes[i] = nonce + "-" + strconv.Itoa(i)
		fill := strings.NewReplacer("{own}", own, "{foreign}", foreignHost, "{nonce}", nonce)
		body := fill.Replace(r.body)
		if r.chunk != "" {
			chunk := fill.Replace(r.chunk)
			body = fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(chunk), chunk)
		}
		heads[i] = strings.NewReplacer("{own}", own, "{foreign}", foreignHost, "{probe}", probes[i], "{nonce}", nonce, "{len}", strconv.Itoa(len(body))).Replace(r.head)
		bodies[i] = body
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp4", own)
	if err != nil {
		t.Fatal(err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("dialed %T, want a TCP connection", conn)
	}
	// A hang guard for a server that neither answers nor closes, not a pacing delay.
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	outcomes := make([]outcome, len(requests))
	statuses := make([]int, len(requests))
	written := make(chan error, 1)
	if pipelined {
		go func() {
			var all strings.Builder
			for i := range requests {
				all.WriteString(heads[i])
				all.WriteString(bodies[i])
			}
			_, err := io.WriteString(tcp, all.String())
			if err == nil && halfClose {
				err = tcp.CloseWrite()
			}
			written <- err
		}()
		closed := false
		for i := range requests {
			if closed {
				outcomes[i] = outClosed
				continue
			}
			outcomes[i], statuses[i] = readOutcome(br, requests[i].head, probes[i])
			closed = outcomes[i] == outClosed
		}
	} else {
		written <- nil
		closed := false
		for i, r := range requests {
			if !closed {
				outcomes[i], statuses[i], closed = sequentialExchange(tcp, br, r, heads[i], bodies[i], probes[i])
			} else {
				outcomes[i] = outClosed
			}
		}
	}
	_ = conn.Close()
	// A write that fails because the server closed the connection is an expected result, not a test failure.
	<-written

	seen := worker.seen(nonce)
	var faults []string
	report := make([]string, len(requests))
	for i, r := range requests {
		n := seen[probes[i]]
		delete(seen, probes[i])
		report[i] = fmt.Sprintf("request %d (%s): %s, status %d, worker received it %d times", i, r.want, outcomes[i], statuses[i], n)
		fault := judge(requests, outcomes, i, n)
		if fault != "" {
			faults = append(faults, fmt.Sprintf("request %d: %s", i, fault))
		}
	}
	for probe, n := range seen {
		faults = append(faults, fmt.Sprintf("worker received %d request(s) marked %q that the client never sent as a request", n, strings.TrimPrefix(probe, nonce)))
	}
	if diff := gocmp.Diff([]string(nil), faults); diff != "" {
		t.Errorf("Host association faults (-want +got):\n%s\nobserved on the connection:\n  %s", diff, strings.Join(report, "\n  "))
	}
}

// sequentialExchange writes one request and reads its answer before the next request is written.
func sequentialExchange(conn *net.TCPConn, br *bufio.Reader, r wireRequest, head, body, probe string) (outcome, int, bool) {
	if _, err := io.WriteString(conn, head); err != nil {
		return outClosed, 0, true
	}
	if r.expect100 {
		resp, err := http.ReadResponse(br, &http.Request{Method: method(head)})
		if err != nil {
			return outClosed, 0, true
		}
		if resp.StatusCode != http.StatusContinue {
			// The server answered without asking for the body; the body is not sent.
			o := classify(resp, probe)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return o, resp.StatusCode, false
		}
	}
	if _, err := io.WriteString(conn, body); err != nil {
		return outClosed, 0, true
	}
	o, status := readOutcome(br, head, probe)
	return o, status, o == outClosed
}

// readOutcome reads the final answer to one request, skipping interim 100 responses.
func readOutcome(br *bufio.Reader, head, probe string) (outcome, int) {
	for {
		resp, err := http.ReadResponse(br, &http.Request{Method: method(head)})
		if err != nil {
			return outClosed, 0
		}
		if resp.StatusCode == http.StatusContinue {
			continue
		}
		o := classify(resp, probe)
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			_ = resp.Body.Close()
			return outClosed, resp.StatusCode
		}
		_ = resp.Body.Close()
		return o, resp.StatusCode
	}
}

func method(head string) string {
	m, _, _ := strings.Cut(head, " ")
	return m
}

func classify(resp *http.Response, probe string) outcome {
	switch echo := resp.Header.Get("X-Probe-Echo"); {
	case echo == probe:
		return outRelayed
	case echo != "":
		return outMisrouted
	case resp.StatusCode >= http.StatusBadRequest:
		return outRefused
	default:
		return outAnswered
	}
}

// judge returns why the observed outcome of request i violates its own Host's verdict, or "" when it does not.
func judge(requests []wireRequest, outcomes []outcome, i, received int) string {
	o, want := outcomes[i], requests[i].want
	switch {
	case received > 1:
		return fmt.Sprintf("the worker received it %d times", received)
	case o == outMisrouted:
		return "the client received the worker's answer to a different request"
	case o == outAnswered && want != abandoned:
		return "answered with a success status without being relayed"
	case o == outRelayed && received == 0:
		return "answered with its own marker although the worker never recorded it"
	}
	closedAfterPrevious := i > 0 && (outcomes[i-1] == outRefused || outcomes[i-1] == outClosed || requests[i-1].mayClose)
	nextClosed := i+1 < len(outcomes) && outcomes[i+1] == outClosed
	switch want {
	case mustRefuse, abandoned:
		if received > 0 {
			return "relayed to the worker although its own Host does not allow it"
		}
	case mustRelay:
		if o == outRelayed || (o == outClosed && closedAfterPrevious) {
			return ""
		}
		return fmt.Sprintf("not relayed although its own Host allows it (%s)", o)
	case relayOrClose:
		if o == outRefused && !nextClosed {
			return "refused although its own Host allows it, and the connection stayed open for the next request"
		}
	case mayRelay:
	}
	return ""
}
