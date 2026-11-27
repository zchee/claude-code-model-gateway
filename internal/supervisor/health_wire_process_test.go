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
	"bytes"
	"io"
	"net/http"
	"strconv"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// rawAnswer is one response as the public listener wrote it on the wire, read until the connection ended.
type rawAnswer struct {
	Status           int
	WorkerResponse   string
	ContentType      string
	ContentLength    []string
	TransferEncoding []string
	// Rest is every byte after the head's blank line: the body, and anything written after it.
	Rest string
}

// exchangeOnce writes one request that asks the public listener to close the connection after its answer, reads
// everything the listener sends until the connection ends, and splits it at the first blank line. The head is parsed
// for its status and fields; the bytes after it are returned as they arrived, whatever framing the head declares.
func exchangeOnce(t *testing.T, authority, method, target string) rawAnswer {
	t.Helper()
	conn, br := dialPublic(t, authority)
	if _, err := io.WriteString(conn, method+" "+target+" HTTP/1.1\r\nHost: "+authority+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read the answer until the connection ended: %v (read %q)", err, wire)
	}
	head, rest, ok := bytes.Cut(wire, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("the connection ended without a complete response head: %q", wire)
	}
	// The head alone is parsed, as a response to HEAD, so that the parser takes no body bytes from it.
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(append(head, "\r\n\r\n"...))), &http.Request{Method: http.MethodHead})
	if err != nil {
		t.Fatalf("parse the response head %q: %v", head, err)
	}
	_ = resp.Body.Close()
	return rawAnswer{
		Status: resp.StatusCode, WorkerResponse: resp.Header.Get("X-Worker-Response"), ContentType: resp.Header.Get("Content-Type"),
		ContentLength: resp.Header.Values("Content-Length"), TransferEncoding: resp.Header.Values("Transfer-Encoding"), Rest: string(rest),
	}
}

// TestHealthBodylessOnTheWire checks the health path's answers that carry no body, as the public listener writes them:
// a HEAD request, and a 204 or 304 answer to GET. They are relayed with the worker's status and fields, without the
// supervisor's health fields, and nothing follows the head. A 204 carries no Content-Length even when the worker sent
// one (RFC 9110, section 8.6); a 304 and an answer to HEAD keep the length the worker declared for the representation.
// The worker here writes its answer byte for byte, so the fields it sends reach the relay unchanged.
func TestHealthBodylessOnTheWire(t *testing.T) {
	tests := map[string]struct {
		method, target string
		worker         string
		want           rawAnswer
	}{
		"success: 204 to GET of the health path is relayed without a body or a length": {
			method: http.MethodGet, target: "/healthz",
			worker: "HTTP/1.1 204 No Content\r\nX-Worker-Response: yes\r\nContent-Length: 5\r\nConnection: close\r\n\r\n",
			want:   rawAnswer{Status: http.StatusNoContent, WorkerResponse: "yes"},
		},
		"success: 304 to GET of the health path with a query keeps the declared length and carries no body": {
			method: http.MethodGet, target: "/healthz?probe=1",
			worker: "HTTP/1.1 304 Not Modified\r\nX-Worker-Response: yes\r\nETag: \"v1\"\r\nContent-Length: 17\r\nConnection: close\r\n\r\n",
			want:   rawAnswer{Status: http.StatusNotModified, WorkerResponse: "yes", ContentLength: []string{"17"}},
		},
		"success: HEAD of the health path keeps the worker's length and carries no body": {
			method: http.MethodHead, target: "/healthz",
			worker: "HTTP/1.1 200 OK\r\nX-Worker-Response: yes\r\nContent-Type: application/json\r\nContent-Length: 20\r\nConnection: close\r\n\r\n{\"workerField\":\"ok\"}",
			want:   rawAnswer{Status: http.StatusOK, WorkerResponse: "yes", ContentType: "application/json", ContentLength: []string{"20"}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			worker := startScriptedWorker(t, tc.worker)
			public := startPublicListener(t, worker.port)
			if diff := gocmp.Diff(tc.want, exchangeOnce(t, public, tc.method, tc.target)); diff != "" {
				t.Errorf("answer on the wire (-want +got):\n%s", diff)
			}
			if got := worker.accepted(); got != 1 {
				t.Errorf("worker accepted %d connections, want 1", got)
			}
		})
	}
}

// TestHealthDocumentOnTheWire checks what the public listener writes for a GET of the health path whose worker
// answer has a body. A JSON object is relayed with the supervisor's version, pid and recovery flag added after the
// worker's members, and its Content-Length is the length of the document the supervisor wrote, not the shorter one
// the worker declared. A body that is not a JSON object, malformed or another kind of JSON value, is answered with 502,
// an empty body and none of the worker's fields.
func TestHealthDocumentOnTheWire(t *testing.T) {
	// The handler of startPublicListener has version 1.2.3 and pid 29.
	const augmented = `{"workerField":"ok","supervisorVersion":"1.2.3","supervisorPid":29,"proxyRecovery":true}`
	badGateway := rawAnswer{Status: http.StatusBadGateway, ContentLength: []string{"0"}}
	tests := map[string]struct {
		body string
		want rawAnswer
	}{
		"success: object gets the supervisor's fields and the length of the written document": {
			body: `{"workerField":"ok"}`,
			want: rawAnswer{Status: http.StatusOK, WorkerResponse: "yes", ContentType: "application/json", ContentLength: []string{strconv.Itoa(len(augmented))}, Rest: augmented},
		},
		"error: malformed text is answered with 502":               {body: "not json", want: badGateway},
		"error: truncated object is answered with 502":             {body: `{"workerField":`, want: badGateway},
		"error: empty body is answered with 502":                   {body: "", want: badGateway},
		"error: array is answered with 502":                        {body: `[{"workerField":"ok"}]`, want: badGateway},
		"error: string is answered with 502":                       {body: `"ok"`, want: badGateway},
		"error: null is answered with 502":                         {body: "null", want: badGateway},
		"error: object followed by more text is answered with 502": {body: `{"workerField":"ok"} {}`, want: badGateway},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			response := "HTTP/1.1 200 OK\r\nX-Worker-Response: yes\r\nContent-Type: application/json\r\nContent-Length: " +
				strconv.Itoa(len(tc.body)) + "\r\nConnection: close\r\n\r\n" + tc.body
			worker := startScriptedWorker(t, response)
			public := startPublicListener(t, worker.port)
			if diff := gocmp.Diff(tc.want, exchangeOnce(t, public, http.MethodGet, "/healthz")); diff != "" {
				t.Errorf("answer on the wire for worker body %q (-want +got):\n%s", tc.body, diff)
			}
		})
	}
}
