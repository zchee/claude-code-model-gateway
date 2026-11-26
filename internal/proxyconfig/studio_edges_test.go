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

package proxyconfig_test

import (
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/proxyconfig"
)

func TestStudioFileUnicodeAndDuplicateMembers(t *testing.T) {
	const defaultOrigin = "http://127.0.0.1:1234"
	const fileOrigin = "http://localhost:4321"
	tests := map[string]struct {
		file string
		want proxyconfig.StudioSettings
	}{
		"error: unpaired surrogate rejects section": {
			file: `{"lmstudio":{"enabled":false,"baseUrl":"` + fileOrigin + `","apiKey":"prefix\ud800suffix"}}`,
			want: proxyconfig.StudioSettings{Enabled: true, BaseURL: defaultOrigin},
		},
		"error: invalid UTF-8 rejects file": {
			file: `{"lmstudio":{"enabled":false,"apiKey":"prefix` + "\xff" + `suffix"}}`,
			want: proxyconfig.StudioSettings{Enabled: true, BaseURL: defaultOrigin},
		},
		"success: well-formed token with replacement rune and surrogate pair": {
			file: `{"lmstudio":{"apiKey":"�🐟"}}`,
			want: proxyconfig.StudioSettings{Enabled: true, BaseURL: defaultOrigin, APIKey: "�🐟"},
		},
		"success: last duplicate section and field win": {
			file: `{"lmstudio":{"enabled":false},"lmstudio":{"baseUrl":"` + fileOrigin + `","apiKey":"\ud800","apiKey":"kept"}}`,
			want: proxyconfig.StudioSettings{Enabled: true, BaseURL: fileOrigin, APIKey: "kept"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tt.file), 0o600); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, proxyconfig.ReadStudio(nil, dir)); diff != "" {
				t.Errorf("ReadStudio (-want +got):\n%s", diff)
			}
		})
	}
}
