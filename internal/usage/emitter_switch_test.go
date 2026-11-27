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

// switchedReply is a protocol switch as net/http recognises one: status 101 with the upgrade headers.
const switchedReply = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"

// TestEmitterSwitchedCollector decides what an export makes of a collector that answers with a
// protocol switch and then stays silent: the export ends at once instead of reading the switched
// connection, which neither its deadline nor the transport would end, Wait returns, and the
// collector sees its connection closed. The injected transport is a bare one, so the emitter's own
// handling decides the row, not a wrapper around the transport.
func TestEmitterSwitchedCollector(t *testing.T) {
	tests := map[string]struct{ injected bool }{
		"error: a collector that switches protocols ends an export on the emitter's own transport at once": {},
		"error: a collector that switches protocols ends an export on an injected transport at once":       {injected: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			output := captureOutput(t)
			// Virtual time shows the export did not wait; the pipe stands in for the collector's socket.
			synctest.Test(t, func(t *testing.T) {
				peer, client := net.Pipe()
				defer func() {
					_ = peer.Close()
					_ = client.Close()
				}()
				answered := make(chan error, 1)
				abandoned := make(chan struct{})
				go func() {
					defer close(abandoned)
					req, err := http.ReadRequest(bufio.NewReader(peer))
					if err == nil {
						_, err = io.Copy(io.Discard, req.Body)
					}
					if err == nil {
						_, err = io.WriteString(peer, switchedReply)
					}
					answered <- err
					// Send nothing more; this read ends only when the exporter closes the connection.
					_, _ = io.Copy(io.Discard, peer)
				}()
				lookup := func(k string) (string, bool) {
					return "http://127.0.0.1:1/v1/logs", k == "CODEX_GATEWAY_USAGE_ENDPOINT"
				}
				dial := func(context.Context, string, string) (net.Conn, error) { return client, nil }
				e := newEmitter(lookup, NewBuilder(options(t)), dial)
				if tt.injected {
					e = NewEmitterWithTransport(lookup, NewBuilder(options(t)), &http.Transport{DialContext: dial, DisableKeepAlives: true, DisableCompression: true})
				}
				start := time.Now()
				done := make(chan time.Duration, 1)
				go func() {
					capture := e.Start(Request{})
					capture.SetResponse(http.StatusOK, nil)
					capture.ObserveJSON([]byte(`{"usage":{"input_tokens":15}}`))
					if capture.Finish() == nil {
						t.Error("the switched collector suppressed the primary usage record")
					}
					e.Wait()
					done <- time.Since(start)
				}()
				select {
				case elapsed := <-done:
					if elapsed != 0 {
						t.Errorf("Wait returned after %s, want at once: the export waited on the switched connection", elapsed)
					}
				case <-time.After(exportTimeout + time.Millisecond):
					t.Error("Wait did not return within the export bound while the collector held a switched connection")
				}
				synctest.Wait()
				select {
				case err := <-answered:
					if err != nil {
						t.Errorf("collector: %v", err)
					}
				default:
					t.Error("the collector never answered with its protocol switch")
				}
				if !closedNow(abandoned) {
					t.Error("the exporter did not close the switched collector connection")
				}
			})
			if raw := output(); len(raw) != 0 {
				t.Errorf("the switched export wrote output: %q", raw)
			}
		})
	}
}

// closedNow reports whether ch is closed, without waiting.
func closedNow(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
