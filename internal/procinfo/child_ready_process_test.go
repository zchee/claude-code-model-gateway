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
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestChildWithoutReadyEcho(t *testing.T) {
	const fixture = "PROCINFO_TEST_NO_ECHO"
	const returned = "returned PID without acknowledgement"
	tests := map[string]struct{ program, diagnostic string }{
		"error: no echo":    {"true", "child readiness:"},
		"error: wrong echo": {"yes", `child readiness: read "-" (1 bytes): <nil>`},
	}
	if tt, ok := tests[os.Getenv(fixture)]; ok {
		path, err := exec.LookPath(tt.program)
		if err != nil {
			t.Fatal(err)
		}
		startChild(t, path)
		t.Fatal(returned)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// The re-executed test binary runs TestMain's venue check before this test: three
			// sequential system queries on Linux (account lookup plus two port listener checks;
			// macOS runs two, caching one listener snapshot for both ports), each bounded by a
			// 10s timeout and 1s wait delay, followed by startChild's 5s readiness deadline and
			// 7s for process start and teardown (3*11s + 5s + 7s = 45s).
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestChildWithoutReadyEcho$")
			cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin", fixture + "=" + name}
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil || err == nil || !bytes.Contains(out, []byte(tt.diagnostic)) || bytes.Contains(out, []byte(returned)) {
				t.Fatalf("want %q without %q, got %v (context: %v)\n%s", tt.diagnostic, returned, err, ctx.Err(), out)
			}
		})
	}
}
