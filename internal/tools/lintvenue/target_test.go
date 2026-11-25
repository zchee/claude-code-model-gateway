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

//go:build process

package main

import (
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestTargetContexts(t *testing.T) {
	targets, err := targetContexts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ arch, feature string }{
		"success: amd64 baseline": {"amd64", "amd64.v1"},
		"success: arm64 baseline": {"arm64", "arm64.v8.0"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			found := false
			for _, target := range targets {
				if target.GOARCH != tc.arch {
					continue
				}
				found = true
				var features []string
				for _, tag := range target.ToolTags {
					if strings.HasPrefix(tag, "amd64.") || strings.HasPrefix(tag, "arm64.") {
						features = append(features, tag)
					}
				}
				if diff := gocmp.Diff([]string{tc.feature}, features); diff != "" {
					t.Errorf("%s/%s feature tags (-want +got):\n%s", target.GOOS, target.GOARCH, diff)
				}
			}
			if !found {
				t.Fatalf("no %s target configured", tc.arch)
			}
		})
	}
}
