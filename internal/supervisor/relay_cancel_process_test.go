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
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

func TestRelayCancellation(t *testing.T) {
	tests := map[string]struct {
		concurrentDial bool
	}{
		"error: cancelled request writes no 503":      {},
		"error: concurrent cancel during worker dial": {concurrentDial: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if !tt.concurrentDial {
				h, err := supervisor.NewHandler(40001, func() int { return 0 }, strings.Repeat("ab", 32), "test-version", 1)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader("{}"))
				r.Host = "127.0.0.1:40001"
				w := httptest.NewRecorder()
				w.Code = 0 // Distinguish no write from an explicit HTTP 200.
				h.ServeHTTP(w, r)
				if w.Code != 0 || w.Body.Len() != 0 {
					t.Errorf("cancelled relay wrote status=%d bytes=%d; want no response", w.Code, w.Body.Len())
				}
				return
			}

			ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			var acceptWG sync.WaitGroup
			var connMu sync.Mutex
			var conns []net.Conn
			acceptWG.Go(func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					connMu.Lock()
					conns = append(conns, c)
					connMu.Unlock()
				}
			})
			defer func() {
				_ = ln.Close()
				acceptWG.Wait()
				connMu.Lock()
				for _, c := range conns {
					_ = c.Close()
				}
				connMu.Unlock()
			}()

			h, err := supervisor.NewHandler(40001, func() int { return port }, strings.Repeat("ab", 32), "test-version", 1)
			if err != nil {
				t.Fatal(err)
			}

			const attempts = 64
			var wg sync.WaitGroup
			for i := range attempts {
				wg.Go(func() {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					start := make(chan struct{})
					var cancelWG sync.WaitGroup
					cancelWG.Go(func() {
						<-start
						for range i % 5 {
							runtime.Gosched()
						}
						cancel()
					})
					r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader("{}"))
					r.Host = "127.0.0.1:40001"
					w := httptest.NewRecorder()
					w.Code = 0
					close(start)
					h.ServeHTTP(w, r)
					cancelWG.Wait()
					if w.Code != 0 || w.Body.Len() != 0 {
						t.Errorf("attempt %d: cancelled relay wrote status=%d bytes=%d", i, w.Code, w.Body.Len())
					}
				})
			}
			wg.Wait()
		})
	}
}
