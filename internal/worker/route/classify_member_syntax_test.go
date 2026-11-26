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
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// memberSyntaxLimit is the deepest nesting the classifier accepts, counted in open containers.
const memberSyntaxLimit = 10000

// objectAtLimit returns a body whose top-level object holds a proxy model and whose last member opens arrays until an
// inner object is the memberSyntaxLimit-th open container; content is written between that object's braces.
func objectAtLimit(content string) string {
	return `{"model":"claude-gpt-9","x":` + strings.Repeat("[", memberSyntaxLimit-2) + `{` + content + `}` + strings.Repeat("]", memberSyntaxLimit-2) + `}`
}

// openObjectAtLimit returns the same body cut off after content, with no container closed.
func openObjectAtLimit(content string) string {
	return `{"model":"claude-gpt-9","x":` + strings.Repeat("[", memberSyntaxLimit-2) + `{` + content
}

// arrayObjectAtLimit returns a top-level array whose innermost element is an object at the memberSyntaxLimit-th open
// container, with content between its braces.
func arrayObjectAtLimit(content string) string {
	return strings.Repeat("[", memberSyntaxLimit-1) + `{` + content + `}` + strings.Repeat("]", memberSyntaxLimit-1)
}

// TestClassifyMalformedMemberNameAtDepthLimit checks that a body whose object at the nesting limit has invalid syntax
// where a member name is required is ordinary malformed JSON, which goes unchanged to the default upstream, even when
// the offending token would open a container past the limit. A well-formed member whose value crosses the limit is
// still refused for its depth, so the outcome is decided by the member-name syntax and not by the depth of the token.
func TestClassifyMalformedMemberNameAtDepthLimit(t *testing.T) {
	s := route.NewSnapshot(policy.Resolver{}, "fallback", nil, nil)
	anthropic := route.Decision{Branch: "anthropic"}
	codex := route.Decision{Branch: "proxy", Provider: "codex", Model: "gpt-9"}
	tooDeep := route.Decision{Branch: "refused", Reason: route.RefusalNestingDepth}
	tests := map[string]struct {
		method, body string
		want         route.Decision
	}{
		"success: well-formed member at the nesting limit":                      {"POST", objectAtLimit(`"k":0`), codex},
		"success: unquoted member name at the nesting limit":                    {"POST", objectAtLimit(`model:0`), anthropic},
		"success: single-quoted member name at the nesting limit":               {"POST", objectAtLimit(`'k':0`), anthropic},
		"success: array after a name without its colon at the limit":            {"POST", objectAtLimit(`"k"[0]`), anthropic},
		"success: object after a name without its colon at the limit":           {"POST", objectAtLimit(`"k"{"a":0}`), anthropic},
		"success: trailing comma before the closing brace at the limit":         {"POST", objectAtLimit(`"k":0,`), anthropic},
		"success: array member name after a comma at the limit":                 {"POST", objectAtLimit(`"k":0,[0]:1`), anthropic},
		"success: object member name after a comma at the limit":                {"POST", objectAtLimit(`"k":0,{"a":0}:1`), anthropic},
		"success: array member name cut off at the limit":                       {"POST", openObjectAtLimit(`[`), anthropic},
		"success: nested arrays as a member name cut off at the limit":          {"POST", openObjectAtLimit(`[[[[`), anthropic},
		"success: object member name cut off at the limit":                      {"POST", openObjectAtLimit(`{`), anthropic},
		"success: array member name in a top-level array at the limit":          {"POST", arrayObjectAtLimit(`[0]:1`), anthropic},
		"success: unquoted member name for a method other than POST":            {"PUT", objectAtLimit(`model:[0]`), anthropic},
		"error: array value of a well-formed member past the limit":             {"POST", objectAtLimit(`"k":[0]`), tooDeep},
		"error: object value of a well-formed member past the limit":            {"POST", objectAtLimit(`"k":{}`), tooDeep},
		"error: array value cut off right after crossing the limit":             {"POST", openObjectAtLimit(`"k":[`), tooDeep},
		"error: array value after a well-formed member past the limit":          {"POST", objectAtLimit(`"k":0,"m":[0]`), tooDeep},
		"error: array value in a top-level array past the limit":                {"POST", arrayObjectAtLimit(`"k":[0]`), tooDeep},
		"error: well-formed member past the limit for a method other than POST": {"PUT", objectAtLimit(`"k":[0]`), tooDeep},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := []byte(tt.body)
			original := bytes.Clone(body)
			if diff := gocmp.Diff(tt.want, s.Classify(tt.method, "/v1/messages", body)); diff != "" {
				t.Errorf("Classify(%q, /v1/messages) (-want +got):\n%s", tt.method, diff)
			}
			if !bytes.Equal(original, body) {
				t.Error("request body changed")
			}
		})
	}
}
