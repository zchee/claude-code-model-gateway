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
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// orderGateReply is the collector's answer to every post.
const orderGateReply = "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"

// orderGateEmitter returns an emitter whose collector posts all go through g.
func orderGateEmitter(g *gatewaytest.OrderGate, builder *Builder) *Emitter {
	return newEmitter(func(k string) (string, bool) {
		return "http://127.0.0.1:1/v1/logs", k == "CODEX_GATEWAY_USAGE_ENDPOINT"
	}, builder, func(ctx context.Context, network, addr string) (net.Conn, error) { return g.Dial(ctx, network, addr) })
}

// orderRecordKey names a posted record by its event, session and MCP server. It returns an error
// instead of failing the test, so that the collector can still answer every post.
func orderRecordKey(payload []byte) (string, error) {
	attrs, err := orderAttributes(payload)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, 3)
	for _, name := range []string{"event.name", "session_id", "mcp_server"} {
		for _, attr := range attrs {
			if attr.Key == name {
				parts = append(parts, fmt.Sprint(attr.Value["stringValue"]))
			}
		}
	}
	return strings.Join(parts, " "), nil
}

// orderAttributes returns the attributes of a post's one log record in wire order.
func orderAttributes(payload []byte) ([]otlpAttr, error) {
	var envelope struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					Attributes []otlpAttr `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode post %s: %w", payload, err)
	}
	if len(envelope.ResourceLogs) != 1 || len(envelope.ResourceLogs[0].ScopeLogs) != 1 || len(envelope.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
		return nil, fmt.Errorf("post %s does not hold exactly one log record", payload)
	}
	return envelope.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes, nil
}

// orderToolsBody declares two MCP servers whose footprint order is pinned by
// TestOrderedExportBytes: aa first, then z.
const orderToolsBody = `{"tools":[{"name":"mcp__z__t"},{"name":"mcp__aa__t"}]}`

// footprintServers returns the MCP servers named by the per-server token attributes of a primary
// record, in the record's attribute order, which is the order of its footprint records in the
// source (lib/usage-observability.js:936-938).
func footprintServers(r *Record) ([]string, error) {
	if r == nil {
		return nil, errors.New("Finish returned no primary record")
	}
	payload, err := r.OTLP()
	if err != nil {
		return nil, err
	}
	attrs, err := orderAttributes(payload)
	if err != nil {
		return nil, err
	}
	var servers []string
	for _, attr := range attrs {
		if m := footprintKey.FindStringSubmatch(attr.Key); m != nil {
			servers = append(servers, m[1])
		}
	}
	return servers, nil
}

// TestEmitterSubmissionOrder checks that the records the emitter is given reach the collector in
// the order it was given them, as in the source: one request's token usage record comes before
// its MCP footprint records, and the records of several requests follow the order in which the
// requests finished, not the order in which they started. The source schedules each post with
// setImmediate, which runs callbacks in the order they were queued, and each callback starts its
// post at once (lib/usage-observability.js:1005-1036, 674-689). The collector (gatewaytest.OrderGate)
// parks every post until the bubble is quiet and records posts that were outstanding together in
// reverse, so the test does not depend on how the goroutines happen to be scheduled.
func TestEmitterSubmissionOrder(t *testing.T) {
	type turn struct {
		session string
		// mcp declares the two MCP servers of orderToolsBody; without it the request has no
		// footprint records.
		mcp bool
	}
	tests := map[string]struct {
		// turns are started in this order.
		turns []turn
		// finish lists indexes into turns in the order their captures finish.
		finish []int
		// want is every record key in submission order.
		want []string
	}{
		"success: one request's token usage precedes its footprints": {
			turns:  []turn{{session: "session-only", mcp: true}},
			finish: []int{0},
			want: []string{
				"gateway.token.usage session-only",
				"gateway.mcp.footprint session-only aa",
				"gateway.mcp.footprint session-only z",
			},
		},
		"success: several requests' records follow the order the requests finished": {
			turns:  []turn{{session: "session-a", mcp: true}, {session: "session-b"}, {session: "session-c", mcp: true}},
			finish: []int{2, 0, 1},
			want: []string{
				"gateway.token.usage session-c",
				"gateway.mcp.footprint session-c aa",
				"gateway.mcp.footprint session-c z",
				"gateway.token.usage session-a",
				"gateway.mcp.footprint session-a aa",
				"gateway.mcp.footprint session-a z",
				"gateway.token.usage session-b",
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			synctest.Test(t, func(t *testing.T) {
				g := gatewaytest.NewOrderGate(tt.want, orderRecordKey, orderGateReply)
				e := orderGateEmitter(g, sharedBuilder(t))
				captures := make([]*Capture, len(tt.turns))
				for i, turn := range tt.turns {
					body := wire(t, `{}`)
					if turn.mcp {
						body = wire(t, orderToolsBody)
					}
					captures[i] = e.Start(Request{Body: body, Headers: http.Header{"X-Claude-Code-Session-Id": {turn.session}}})
					captures[i].SetResponse(http.StatusOK, nil)
					captures[i].ObserveJSON([]byte(`{"usage":{"input_tokens":41,"output_tokens":1}}`))
				}
				// Finish submits each capture's records; TestEmitterDelivery already requires that
				// it returns while the collector holds every post.
				primaries := make([]*Record, len(tt.finish))
				for i, index := range tt.finish {
					primaries[i] = captures[index].Finish()
				}
				waited := make(chan struct{})
				go func() {
					defer close(waited)
					e.Wait()
				}()
				g.Serve(t, waited)
				for i, index := range tt.finish {
					var want []string
					if tt.turns[index].mcp {
						want = []string{"aa", "z"}
					}
					servers, err := footprintServers(primaries[i])
					if err != nil {
						t.Errorf("test precondition: %s: %v", tt.turns[index].session, err)
						continue
					}
					if d := gocmp.Diff(want, servers); d != "" {
						t.Errorf("test precondition: %s footprint servers in primary record order (-want +got):\n%s", tt.turns[index].session, d)
					}
				}
				g.Check(t, tt.want)
			})
		})
	}
}

// TestEmitterHandOff decides when the emitter moves from one record to the next: it starts the
// next post once the previous request is wholly on its connection, not when only its headers are
// written and not after waiting for the collector's reply, as the source starts each post without
// waiting for an earlier one's reply (lib/usage-observability.js:674-689). Two requests' records
// are submitted; the collector holds back the first post's last body byte and then its reply.
func TestEmitterHandOff(t *testing.T) {
	tests := map[string]struct {
		sessions []string
	}{
		"success: the second record is posted once the first is written and before its reply": {
			sessions: []string{"session-first", "session-second"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			synctest.Test(t, func(t *testing.T) {
				want := make([]string, len(tt.sessions))
				for i, session := range tt.sessions {
					want[i] = "gateway.token.usage " + session
				}
				g := gatewaytest.NewOrderGate(want, orderRecordKey, orderGateReply)
				e := orderGateEmitter(g, sharedBuilder(t))
				for _, session := range tt.sessions {
					capture := e.Start(Request{Body: wire(t, `{}`), Headers: http.Header{"X-Claude-Code-Session-Id": {session}}})
					capture.SetResponse(http.StatusOK, nil)
					capture.ObserveJSON([]byte(`{"usage":{"input_tokens":41,"output_tokens":1}}`))
					if capture.Finish() == nil {
						t.Errorf("test precondition: %s: Finish returned no primary record", session)
					}
				}
				findings := g.HandOff()
				waited := make(chan struct{})
				go func() {
					defer close(waited)
					e.Wait()
				}()
				g.Serve(t, waited)
				if d := gocmp.Diff([]string(nil), findings); d != "" {
					t.Errorf("how the emitter hands one post over to the next (-want +got):\n%s", d)
				}
				g.Check(t, want)
			})
		})
	}
}
