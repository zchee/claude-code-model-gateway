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

package main

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestCheckTree(t *testing.T) {
	tests := map[string]struct {
		dir, message             string
		defaultFail, processFail bool
	}{
		"success: shared guard includes tagged tests": {dir: "shared"},
		"success: nested module excluded":             {dir: "nested"},
		"error: nested module scanned as root":        {filepath.Join("nested", "child"), "sample_test.go: missing guarded TestMain", true, true},
		"error: architecture feature guard missing":   {"feature", "sample_test.go: missing guarded TestMain", true, true},
		"success: aliased venue import":               {dir: "alias"},
		"error: missing guard":                        {"missing", "sample_test.go: missing guarded TestMain", true, true},
		"error: late guard":                           {"late", "main_test.go: TestMain must begin with venue.Require()", true, true},
		"error: guard excluded from process build":    {"excluded", "process_test.go: missing guarded TestMain", false, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("testdata", tc.dir)
			target := build.Default
			target.GOOS, target.GOARCH, target.ToolTags = "linux", "amd64", []string{"amd64.v1"}
			err := checkTree(dir, []build.Context{target})
			var msg string
			if err != nil {
				msg = err.Error()
			}
			want := []bool{tc.message != "", tc.defaultFail, tc.processFail}
			got := []bool{err != nil, strings.Contains(msg, "default: "+filepath.Join(dir, tc.message)), strings.Contains(msg, "process: "+filepath.Join(dir, tc.message))}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("refusal, default and process diagnostics (-want +got):\n%s\nerrors: %s", diff, msg)
			}
		})
	}
}
