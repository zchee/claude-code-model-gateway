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
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestRequestBodyHighWater(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	stamp := time.Unix(123, 123456789).UTC()
	clock := func() time.Time { return stamp }
	h := NewHighWater(directory, clock)
	if diff := gocmp.Diff(new(int64(1234)), h.Record("high-water-session", 1234)); diff != "" {
		t.Errorf("first observation (-want +got):\n%s", diff)
	}
	stamp = stamp.Add(time.Hour)
	if diff := gocmp.Diff(new(int64(1234)), h.Record("high-water-session", 1000)); diff != "" {
		t.Errorf("smaller observation (-want +got):\n%s", diff)
	}
	path := filepath.Join(directory, "aGlnaC13YXRlci1zZXNzaW9u.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(`{"value":1234,"observed_at":"1970-01-01T00:02:03.123Z"}`, string(got)); diff != "" {
		t.Errorf("state bytes (-want +got):\n%s", diff)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state mode = %o, want 0600", info.Mode().Perm())
	}
	fresh := NewHighWater(directory, clock)
	if diff := gocmp.Diff(new(int64(1234)), fresh.Record("high-water-session", 50)); diff != "" {
		t.Errorf("restored maximum (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(new(int64(50)), fresh.Record("other-session", 50)); diff != "" {
		t.Errorf("independent session (-want +got):\n%s", diff)
	}
}

func TestProjectName(t *testing.T) {
	tests := map[string]struct {
		linked   bool
		basename string
	}{"success: transcript project": {false, "eigenwise-toolshed"}, "success: linked repository": {true, "repo-root"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, tt.basename)
			if err := os.Mkdir(repo, 0o700); err != nil {
				t.Fatal(err)
			}
			cwd := repo
			if tt.linked {
				if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(repo, "packages", "api"), 0o700); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(root, "alias")
				if err := os.Symlink(repo, alias); err != nil {
					t.Fatal(err)
				}
				cwd = filepath.Join(alias, "packages", "api")
			}
			opts := options(t)
			d := filepath.Join(opts.ProjectsDir, "transcripts")
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "session-project.jsonl"), append(encoded(t, map[string]string{"cwd": cwd}), '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			records := observe(t, NewBuilder(opts), `{"tools":[{"name":"mcp__sidequest__claim"}],"messages":[]}`, "session-project", "agent-project", "", "", 10)
			onlyEvent(t, records, "gateway.mcp.footprint")
			for _, r := range records {
				checkJSON(t, tt.basename, r.Attributes["project_name"])
				payload, err := r.OTLP()
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(payload, []byte(root)) {
					t.Errorf("%s payload exposes project path", r.EventName)
				}
				var logs struct {
					ResourceLogs []struct {
						Resource struct {
							Attributes []struct {
								Key   string
								Value map[string]string
							}
						}
						ScopeLogs []struct{ LogRecords []struct{ EventName string } }
					}
				}
				if err := json.Unmarshal(payload, &logs, json.MatchCaseInsensitiveNames(true)); err != nil {
					t.Fatal(err)
				}
				if len(logs.ResourceLogs) != 1 {
					t.Fatalf("got %d resources, want one", len(logs.ResourceLogs))
				}
				resource := logs.ResourceLogs[0]
				got := map[string]map[string]string{}
				for _, attr := range resource.Resource.Attributes {
					got[attr.Key] = attr.Value
				}
				checkJSON(t, map[string]map[string]string{"service.name": {"stringValue": "codex-gateway"}}, got)
				if len(resource.ScopeLogs) != 1 || len(resource.ScopeLogs[0].LogRecords) != 1 || resource.ScopeLogs[0].LogRecords[0].EventName != r.EventName {
					t.Errorf("OTLP envelope did not contain the %q record", r.EventName)
				}
			}
		})
	}
}
