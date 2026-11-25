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

package anthropiccatalog_test

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func checkJSON(t *testing.T, want string, got jsontext.Value) {
	t.Helper()
	var expected, actual any
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatalf("invalid result %q: %v", got, err)
	}
	if diff := gocmp.Diff(expected, actual); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestPrevious(t *testing.T) {
	const server = `[{"value":"claude-fable-5-1[1m]","label":"Fable","description":"Fable 5.1 · Most capable for your hardest and longest-running tasks"},{"value":"claude-opus-5-5[1m]"},{"value":"claude-sonnet-6"},{"value":"not-a-model"},null]`
	const previous = `{"id":"claude-fable-5-1[1m]","display_name":"Fable 5.1 (previous)"}`
	const codex = `{"id":"claude-gpt-5.5[1m]","display_name":"GPT-5.5 (Codex)"}`
	current := map[string]string{
		"fable":  "claude-fable-5-2[1m]",
		"opus":   "claude-opus-5-5[1m]",
		"sonnet": "claude-sonnet-5-5[1m]",
	}
	tests := map[string]struct{ models, server, want string }{
		"success: only older family row": {
			"[]",
			server,
			"[" + previous + "]",
		},
		"success: replace stale row keep other models": {
			`[{"id":"claude-fable-5-1[1m]","display_name":"Claude Fable 5.1"},` + codex + `]`,
			server,
			"[" + previous + "," + codex + "]",
		},
		"success: keep server duplicates and skip absent family": {
			"[]",
			`[{"value":"claude-opus-4"},{"value":"claude-haiku-3"},{"value":"claude-opus-4"},{"value":12}]`,
			`[{"id":"claude-opus-4","display_name":"Opus 4 (previous)"},{"id":"claude-opus-4","display_name":"Opus 4 (previous)"}]`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			checkJSON(
				t,
				tt.want,
				anthropiccatalog.WithPrevious(
					jsontext.Value(tt.models),
					jsontext.Value(tt.server),
					current,
				),
			)
		})
	}
}

func TestWithCached(t *testing.T) {
	const cached = `{"id":"claude-fable-5-2","display_name":"current","lifecycle":"active","meta":{"family":"fable"}}`
	const other = `{"id":"gateway","extra":1}`
	tests := map[string]struct{ models, rows, want string }{
		"success: duplicates preserved within each layer": {
			"[" + other + `,{"id":"claude-fable-5-2"},` + other + "]",
			"[" + cached + "," + cached + "]",
			"[" + cached + "," + cached + "," + other + "," + other + "]",
		},
		"success: compare after advertising ids": {
			`[{"id":"claude-gpt-6-astra[1m]"}]`,
			`[{"id":"claude-gpt-6-astra","display_name":"upstream"}]`,
			`[{"id":"claude-gpt-6-astra[1m]","display_name":"upstream"}]`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			checkJSON(
				t,
				tt.want,
				anthropiccatalog.WithCached(
					jsontext.Value(tt.models),
					jsontext.Value(tt.rows),
					policy.Resolver{},
				),
			)
		})
	}
}

func TestRead(t *testing.T) {
	const empty = `{"data":[],"updatedAt":0}`
	const row = `{"id":"claude-a._-0","display_name":"","z":42}`
	tests := map[string]struct{ body, want string }{
		"success: missing file": {
			"",
			empty,
		},
		"success: malformed file":  {"{", empty},
		"success: wrong data type": {`{"data":{},"error":"old","updatedAt":10}`, empty},
		"success: nonobject":       {"null", empty},
		"success: keep whole valid rows and normalize clock": {
			`{"extra":1,"updatedAt":"0x10","error":"bad","attemptedAt":22,"data":[null,{},` + row + `,{"id":"Claude-a","display_name":"a"},{"id":"claude-a[1m]","display_name":"b"},{"id":"claude-a","display_name":42},{"id":"claude-A","display_name":"UPPER"}]}`,
			`{"extra":1,"updatedAt":16,"error":"bad","attemptedAt":22,"data":[` + row + `]}`,
		},
		"success: invalid time becomes zero": {
			`{"data":[],"updatedAt":"abc"}`,
			empty,
		},
		"success: absent time becomes zero": {`{"data":[]}`, empty},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "anthropic-models.json")
			if tt.body != "" {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			checkJSON(t, tt.want, anthropiccatalog.Read(path))
		})
	}
}

func TestCacheBytes(t *testing.T) {
	tests := map[string]struct{ snapshot, rows, message, want string }{
		"success: complete replacement": {
			rows: `[{"display_name":"A & B","id":"claude-a","z_extra":true}]`,
			want: `{
  "data": [
    {
      "display_name": "A & B",
      "id": "claude-a",
      "z_extra": true
    }
  ],
  "updatedAt": 12345,
  "error": null
}
`,
		},
		"success: failure preserves snapshot fields and their positions": {
			snapshot: `{"x_before":1,"error":null,"data":[{"id":"claude-old","display_name":"Old"}],"updatedAt":17,"extra":true}`,
			message:  "upstream HTTP 503",
			want: `{
  "x_before": 1,
  "error": "upstream HTTP 503",
  "data": [
    {
      "id": "claude-old",
      "display_name": "Old"
    }
  ],
  "updatedAt": 17,
  "extra": true,
  "attemptedAt": 12345
}
`,
		},
		"success: first failure": {
			snapshot: `{"data":[],"updatedAt":0}`,
			message:  "catalog timeout",
			want: `{
  "data": [],
  "updatedAt": 0,
  "error": "catalog timeout",
  "attemptedAt": 12345
}
`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state", "anthropic-models.json")
			at := time.UnixMilli(12345)
			var err error
			if tt.message == "" {
				err = anthropiccatalog.WriteSuccess(path, jsontext.Value(tt.rows), at)
			} else {
				err = anthropiccatalog.WriteFailure(
					path,
					jsontext.Value(tt.snapshot),
					tt.message,
					at,
				)
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, string(data)); diff != "" {
				t.Fatalf("cache bytes (-want +got):\n%s", diff)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Errorf("permissions = %o, want 600", got)
			}
		})
	}
}
