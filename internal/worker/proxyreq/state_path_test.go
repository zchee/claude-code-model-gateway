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

package proxyreq_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
)

// stateTree builds a startup directory holding real/sub, state, and link -> real/sub. Through the link,
// "link/.." names real for the kernel but the startup directory itself once the spelling is cleaned lexically.
func stateTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"real/sub", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("real", "sub"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func stateHandler(t *testing.T, dir, path string) *proxyreq.Handler {
	t.Helper()
	h, err := proxyreq.New(proxyreq.Options{StartupDir: dir, DispatchPath: path, Now: func() time.Time { return time.UnixMilli(1234) }})
	if err != nil {
		t.Fatalf("New(StartupDir %q, DispatchPath %q): %v", dir, path, err)
	}
	return h
}

// TestUncleanRelativeDispatchPathRefused checks that a relative dispatch state path is accepted only in its clean
// spelling. Cleaning an unclean spelling before anchoring it can select a different file than the one the same
// spelling names from the startup directory, so New refuses it, without echoing the path, and touches no state file.
func TestUncleanRelativeDispatchPathRefused(t *testing.T) {
	// Every candidate file holds a valid but non-canonical empty state; loading it rewrites it canonically,
	// so any handler that opened one of them is visible in its bytes.
	const planted = "{ \"version\": 1, \"routes\": [ ] }\n"
	candidates := []string{"routes.json", "real/routes.json", "real/sub/routes.json", "state/routes.json"}
	tests := map[string]struct {
		path string
	}{
		"error: dot component":                       {"./routes.json"},
		"error: parent component after a symlink":    {"link/../routes.json"},
		"error: parent component after a directory":  {"state/../routes.json"},
		"error: parent component leaves the startup": {"../routes.json"},
		"error: empty component":                     {"state//routes.json"},
		"error: inner dot component":                 {"state/./routes.json"},
		"error: trailing separator":                  {"routes.json/"},
		"error: trailing separator after directory":  {"state/routes.json/"},
		"error: dot alone":                           {"."},
		"error: parent alone":                        {".."},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := stateTree(t)
			for _, c := range candidates {
				if err := os.WriteFile(filepath.Join(dir, c), []byte(planted), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h, err := proxyreq.New(proxyreq.Options{StartupDir: dir, DispatchPath: tc.path, Now: func() time.Time { return time.UnixMilli(1234) }})
			if err == nil {
				t.Errorf("New accepted the unclean relative dispatch path %q (handler %v)", tc.path, h != nil)
			} else if len(tc.path) > 2 && strings.Contains(err.Error(), tc.path) {
				// "." and ".." can occur in any sentence, so only a longer spelling shows an echoed path.
				t.Errorf("refusal echoes the dispatch path %q: %v", tc.path, err)
			}
			for _, c := range candidates {
				got, err := os.ReadFile(filepath.Join(dir, c))
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(planted, string(got)); diff != "" {
					t.Errorf("dispatch path %q touched %s (-planted +now): %s", tc.path, c, diff)
				}
			}
		})
	}
}

// TestCleanRelativeDispatchPathSelection checks which file a clean relative dispatch state path reads and writes:
// the file the spelling names from the startup directory, following a symbolic link component like the kernel does.
// The routes are planted through handlers that use absolute paths, so the test depends on no file format.
func TestCleanRelativeDispatchPathSelection(t *testing.T) {
	const path = "/v1/messages"
	headers := http.Header{"X-Claude-Code-Session-Id": {"s"}, "X-Claude-Code-Agent-Id": {"a"}}
	brief := func(t *testing.T, h *proxyreq.Handler, model string) {
		t.Helper()
		got := outcomeOf(newPendingTurn(t, path, autoBody("[sidequest-route model="+model+"]"), headers).prepare(h))
		if want := (turnOutcome{Via: "dispatch", Model: model, Ticket: "<none>"}); got != want {
			t.Fatalf("briefing = %+v, want %+v", got, want)
		}
	}
	followUp := func(t *testing.T, h *proxyreq.Handler) turnOutcome {
		t.Helper()
		return outcomeOf(newPendingTurn(t, path, autoBody("follow up"), headers).prepare(h))
	}
	tests := map[string]struct {
		path, selected string
	}{
		"success: plain file name":             {"routes.json", "routes.json"},
		"success: file in a subdirectory":      {"state/routes.json", "state/routes.json"},
		"success: file behind a symlink":       {"link/routes.json", "real/sub/routes.json"},
		"success: other file behind a symlink": {"link/nested.json", "real/sub/nested.json"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := stateTree(t)
			// A decoy route in every other candidate file shows a wrong selection as a wrong model.
			for _, decoy := range []string{"routes.json", "state/routes.json", "real/routes.json", "real/sub/routes.json"} {
				if decoy != tc.selected {
					brief(t, stateHandler(t, dir, filepath.Join(dir, decoy)), "gpt-decoy")
				}
			}
			brief(t, stateHandler(t, dir, filepath.Join(dir, tc.selected)), "gpt-selected")

			h := stateHandler(t, dir, tc.path)
			want := turnOutcome{Via: "dispatch-cached", Model: "gpt-selected", Ticket: "<none>"}
			if diff := gocmp.Diff(want, followUp(t, h)); diff != "" {
				t.Fatalf("follow-up through %q did not read %s (-want +got): %s", tc.path, tc.selected, diff)
			}

			// A new binding through the relative spelling lands in the selected file and nowhere else.
			brief(t, h, "gpt-rebound")
			want.Model = "gpt-rebound"
			if diff := gocmp.Diff(want, followUp(t, stateHandler(t, dir, filepath.Join(dir, tc.selected)))); diff != "" {
				t.Errorf("binding through %q not persisted to %s (-want +got): %s", tc.path, tc.selected, diff)
			}
			if tc.selected != "routes.json" {
				decoy := turnOutcome{Via: "dispatch-cached", Model: "gpt-decoy", Ticket: "<none>"}
				if diff := gocmp.Diff(decoy, followUp(t, stateHandler(t, dir, filepath.Join(dir, "routes.json")))); diff != "" {
					t.Errorf("binding through %q changed the startup directory's routes.json (-want +got): %s", tc.path, diff)
				}
			}
		})
	}
}
