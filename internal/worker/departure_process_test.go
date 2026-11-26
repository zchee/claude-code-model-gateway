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

package worker_test

import (
	"context"
	"encoding/json/jsontext"
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

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// departureBody passes the request body through and, once the body has ended, returns its end only after the
// request's context has ended. net/http watches the connection for the client's end once the body is read, so with a
// client that has half-closed, the handler continues only after net/http has seen the departure.
type departureBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b departureBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		<-b.ctx.Done()
	}
	return n, err
}

// TestDepartedClientGetsNoBytes serves the worker handler with net/http on a loopback connection whose client sends
// one complete request and half-closes. The handler notices the departure before it would answer; nothing may be
// written for that client, because net/http turns a handler that returns without writing into a 200 with an empty
// body. The departure is made certain before the answer: the request body, or the catalog hook, waits for the end of
// the request's context.
func TestDepartedClientGetsNoBytes(t *testing.T) {
	tests := map[string]struct {
		method, target, body string
		noBranch, models     bool
	}{
		"error: client leaves before classifier refusal":  {method: http.MethodPost, target: "/v1/messages", body: `{"model":"claude-grok-test\ud800"}`},
		"error: client leaves before unconfigured branch": {method: http.MethodPost, target: "/v1/messages", noBranch: true},
		"error: client leaves before branch dispatch":     {method: http.MethodPost, target: "/v1/messages"},
		"error: client leaves during the model listing":   {method: http.MethodGet, target: "/v1/models", models: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var terminals []outcome.Result
			var forwarded, listings atomic.Int32
			opts := worker.Options{}
			if tc.models {
				opts.Models = func(ctx context.Context, _ http.Header) (jsontext.Value, error) {
					listings.Add(1)
					<-ctx.Done()
					return nil, ctx.Err()
				}
			} else {
				opts.BeforeBody = func(r *http.Request) func(outcome.Result) {
					r.Body = departureBody{ReadCloser: r.Body, ctx: r.Context()}
					return func(res outcome.Result) {
						mu.Lock()
						defer mu.Unlock()
						terminals = append(terminals, res)
					}
				}
			}
			if !tc.noBranch {
				opts.Anthropic = func(http.ResponseWriter, *http.Request, route.Snapshot, route.Decision, []byte, admission) (outcome.Result, error) {
					forwarded.Add(1)
					return outcome.Result{}, nil
				}
			}
			h := handler(t, opts)
			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			var cfg net.ListenConfig
			ln, err := env.Listen(t.Context(), "tcp4", "127.0.0.1:0", cfg.Listen)
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: h}
			served := make(chan error, 1)
			go func() { served <- srv.Serve(ln) }()
			defer func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				if err := srv.Shutdown(ctx); err != nil {
					t.Error(errors.Join(err, srv.Close()))
				}
				if err := <-served; !errors.Is(err, http.ErrServerClosed) {
					t.Error(err)
				}
			}()

			var dialer net.Dialer
			conn, err := dialer.DialContext(t.Context(), "tcp4", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			// A hang guard for a server that neither answers nor closes, not a pacing delay.
			if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatal(err)
			}
			head := tc.method + " " + tc.target + " HTTP/1.1\r\nHost: " + ln.Addr().String() + "\r\n"
			if tc.method == http.MethodPost {
				head += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(tc.body)) + "\r\n"
			}
			if _, err := io.WriteString(conn, head+"\r\n"+tc.body); err != nil {
				t.Fatal(err)
			}
			if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(conn)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("the worker neither answered nor closed the connection (%d bytes so far)", len(data))
			}
			if len(data) != 0 {
				line, _, _ := strings.Cut(string(data), "\r\n")
				t.Errorf("the worker wrote %d bytes for a client that had left, starting %q; want none", len(data), line)
			}
			if forwarded.Load() != 0 {
				t.Errorf("a departed client's request was dispatched %d times", forwarded.Load())
			}
			if tc.models {
				if diff := gocmp.Diff(int32(1), listings.Load()); diff != "" {
					t.Errorf("catalog hook calls (-want +got):\n%s", diff)
				}
				return
			}
			mu.Lock()
			got := append([]outcome.Result(nil), terminals...)
			mu.Unlock()
			if len(got) != 1 || !got[0].Cancelled || got[0].Contacted {
				t.Errorf("terminal results %+v; want exactly one cancelled result that contacted nothing", got)
			}
		})
	}
}
