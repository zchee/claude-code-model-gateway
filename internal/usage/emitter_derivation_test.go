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
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// derivationRandom is a builder's identifier source that counts its reads once armed and panics
// once, on the armed read numbered panicAt (from 1); a zero panicAt never panics. Every
// identifier is one 16-byte read.
type derivationRandom struct {
	mu      sync.Mutex
	next    byte
	armed   bool
	reads   int
	panicAt int
	fired   bool
}

func (r *derivationRandom) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.armed {
		r.reads++
		if !r.fired && r.reads == r.panicAt {
			r.fired = true
			panic("injected derivation panic")
		}
	}
	for i := range p {
		p[i] = r.next
		r.next++
	}
	return len(p), nil
}

// arm restarts the read count and makes the read numbered panicAt panic.
func (r *derivationRandom) arm(panicAt int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed, r.reads, r.panicAt = true, 0, panicAt
}

// disarm stops counting and returns the number of reads made since arm.
func (r *derivationRandom) disarm() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed = false
	return r.reads
}

// eventNames returns the sorted event names of exported payloads.
func eventNames(t *testing.T, payloads [][]byte) []string {
	t.Helper()
	names := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		name, _ := logFields(t, payload)["event.name"].(string)
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestEmitterDerivationPanic checks what survives a panic while the emitter derives the records of
// a token-usage turn. The emitter advances the session's tool-result baseline before it derives
// any record, and schedules the derived records only after all of them are built: a panic while
// deriving either the MCP footprint record or the tool-result record leaves the primary record as
// the turn's only export, and the next turn measures its tool results against the panicking
// turn's messages. The source emits the footprint records before it advances the baseline and
// before it derives the tool-result records, so there a panic in the footprint leaves the old
// baseline, and a panic in the tool-result record leaves the footprint exported. Finish called
// again on the capture whose emitter panicked returns nil without drawing an identifier or
// exporting a record.
func TestEmitterDerivationPanic(t *testing.T) {
	const (
		tools    = `"tools":[{"name":"mcp__alpha__one","description":"alpha schema"}]`
		messages = `{"role":"user","content":"run the tool"},{"role":"assistant","content":[{"type":"tool_use","id":"tool-exact","name":"mcp__alpha__one","input":{}}]}`
		// baseBody holds a tool use; nextBody adds its result, the one tool-result block the
		// emitter attributes context growth to after baseBody.
		baseBody = `{` + tools + `,"messages":[` + messages + `]}`
		nextBody = `{` + tools + `,"messages":[` + messages + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-exact","content":"exact result"}]}]}`
		primary  = "gateway.token.usage"
		foot     = "gateway.mcp.footprint"
		tool     = "gateway.tool_result.usage"
	)
	// The literal record sets each turn exports: a turn with one MCP server derives one footprint
	// record, and a turn whose context grew over one added tool result one tool-result record.
	withoutTool := []string{foot, primary}
	withTool := []string{foot, primary, tool}
	tests := map[string]struct {
		// fromEnd places the panic on the derivation read counted back from the last read of the
		// panicking turn's Finish: the footprint record is derived before the tool-result record,
		// so 2 is the footprint and 1 the tool-result record. Zero runs the turn without a panic.
		fromEnd int
		// skipMiddle leaves out the middle turn, so the last turn follows the first directly.
		skipMiddle bool
		wantMiddle []string
		wantLast   []string
	}{
		"success: control without a panic attributes the result in the middle turn": {
			wantMiddle: withTool, wantLast: withoutTool,
		},
		"success: control without a middle turn attributes the result in the last turn": {
			skipMiddle: true, wantLast: withTool,
		},
		"error: panic deriving the footprint record keeps the advanced baseline": {
			fromEnd: 2, wantMiddle: []string{primary}, wantLast: withoutTool,
		},
		"error: panic deriving the tool-result record drops the built footprint": {
			fromEnd: 1, wantMiddle: []string{primary}, wantLast: withoutTool,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				random := &derivationRandom{}
				c := &pipeCollector{}
				e := c.emitter(NewBuilder(Options{ProjectsDir: t.TempDir(), Now: func() time.Time { return time.Unix(123, 0) }, Random: random}))
				exported := 0
				// turn runs one capture to Finish, waits for its exports and returns them, the
				// Finish panic, the capture and the number of identifier reads Finish made.
				turn := func(body string, input, panicAt int) ([]string, any, *Capture, int) {
					t.Helper()
					capture := e.Start(Request{Body: wire(t, body), BodyBytes: int64(len(body)), Headers: http.Header{"X-Claude-Code-Session-Id": {"session-derivation"}}})
					capture.SetResponse(http.StatusOK, nil)
					capture.ObserveJSON([]byte(`{"usage":{"input_tokens":` + strconv.Itoa(input) + `,"output_tokens":1}}`))
					random.arm(panicAt)
					var panicked any
					func() {
						defer func() { panicked = recover() }()
						capture.Finish()
					}()
					reads := random.disarm()
					e.Wait()
					all := c.received(t)
					names := eventNames(t, all[exported:])
					exported = len(all)
					return names, panicked, capture, reads
				}

				if got, panicked, _, _ := turn(baseBody, 100, 0); panicked != nil || !slices.Equal(got, withoutTool) {
					t.Fatalf("first turn exported %q (panic %v), want %q", got, panicked, withoutTool)
				}
				if !tt.skipMiddle {
					panicAt := 0
					if tt.fromEnd != 0 {
						// A control run on its own emitter finds the number of reads the middle
						// turn's Finish makes when nothing panics.
						reads := derivationReads(t, baseBody, nextBody)
						if reads < 3 {
							t.Fatalf("test precondition: the middle turn's Finish made %d identifier reads, want the primary record and two derived records", reads)
						}
						panicAt = reads + 1 - tt.fromEnd
					}
					got, panicked, capture, _ := turn(nextBody, 137, panicAt)
					if tt.fromEnd != 0 {
						if diff := gocmp.Diff(any("injected derivation panic"), panicked); diff != "" {
							t.Errorf("panic reaching the caller of Finish (-want +got):\n%s", diff)
						}
						// The capture's one eligible Finish has run: a second call returns nil,
						// does not panic, draws no identifier and exports nothing.
						random.arm(0)
						var again *Record
						var repanicked any
						func() {
							defer func() { repanicked = recover() }()
							again = capture.Finish()
						}()
						reads := random.disarm()
						e.Wait()
						all := c.received(t)
						if again != nil || repanicked != nil || reads != 0 || len(all) != exported {
							t.Errorf("second Finish = (%+v, panic %v) with %d identifier reads and %d new exports, want nil, no panic, 0 and 0", again, repanicked, reads, len(all)-exported)
						}
						exported = len(all)
					} else if panicked != nil {
						t.Fatalf("middle turn panicked without an injected panic: %v", panicked)
					}
					if diff := gocmp.Diff(tt.wantMiddle, got); diff != "" {
						t.Errorf("middle turn exports (-want +got):\n%s", diff)
					}
				}
				got, panicked, _, _ := turn(nextBody, 150, 0)
				if panicked != nil {
					t.Fatalf("last turn panicked: %v", panicked)
				}
				if diff := gocmp.Diff(tt.wantLast, got); diff != "" {
					t.Errorf("last turn exports (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// derivationReads runs a first turn with first and a second with second through a fresh emitter
// with export disabled, and returns the number of identifier reads the second turn's Finish made.
func derivationReads(t *testing.T, first, second string) int {
	t.Helper()
	random := &derivationRandom{}
	e := NewEmitter(func(k string) (string, bool) { return "0", k == "CODEX_GATEWAY_USAGE_ENDPOINT" }, NewBuilder(Options{ProjectsDir: t.TempDir(), Now: func() time.Time { return time.Unix(123, 0) }, Random: random}))
	defer e.Wait()
	for i, body := range []string{first, second} {
		capture := e.Start(Request{Body: wire(t, body), BodyBytes: int64(len(body)), Headers: http.Header{"X-Claude-Code-Session-Id": {"session-derivation"}}})
		capture.SetResponse(http.StatusOK, nil)
		capture.ObserveJSON([]byte(`{"usage":{"input_tokens":` + strconv.Itoa(100+37*i) + `,"output_tokens":1}}`))
		random.arm(0)
		if capture.Finish() == nil {
			t.Fatal("control turn returned no primary record")
		}
		if reads := random.disarm(); i == 1 {
			return reads
		}
	}
	return 0
}
