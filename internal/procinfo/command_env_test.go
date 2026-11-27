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

//go:build process && (darwin || linux)

package procinfo

import (
	"os"
	"slices"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestProbeCommandEnvironment observes the environment a probe command actually runs with: exactly the C locale
// and the fixed search path, nothing the caller has set (its French locale, its unrelated variables), and the
// caller's own environment left as it was. The source instead runs its probes with the caller's environment plus
// LC_ALL=C (lib/process-supervision.js:236-238); the port pins the whole environment.
func TestProbeCommandEnvironment(t *testing.T) {
	for _, key := range []string{"LANG", "LC_ALL", "LC_TIME"} {
		t.Setenv(key, "fr_FR.UTF-8")
	}
	t.Setenv("MODEL_GATEWAY_LOCALE_SENTINEL", "keep-me")
	before := os.Environ()
	out, err := darwinCommand(t.Context(), "/usr/bin/env")
	if err != nil {
		t.Fatalf("probe command: %v; output %q", err, out)
	}
	got := strings.Fields(string(out))
	slices.Sort(got)
	if diff := gocmp.Diff([]string{"LC_ALL=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}, got); diff != "" {
		t.Errorf("the probe command's environment (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(before, os.Environ()); diff != "" {
		t.Errorf("the caller's environment changed (-before +after):\n%s", diff)
	}
}
