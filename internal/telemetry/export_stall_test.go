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

package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"
)

// TestTraceExportNeverCompletes checks two collectors that never finish an export: one that
// answers with a protocol switch and then stays silent, and one behind an https endpoint that
// reads the start of the TLS ClientHello and never answers it. Either export ends as a silent
// failure no later than the 500ms export bound, so Wait returns. A switched protocol is no answer
// to an export: the exporter refuses it and closes the connection at once, so the collector sees
// the end of its connection and Wait returns at the virtual instant the switch was delivered,
// with no virtual time spent reading the switched connection. A connection whose establishment is
// still pending belongs to the owner of the exporter's transport, not to the exporter: the worker
// closes it when it stops, so this test only releases it at cleanup.
func TestTraceExportNeverCompletes(t *testing.T) {
	// exportBound restates exportTimeout.
	const exportBound = 500 * time.Millisecond
	tests := map[string]struct {
		endpoint string
		// reply is written after the whole request was read and replyDelay has passed; empty
		// reads the start of a TLS ClientHello instead and never answers.
		reply      string
		replyDelay time.Duration
		// closedOnReply requires the collector to see its connection end, and Wait to return, at
		// the virtual instant the reply was delivered.
		closedOnReply bool
	}{
		// The reply is held back so that its delivery is a later instant than the start of the
		// export, and both are well inside the bound.
		"error: collector switches protocols and then stays silent": {
			endpoint:      "http://127.0.0.1:1/v1/traces",
			reply:         "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n",
			replyDelay:    100 * time.Millisecond,
			closedOnReply: true,
		},
		// The pending handshake is the transport owner's to close; the test closes the
		// collector's end before the bubble returns, so nothing is left running.
		"error: collector never completes the TLS handshake": {
			endpoint: "https://127.0.0.1:1/v1/traces",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			synctest.Test(t, func(t *testing.T) {
				peer, client := net.Pipe()
				defer func() {
					_ = peer.Close()
					_ = client.Close()
				}()
				// answered receives the collector's error once it has seen the export: the whole
				// post answered with the reply, or the start of a TLS ClientHello.
				answered := make(chan error, 1)
				// delivered receives the virtual instant at which the client had read the reply.
				delivered := make(chan time.Time, 1)
				// ended receives the virtual instant at which the collector saw its connection end.
				ended := make(chan time.Time, 1)
				closed := make(chan struct{})
				go func() {
					defer close(closed)
					var err error
					if tt.reply != "" {
						var req *http.Request
						req, err = http.ReadRequest(bufio.NewReader(peer))
						if err == nil {
							_, err = io.Copy(io.Discard, req.Body)
						}
						if err == nil {
							time.Sleep(tt.replyDelay)
							// A write on the pipe returns once the client has read all of it.
							_, err = io.WriteString(peer, tt.reply)
						}
						if err == nil {
							delivered <- time.Now()
						}
					} else {
						// A TLS record header (content type 22 = handshake, protocol major version
						// 3, one byte of minor version, two bytes of length) followed by the
						// handshake type 1 = ClientHello. Only an exporter that dialed and began the
						// handshake sends these bytes.
						var hello [6]byte
						if _, err = io.ReadFull(peer, hello[:]); err == nil && (hello[0] != 22 || hello[1] != 3 || hello[5] != 1) {
							err = fmt.Errorf("first bytes %x are not the start of a TLS ClientHello", hello)
						}
					}
					answered <- err
					// Nothing more is sent; this read ends only when the connection is closed.
					_, _ = io.Copy(io.Discard, peer)
					ended <- time.Now()
				}()
				opts := recorderOptions(map[string]string{"CODEX_GATEWAY_TELEMETRY_ENDPOINT": tt.endpoint, "CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1"})
				var diagnostics bytes.Buffer
				opts.Diagnostics = &diagnostics
				r := newRecorder(opts, func(context.Context, string, string) (net.Conn, error) { return client, nil })
				if r.endpoint == nil || r.endpoint.String() != tt.endpoint {
					t.Fatalf("test precondition: export endpoint %v, want %s", r.endpoint, tt.endpoint)
				}
				start := time.Now()
				done := make(chan time.Time, 1)
				go func() {
					r.Start(nil).Finish(Route{Backend: "codex"}, http.StatusOK, "")
					r.Wait()
					done <- time.Now()
				}()
				var waited time.Time
				select {
				case waited = <-done:
					if elapsed := waited.Sub(start); elapsed > exportBound {
						t.Errorf("Wait returned at %s, want no later than %s", elapsed, exportBound)
					}
				case <-time.After(exportBound + time.Millisecond):
					t.Errorf("Wait did not return within the %s export bound", exportBound)
				}
				synctest.Wait()
				select {
				case err := <-answered:
					if err != nil {
						t.Errorf("collector: %v", err)
					}
				default:
					t.Error("collector never received the export")
				}
				if tt.closedOnReply {
					var replied time.Time
					select {
					case replied = <-delivered:
					default:
						t.Error("collector never delivered its reply")
					}
					select {
					case at := <-ended:
						if !replied.IsZero() && !at.Equal(replied) {
							t.Errorf("collector saw its connection end %s after the reply was delivered, want at once", at.Sub(replied))
						}
					default:
						t.Error("exporter did not close the collector connection")
					}
					if !replied.IsZero() && !waited.IsZero() && !waited.Equal(replied) {
						t.Errorf("Wait returned %s after the reply was delivered, want at once", waited.Sub(replied))
					}
				}
				// Release the collector: closing its end ends a handshake still pending and the
				// collector's read, so the test returns with nothing left running.
				_ = peer.Close()
				<-closed
				if diagnostics.Len() != 0 {
					t.Errorf("export failure wrote diagnostic: %q", diagnostics.String())
				}
			})
		})
	}
}
