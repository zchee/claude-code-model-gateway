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
	"errors"
	"fmt"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestDarwinMappedVnodeOutput(t *testing.T) {
	const pid = 701
	tests := map[string]struct {
		path, field string
		wantInode   uint64
	}{
		"success: running shell and another txt mapping":           {"/fixture/shell", "/fixture/shell", 1506186249},
		"success: renamed executable":                              {"/fixture/shell.moved", "/fixture/shell.moved", 1506186251},
		"success: unlinked executable retains its path":            {"/fixture/shell", "/fixture/shell", 1506186253},
		"success: overwritten executable retains its mapped inode": {"/fixture/shell", "/fixture/shell", 1506186255},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			data := []byte(fmt.Sprintf("p701\x00\nftxt\x00D0x100000d\x00i%d\x00n%s\x00\nftxt\x00D0x100000d\x00i1152921500312633471\x00n/usr/share/locale/en_US.UTF-8/LC_COLLATE\x00\n", tt.wantInode, tt.field))
			got, err := mappedImage(data, pid, tt.path)
			if err != nil {
				t.Fatal(err)
			}
			if d := gocmp.Diff(Image{tt.path, 0x100000d, tt.wantInode}, got); d != "" {
				t.Fatal(d)
			}
		})
	}
}

func TestDarwinSocketToolOutput(t *testing.T) {
	got, err := lsofListener([]byte("p701\x00\nf3\x00d0x1020304050607080\x00\n"), 0)
	if got != (Listener{ListenerIdentified, 701}) || err != nil {
		t.Fatalf("lsof device-character record: %+v err=%v", got, err)
	}
}

func TestDarwinCommandDiagnostic(t *testing.T) {
	tests := map[string]struct{ stderr, want string }{"success: clean": {}, "error: raw marker and controls are discarded": {"private-marker\n\x1b", ", stderr present (16 bytes)"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s stderrSize
			if n, err := s.Write([]byte(tt.stderr)); n != len(tt.stderr) || err != nil {
				t.Fatal("stderr not fully consumed")
			}
			if got := s.diagnostic(); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListenerFailureDetail(t *testing.T) {
	cause := errors.New("lsof listener warning")
	p := Probe{sockets: func(context.Context) ([]socketEvidence, error) { return nil, cause }}
	got, err := p.ListenerEvidence(t.Context(), 27123)
	if got.State != ListenerUnknown || !errors.Is(err, cause) {
		t.Fatalf("state=%s err=%v", got.State, err)
	}
}
