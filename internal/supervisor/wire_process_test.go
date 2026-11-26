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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// TestListenerHostAsSent writes raw HTTP/1.1 to the public listener so that the Host header is judged as the client
// sent it. net/http replaces Request.Host with the authority of an absolute-form target and drops the Host header
// before a handler runs, which a request built with net/http cannot reproduce.
func TestListenerHostAsSent(t *testing.T) {
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	var calls atomic.Int32
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	base := env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	workerPort, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("ef", 32)
	public := listener.Addr().String()
	h, err := supervisor.NewHandler(listener.Addr().(*net.TCPAddr).Port, func() int { return workerPort }, token, "1.2.3", 23)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := supervisor.Serve(ctx, listener, h)
		if ctx.Err() == nil {
			t.Errorf("public listener stopped before cancellation: %v", err)
		}
		_ = listener.Close()
	}()
	defer func() {
		cancel()
		_ = listener.Close()
		<-done
	}()

	absolute := "http://" + public
	own := "Host: " + public
	foreign := "Host: outside.invalid"
	media := "Content-Type: application/json"
	credential := "X-Model-Gateway-Control-Token: " + token
	type result struct {
		Status      int
		BodyBytes   int
		WorkerCalls int32
	}
	refused := result{Status: http.StatusForbidden}
	relayed := result{Status: http.StatusNoContent, WorkerCalls: 1}
	tests := map[string]struct {
		target string
		header []string
		want   result
	}{
		"error: absolute-form drain with foreign Host and valid token": {absolute + "/drain", []string{foreign, media, credential}, refused},
		"error: absolute-form relay path with foreign Host":            {absolute + "/sample", []string{foreign}, refused},
		"success: absolute-form relay path with own Host":              {absolute + "/sample", []string{own}, relayed},
		"success: absolute-form drain with own Host and valid token":   {absolute + "/drain", []string{own, media, credential}, relayed},
		// The control check matches the raw request target before its query, so an absolute-form drain is relayed.
		"success: absolute-form drain without token is not a control path": {absolute + "/drain", []string{own, media}, relayed},
		"error: origin-form drain without token":                           {"/drain", []string{own, media}, refused},
		"error: Origin lines in mixed case on relay path":                  {"/sample", []string{own, "oRiGiN: ", "ORIGIN: null"}, refused},
		"error: Origin lines in mixed case on drain":                       {"/drain", []string{own, media, credential, "origin: null", "Origin: "}, refused},
		"success: control headers in mixed case on own port":               {"/drain", []string{"hOsT: " + public, "cOnTeNt-TyPe: application/json", "x-MODEL-gateway-CONTROL-token: " + token}, relayed},
		"error: media type with parameter":                                 {"/drain", []string{own, "Content-Type: application/json; charset=utf-8", credential}, refused},
		"error: Host with wrong port on drain":                             {"/drain", []string{"Host: 127.0.0.1:0", media, credential}, refused},
		"error: Host with wrong port on relay path":                        {"/sample", []string{"Host: 127.0.0.1:0"}, refused},
		"error: origin-form relay path with foreign Host":                  {"/sample", []string{foreign}, refused},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var dialer net.Dialer
			conn, err := dialer.DialContext(t.Context(), "tcp4", public)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			const body = "{}"
			var head strings.Builder
			fmt.Fprintf(&head, "POST %s HTTP/1.1\r\n", tc.target)
			for _, line := range tc.header {
				head.WriteString(line + "\r\n")
			}
			fmt.Fprintf(&head, "Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			before := calls.Load()
			if _, err := io.WriteString(conn, head.String()); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("read response to %q: %v", head.String(), err)
			}
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			got := result{Status: resp.StatusCode, BodyBytes: len(b), WorkerCalls: calls.Load() - before}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("request %q (-want +got):\n%s", head.String(), diff)
			}
		})
	}
}
