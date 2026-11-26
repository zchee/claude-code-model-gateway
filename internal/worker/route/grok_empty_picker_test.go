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

package route_test

import (
	"bytes"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// TestGrokObjectPickerWithEmptyList checks that a snapshot with zero cached Grok IDs routes the picker
// [object Object] by the ordinary no-match rule to grok-[object Object], rather than matching a default model
// object as in lib/request-worker.js:2596-2604 and lib/grok-backend.js:413-423.
func TestGrokObjectPickerWithEmptyList(t *testing.T) {
	tests := map[string]struct {
		grokIDs []string
		body    string
		want    route.Decision
	}{
		"success: object picker with nil Grok IDs uses no-match rule": {
			grokIDs: nil,
			body:    `{"model":"claude-grok-[object Object]"}`,
			want:    route.Decision{Branch: "grok", Provider: "grok", Model: "grok-[object Object]"},
		},
		"success: extended object picker with empty Grok IDs uses no-match rule": {
			grokIDs: []string{},
			body:    `{"model":"claude-grok-[object Object][1m]"}`,
			want:    route.Decision{Branch: "grok", Provider: "grok", Model: "grok-[object Object]"},
		},
		"success: object picker matches an explicit unprefixed entry": {
			grokIDs: []string{"[object Object]"},
			body:    `{"model":"claude-grok-[object Object]"}`,
			want:    route.Decision{Branch: "grok", Provider: "grok", Model: "[object Object]"},
		},
		"success: flattened default version with empty Grok IDs uses no-match rule": {
			grokIDs: []string{},
			body:    `{"model":"claude-grok-4-7"}`,
			want:    route.Decision{Branch: "grok", Provider: "grok", Model: "grok-4-7"},
		},
		"success: flattened version matches when listed": {
			grokIDs: []string{"grok-4.7"},
			body:    `{"model":"claude-grok-4-7"}`,
			want:    route.Decision{Branch: "grok", Provider: "grok", Model: "grok-4.7"},
		},
		"success: non-Grok object picker takes the default route": {
			grokIDs: nil,
			body:    `{"model":"claude-[object Object]"}`,
			want:    route.Decision{Branch: "anthropic"},
		},
		"success: bare Grok backend spelling takes the default route": {
			grokIDs: nil,
			body:    `{"model":"grok-[object Object]"}`,
			want:    route.Decision{Branch: "anthropic"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := route.NewSnapshot(policy.Resolver{}, "fallback", nil, tt.grokIDs)
			body := []byte(tt.body)
			original := bytes.Clone(body)
			got := s.Classify("POST", "/v1/messages", body)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Classify(%s) (-want +got):\n%s", tt.body, diff)
			}
			if !bytes.Equal(original, body) {
				t.Error("request body changed")
			}
		})
	}
}
