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
	"context"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// readStartToken calls Probe.StartToken and turns a panic into a test failure,
// so one failing token read does not abort the rest of the package's tests.
func readStartToken(t *testing.T, ctx context.Context, p Probe, pid int) (token StartToken, err error) {
	t.Helper()
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("StartToken(%d) panicked: %v", pid, v)
		}
	}()
	return p.StartToken(ctx, pid)
}

func TestRecordStartToken(t *testing.T) {
	tests := map[string]struct {
		token StartToken
		// suffix follows the image object in the exact encoding.
		suffix string
	}{
		"success: token is the last field under startToken":        {token: "boot-a/4242", suffix: `,"startToken":"boot-a/4242"}`},
		"success: record from before the token keeps its encoding": {suffix: `}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			record, _ := recordFixture(root)
			record.StartToken = tt.token
			quotedRoot, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			wire := `{"pid":702,"command":"router serve-shim","startedAt":"Sat Feb  3 22:15:09 2001","role":"guardian","stateRoot":` + string(quotedRoot) + `,"generation":"generation-a","port":27123,"version":"1.2.3","image":{"path":"/opt/bin/router","device":11,"inode":55}` + tt.suffix
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(wire, string(encoded)); diff != "" {
				t.Errorf("encoded record (-want +got): %s", diff)
			}
			var decoded Record
			if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
				t.Fatalf("decode %s: %v", wire, err)
			}
			if diff := gocmp.Diff(record, decoded); diff != "" {
				t.Errorf("decoded record (-want +got): %s", diff)
			}
			if err := WriteRecord(root, "guardian", record); err != nil {
				t.Fatalf("publish record: %v", err)
			}
			if diff := gocmp.Diff(wire, readFile(t, filepath.Join(root, "guardian.pid.json"))); diff != "" {
				t.Errorf("published bytes (-want +got): %s", diff)
			}
			got, err := ReadRecord(root, "guardian")
			if err != nil {
				t.Fatalf("read record: %v", err)
			}
			if diff := gocmp.Diff(record, got); diff != "" {
				t.Errorf("read record (-want +got): %s", diff)
			}
		})
	}
}

func TestIdentityKeepsSampledStartToken(t *testing.T) {
	complete := Identity{
		PID: 17, ParentPID: 11, Argv: []string{"/bin/cat", "-"}, Birth: time.Unix(100, 0),
		Image: ImageEvidence{Path: "/bin/cat", Device: 1, Inode: 2, DirectFromExecutable: true}, StartToken: "sampled-with-birth",
	}
	incomplete := complete
	incomplete.Argv = nil
	tests := map[string]struct {
		sample  Identity
		want    Identity
		wantErr error
	}{
		"success: token comes from the inspection sample": {sample: complete, want: complete},
		"error: incomplete sample yields no token":        {sample: incomplete, wantErr: ErrUnknown},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inspections := 0
			p := Probe{
				inspect:   func(context.Context, int) (Identity, error) { inspections++; return tt.sample, nil },
				existence: func(context.Context, int) error { return nil },
			}
			got, err := p.Identity(t.Context(), tt.sample.PID)
			if !errors.Is(err, tt.wantErr) || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Identity error = %v; want %v", err, tt.wantErr)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("identity (-want +got): %s", diff)
			}
			if inspections != 1 {
				t.Errorf("inspections = %d; want one sample for birth and token together", inspections)
			}
		})
	}
}

func TestStartTokenInvalidPID(t *testing.T) {
	tests := map[string]struct{ pid int }{
		"error: process number zero is never a process":     {pid: 0},
		"error: negative process number is never a process": {pid: -1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := Probe{existence: func(context.Context, int) error {
				t.Error("an invalid process number reached the existence probe")
				return nil
			}}
			token, err := readStartToken(t, t.Context(), p, tt.pid)
			if err == nil || token != "" {
				t.Errorf("StartToken(%d) = %q, %v; want an empty token and an error", tt.pid, token, err)
			}
		})
	}
}
