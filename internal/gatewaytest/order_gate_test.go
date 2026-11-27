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

package gatewaytest_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// gateReply is the collector's answer in these controls.
const gateReply = "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"

// bodyKey names a control post by its whole body.
func bodyKey(body []byte) (string, error) { return string(body), nil }

// TestOrderGate shows that the gate records exactly what it receives: posts sent one after
// another, each after the previous one was answered, are recorded in that order with their bodies
// intact, and posts outstanding together are recorded in reverse, however the goroutines that sent
// them were scheduled.
func TestOrderGate(t *testing.T) {
	tests := map[string]struct {
		together bool
	}{
		"success: posts sent one after another are recorded in that order": {},
		"success: posts outstanding together are recorded in reverse":      {together: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			synctest.Test(t, func(t *testing.T) {
				submitted := []string{"post-a", "post-b", "post-c"}
				g := gatewaytest.NewOrderGate(submitted, bodyKey, gateReply)
				client := &http.Client{Transport: &http.Transport{DialContext: g.Dial, DisableKeepAlives: true, DisableCompression: true}}
				post := func(payload string) {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:1/v1/logs", bytes.NewReader([]byte(payload)))
					if err != nil {
						t.Errorf("post: %v", err)
						return
					}
					resp, err := client.Do(req)
					if err != nil {
						t.Errorf("post: %v", err)
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				var senders sync.WaitGroup
				if tt.together {
					for _, payload := range submitted {
						senders.Go(func() { post(payload) })
					}
				} else {
					senders.Go(func() {
						for _, payload := range submitted {
							post(payload)
						}
					})
				}
				waited := make(chan struct{})
				go func() {
					defer close(waited)
					senders.Wait()
				}()
				g.Serve(t, waited)
				want := slices.Clone(submitted)
				if tt.together {
					slices.Reverse(want)
				}
				g.Check(t, want)
				wantBodies := make([][]byte, len(want))
				for i, key := range want {
					wantBodies[i] = []byte(key)
				}
				if d := gocmp.Diff(wantBodies, g.Bodies()); d != "" {
					t.Errorf("bodies the collector recorded (-sent +recorded):\n%s", d)
				}
			})
		})
	}
}

// rawSender is how a control sender moves from one post to the next.
type rawSender int

const (
	// sendWhenWritten starts the next post once the previous request is wholly written and reads
	// each reply alongside: the hand-off the exporters must make.
	sendWhenWritten rawSender = iota
	// sendWhenHeadersWritten starts the next post once only the previous request's headers are
	// written.
	sendWhenHeadersWritten
	// sendWhenAnswered starts the next post only after the previous reply was read.
	sendWhenAnswered
	// sendTogether starts every post at once.
	sendTogether
)

// sendRaw writes each payload as its own HTTP/1.1 post over a connection from g, moving between
// posts as mode says, and closes done once every post was answered.
func sendRaw(t *testing.T, g *gatewaytest.OrderGate, mode rawSender, payloads []string) <-chan struct{} {
	done := make(chan struct{})
	var posts sync.WaitGroup
	write := func(conn net.Conn, p []byte) {
		if _, err := conn.Write(p); err != nil {
			t.Errorf("write a post: %v", err)
		}
	}
	answer := func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Errorf("read a reply: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	go func() {
		defer close(done)
		defer posts.Wait()
		for _, payload := range payloads {
			head := fmt.Appendf(nil, "POST /v1/logs HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(payload))
			body := []byte(payload)
			if mode == sendTogether {
				posts.Go(func() {
					conn, _ := g.Dial(t.Context(), "tcp", "127.0.0.1:1")
					write(conn, head)
					write(conn, body)
					answer(conn)
				})
				continue
			}
			conn, _ := g.Dial(t.Context(), "tcp", "127.0.0.1:1")
			write(conn, head)
			switch mode {
			case sendWhenWritten:
				write(conn, body)
				posts.Go(func() { answer(conn) })
			case sendWhenHeadersWritten:
				posts.Go(func() {
					write(conn, body)
					answer(conn)
				})
			case sendWhenAnswered:
				write(conn, body)
				answer(conn)
			}
		}
	}()
	return done
}

// TestOrderGateHandOff shows that HandOff tells the required hand-off apart from its near misses:
// a sender that starts the next post once the previous request is wholly written passes, and a
// sender that starts it once only the headers are written, one that waits for each reply, and one
// that sends both posts at once are each reported.
func TestOrderGateHandOff(t *testing.T) {
	tests := map[string]struct {
		mode         rawSender
		wantFindings []string
		// wantOrder is the order the collector must record; nil skips the order check.
		wantOrder []string
	}{
		"success: a sender that starts the next post once the previous one is written passes": {
			mode: sendWhenWritten, wantOrder: []string{"post-a", "post-b"},
		},
		"error: a sender that starts the next post once only the headers are written is reported": {
			mode:         sendWhenHeadersWritten,
			wantFindings: []string{"1 post(s) started while the first post's last body byte was still unwritten"},
			wantOrder:    []string{"post-a", "post-b"},
		},
		"error: a sender that waits for each reply is reported": {
			mode:         sendWhenAnswered,
			wantFindings: []string{"no next post started once the first post was written, while its reply was withheld"},
			wantOrder:    []string{"post-a", "post-b"},
		},
		"error: a sender with both posts outstanding at once is reported": {
			mode:         sendTogether,
			wantFindings: []string{"2 posts were outstanding when the bubble first went quiet, want only the first"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			synctest.Test(t, func(t *testing.T) {
				submitted := []string{"post-a", "post-b"}
				g := gatewaytest.NewOrderGate(submitted, bodyKey, gateReply)
				done := sendRaw(t, g, tt.mode, submitted)
				findings := g.HandOff()
				g.Serve(t, done)
				if d := gocmp.Diff(tt.wantFindings, findings); d != "" {
					t.Errorf("hand-off findings (-want +got):\n%s", d)
				}
				if tt.wantOrder != nil {
					g.Check(t, tt.wantOrder)
				}
			})
		})
	}
}
