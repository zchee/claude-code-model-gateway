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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

func TestRawIdentifierBytes(t *testing.T) {
	bad := string([]byte{0xff})
	tests := map[string]struct {
		preferred, models, want string
		previous                bool
	}{
		"success: invalid preferred and lower ids do not collide": {
			`[{"id":"claude-` + bad + `","display_name":"discard"},{"id":"claude-good","display_name":"` + bad + `"}]`,
			`[{"id":"claude-�","label":"keep"},{"id":"` + bad + `"}]`,
			`[{"id":"claude-good","display_name":"�"},{"id":"claude-�","label":"keep"}]`, false,
		},
		"success: last duplicate id wins before checking bytes": {
			`[{"id":"` + bad + `","id":"claude-good"},{"id":"claude-good","id":"` + bad + `"}]`, `[]`, `[{"id":"claude-good"}]`, false,
		},
		"success: invalid server value does not relabel lower id": {
			`[{"value":"claude-` + bad + `-1"},{"value":"claude-opus-4"}]`, `[{"id":"claude-�-1","label":"keep"}]`,
			`[{"id":"claude-opus-4","display_name":"Opus 4 (previous)"},{"id":"claude-�-1","label":"keep"}]`, true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			models, preferred := jsontext.Value(tt.models), jsontext.Value(tt.preferred)
			if tt.previous {
				checkJSON(t, tt.want, anthropiccatalog.WithPrevious(models, preferred, map[string]string{"�": "claude-�-2", "opus": "claude-opus-5"}))
				return
			}
			checkJSON(t, tt.want, anthropiccatalog.WithCached(models, preferred, policy.Resolver{}))
		})
	}
}

func TestCatalogErrorsExcludeInput(t *testing.T) {
	const marker = "private-member-marker"
	bad := jsontext.Value(`{"` + marker + `":[}`)
	tests := map[string]struct {
		success bool
		want    string
	}{
		"error: malformed upstream rows":    {true, "invalid catalog data"},
		"error: malformed failure snapshot": {false, "invalid catalog snapshot"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "catalog.json")
			var err error
			if tt.success {
				err = anthropiccatalog.WriteSuccess(path, bad, time.Time{})
			} else {
				err = anthropiccatalog.WriteFailure(path, anthropiccatalog.Snapshot{Object: bad}, "catalog timeout", time.Time{})
			}
			if err == nil || strings.Contains(err.Error(), marker) || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q without input", err, tt.want)
			}
		})
	}
}
