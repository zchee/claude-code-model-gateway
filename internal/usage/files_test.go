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
	"encoding/base64"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestRequestBodyHighWater(t *testing.T) {
	const initial = `{"value":1234,"observed_at":"1970-01-01T00:02:03.123Z"}`
	const seeded = `{"value":4321,"observed_at":"1970-01-01T00:00:01.000Z"}`
	tests := map[string]struct {
		seed, wantFile string
		maximum        int64
	}{
		"success: record new maximum":             {wantFile: initial, maximum: 1234},
		"success: restore independent state file": {seed: seeded, wantFile: seeded, maximum: 4321},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "state")
			path := filepath.Join(directory, "aGlnaC13YXRlci1zZXNzaW9u.json")
			if tt.seed != "" {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tt.seed), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stamp := time.Unix(123, 123456789).UTC()
			clock := func() time.Time { return stamp }
			h := NewHighWater(directory, clock)
			if diff := gocmp.Diff(new(tt.maximum), h.Record("high-water-session", 1234)); diff != "" {
				t.Errorf("first observation (-want +got):\n%s", diff)
			}
			stamp = stamp.Add(time.Hour)
			if diff := gocmp.Diff(new(tt.maximum), h.Record("high-water-session", 1000)); diff != "" {
				t.Errorf("smaller observation (-want +got):\n%s", diff)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.wantFile, string(got)); diff != "" {
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
			if diff := gocmp.Diff(new(tt.maximum), fresh.Record("high-water-session", 50)); diff != "" {
				t.Errorf("restored maximum (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(new(int64(50)), fresh.Record("other-session", 50)); diff != "" {
				t.Errorf("independent session (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTelemetryIndependentSessionHighWater(t *testing.T) {
	const (
		firstSession  = "session-alpha"
		secondSession = "session-beta"
		firstAt       = "1970-01-01T00:02:03.123Z"
		secondAt      = "1970-01-01T01:02:03.456Z"
		updatedAt     = "1970-01-01T02:02:03.789Z"
	)
	firstName := base64.RawURLEncoding.EncodeToString([]byte(firstSession)) + ".json"
	secondName := base64.RawURLEncoding.EncodeToString([]byte(secondSession)) + ".json"
	tests := map[string]struct {
		seedFirst   string
		firstInput  int64
		firstMax    int64
		firstBytes  string
		secondInput int64
		secondBytes string
	}{
		"success: smaller second session persists its own lower maximum": {
			firstInput:  4096,
			firstMax:    4096,
			firstBytes:  `{"value":4096,"observed_at":"` + firstAt + `"}`,
			secondInput: 1024,
			secondBytes: `{"value":1024,"observed_at":"` + secondAt + `"}`,
		},
		"success: second session stays independent after first session restored from disk": {
			seedFirst:   `{"value":8192,"observed_at":"` + firstAt + `"}`,
			firstInput:  2048,
			firstMax:    8192,
			firstBytes:  `{"value":8192,"observed_at":"` + firstAt + `"}`,
			secondInput: 512,
			secondBytes: `{"value":512,"observed_at":"` + secondAt + `"}`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "request-body")
			firstPath := filepath.Join(directory, firstName)
			secondPath := filepath.Join(directory, secondName)
			if tt.seedFirst != "" {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(firstPath, []byte(tt.seedFirst), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stamp := time.Date(1970, 1, 1, 0, 2, 3, 123_000_000, time.UTC)
			clock := func() time.Time { return stamp }
			h := NewHighWater(directory, clock)

			if diff := gocmp.Diff(new(tt.firstMax), h.Record(firstSession, tt.firstInput)); diff != "" {
				t.Errorf("first session observation (-want +got):\n%s", diff)
			}
			stamp = time.Date(1970, 1, 1, 1, 2, 3, 456_000_000, time.UTC)
			if diff := gocmp.Diff(new(tt.secondInput), h.Record(secondSession, tt.secondInput)); diff != "" {
				t.Errorf("second session observation (-want +got):\n%s", diff)
			}

			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			gotNames := make([]string, len(entries))
			for i, entry := range entries {
				gotNames[i] = entry.Name()
			}
			wantNames := []string{firstName, secondName}
			slices.Sort(wantNames)
			if diff := gocmp.Diff(wantNames, gotNames); diff != "" {
				t.Errorf("high-water directory entries (-want +got):\n%s", diff)
			}

			for _, checkFile := range []struct{ path, want string }{
				{firstPath, tt.firstBytes},
				{secondPath, tt.secondBytes},
			} {
				got, err := os.ReadFile(checkFile.path)
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(checkFile.want, string(got)); diff != "" {
					t.Errorf("%s bytes (-want +got):\n%s", filepath.Base(checkFile.path), diff)
				}
				info, err := os.Stat(checkFile.path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Errorf("%s mode = %o, want 0600", filepath.Base(checkFile.path), info.Mode().Perm())
				}
			}

			// A fresh tracker restores both session files independently and updating the second
			// session leaves the first session's higher maximum untouched.
			stamp = time.Date(1970, 1, 1, 2, 2, 3, 789_000_000, time.UTC)
			fresh := NewHighWater(directory, clock)
			if diff := gocmp.Diff(new(tt.secondInput), fresh.Record(secondSession, tt.secondInput-1)); diff != "" {
				t.Errorf("restored second session (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(new(tt.firstMax), fresh.Record(firstSession, tt.secondInput)); diff != "" {
				t.Errorf("restored first session (-want +got):\n%s", diff)
			}
			raised := tt.secondInput + 256
			if diff := gocmp.Diff(new(raised), fresh.Record(secondSession, raised)); diff != "" {
				t.Errorf("raised second session (-want +got):\n%s", diff)
			}
			firstAfter, err := os.ReadFile(firstPath)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.firstBytes, string(firstAfter)); diff != "" {
				t.Errorf("first session bytes after second session update (-want +got):\n%s", diff)
			}
			secondAfter, err := os.ReadFile(secondPath)
			if err != nil {
				t.Fatal(err)
			}
			wantSecondAfter := `{"value":` + string(encoded(t, raised)) + `,"observed_at":"` + updatedAt + `"}`
			if diff := gocmp.Diff(wantSecondAfter, string(secondAfter)); diff != "" {
				t.Errorf("second session bytes after update (-want +got):\n%s", diff)
			}
		})
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
