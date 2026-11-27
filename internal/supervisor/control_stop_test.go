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
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"
)

// ctlStopBodyLimit is the largest stop entity the supervisor reads.
const ctlStopBodyLimit = 4096

// ctlStopRequest is an authenticated stop with the given entity: exact numeric Host, no Origin, this root's token
// and an exact JSON content type.
func ctlStopRequest(port int, body string, extra ...string) string {
	headers := append([]string{ctlHost(port), ctlTokenName + ": " + ctlToken, "Content-Type: application/json", "Content-Length: " + strconv.Itoa(len(body))}, extra...)
	return ctlHead("POST /internal/stop HTTP/1.1", headers...) + body
}

// ctlCheckStopped decides the acknowledgement of an accepted stop.
func ctlCheckStopped(t *testing.T, resp ctlAnswer, body []byte) {
	t.Helper()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop status %d, body %q; want 202", resp.StatusCode, body)
	}
	if diff := gocmp.Diff([]string{"application/json"}, resp.Header.Values("Content-Type")); diff != "" {
		t.Errorf("Content-Type (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff([]string{"no-store"}, resp.Header.Values("Cache-Control")); diff != "" {
		t.Errorf("Cache-Control (-want +got):\n%s", diff)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("stop body %q is not one JSON object: %v", body, err)
	}
	if diff := gocmp.Diff(map[string]any{"ok": true, "stopping": true}, got); diff != "" {
		t.Errorf("stop body (-want +got):\n%s", diff)
	}
}

// TestControlProofThenStopOnOneConnection decides the server half of the stop client's exchange: the proof is
// answered on a connection that stays open, and the token-bearing stop that follows on that same connection is
// acknowledged after the stop inhibition was set.
func TestControlProofThenStopOnOneConnection(t *testing.T) {
	f := ctlServe(t, nil)
	c := ctlDial(t, f.port)
	nonce := ctlNonce(t)
	resp, body := c.exchange(t, ctlIdentityRequest(f.port, nonce))
	ctlCheckProof(t, resp, body, nonce)
	proven := c.received.Len()
	resp, body = c.exchange(t, ctlStopRequest(f.port, "{}"))
	ctlCheckStopped(t, resp, body)
	if n := f.stops.Load(); n != 1 {
		t.Errorf("Stop invoked %d times; want 1", n)
	}
	ctlNoEcho(t, c.received.String()[:proven], nonce)
	ctlNoEcho(t, c.received.String(), "")
	if n := f.relays.Load(); n != 0 {
		t.Errorf("the exchange reached the relay %d times", n)
	}
}

// ctlChunked frames body as a chunked entity of at most size bytes per chunk.
func ctlChunked(body string, size int) string {
	var b strings.Builder
	for len(body) > 0 {
		n := min(size, len(body))
		fmt.Fprintf(&b, "%x\r\n%s\r\n", n, body[:n])
		body = body[n:]
	}
	return b.String() + "0\r\n\r\n"
}

// ctlChunkedStop is an authenticated stop whose entity is chunked, so its length is not known from the head.
func ctlChunkedStop(port int, body string) string {
	return ctlHead("POST /internal/stop HTTP/1.1", ctlHost(port), ctlTokenName+": "+ctlToken, "Content-Type: application/json", "Transfer-Encoding: chunked") + ctlChunked(body, 1000)
}

// TestControlStopAccepted decides the accepted stops. Each case runs against a supervisor of its own, so that the
// inhibition observed at its acknowledgment is the one that acknowledgment set: Stop has been called by the time
// the 202 arrives, every time.
func TestControlStopAccepted(t *testing.T) {
	padded := "{" + strings.Repeat(" ", ctlStopBodyLimit-2) + "}"
	tests := map[string]struct {
		raw   func(port int) string
		times int
	}{
		"success: an empty object":                       {raw: func(p int) string { return ctlStopRequest(p, "{}") }},
		"success: an empty object with JSON white space": {raw: func(p int) string { return ctlStopRequest(p, " \t\r\n{ \n}\r\n ") }},
		"success: an empty object padded to the limit":   {raw: func(p int) string { return ctlStopRequest(p, padded) }},
		"success: a JSON content type followed by another one": {raw: func(p int) string {
			return ctlHead("POST /internal/stop HTTP/1.1", ctlHost(p), ctlTokenName+": "+ctlToken,
				"Content-Type: application/json", "Content-Type: text/plain", "Content-Length: 2") + "{}"
		}},
		"success: a chunked empty object":              {raw: func(p int) string { return ctlChunkedStop(p, "{}") }},
		"success: a chunked empty object at the limit": {raw: func(p int) string { return ctlChunkedStop(p, padded) }},
		"success: repeated stops on fresh connections while the supervisor serves": {
			raw: func(p int) string { return ctlStopRequest(p, "{}") }, times: 3,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := ctlServe(t, nil)
			for i := range max(tt.times, 1) {
				c := ctlDial(t, f.port)
				resp, got := c.exchange(t, tt.raw(f.port))
				ctlCheckStopped(t, resp, got)
				if n := f.stops.Load(); n < 1 {
					t.Errorf("acknowledgment %d arrived before any stop inhibition was set", i+1)
				}
			}
			if n := f.relays.Load(); n != 0 {
				t.Errorf("stop requests reached the relay %d times", n)
			}
		})
	}
}

func TestControlStopRefusals(t *testing.T) {
	f := ctlServe(t, nil)
	port := f.port
	host := ctlHost(port)
	token := ctlTokenName + ": " + ctlToken
	jsonType := "Content-Type: application/json"
	post := func(headers ...string) string {
		return ctlHead("POST /internal/stop HTTP/1.1", append(headers, "Content-Length: 2")...) + "{}"
	}
	tests := map[string]struct {
		raw        string
		wantStatus int
	}{
		"error: GET is not the stop's method":          {raw: ctlHead("GET /internal/stop HTTP/1.1", host, token, jsonType), wantStatus: http.StatusForbidden},
		"error: PUT is not the stop's method":          {raw: ctlHead("PUT /internal/stop HTTP/1.1", host, token, jsonType, "Content-Length: 2") + "{}", wantStatus: http.StatusForbidden},
		"error: the name localhost":                    {raw: post("Host: localhost:"+strconv.Itoa(port), token, jsonType), wantStatus: http.StatusForbidden},
		"error: another port in the Host":              {raw: post("Host: 127.0.0.1:"+strconv.Itoa(port+1), token, jsonType), wantStatus: http.StatusForbidden},
		"error: an Origin":                             {raw: post(host, "Origin: http://127.0.0.1:"+strconv.Itoa(port), token, jsonType), wantStatus: http.StatusForbidden},
		"error: an empty Origin":                       {raw: post(host, "Origin:", token, jsonType), wantStatus: http.StatusForbidden},
		"error: no token":                              {raw: post(host, jsonType), wantStatus: http.StatusForbidden},
		"error: another root's token":                  {raw: post(host, ctlTokenName+": "+ctlOtherToken, jsonType), wantStatus: http.StatusForbidden},
		"error: the token twice":                       {raw: post(host, token, token, jsonType), wantStatus: http.StatusForbidden},
		"error: the token as a bearer credential":      {raw: post(host, "Authorization: Bearer "+ctlToken, jsonType), wantStatus: http.StatusForbidden},
		"error: no content type":                       {raw: post(host, token), wantStatus: http.StatusForbidden},
		"error: a content type with parameters":        {raw: post(host, token, "Content-Type: application/json; charset=utf-8"), wantStatus: http.StatusForbidden},
		"error: a content type in another case":        {raw: post(host, token, "Content-Type: Application/JSON"), wantStatus: http.StatusForbidden},
		"error: a first content type that is not JSON": {raw: post(host, token, "Content-Type: text/plain", jsonType), wantStatus: http.StatusForbidden},
		"error: the token in the query":                {raw: ctlHead("POST /internal/stop?"+url.Values{"token": {ctlToken}}.Encode()+" HTTP/1.1", host, jsonType, "Content-Length: 2") + "{}", wantStatus: http.StatusForbidden},
		"error: an authenticated empty entity":         {raw: ctlStopRequest(port, ""), wantStatus: http.StatusBadRequest},
		"error: an authenticated array":                {raw: ctlStopRequest(port, "[]"), wantStatus: http.StatusBadRequest},
		"error: an authenticated null":                 {raw: ctlStopRequest(port, "null"), wantStatus: http.StatusBadRequest},
		"error: an authenticated object with a member": {raw: ctlStopRequest(port, `{"pid":1}`), wantStatus: http.StatusBadRequest},
		"error: an authenticated object with a signal": {raw: ctlStopRequest(port, `{"signal":"SIGKILL"}`), wantStatus: http.StatusBadRequest},
		"error: two authenticated objects":             {raw: ctlStopRequest(port, "{}{}"), wantStatus: http.StatusBadRequest},
		"error: an authenticated trailing value":       {raw: ctlStopRequest(port, "{} 1"), wantStatus: http.StatusBadRequest},
		"error: an authenticated truncated object":     {raw: ctlStopRequest(port, "{"), wantStatus: http.StatusBadRequest},
		"error: an authenticated chunked entity over the limit": {
			raw: ctlChunkedStop(port, "{"+strings.Repeat(" ", ctlStopBodyLimit-1)+"}"), wantStatus: http.StatusRequestEntityTooLarge,
		},
		"error: an authenticated entity over the limit": {
			raw: ctlStopRequest(port, "{"+strings.Repeat(" ", ctlStopBodyLimit-1)+"}"), wantStatus: http.StatusRequestEntityTooLarge,
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
			ctlNoEcho(t, c.received.String(), "")
		})
	}
	if n := f.stops.Load(); n != 0 {
		t.Errorf("refused stops invoked Stop %d times", n)
	}
	if n := f.relays.Load(); n != 0 {
		t.Errorf("refused stops reached the relay %d times", n)
	}
	if diff := gocmp.Diff([]ctlSigned(nil), f.signatures()); diff != "" {
		t.Errorf("refused stops reached the signer (-want +got):\n%s", diff)
	}
}

// TestControlStopRefusedBeforeTheEntity decides that an unauthenticated stop is refused from its head alone: the
// refusal arrives while the announced entity has not been sent.
func TestControlStopRefusedBeforeTheEntity(t *testing.T) {
	f := ctlServe(t, nil)
	tests := map[string][]string{
		"error: another root's token": {ctlHost(f.port), ctlTokenName + ": " + ctlOtherToken, "Content-Type: application/json"},
		"error: an Origin":            {ctlHost(f.port), "Origin: http://example.test", ctlTokenName + ": " + ctlToken, "Content-Type: application/json"},
		"error: a wrong content type": {ctlHost(f.port), ctlTokenName + ": " + ctlToken, "Content-Type: text/plain"},
	}
	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			c := ctlDial(t, f.port)
			head := ctlHead("POST /internal/stop HTTP/1.1", append(headers, "Content-Length: 64")...)
			if _, err := io.WriteString(c.conn, head); err != nil {
				t.Fatal(err)
			}
			// The connection's deadline is only a watchdog: a correct server answers without the entity.
			resp, body := c.readFor(t, "POST")
			if resp.StatusCode != http.StatusForbidden || len(body) != 0 {
				t.Errorf("got %d %q before the entity was sent; want an empty 403", resp.StatusCode, body)
			}
		})
	}
	if n := f.stops.Load(); n != 0 {
		t.Errorf("refused stops invoked Stop %d times", n)
	}
}

// ctlRecorder is a response writer that records whether anything was written, under a lock, so that the test can
// look at it while the handler is parked inside Stop.
type ctlRecorder struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    strings.Builder
	written bool
}

func (r *ctlRecorder) Header() http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.header
}

func (r *ctlRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.written {
		r.status, r.written = status, true
	}
}

func (r *ctlRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.written {
		r.status, r.written = http.StatusOK, true
	}
	return r.body.Write(p)
}

func (r *ctlRecorder) touched() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.written || r.body.Len() > 0
}

// TestControlStopInhibitsBeforeAcknowledging decides that Stop has returned before any part of the acknowledgement
// is written: with Stop parked and every goroutine of the bubble quiescent, the response is untouched, and the
// acknowledgement follows once Stop returns. A deadlock of the bubble fails the test instead of hanging it.
func TestControlStopInhibitsBeforeAcknowledging(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var f ctlFixture
		release, entered := make(chan struct{}), make(chan struct{}, 1)
		f.stopHook = func() {
			entered <- struct{}{}
			<-release
		}
		const port = 18764
		handler, err := ctlNewGateway(t, ctlOptions(&f, port))
		if err != nil {
			t.Fatalf("NewGateway: %v", err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/internal/stop", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.RequestURI, req.Host, req.ContentLength = "/internal/stop", "127.0.0.1:"+strconv.Itoa(port), 2
		req.Header = http.Header{"Content-Type": {"application/json"}, ctlTokenName: {ctlToken}}
		rec := &ctlRecorder{header: http.Header{}}
		done := make(chan any, 1)
		go func() {
			defer func() { done <- recover() }()
			handler.ServeHTTP(rec, req)
		}()
		synctest.Wait()
		select {
		case <-entered:
		default:
			close(release)
			t.Fatalf("the handler did not invoke Stop (panic: %v); status %d", <-done, rec.status)
		}
		if rec.touched() {
			t.Errorf("the acknowledgement was started while Stop had not returned: status %d, body %q", rec.status, rec.body.String())
		}
		close(release)
		synctest.Wait()
		if p := <-done; p != nil {
			t.Fatalf("handler panicked: %v", p)
		}
		if rec.status != http.StatusAccepted {
			t.Errorf("status %d after Stop returned; want 202", rec.status)
		}
	})
}

// TestControlProofDoesNotAuthorizeTheStop decides that the proof grants nothing by itself: a stop without the token
// after a valid proof on the same connection is refused.
func TestControlProofDoesNotAuthorizeTheStop(t *testing.T) {
	f := ctlServe(t, nil)
	c := ctlDial(t, f.port)
	nonce := ctlNonce(t)
	resp, body := c.exchange(t, ctlIdentityRequest(f.port, nonce))
	ctlCheckProof(t, resp, body, nonce)
	resp, body = c.exchange(t, ctlHead("POST /internal/stop HTTP/1.1", ctlHost(f.port), "Content-Type: application/json", ctlNonceName+": "+nonce, "Content-Length: 2")+"{}")
	if resp.StatusCode != http.StatusForbidden || len(body) != 0 {
		t.Errorf("stop without token after a proof got %d %q; want an empty 403", resp.StatusCode, body)
	}
	if n := f.stops.Load(); n != 0 {
		t.Errorf("Stop invoked %d times", n)
	}
}
