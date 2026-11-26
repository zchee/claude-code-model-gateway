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
	"os"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestIdentityDuringExec(t *testing.T) {
	tests := map[string]struct{ argv []string }{
		"error: absent argv":          {},
		"error: empty first argument": {[]string{"", "-"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			id := Identity{
				PID: 17, ParentPID: 11, Birth: time.Unix(100, 0), Argv: tt.argv,
				Image: ImageEvidence{Path: "/bin/cat", Device: 1, Inode: 2, DirectFromExecutable: true},
			}
			p := Probe{
				inspect:   func(context.Context, int) (Identity, error) { return id, nil },
				existence: func(context.Context, int) error { return nil },
			}
			got, err := p.Identity(t.Context(), id.PID)
			if !errors.Is(err, ErrUnknown) || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incomplete identity error = %v; want unknown, not absent", err)
			}
			if diff := gocmp.Diff(Identity{}, got); diff != "" {
				t.Fatalf("incomplete identity (-want +got): %s", diff)
			}
			id.Argv = []string{"/bin/cat", "-"}
			got, err = p.Identity(t.Context(), id.PID)
			if err != nil {
				t.Fatalf("complete identity: %v", err)
			}
			if diff := gocmp.Diff(id, got); diff != "" {
				t.Fatalf("complete identity (-want +got): %s", diff)
			}
		})
	}
}
