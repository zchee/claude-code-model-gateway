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

package sentry

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

func TestIdentifierEncoding(t *testing.T) {
	tests := map[string]struct {
		session string
		agent   *string
		want    string
	}{
		"error: ill-formed session":              {"\xff", nil, ""},
		"success: replacement character session": {"�", nil, `["�",null]`},
		"error: ill-formed agent":                {"s", new("\xff"), ""},
		"success: replacement character agent":   {"s", new("�"), `["s","�"]`},
		"success: absent agent":                  {"s", nil, `["s",null]`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, SessionKey(true, tt.session, tt.agent)); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestModelIdentity(t *testing.T) {
	tests := map[string]struct {
		id, key string
		enabled bool
	}{
		"error: ill-formed model":                   {"gpt-\xff", "", false},
		"success: model with replacement character": {"gpt-�", "gpt-�", true},
		"success: absent model uses default key":    {"", "default", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveTarget(policy.Resolver{}, tt.id, 0, 0, 0)
			if err != nil || got.Key != tt.key || (got.Policy != nil) != tt.enabled {
				t.Errorf("ResolveTarget(%q) = %+v, %v; want key %q, enabled %t", tt.id, got, err, tt.key, tt.enabled)
			}
		})
	}
}

func TestDisabledIdentityStoresNothing(t *testing.T) {
	tests := map[string]struct{ session, model string }{
		"error: invalid direct session key": {"\xff", "gpt-�"},
		"error: invalid direct model key":   {"�", "gpt-\xff"},
		"error: disabled session":           {"", "gpt-�"},
		"error: disabled model":             {"�", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s State
			m := Target{Key: tt.model, Policy: &policy.Sentry{BackendWindow: 110, CompactTrigger: 100, Source: "derived"}, Headroom: 10}
			s.Observe(tt.session, m, []byte(`{"type":"message_delta","usage":{"input_tokens":115}}`), true)
			s.ObserveJSON(tt.session, m, []byte(`{}`), []byte(`{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":150}}`))
			if _, refused := s.Check(tt.session, m, []byte(`{}`)); refused {
				t.Error("disabled request was refused")
			}
			if got := s.Overflow(tt.session, m, true); got != 0 {
				t.Errorf("disabled overflow returned usage %v", got)
			}
			if len(s.sessions) != 0 || len(s.models) != 0 {
				t.Errorf("disabled request retained %d latches and %d model entries", len(s.sessions), len(s.models))
			}
		})
	}
}
