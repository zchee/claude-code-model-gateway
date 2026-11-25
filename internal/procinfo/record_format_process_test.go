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

//go:build process && (linux || darwin)

package procinfo

import (
	"context"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

func TestRecordFormatAndPermissions(t *testing.T) {
	const marker = "--record-umask"
	if i := len(os.Args) - 3; i > 0 && os.Args[i] == marker {
		root := os.Args[i+1]
		mask, err := strconv.ParseUint(os.Args[i+2], 8, 9)
		if err != nil {
			t.Fatal(err)
		}
		old := unix.Umask(int(mask))
		defer unix.Umask(old)
		r, _ := recordFixture(root)
		if err := WriteRecord(root, "guardian", r); err != nil {
			t.Fatal(err)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ mask string }{
		"success: permissive creation mask": {"0000"},
		"success: ordinary creation mask":   {"0022"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dir.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestRecordFormatAndPermissions$", "--", marker, root, tt.mask)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("masked record helper: %v: %s", err, out)
			}
			quotedRoot, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"pid":702,"command":"router serve-shim","startedAt":"Sat Feb  3 22:15:09 2001","role":"guardian","stateRoot":` + string(quotedRoot) + `,"generation":"generation-a","port":27123,"version":"1.2.3","image":{"path":"/opt/bin/router","device":11,"inode":55}}`
			for file, content := range map[string]string{"guardian.pid.json": want, "guardian.pid": "702"} {
				info, err := dir.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode() != 0o600 {
					t.Errorf("%s mode under umask %s = %v; want 0600", file, tt.mask, info.Mode())
				}
				data, err := dir.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if d := gocmp.Diff(content, string(data)); d != "" {
					t.Errorf("%s exact bytes (-want +got): %s", file, d)
				}
			}
		})
	}
}
