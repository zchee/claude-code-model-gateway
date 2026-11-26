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

package policy_test

import (
	"encoding/json/jsontext"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

func TestWindowPathPrecondition(t *testing.T) {
	tests := map[string]struct{ path string }{
		"error: relative file":               {"windows.json"},
		"error: unresolved parent component": {t.TempDir() + string(filepath.Separator) + "child" + string(filepath.Separator) + ".." + string(filepath.Separator) + "windows.json"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, readErr := policy.ReadWindows(tt.path, nil)
			writeErr := policy.WriteWindows(tt.path, jsontext.Value(`{}`))
			if readErr == nil || writeErr == nil {
				t.Fatalf("read error = %v, write error = %v; both must refuse %q", readErr, writeErr, tt.path)
			}
			want := "context window path must be clean and absolute: " + tt.path
			equal(t, want, readErr.Error())
			equal(t, want, writeErr.Error())
		})
	}
}

func TestReadWindowsExcessiveDepth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "windows.json")
	data := `{"codex":300000,"unused":` + strings.Repeat("[", 10000) + "0" + strings.Repeat("]", 10000) + "}"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := policy.ReadWindows(path, nil)
	if _, ok := errors.AsType[*jsonx.WireDepthError](err); !ok {
		t.Fatalf("ReadWindows returned %v, %v; want a WireDepthError", got, err)
	}
	equal(t, policy.Windows(nil), got)
}

func TestSavedWindowPrimitiveConversion(t *testing.T) {
	tests := map[string]struct {
		value string
		fails bool
	}{
		"error: toString is shadowed":                                {`{"toString":null}`, true},
		"error: nested array joins all children":                     {`[1,[{"toString":"bad"}]]`, true},
		"success: unconvertible numeric spelling falls back":         {`"large"`, false},
		"success: plain object falls back":                           {`{"valueOf":false}`, false},
		"success: object properties are not stringified recursively": {`{"nested":{"toString":null}}`, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "windows.json")
			if err := os.WriteFile(path, []byte(`{"codex":`+tt.value+`}`), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := policy.ReadWindows(path, nil)
			if tt.fails {
				if err == nil {
					t.Fatalf("ReadWindows silently accepted %s: %v", tt.value, got)
				}
				equal(t, "cannot convert codex context window to a number", err.Error())
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			equal(t, window(272000, "default"), got["codex"])
		})
	}
}
