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
	"strings"
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
	if _, err := countTCPListeners(nil, 27000); !errors.Is(err, ErrUnknown) || !strings.Contains(err.Error(), "no TCP table header") {
		t.Fatalf("empty successful netstat output: %v", err)
	}
	rows, err := listenerFiles([]byte("p701\x00\nf3\x00d0x1020304050607080\x00\n"), 27000, 1)
	if got := classifyListener(rows, err, 27000); got != (Listener{ListenerIdentified, 701}) {
		t.Fatalf("lsof device-character record: %+v err=%v", got, err)
	}
}

func TestDarwinCommandDiagnostic(t *testing.T) {
	tests := map[string]struct{ stderr, want string }{"success: clean": {}, "error: first warning only": {"lsof: WARNING: cannot stat unrelated volume\nOutput information may be incomplete.\n", `, stderr "lsof: WARNING: cannot stat unrelated volume"`}, "error: escaped terminal control": {"failure\x1b\nprivate second line", `, stderr "failure\x1b"`}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := commandDiagnostic([]byte(tt.stderr)); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListenerFailureDetail(t *testing.T) {
	cause := errors.New("decode netstat TCP table: no header")
	p := Probe{sockets: func(context.Context) ([]socketEvidence, error) { return nil, cause }}
	got, err := p.ListenerEvidence(t.Context(), 27123)
	if got.State != ListenerUnknown || !errors.Is(err, cause) {
		t.Fatalf("state=%s err=%v", got.State, err)
	}
}
