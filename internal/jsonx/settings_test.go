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

package jsonx_test

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAuthoredSettingsCorpus(t *testing.T) {
	var edits map[string][]jsonx.Edit
	if err := json.Unmarshal(fixture(t, "edits.json"), &edits, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		before, after, edit string
	}{
		"Replace":             {"spelling.before.json", "spelling.after.json", "spelling"},
		"Insert":              {"insertion.before.json", "insertion.after.json", "insertion"},
		"Remove":              {"removal.before.json", "removal.after.json", "removal"},
		"NoEdit":              {"spelling.before.json", "spelling.before.json", ""},
		"RemoveMiddleHook":    {"hooks.before.json", "hooks-removed.after.json", "removeHook"},
		"ReplaceMiddleHook":   {"hooks.before.json", "hooks-replaced.after.json", "replaceHook"},
		"AppendHook":          {"hooks-removed.after.json", "hooks-appended.after.json", "appendHook"},
		"AppendToEmptyArray":  {"hooks-empty.before.json", "hooks-empty.after.json", "appendHook"},
		"DuplicateUnownedKey": {"duplicate-user.before.json", "duplicate-user.after.json", "spelling"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before, want := fixture(t, tt.before), fixture(t, tt.after)
			changes := edits[tt.edit]
			if tt.edit != "" && len(changes) == 0 {
				t.Fatalf("missing fixture edit %q", tt.edit)
			}
			got, err := jsonx.EditSettings(before, changes...)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(string(want), string(got)); diff != "" {
				t.Errorf("settings bytes (-want +got):\n%s", diff)
			}
		})
	}
	for name, secondKey := range map[string]string{
		"DuplicateOwnedKey":        `ANTHROPIC_BASE_URL`,
		"EscapedDuplicateOwnedKey": `\u0041NTHROPIC_BASE_URL`,
	} {
		t.Run(name, func(t *testing.T) {
			duplicate := `{"env":{"ANTHROPIC_BASE_URL":"before","` + secondKey + `":"other"}}`
			got, err := jsonx.EditSettings([]byte(duplicate), edits["spelling"]...)
			if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_BASE_URL") {
				t.Errorf("duplicate owned name: got %q, err=%v; want error naming ANTHROPIC_BASE_URL", got, err)
			}
		})
	}
}
