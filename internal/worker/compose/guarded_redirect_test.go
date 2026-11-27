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

package compose_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
)

// redirectModel is a proxy model whose compaction requests the stream guard takes.
const redirectModel = "claude-gpt-6.1-sol[1m]"

// redirectWireRecorder is a client writer that keeps every head it is given, the header map each
// head committed, and every body byte, so a second head or a stray heartbeat is visible. Writes
// after markDeparted fail as a closed connection does and are counted as late.
type redirectWireRecorder struct {
	mu       sync.Mutex
	header   http.Header
	heads    []int
	headers  []http.Header
	body     bytes.Buffer
	departed bool
	late     int
}

func newRedirectWireRecorder() *redirectWireRecorder {
	return &redirectWireRecorder{header: http.Header{}}
}

func (w *redirectWireRecorder) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.header
}

func (w *redirectWireRecorder) countLocked() {
	if w.departed {
		w.late++
	}
}

func (w *redirectWireRecorder) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.countLocked()
	w.heads = append(w.heads, code)
	w.headers = append(w.headers, w.header.Clone())
}

func (w *redirectWireRecorder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.countLocked()
	if w.departed {
		return 0, io.ErrClosedPipe
	}
	if len(w.heads) == 0 {
		// net/http commits an implicit 200 head before the first body byte.
		w.heads = append(w.heads, http.StatusOK)
		w.headers = append(w.headers, w.header.Clone())
	}
	return w.body.Write(p)
}

func (w *redirectWireRecorder) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.countLocked()
}

func (w *redirectWireRecorder) markDeparted() {
	w.mu.Lock()
	w.departed = true
	w.mu.Unlock()
}

// redirectWireView is what a recorder holds at one moment. The number of Write and Flush calls is
// left out: a different read split or a harmless flush changes it while the heads and bytes do not.
type redirectWireView struct {
	Heads []int
	Body  string
	Late  int
}

func (w *redirectWireRecorder) view() redirectWireView {
	w.mu.Lock()
	defer w.mu.Unlock()
	return redirectWireView{Heads: append([]int(nil), w.heads...), Body: w.body.String(), Late: w.late}
}

func (w *redirectWireRecorder) firstHeadHeader(name string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.headers) == 0 {
		return nil
	}
	return w.headers[0].Values(name)
}

// redirectWireAnswer is the client's view of one gateway answer read over HTTP.
type redirectWireAnswer struct {
	Status  int
	Header  http.Header
	Body    string
	ReadErr string
}

// redirectRoundTrip posts body to the gateway with a client that follows no redirect, and returns
// what arrived, including a read that broke, as data.
func redirectRoundTrip(t *testing.T, h *harness, body string) redirectWireAnswer {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.gatewaySrv.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Transport:     h.gatewaySrv.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return redirectWireAnswer{ReadErr: "no response: " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	answer := redirectWireAnswer{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: string(raw)}
	if err != nil {
		answer.ReadErr = err.Error()
	}
	// The date is the only field that differs between two runs of the same answer.
	answer.Header.Del("Date")
	assertNoSecrets(t, "redirect answer body", raw)
	return answer
}

// redirectResultsBound is how many terminal results one harness may deliver before the observer
// counts the rest as overflow; no test here sends more than two requests to one harness.
const redirectResultsBound = 4

// redirectResults records every terminal result the worker root delivers, and hands each one to a
// bounded channel at the moment it is delivered, so a test can wait for one request's result before
// it sends the next request.
type redirectResults struct {
	mu        sync.Mutex
	results   []outcome.Result
	delivered chan outcome.Result
	overflow  int
}

func newRedirectResults() *redirectResults {
	return &redirectResults{delivered: make(chan outcome.Result, redirectResultsBound)}
}

func (c *redirectResults) observe(o *worker.Options) {
	start := o.BeforeBody
	o.BeforeBody = func(r *http.Request) func(outcome.Result) {
		var finish func(outcome.Result)
		if start != nil {
			finish = start(r)
		}
		return func(v outcome.Result) {
			if finish != nil {
				finish(v)
			}
			c.mu.Lock()
			c.results = append(c.results, v)
			select {
			case c.delivered <- v:
			default:
				c.overflow++
			}
			c.mu.Unlock()
		}
	}
}

// next waits for the next delivered terminal result. The bound only ends a wait for a result that
// should already have been delivered.
func (c *redirectResults) next(t *testing.T, what string) outcome.Result {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case v := <-c.delivered:
		return v
	case <-timer.C:
		t.Fatalf("no terminal result was delivered for %s", what)
		return outcome.Result{}
	}
}

// none reports a terminal result delivered beyond the ones a test has received.
func (c *redirectResults) none(t *testing.T, what string) {
	t.Helper()
	c.mu.Lock()
	overflow := c.overflow
	c.mu.Unlock()
	select {
	case v := <-c.delivered:
		t.Errorf("%s: an extra terminal result was delivered: %+v", what, redirectFactsOf([]outcome.Result{v}))
	default:
	}
	if overflow != 0 {
		t.Errorf("%s: %d terminal results beyond the observer's bound", what, overflow)
	}
}

func (c *redirectResults) snapshot() []outcome.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]outcome.Result(nil), c.results...)
}

// redirectFacts are the terminal facts a test decides on; the admitted route is the same on both paths.
type redirectFacts struct {
	Status         int
	StatusOverride string
	Contacted      bool
	Cancelled      bool
	Compaction     *outcome.Compaction
}

func redirectFactsOf(results []outcome.Result) []redirectFacts {
	out := make([]redirectFacts, 0, len(results))
	for _, r := range results {
		out = append(out, redirectFacts{Status: r.Status, StatusOverride: r.StatusOverride, Contacted: r.Contacted, Cancelled: r.Cancelled, Compaction: r.Route.Compaction})
	}
	return out
}

// redirectUpstreamAnswer is one non-stream open response a proxy upstream gives.
type redirectUpstreamAnswer struct {
	status      int
	location    string
	contentType string
	body        string
}

func (a redirectUpstreamAnswer) handler(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	if a.location != "" {
		w.Header().Set("Location", a.location)
	}
	if a.contentType != "" {
		w.Header().Set("Content-Type", a.contentType)
	}
	w.Header().Set("X-Redirect-Fixture", "kept")
	w.WriteHeader(a.status)
	_, _ = io.WriteString(w, a.body)
}

// redirectCases are the upstream answers every redirect test runs; their bodies and headers are
// what the client must receive unchanged.
var redirectCases = map[string]redirectUpstreamAnswer{
	"302 with a location and a JSON body": {
		status: http.StatusFound, location: "https://relay.example.test/v1/messages?moved=1", contentType: "application/json",
		body: `{"type":"error","error":{"type":"moved","message":"the endpoint moved"}}`,
	},
	"303 with a location and an HTML body": {
		status: http.StatusSeeOther, location: "/v1/messages/elsewhere", contentType: "text/html; charset=utf-8",
		body: "<html><body><a href=\"/v1/messages/elsewhere\">See Other</a></body></html>\n",
	},
	"307 whose body ends without any event-stream frame": {
		status: http.StatusTemporaryRedirect, location: "https://relay.example.test/v1/messages", contentType: "text/plain",
		body: "temporary redirect; no event follows",
	},
	"307 whose body looks like an incomplete event stream": {
		status: http.StatusTemporaryRedirect, location: "https://relay.example.test/v1/messages", contentType: "text/event-stream",
		body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_redirect\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":7}}}\n\n",
	},
}

// redirectRun is what one path made of one upstream answer.
type redirectRun struct {
	wire          redirectWireAnswer
	wireContacts  int64
	wireFacts     []redirectFacts
	direct        redirectWireView
	directHeader  []string
	directContact int64
	directFacts   []redirectFacts
}

// runRedirectPath sends body once over HTTP and once straight to the handler, on a fresh harness
// whose proxy upstream gives answer, and counts the upstream contacts of each.
func runRedirectPath(t *testing.T, answer redirectUpstreamAnswer, body string) redirectRun {
	t.Helper()
	results := newRedirectResults()
	h := newHarness(t, harnessConfig{disableUsage: true, disableTrace: true, proxyHandler: answer.handler, observeOptions: results.observe})
	var run redirectRun
	run.wire = redirectRoundTrip(t, h, body)
	// The client can read a short body to its end while the worker is still in the relay, so the
	// HTTP request's result is received before the direct request starts.
	run.wireFacts = redirectFactsOf([]outcome.Result{results.next(t, "the request over HTTP")})
	run.wireContacts = h.proxyHits.Load()

	rec := newRedirectWireRecorder()
	serveDirect(t.Context(), t, h, rec, body, nil)
	run.directFacts = redirectFactsOf([]outcome.Result{results.next(t, "the request served directly")})
	run.direct = rec.view()
	run.directHeader = rec.firstHeadHeader("Location")
	run.directContact = h.proxyHits.Load() - run.wireContacts
	results.none(t, "after both requests")
	return run
}

// TestGuardedRedirectRelayedOnce decides what a guarded compaction request makes of a proxy upstream
// that answers with a redirect: the status, the relay headers and the body bytes reach the client
// exactly once and unchanged, the upstream is contacted once, nothing else is written, and the
// terminal result keeps the compaction outcome recorded for a non-2xx status. A request the guard
// does not take gets the same client bytes from the same upstream answer.
func TestGuardedRedirectRelayedOnce(t *testing.T) {
	for name, answer := range redirectCases {
		t.Run("success: "+name, func(t *testing.T) {
			guarded := runRedirectPath(t, answer, compactRequestJSON(redirectModel))
			unguarded := runRedirectPath(t, answer, messageRequestJSON(redirectModel, true))

			wantStatus := answer.status
			wantCompaction := &outcome.Compaction{Outcome: "upstream_error", ErrorCode: "upstream_http_error", UpstreamStatus: &wantStatus}
			paths := map[string]struct {
				run        redirectRun
				compaction *outcome.Compaction
			}{
				"guarded":   {guarded, wantCompaction},
				"unguarded": {unguarded, nil},
			}
			for label, path := range paths {
				run := path.run
				if diff := gocmp.Diff(answer.status, run.wire.Status); diff != "" {
					t.Errorf("%s: client status (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff(answer.location, run.wire.Header.Get("Location")); diff != "" {
					t.Errorf("%s: client Location header (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff("kept", run.wire.Header.Get("X-Redirect-Fixture")); diff != "" {
					t.Errorf("%s: relayed upstream header (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff(answer.body, run.wire.Body); diff != "" {
					t.Errorf("%s: client body bytes (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff("", run.wire.ReadErr); diff != "" {
					t.Errorf("%s: client read (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff(int64(1), run.wireContacts); diff != "" {
					t.Errorf("%s: upstream contacts for one request over HTTP (-want +got):\n%s", label, diff)
				}
				wantFacts := []redirectFacts{{Status: answer.status, Contacted: true, Compaction: path.compaction}}
				if diff := gocmp.Diff(wantFacts, run.wireFacts); diff != "" {
					t.Errorf("%s: terminal result over HTTP (-want +got):\n%s", label, diff)
				}

				wantDirect := redirectWireView{Heads: []int{answer.status}, Body: answer.body}
				if diff := gocmp.Diff(wantDirect, run.direct); diff != "" {
					t.Errorf("%s: heads and body written to the handler's writer (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff([]string{answer.location}, run.directHeader); diff != "" {
					t.Errorf("%s: Location committed with the head (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff(int64(1), run.directContact); diff != "" {
					t.Errorf("%s: upstream contacts for one request served directly (-want +got):\n%s", label, diff)
				}
				if diff := gocmp.Diff(wantFacts, run.directFacts); diff != "" {
					t.Errorf("%s: terminal result served directly (-want +got):\n%s", label, diff)
				}
			}

			if diff := gocmp.Diff(unguarded.wire, guarded.wire); diff != "" {
				t.Errorf("client answer over HTTP, guarded against unguarded (-unguarded +guarded):\n%s", diff)
			}
			if diff := gocmp.Diff(unguarded.wireContacts, guarded.wireContacts); diff != "" {
				t.Errorf("upstream contacts, guarded against unguarded (-unguarded +guarded):\n%s", diff)
			}
			if diff := gocmp.Diff(unguarded.direct, guarded.direct); diff != "" {
				t.Errorf("heads, body and late writes served directly, guarded against unguarded (-unguarded +guarded):\n%s", diff)
			}
		})
	}
}

// redirectOutcomeLabel is the label and upstream status of a compaction outcome; the zero value
// stands for no outcome.
type redirectOutcomeLabel struct {
	Outcome        string
	UpstreamStatus int
}

func redirectOutcomeLabelOf(c *outcome.Compaction) redirectOutcomeLabel {
	if c == nil {
		return redirectOutcomeLabel{}
	}
	label := redirectOutcomeLabel{Outcome: c.Outcome}
	if c.UpstreamStatus != nil {
		label.UpstreamStatus = *c.UpstreamStatus
	}
	return label
}

// TestGuardedStreamControls holds the guarded answers the redirect ruling leaves as they are: a
// successful event stream that ends incomplete is retried, and a successful answer that is not an
// event stream is settled by the reply handler.
func TestGuardedStreamControls(t *testing.T) {
	const (
		controlDiscardedStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_control_discarded\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":11}}}\n\n"
		controlDiscardedText  = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"discarded partial summary\"}}\n\n"
		controlStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_control_delivered\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":11}}}\n\n"
		controlText  = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"delivered summary\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
		controlDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n"
		controlStop  = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	)
	tests := map[string]struct {
		handler      func(attempt int64) func(http.ResponseWriter, *http.Request)
		wantStatus   int
		wantContacts int64
		check        func(t *testing.T, body string)
		// wantFacts leaves the compaction outcome out; wantOutcome names its label and status.
		wantFacts   []redirectFacts
		wantOutcome []redirectOutcomeLabel
	}{
		"success: a guarded event stream that ends incomplete is retried and the complete attempt is delivered": {
			handler: func(attempt int64) func(http.ResponseWriter, *http.Request) {
				return func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					if attempt == 1 {
						// Incomplete: no message_stop, so the guard discards this attempt and retries.
						_, _ = io.WriteString(w, controlDiscardedStart+controlDiscardedText)
						return
					}
					_, _ = io.WriteString(w, controlStart+controlText+controlDelta+controlStop)
				}
			},
			wantStatus:   http.StatusOK,
			wantContacts: 2,
			check: func(t *testing.T, body string) {
				t.Helper()
				if diff := gocmp.Diff(1, strings.Count(body, "event: message_stop\n")); diff != "" {
					t.Errorf("terminal frames delivered (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(1, strings.Count(body, "event: message_start\n")); diff != "" {
					t.Errorf("start frames delivered; the discarded attempt must not reach the client (-want +got):\n%s", diff)
				}
				delivered := map[string]bool{
					`"msg_control_delivered"`:     strings.Contains(body, `"msg_control_delivered"`),
					`"delivered summary"`:         strings.Contains(body, `"delivered summary"`),
					`"msg_control_discarded"`:     strings.Contains(body, `"msg_control_discarded"`),
					`"discarded partial summary"`: strings.Contains(body, `"discarded partial summary"`),
				}
				want := map[string]bool{
					`"msg_control_delivered"`:     true,
					`"delivered summary"`:         true,
					`"msg_control_discarded"`:     false,
					`"discarded partial summary"`: false,
				}
				if diff := gocmp.Diff(want, delivered); diff != "" {
					t.Errorf("which attempt's id and summary text reached the client (-want +got):\n%s", diff)
				}
			},
			wantFacts:   []redirectFacts{{Status: http.StatusOK, Contacted: true}},
			wantOutcome: []redirectOutcomeLabel{{Outcome: "completed", UpstreamStatus: http.StatusOK}},
		},
		"success: a guarded successful JSON answer is settled by the reply handler": {
			handler: func(int64) func(http.ResponseWriter, *http.Request) {
				return func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"msg_json","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"summary"}],"usage":{"input_tokens":9,"output_tokens":3}}`)
				}
			},
			wantStatus:   http.StatusOK,
			wantContacts: 1,
			check: func(t *testing.T, body string) {
				t.Helper()
				want := decodeJSONObject(t, []byte(`{"id":"msg_json","type":"message","model":"claude-gpt-6.1-sol[1m]","stop_reason":"end_turn","content":[{"type":"text","text":"summary"}],"usage":{"input_tokens":9,"output_tokens":3}}`))
				if diff := gocmp.Diff(want, decodeJSONObject(t, []byte(body))); diff != "" {
					t.Errorf("settled JSON body (-want +got):\n%s", diff)
				}
			},
			wantFacts:   []redirectFacts{{Status: http.StatusOK, Contacted: true}},
			wantOutcome: []redirectOutcomeLabel{{}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			results := newRedirectResults()
			var attempts struct {
				mu sync.Mutex
				n  int64
			}
			h := newHarness(t, harnessConfig{
				disableUsage: true, disableTrace: true, observeOptions: results.observe,
				proxyHandler: func(w http.ResponseWriter, r *http.Request) {
					attempts.mu.Lock()
					attempts.n++
					n := attempts.n
					attempts.mu.Unlock()
					tt.handler(n)(w, r)
				},
			})
			answer := redirectRoundTrip(t, h, compactRequestJSON(redirectModel))
			if diff := gocmp.Diff("", answer.ReadErr); diff != "" {
				t.Errorf("client read (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantStatus, answer.Status); diff != "" {
				t.Errorf("client status (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantContacts, h.proxyHits.Load()); diff != "" {
				t.Errorf("upstream contacts (-want +got):\n%s", diff)
			}
			tt.check(t, answer.Body)
			facts := redirectFactsOf([]outcome.Result{results.next(t, "the guarded request")})
			results.none(t, "after the guarded request")
			labels := make([]redirectOutcomeLabel, 0, len(facts))
			for i := range facts {
				labels = append(labels, redirectOutcomeLabelOf(facts[i].Compaction))
				facts[i].Compaction = nil
			}
			if diff := gocmp.Diff(tt.wantFacts, facts); diff != "" {
				t.Errorf("terminal result (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantOutcome, labels); diff != "" {
				t.Errorf("compaction outcome of the terminal result (-want +got):\n%s", diff)
			}
		})
	}
}
