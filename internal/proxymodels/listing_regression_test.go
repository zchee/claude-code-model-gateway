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

package proxymodels_test

import (
	"encoding/json/jsontext"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

func TestParseIgnoresIllFormedIDs(t *testing.T) {
	const pairedEmoji = "\x5cud83d\x5cude00"
	tests := map[string]struct {
		parse func(jsontext.Value) []proxymodels.Route
		input string
		want  []proxymodels.Route
	}{
		"success: retain real replacement character and discovery order": {
			parse: proxymodels.ParseListing,
			input: `[
				{"id":"gpt-first"},
				{"id":"x\ud800","display_name":"x\ud800 (wrong)"},
				{"id":"x\ud801","display_name":"x\ud801 (wrong)"},
				{"id":"x�","display_name":"x� (kimi)"},
				{"id":"x�","display_name":"x� (other)"},
				{"id":"x` + pairedEmoji + `","display_name":"x😀 (vertex)"}
			]`,
			want: []proxymodels.Route{
				{ID: "gpt-first", Provider: "codex"},
				{ID: "x�", Provider: "kimi"},
				{ID: "x😀", Provider: "vertex"},
			},
		},
		"success: raw invalid UTF-8 does not discard neighboring rows": {
			parse: proxymodels.ParseListing,
			input: `[{"id":"gpt-bad` + "\xff" + `"},{"id":"gpt-bad�","display_name":"gpt-bad� (vertex)"}]`,
			want:  []proxymodels.Route{{ID: "gpt-bad�", Provider: "vertex"}},
		},
		"success: legacy file skips both malformed forms": {
			parse: proxymodels.ModelsFileRoutes,
			input: `["gpt-lo\udfff","gpt-hi\ud800","gpt-bad` + "\xff" + `","gpt-next","gpt-�","gpt-next"]`,
			want:  []proxymodels.Route{{ID: "gpt-next", Provider: "codex"}, {ID: "gpt-�", Provider: "codex"}},
		},
		"success: malformed display name cannot attribute a provider": {
			parse: proxymodels.ParseListing,
			input: `[
				{"id":"x�","display_name":"x\ud800 (wrong)"},
				{"id":"x�","display_name":"x� (kimi)"},
				{"id":"gpt-�","display_name":"gpt-` + "\xff" + ` (wrong)"},
				{"id":"gpt-other","display_name":"gpt-other (bad\ud800)"}
			]`,
			want: []proxymodels.Route{
				{ID: "x�", Provider: "kimi"},
				{ID: "gpt-�", Provider: "codex"},
				{ID: "gpt-other", Provider: "codex"},
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, tt.parse(jsontext.Value(tt.input))); diff != "" {
				t.Errorf("routes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseListingMemberNames(t *testing.T) {
	const escapedID = "\x5cu0069d"
	const escapedDisplayName = "display\x5cu005fname"
	tests := map[string]struct {
		input string
		want  []proxymodels.Route
	}{
		"success: last duplicate id and display name win": {
			input: `[{"id":"wrong","id":"x","display_name":"x (wrong)","display_name":"x (vertex)"}]`,
			want:  []proxymodels.Route{{ID: "x", Provider: "vertex"}},
		},
		"success: decoded member names match exactly": {
			input: `[{"id":"wrong","` + escapedID + `":"x","ID":"wrong","` + escapedDisplayName + `":"x (kimi)","DISPLAY_NAME":"x (wrong)"}]`,
			want:  []proxymodels.Route{{ID: "x", Provider: "kimi"}},
		},
		"success: last duplicate can invalidate id": {
			input: `[{"id":"gpt-old","` + escapedID + `":"gpt-old\ud800"},{"id":"gpt-next"}]`,
			want:  []proxymodels.Route{{ID: "gpt-next", Provider: "codex"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, proxymodels.ParseListing(jsontext.Value(tt.input))); diff != "" {
				t.Errorf("ParseListing (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAdvertisedRoutesNonGemini(t *testing.T) {
	bare := proxymodels.Route{ID: "endpoint/my-llm", Provider: "vertex"}
	twin := proxymodels.Route{ID: "vertex/endpoint/my-llm", Provider: "vertex"}
	tests := map[string]struct{ routes, want []proxymodels.Route }{
		"success: drop a non-Gemini Vertex twin": {[]proxymodels.Route{twin, bare}, []proxymodels.Route{bare}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, proxymodels.AdvertisedRoutes(tt.routes)); diff != "" {
				t.Errorf("AdvertisedRoutes (-want +got):\n%s", diff)
			}
		})
	}
}
