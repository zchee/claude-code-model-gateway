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
	"bytes"
	"cmp"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
)

// orderGateCollector serves every collector connection over an in-memory pipe inside a synctest
// bubble and decides when each post reaches it. A new connection is parked unread, so the post's
// sender stays blocked writing it, until every goroutine of the bubble is blocked. Then the
// collector takes every parked post. One parked post is recorded as it is. Several parked posts
// were outstanding together: nothing their sender did fixes the order in which independent
// connections reach a collector, so the collector records them in the reverse of their submission
// order, which a network is free to do. A sender that starts a post only after the collector took
// the previous one therefore has its posts recorded in submission order, whatever the scheduler
// does, and a sender with two posts outstanding at once never does.
type orderGateCollector struct {
	// rank is the submission index of each record key the test submits.
	rank map[string]int
	// dialed is signalled, without blocking, whenever a connection is parked.
	dialed chan struct{}

	mu     sync.Mutex // guards parked
	parked []net.Conn

	// The fields below are used only by the bubble's test goroutine.
	keys   []string // record keys in the order the collector took the posts
	bodies [][]byte // the posts' bodies, in the same order
	errs   []error
}

func newOrderGateCollector(want []string) *orderGateCollector {
	c := &orderGateCollector{rank: make(map[string]int, len(want)), dialed: make(chan struct{}, 1)}
	for i, key := range want {
		c.rank[key] = i
	}
	return c
}

// dial hands the sender one side of a new pipe and parks the other side unread.
func (c *orderGateCollector) dial(context.Context, string, string) (net.Conn, error) {
	peer, client := net.Pipe()
	c.mu.Lock()
	c.parked = append(c.parked, peer)
	c.mu.Unlock()
	select {
	case c.dialed <- struct{}{}:
	default:
	}
	return client, nil
}

func (c *orderGateCollector) emitter(builder *Builder) *Emitter {
	return newEmitter(func(k string) (string, bool) {
		return "http://127.0.0.1:1/v1/logs", k == "CODEX_GATEWAY_USAGE_ENDPOINT"
	}, builder, c.dial)
}

// gatePost is one post the collector has read and not yet answered.
type gatePost struct {
	conn net.Conn
	key  string
	body []byte
	rank int
}

// take reads, records and answers every parked post, and reports whether there was any. It must
// run only when every other goroutine of the bubble is blocked, so that the parked set is every
// post outstanding at that point.
func (c *orderGateCollector) take() bool {
	c.mu.Lock()
	batch := c.parked
	c.parked = nil
	c.mu.Unlock()
	posts := make([]gatePost, 0, len(batch))
	for _, conn := range batch {
		post, err := c.read(conn)
		if err != nil {
			c.errs = append(c.errs, err)
			_ = conn.Close()
			continue
		}
		posts = append(posts, post)
	}
	slices.SortFunc(posts, func(a, b gatePost) int { return cmp.Compare(b.rank, a.rank) })
	for _, post := range posts {
		c.keys = append(c.keys, post.key)
		c.bodies = append(c.bodies, post.body)
		// A write on the pipe returns once the sender has read all of it.
		if _, err := io.WriteString(post.conn, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"); err != nil {
			c.errs = append(c.errs, fmt.Errorf("answer %s: %w", post.key, err))
		}
		_ = post.conn.Close()
	}
	return len(batch) > 0
}

func (c *orderGateCollector) read(conn net.Conn) (gatePost, error) {
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return gatePost{}, fmt.Errorf("read post: %w", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return gatePost{}, fmt.Errorf("read post body: %w", err)
	}
	key, err := orderRecordKey(body)
	if err != nil {
		return gatePost{}, err
	}
	rank, ok := c.rank[key]
	if !ok {
		return gatePost{}, fmt.Errorf("collector received %q, which the test did not submit", key)
	}
	return gatePost{conn: conn, key: key, body: body, rank: rank}, nil
}

// serve answers posts until waited is closed and no post is parked, so that no connection is left
// open when the bubble ends. The bound is virtual time and only aborts a sender that neither posts
// nor lets Wait return.
func (c *orderGateCollector) serve(t *testing.T, waited <-chan struct{}) {
	t.Helper()
	abort := time.After(time.Hour)
	for {
		synctest.Wait()
		if c.take() {
			continue
		}
		select {
		case <-waited:
			return
		case <-c.dialed:
		case <-abort:
			t.Error("the emitter neither posted nor returned from Wait within an hour of virtual time")
			return
		}
	}
}

// check reports the collector's failures, then compares the records it took with want: first as
// sets, so that a lost or extra record is told apart from a reordering, then in order.
func (c *orderGateCollector) check(t *testing.T, want []string) {
	t.Helper()
	for _, err := range c.errs {
		t.Errorf("order gate collector: %v", err)
	}
	sortedWant, sortedGot := slices.Sorted(slices.Values(want)), slices.Sorted(slices.Values(c.keys))
	if d := gocmp.Diff(sortedWant, sortedGot); d != "" {
		t.Errorf("records the collector received, ignoring order (-want +got):\n%s", d)
		return
	}
	if d := gocmp.Diff(want, c.keys); d != "" {
		t.Errorf("records reached the collector out of submission order (-submitted +received):\n%s", d)
	}
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
// post at once (lib/usage-observability.js:1005-1036, 674-689). The collector parks every post
// until the bubble is quiet and records posts that were outstanding together in reverse, so the
// test does not depend on how the goroutines happen to be scheduled.
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
				c := newOrderGateCollector(tt.want)
				e := c.emitter(sharedBuilder(t))
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
				c.serve(t, waited)
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
				c.check(t, tt.want)
			})
		})
	}
}

// TestOrderGateCollector shows that the collector of TestEmitterSubmissionOrder records exactly
// what it receives: posts sent one after another, each after the previous one was answered, are
// recorded in that order with their bodies intact, and posts outstanding together are recorded in
// reverse, however the goroutines that sent them were scheduled.
func TestOrderGateCollector(t *testing.T) {
	tests := map[string]struct {
		together bool
		// reverse requires the reverse of the submission order.
		reverse bool
	}{
		"success: posts sent one after another are recorded in that order": {},
		"success: posts outstanding together are recorded in reverse":      {together: true, reverse: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			synctest.Test(t, func(t *testing.T) {
				records := NewBuilder(options(t)).Build(Request{Body: wire(t, orderToolsBody), Headers: http.Header{"X-Claude-Code-Session-Id": {"session-control"}}, StatusCode: http.StatusOK, WireUsage: wire(t, `{"input_tokens":41}`)})
				submitted := []string{
					"gateway.token.usage session-control",
					"gateway.mcp.footprint session-control aa",
					"gateway.mcp.footprint session-control z",
				}
				if len(records) != len(submitted) {
					t.Fatalf("test precondition: Build returned %d records, want %d", len(records), len(submitted))
				}
				payloads := make([][]byte, len(records))
				for i := range records {
					payload, err := records[i].OTLP()
					if err != nil {
						t.Fatal(err)
					}
					if key, err := orderRecordKey(payload); err != nil || key != submitted[i] {
						t.Fatalf("test precondition: record %d is %q (%v), want %q", i, key, err, submitted[i])
					}
					payloads[i] = payload
				}
				c := newOrderGateCollector(submitted)
				client := &http.Client{Transport: &http.Transport{DialContext: c.dial, DisableKeepAlives: true, DisableCompression: true}}
				post := func(payload []byte) {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:1/v1/logs", bytes.NewReader(payload))
					if err != nil {
						t.Errorf("post: %v", err)
						return
					}
					req.Header.Set("Content-Type", "application/json")
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
					for _, payload := range payloads {
						senders.Go(func() { post(payload) })
					}
				} else {
					senders.Go(func() {
						for _, payload := range payloads {
							post(payload)
						}
					})
				}
				waited := make(chan struct{})
				go func() {
					defer close(waited)
					senders.Wait()
				}()
				c.serve(t, waited)
				want, wantBodies := slices.Clone(submitted), slices.Clone(payloads)
				if tt.reverse {
					slices.Reverse(want)
					slices.Reverse(wantBodies)
				}
				c.check(t, want)
				if d := gocmp.Diff(wantBodies, c.bodies); d != "" {
					t.Errorf("bodies the collector recorded (-sent +recorded):\n%s", d)
				}
			})
		})
	}
}
