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
	"os"
	"path/filepath"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

func TestMergeIdentity(t *testing.T) {
	tests := map[string]struct{ cached, models, want string }{
		"success: ignore unpaired surrogate without hiding literal replacement": {
			`[{"id":"claude-\ud800","display_name":"bad"}]`,
			`[{"id":"claude-\udc00"},{"id":"claude-�"}]`, `[{"id":"claude-�"}]`,
		},
		"success: preserve valid surrogate pair and suppress only its duplicate": {
			`[{"id":"claude-😀","display_name":"pair"}]`,
			`[{"id":"claude-😀"},{"id":"claude-�"}]`, `[{"id":"claude-😀","display_name":"pair"},{"id":"claude-�"}]`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			checkJSON(t, tt.want, anthropiccatalog.WithCached(jsontext.Value(tt.models), jsontext.Value(tt.cached), policy.Resolver{}))
		})
	}
}

func TestTimestampConversion(t *testing.T) {
	tests := map[string]struct{ stamp, want string }{
		"success: nested singleton":                        {`[["0b101"]]`, `{"data":[],"extra":true,"updatedAt":5}`},
		"success: ordinary object gives zero":              {`{}`, `{"data":[],"extra":true,"updatedAt":0}`},
		"error: shadowed conversion resets whole snapshot": {`[1,{"toString":false}]`, `{"data":[],"updatedAt":0}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "catalog.json")
			if err := os.WriteFile(path, []byte(`{"data":[],"extra":true,"updatedAt":`+tt.stamp+`}`), 0o600); err != nil {
				t.Fatal(err)
			}
			checkJSON(t, tt.want, anthropiccatalog.Read(path).Object)
		})
	}
}
