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

package rccompat_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/rccompat"
)

func TestUnsafeProcessOverride(t *testing.T) {
	const report = "preflight.json"
	const child = "--rccompat-preflight-child"
	if os.Args[len(os.Args)-1] == child {
		value := rccompat.UnsafeProcessOverride(os.Getenv("ANTHROPIC_BASE_URL"))
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(report, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		url    string
		unsafe bool
	}{
		"HTTPS":            {"https://api.anthropic.com", true},
		"normalized HTTPS": {"HTTPS://API.ANTHROPIC.COM:443/", true},
		"HTTP":             {"http://api.anthropic.com", false},
		"loopback HTTP":    {"http://127.0.0.1:9", false},
		"other HTTPS port": {"https://api.anthropic.com:444", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestUnsafeProcessOverride$", "--", child)
			cmd.Dir = t.TempDir()
			cmd.Env = []string{"ANTHROPIC_BASE_URL=" + tc.url}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatalf("start preflight: %v", err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("expected successful preflight exit: %v; %s", err, &output)
			}
			data, err := os.ReadFile(filepath.Join(cmd.Dir, report))
			if err != nil {
				t.Fatal(err)
			}
			var got, want *string
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if tc.unsafe {
				want = &tc.url
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("unsafe process override (-want +got):\n%s", diff)
			}
		})
	}
}
