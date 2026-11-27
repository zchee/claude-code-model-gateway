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

package sse_test

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
)

// callbackPanic is the value the callbacks below panic with.
type callbackPanic struct{ Callback string }

// TestObservationCallbacksAreNotRecovered checks that a panic in an observation callback reaches
// the caller of the Transformer or EventObserver method that ran it, unchanged. The source
// catches exceptions from its passthrough observer and its overflow callback; here a callback's
// panic is the caller's to handle, and nothing in the observation path swallows it. In the
// end-of-stream cases the call that hands over the unterminated frame must return without running
// the callback, and only the End call may panic.
func TestObservationCallbacksAreNotRecovered(t *testing.T) {
	const frame = "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n"
	panicking := func(name string) func(jsonx.WireValue) {
		return func(jsonx.WireValue) { panic(callbackPanic{Callback: name}) }
	}
	tests := map[string]struct {
		// prepare builds the object under test and returns its first call and, for the
		// end-of-stream cases, the End call that must run the callback.
		prepare func(t *testing.T) (first, end func())
		want    callbackPanic
	}{
		"error: a panic in the transformer's Observe reaches the Feed caller": {
			prepare: func(t *testing.T) (first, end func()) {
				tr := sse.NewTransformer(sse.TransformerConfig{Provider: "codex", Observe: panicking("transformer observe")})
				return func() { _ = tr.Feed([]byte(frame), func([]byte) error { return nil }) }, nil
			},
			want: callbackPanic{Callback: "transformer observe"},
		},
		"error: a panic in the transformer's Observe reaches the End caller for an unterminated frame": {
			prepare: func(t *testing.T) (first, end func()) {
				tr := sse.NewTransformer(sse.TransformerConfig{Provider: "codex", Observe: panicking("transformer observe")})
				first = func() {
					if err := tr.Feed([]byte(frame[:len(frame)-2]), func([]byte) error { return nil }); err != nil {
						t.Errorf("Feed of an unterminated frame: %v", err)
					}
				}
				return first, func() { _ = tr.End(func([]byte) error { return nil }) }
			},
			want: callbackPanic{Callback: "transformer observe"},
		},
		"error: a panic in the event observer's observe reaches the Write caller": {
			prepare: func(t *testing.T) (first, end func()) {
				obs := sse.NewEventObserver(0, panicking("observer observe"), nil)
				return func() { obs.Write([]byte(frame)) }, nil
			},
			want: callbackPanic{Callback: "observer observe"},
		},
		"error: a panic in the event observer's observe reaches the End caller for an unterminated frame": {
			prepare: func(t *testing.T) (first, end func()) {
				obs := sse.NewEventObserver(0, panicking("observer observe"), nil)
				return func() { obs.Write([]byte(frame[:len(frame)-2])) }, obs.End
			},
			want: callbackPanic{Callback: "observer observe"},
		},
		"error: a panic in the event observer's overflow callback reaches the Write caller": {
			prepare: func(t *testing.T) (first, end func()) {
				obs := sse.NewEventObserver(len(frame)-1, nil, func() { panic(callbackPanic{Callback: "observer overflow"}) })
				return func() { obs.Write([]byte(frame)) }, nil
			},
			want: callbackPanic{Callback: "observer overflow"},
		},
	}
	// recovered runs call and returns the value of a panic that reached it, or nil.
	recovered := func(call func()) (value any) {
		defer func() { value = recover() }()
		call()
		return nil
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			first, end := tt.prepare(t)
			got := recovered(first)
			if end != nil {
				if got != nil {
					t.Fatalf("the call before End panicked with %#v; the callback must not run before End", got)
				}
				got = recovered(end)
			}
			if diff := gocmp.Diff(any(tt.want), got); diff != "" {
				t.Errorf("value that reached the caller (-want panic +got recovered):\n%s", diff)
			}
		})
	}
}
