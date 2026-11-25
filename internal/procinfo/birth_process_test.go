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
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeBirthOnly(t *testing.T) {
	pid := startChild(t, "/bin/cat")
	ps := exec.CommandContext(t.Context(), "/bin/ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	ps.Env = []string{"LC_ALL=C", "TZ=UTC0"}
	out, err := ps.Output()
	if err != nil {
		t.Fatal(err)
	}
	want, err := time.Parse(time.ANSIC, strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("error: confirmed missing process", func(t *testing.T) {
		const target = 701
		p := Probe{existence: func(_ context.Context, got int) error {
			if got != target {
				t.Fatalf("checked pid %d; want %d", got, target)
			}
			return syscall.ESRCH
		}}
		if err := p.inspectionError(t.Context(), target, syscall.EIO); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed birth inspection followed by ESRCH: %v", err)
		}
	})
	tests := map[string]struct {
		pid    int
		cancel bool
		want   time.Time
		err    error
	}{"success: kernel birth matches ps": {pid: pid, want: want}, "error: expired context is not evidence of death": {pid: pid, cancel: true, err: ErrUnknown}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			got, err := (Probe{}).Birth(ctx, tt.pid)
			if !errors.Is(err, tt.err) || !got.Truncate(time.Second).Equal(tt.want) {
				t.Fatalf("Birth(%d)=%s,%v; want %s,%v", tt.pid, got, err, tt.want, tt.err)
			}
		})
	}
}
