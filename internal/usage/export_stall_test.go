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

package usage

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"
)

// TestEmitterExportNeverCompletes checks two collectors that never finish an export: one that
// answers with a protocol switch and then stays silent, and one behind an https endpoint that
// accepts the connection and never answers the TLS handshake. Either export ends as a silent
// failure no later than the 500ms export bound, so Wait returns. A switched protocol is no answer
// to an export: the exporter refuses it and closes the connection by the bound. A connection whose
// establishment is still pending belongs to the owner of the exporter's transport, not to the
// exporter: the worker closes it when it stops, so this test only releases it at cleanup.
func TestEmitterExportNeverCompletes(t *testing.T) {
	// exportBound restates exportTimeout.
	const exportBound = 500 * time.Millisecond
	tests := map[string]struct {
		endpoint string
		// reply is written after the whole request was read; empty reads the client's bytes,
		// such as a TLS ClientHello, and never answers.
		reply string
		// closedByBound requires the collector to see its connection closed by the export bound.
		closedByBound bool
	}{
		"error: collector switches protocols and then stays silent": {
			endpoint:      "http://127.0.0.1:1/v1/logs",
			reply:         "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n",
			closedByBound: true,
		},
		// The pending handshake is the transport owner's to close; the deferred closes of both
		// pipe ends end it before the bubble returns, so nothing is left running.
		"error: collector never completes the TLS handshake": {
			endpoint: "https://127.0.0.1:1/v1/logs",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			output := captureOutput(t)
			synctest.Test(t, func(t *testing.T) {
				peer, client := net.Pipe()
				defer func() {
					_ = peer.Close()
					_ = client.Close()
				}()
				answered := make(chan error, 1)
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
							_, err = io.WriteString(peer, tt.reply)
						}
					}
					answered <- err
					// Nothing more is sent; this read ends only when the exporter closes the connection.
					_, _ = io.Copy(io.Discard, peer)
				}()
				e := newEmitter(func(k string) (string, bool) { return tt.endpoint, k == "CODEX_GATEWAY_USAGE_ENDPOINT" }, NewBuilder(options(t)), func(context.Context, string, string) (net.Conn, error) { return client, nil })
				if e.endpoint == nil || e.endpoint.String() != tt.endpoint {
					t.Fatalf("test precondition: export endpoint %v, want %s", e.endpoint, tt.endpoint)
				}
				start := time.Now()
				done := make(chan time.Duration, 1)
				go func() {
					capture := e.Start(Request{})
					capture.SetResponse(http.StatusOK, nil)
					capture.ObserveJSON([]byte(`{"usage":{"input_tokens":15}}`))
					if capture.Finish() == nil {
						t.Error("a collector that never answers suppressed the primary usage record")
					}
					e.Wait()
					done <- time.Since(start)
				}()
				select {
				case elapsed := <-done:
					if elapsed > exportBound {
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
				if tt.closedByBound {
					select {
					case <-closed:
					default:
						t.Error("exporter did not close the collector connection")
					}
				}
				// Release the collector: closing its end ends a handshake still pending and the
				// collector's read, so the test returns with nothing left running.
				_ = peer.Close()
				<-closed
			})
			if raw := output(); len(raw) != 0 {
				t.Errorf("export failure wrote output: %q", raw)
			}
		})
	}
}
