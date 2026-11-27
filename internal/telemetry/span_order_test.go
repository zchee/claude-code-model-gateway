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
	"cmp"
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
)

// spanGateCollector serves every trace collector connection over an in-memory pipe inside a
// synctest bubble and decides when each post reaches it. A new connection is parked unread, so the
// post's sender stays blocked writing it, until every goroutine of the bubble is blocked. Then the
// collector takes every parked post. One parked post is recorded as it is. Several parked posts were
// outstanding together: nothing their sender did fixes the order in which independent connections
// reach a collector, so the collector records them in the reverse of their submission order, which
// a network is free to do. A sender that starts a post only after the previous one is on its
// connection (on a pipe, read by the collector) has its posts recorded in submission order, whatever
// the scheduler does, and a sender with two posts outstanding at once never does.
type spanGateCollector struct {
	// rank is the submission index of each span key the test submits.
	rank map[string]int
	// dialed is signalled, without blocking, whenever a connection is parked.
	dialed chan struct{}

	mu     sync.Mutex // guards parked
	parked []net.Conn

	// The fields below are used only by the bubble's test goroutine.
	keys   []string // span keys in the order the collector took the posts
	bodies [][]byte // the posts' bodies, in the same order
	errs   []error
}

func newSpanGateCollector(want []string) *spanGateCollector {
	c := &spanGateCollector{rank: make(map[string]int, len(want)), dialed: make(chan struct{}, 1)}
	for i, key := range want {
		c.rank[key] = i
	}
	return c
}

// dial hands the sender one side of a new pipe and parks the other side unread.
func (c *spanGateCollector) dial(context.Context, string, string) (net.Conn, error) {
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

// spanGatePost is one post the collector has read and not yet answered.
type spanGatePost struct {
	conn net.Conn
	key  string
	body []byte
	rank int
}

// take reads, records and answers every parked post, and reports whether there was any. It must run
// only when every other goroutine of the bubble is blocked, so that the parked set is every post
// outstanding at that point.
func (c *spanGateCollector) take() bool {
	c.mu.Lock()
	batch := c.parked
	c.parked = nil
	c.mu.Unlock()
	posts := make([]spanGatePost, 0, len(batch))
	for _, conn := range batch {
		post, err := c.read(conn)
		if err != nil {
			c.errs = append(c.errs, err)
			_ = conn.Close()
			continue
		}
		posts = append(posts, post)
	}
	slices.SortFunc(posts, func(a, b spanGatePost) int { return cmp.Compare(b.rank, a.rank) })
	for _, post := range posts {
		c.keys = append(c.keys, post.key)
		c.bodies = append(c.bodies, post.body)
		// A write on the pipe returns once the sender has read all of it.
		if _, err := io.WriteString(post.conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"); err != nil {
			c.errs = append(c.errs, fmt.Errorf("answer %s: %w", post.key, err))
		}
		_ = post.conn.Close()
	}
	return len(batch) > 0
}

func (c *spanGateCollector) read(conn net.Conn) (spanGatePost, error) {
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return spanGatePost{}, fmt.Errorf("read post: %w", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return spanGatePost{}, fmt.Errorf("read post body: %w", err)
	}
	key, err := spanOrderKey(body)
	if err != nil {
		return spanGatePost{}, err
	}
	rank, ok := c.rank[key]
	if !ok {
		return spanGatePost{}, fmt.Errorf("collector received the span of %q, which the test did not submit", key)
	}
	return spanGatePost{conn: conn, key: key, body: body, rank: rank}, nil
}

// serve answers posts until waited is closed and no post is parked, so that no connection is left
// open when the bubble ends on every path where the sender posts or lets Wait return. The bound is
// virtual time and only aborts a sender that does neither.
func (c *spanGateCollector) serve(t *testing.T, waited <-chan struct{}) {
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
			t.Error("the trace exporter neither posted nor returned from Wait within an hour of virtual time")
			return
		}
	}
}

// check reports the collector's failures, then compares the spans it took with want: first as sets,
// so that a lost or extra span is told apart from a reordering, then in order.
func (c *spanGateCollector) check(t *testing.T, want []string) {
	t.Helper()
	for _, err := range c.errs {
		t.Errorf("span gate collector: %v", err)
	}
	sortedWant, sortedGot := slices.Sorted(slices.Values(want)), slices.Sorted(slices.Values(c.keys))
	if d := gocmp.Diff(sortedWant, sortedGot); d != "" {
		t.Errorf("spans the collector received, ignoring order (-want +got):\n%s", d)
		return
	}
	if d := gocmp.Diff(want, c.keys); d != "" {
		t.Errorf("spans reached the collector out of submission order (-submitted +received):\n%s", d)
	}
}

// spanOrderKey names a posted span by its selected_model attribute. It returns an error instead of
// failing the test, so that the collector can still answer every post.
func spanOrderKey(payload []byte) (string, error) {
	var envelope struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					Attributes []struct {
						Key   string         `json:"key"`
						Value map[string]any `json:"value"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", fmt.Errorf("decode post %s: %w", payload, err)
	}
	if len(envelope.ResourceSpans) != 1 || len(envelope.ResourceSpans[0].ScopeSpans) != 1 || len(envelope.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
		return "", fmt.Errorf("post %s does not hold exactly one span", payload)
	}
	for _, attr := range envelope.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes {
		if attr.Key == "selected_model" {
			return fmt.Sprint(attr.Value["stringValue"]), nil
		}
	}
	return "", fmt.Errorf("post %s has no selected_model attribute", payload)
}

// spanOrderPayload is the smallest trace export spanOrderKey accepts, for the collector's control.
func spanOrderPayload(model string) []byte {
	return []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"attributes":[{"key":"selected_model","value":{"stringValue":"` + model + `"}}]}]}]}]}`)
}

// TestTraceSubmissionOrder checks that the route spans the recorder is given reach the collector in
// the order they finished, as in the source: a request's span is posted when its route finishes
// (lib/request-worker.js:1106), and each post starts at once in that call (:984-1008), so spans
// leave in finish order, not start order. The collector parks every post until the bubble is quiet
// and records posts that were outstanding together in reverse, so the test does not depend on how
// the goroutines happen to be scheduled.
func TestTraceSubmissionOrder(t *testing.T) {
	tests := map[string]struct {
		// started lists the selected models of the spans in the order they start.
		started []string
		// finished lists indexes into started in the order the spans finish.
		finished []int
		// want is every span key in submission (finish) order.
		want []string
	}{
		"success: spans finished one after another arrive in that order": {
			started:  []string{"model-a", "model-b", "model-c"},
			finished: []int{0, 1, 2},
			want:     []string{"model-a", "model-b", "model-c"},
		},
		"success: spans arrive in the order they finished, not the order they started": {
			started:  []string{"model-a", "model-b", "model-c"},
			finished: []int{2, 0, 1},
			want:     []string{"model-c", "model-a", "model-b"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			synctest.Test(t, func(t *testing.T) {
				c := newSpanGateCollector(tt.want)
				opts := recorderOptions(map[string]string{"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "http://127.0.0.1:1/v1/traces", "CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1"})
				// Each unlinked span reads a trace id, a span id and a route id: 40 bytes.
				opts.Random = bytes.NewReader(bytes.Repeat([]byte{0x11}, 40*len(tt.started)))
				r := newRecorder(opts, c.dial)
				if !r.Enabled() {
					t.Fatal("test precondition: the recorder does not export spans")
				}
				spans := make([]*Span, len(tt.started))
				for i := range tt.started {
					spans[i] = r.Start(nil)
				}
				// Finish queues each span without waiting for the collector; TestTraceExportFailure
				// already requires that it returns while the collector never replies.
				for _, index := range tt.finished {
					spans[index].Finish(Route{Backend: "codex", SelectedModel: tt.started[index]}, http.StatusOK, "")
				}
				waited := make(chan struct{})
				go func() {
					defer close(waited)
					r.Wait()
				}()
				c.serve(t, waited)
				c.check(t, tt.want)
			})
		})
	}
}

// TestSpanGateCollector shows that the collector of TestTraceSubmissionOrder records exactly what it
// receives: posts sent one after another, each after the previous one was answered, are recorded in
// that order with their bodies intact, and posts outstanding together are recorded in reverse,
// however the goroutines that sent them were scheduled.
func TestSpanGateCollector(t *testing.T) {
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
				submitted := []string{"model-a", "model-b", "model-c"}
				payloads := make([][]byte, len(submitted))
				for i, model := range submitted {
					payloads[i] = spanOrderPayload(model)
					if key, err := spanOrderKey(payloads[i]); err != nil || key != model {
						t.Fatalf("test precondition: payload %d is %q (%v), want %q", i, key, err, model)
					}
				}
				c := newSpanGateCollector(submitted)
				client := &http.Client{Transport: &http.Transport{DialContext: c.dial, DisableKeepAlives: true, DisableCompression: true}}
				post := func(payload []byte) {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:1/v1/traces", bytes.NewReader(payload))
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
				if tt.together {
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
