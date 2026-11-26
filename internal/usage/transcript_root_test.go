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

package usage

import (
	"bytes"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestTranscriptLinkOutsideRoot(t *testing.T) {
	// lib/usage-observability.js:115-162 opens <projects>/<dir>/<session>.jsonl directly,
	// following symlinks that leave the projects root. The port confines transcript opens
	// to the configured projects root and leaves project_name unset when none resolves inside it.
	tests := map[string]struct {
		relative    bool
		chained     bool
		withInRoot  bool
		wantProject any
	}{
		"success: absolute symlink outside projects root leaves project name unset": {
			wantProject: nil,
		},
		"success: relative symlink escaping projects root leaves project name unset": {
			relative:    true,
			wantProject: nil,
		},
		"success: chained symlink escaping projects root leaves project name unset": {
			relative:    true,
			chained:     true,
			wantProject: nil,
		},
		"success: escaping symlink is ignored when a readable in-root transcript exists": {
			withInRoot:  true,
			wantProject: "inside-repo",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			projectsDir := filepath.Join(workspace, "projects")
			outsideRepo := filepath.Join(workspace, "outside-repo")
			outsideDir := filepath.Join(workspace, "outside")
			escapedDir := filepath.Join(projectsDir, "00-escaped")
			for _, dir := range []string{filepath.Join(outsideRepo, ".git"), outsideDir, escapedDir} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			outsideTranscript := filepath.Join(outsideDir, "escaped.jsonl")
			if err := os.WriteFile(outsideTranscript, append(encoded(t, map[string]string{"cwd": outsideRepo}), '\n'), 0o600); err != nil {
				t.Fatal(err)
			}

			const session = "session-transcript-root"
			target := outsideTranscript
			if tt.relative {
				rel, err := filepath.Rel(escapedDir, outsideTranscript)
				if err != nil {
					t.Fatal(err)
				}
				target = rel
			}
			linkPath := filepath.Join(escapedDir, session+".jsonl")
			if tt.chained {
				hop := filepath.Join(escapedDir, "hop.jsonl")
				if err := os.Symlink(target, hop); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("hop.jsonl", linkPath); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(target, linkPath); err != nil {
				t.Fatal(err)
			}

			if tt.withInRoot {
				insideRepo := filepath.Join(workspace, "inside-repo")
				inRootDir := filepath.Join(projectsDir, "99-in-root")
				for _, dir := range []string{filepath.Join(insideRepo, ".git"), inRootDir} {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				inRootTranscript := filepath.Join(inRootDir, session+".jsonl")
				if err := os.WriteFile(inRootTranscript, append(encoded(t, map[string]string{"cwd": insideRepo}), '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			opts := options(t)
			opts.ProjectsDir = projectsDir
			records := observe(t, NewBuilder(opts), `{"tools":[{"name":"mcp__sidequest__claim"}],"messages":[]}`, session, "agent-transcript-root", "", "", 10)
			onlyEvent(t, records, "gateway.mcp.footprint")

			for _, r := range records {
				gotAttr, exists := r.Attributes["project_name"]
				if diff := gocmp.Diff(true, exists); diff != "" {
					t.Fatalf("%s project_name key presence (-want +got):\n%s", r.EventName, diff)
				}
				if diff := gocmp.Diff(tt.wantProject, gotAttr); diff != "" {
					t.Errorf("%s Attributes[project_name] (-want +got):\n%s", r.EventName, diff)
				}

				payload, err := r.OTLP()
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(payload, []byte("outside-repo")) || bytes.Contains(payload, []byte(workspace)) {
					t.Errorf("%s OTLP payload leaked outside path: %s", r.EventName, payload)
				}
				var envelope struct {
					ResourceLogs []struct {
						ScopeLogs []struct {
							LogRecords []struct {
								Attributes []struct {
									Key   string            `json:"key"`
									Value map[string]string `json:"value"`
								} `json:"attributes"`
							} `json:"logRecords"`
						} `json:"scopeLogs"`
					} `json:"resourceLogs"`
				}
				if err := json.Unmarshal(payload, &envelope); err != nil {
					t.Fatal(err)
				}
				var gotExported any
				for _, attr := range envelope.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes {
					if attr.Key == "project_name" {
						gotExported = attr.Value["stringValue"]
					}
				}
				if diff := gocmp.Diff(tt.wantProject, gotExported); diff != "" {
					t.Errorf("%s OTLP project_name (-want +got):\n%s", r.EventName, diff)
				}
			}
		})
	}
}
