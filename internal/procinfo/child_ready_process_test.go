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
	"os"
	"os/exec"
	"testing"
)

func TestChildWithoutReadyEcho(t *testing.T) {
	const fixture = "PROCINFO_TEST_NO_ECHO"
	if os.Getenv(fixture) == "1" {
		path, err := exec.LookPath("true")
		if err != nil {
			t.Fatal(err)
		}
		startChild(t, path)
		t.Fatal("returned PID without acknowledgement")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestChildWithoutReadyEcho$")
	cmd.Env = append(cmd.Environ(), fixture+"=1")
	if out, err := cmd.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("child readiness:")) {
		t.Fatalf("child without echo: want readiness failure, got %v\n%s", err, out)
	}
}
