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

package cli

import (
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestCommandName(t *testing.T) {
	tests := map[string]struct {
		argv []string
		want string
	}{
		"success: stable name": {
			argv: []string{"model-gateway"},
			want: "model-gateway",
		},
		"success: stable name with directory": {
			argv: []string{filepath.Join("bin", "model-gateway")},
			want: "model-gateway",
		},
		"success: stable name with executable suffix": {
			argv: []string{"model-gateway.exe"},
			want: "model-gateway",
		},
		"success: go install name": {
			argv: []string{"claude-code-model-gateway"},
			want: "claude-code-model-gateway",
		},
		"success: go install name with directory": {
			argv: []string{filepath.Join("bin", "claude-code-model-gateway")},
			want: "claude-code-model-gateway",
		},
		"success: go install name with executable suffix": {
			argv: []string{"claude-code-model-gateway.exe"},
			want: "claude-code-model-gateway",
		},
		"success: go install name with directory and executable suffix": {
			argv: []string{filepath.Join("bin", "claude-code-model-gateway.exe")},
			want: "claude-code-model-gateway",
		},
		"success: renamed executable falls back": {
			argv: []string{"custom", "claude-code-model-gateway"},
			want: "model-gateway",
		},
		"success: test binary falls back": {
			argv: []string{"cli.test"},
			want: "model-gateway",
		},
		"success: empty name falls back": {
			argv: []string{""},
			want: "model-gateway",
		},
		"success: empty argument vector falls back": {
			want: "model-gateway",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, commandName(tt.argv)); diff != "" {
				t.Errorf("commandName(%q) mismatch (-want +got):\n%s", tt.argv, diff)
			}
		})
	}
}
