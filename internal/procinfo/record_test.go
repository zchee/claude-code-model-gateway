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

package procinfo

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func recordFixture(root string) (Record, string) {
	record := Record{
		PID: 702, Command: "router serve-shim", StartedAt: "Sat Feb  3 22:15:09 2001",
		Role: "guardian", StateRoot: root, Generation: "generation-a", Port: 27123, Version: "1.2.3",
		Image: Image{Path: "/opt/bin/router", Device: 11, Inode: 55},
	}
	wire := fmt.Sprintf(`{
		"pid":702, "command":"router serve-shim", "startedAt":"Sat Feb  3 22:15:09 2001",
		"role":"guardian", "stateRoot":%q, "generation":"generation-a", "port":27123, "version":"1.2.3",
		"image":{"path":"/opt/bin/router", "device":11, "inode":55}
	}`, root)
	return record, wire
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func putFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRecord(t *testing.T) {
	root := t.TempDir()
	record, wire := recordFixture(root)
	path := filepath.Join(root, "guardian.pid.json")
	mirror := filepath.Join(root, "guardian.pid")
	old := strings.Replace(wire, `"pid":702`, `"pid":701`, 1)
	putFile(t, path, old)
	putFile(t, mirror, "701")
	// An open descriptor must continue to see the complete previous generation.
	prior, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prior.Close() })
	if err := WriteRecord(root, "guardian", record); err != nil {
		t.Fatalf("publish record: %v", err)
	}
	previous, err := io.ReadAll(prior)
	if err != nil {
		t.Fatal(err)
	}
	if string(previous) != old {
		t.Error("writer modified the previous JSON inode in place")
	}
	var got, want map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wire), &want); err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("record schema (-want +got): %s", diff)
	}
	if got := strings.TrimSpace(readFile(t, mirror)); got != "702" {
		t.Errorf("decimal mirror = %q, want 702", got)
	}
}

func TestReadRecord(t *testing.T) {
	tests := map[string]struct{ mirror string }{"missing_mirror": {}, "stale_mirror": {mirror: "701"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			want, wire := recordFixture(root)
			putFile(t, filepath.Join(root, "guardian.pid.json"), wire)
			path := filepath.Join(root, "guardian.pid")
			if tt.mirror != "" {
				putFile(t, path, tt.mirror)
			}
			got, err := ReadRecord(root, "guardian")
			if err != nil {
				t.Fatalf("recover authoritative JSON: %v", err)
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("record (-want +got): %s", diff)
			}
			if got := strings.TrimSpace(readFile(t, path)); got != "702" {
				t.Errorf("repaired mirror = %q, want 702", got)
			}
		})
	}
}

func TestRecordPublicationOrder(t *testing.T) {
	root := t.TempDir()
	want, _ := recordFixture(root)
	mirror := filepath.Join(root, "guardian.pid")
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	err := WriteRecord(root, "guardian", want)
	if err == nil {
		t.Error("mirror publication failure was not reported")
	}
	if err := os.Remove(mirror); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRecord(root, "guardian")
	if err != nil {
		t.Fatalf("no recoverable JSON after mirror publication failed: %v", err)
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("published record (-want +got): %s", diff)
	}
	if got := strings.TrimSpace(readFile(t, mirror)); got != "702" {
		t.Errorf("recreated mirror = %q, want 702", got)
	}
}

func TestDecimalAloneIsNotARecord(t *testing.T) {
	root := t.TempDir()
	putFile(t, filepath.Join(root, "guardian.pid"), "702")
	got, err := ReadRecord(root, "guardian")
	if !errors.Is(err, os.ErrNotExist) || got != (Record{}) {
		t.Errorf("decimal without JSON returned %+v, %v; want no record and not-exist", got, err)
	}
}
