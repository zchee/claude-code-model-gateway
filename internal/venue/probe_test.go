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

package venue

import (
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestSSListener(t *testing.T) {
	const header = "State Recv-Q Send-Q Local Address:Port Peer Address:Port\n"
	const listener = "LISTEN 0 128 0.0.0.0:18764 0.0.0.0:*   \n"
	tests := map[string]struct {
		table        string
		busy, failed bool
	}{
		"success: header only":                       {header, false, false},
		"success: IPv4 and IPv6 listeners":           {header + listener + "LISTEN 0 128 [::]:18764 [::]:*   \n", true, false},
		"error: missing header":                      {"", false, true},
		"error: unterminated header":                 {strings.TrimSuffix(header, "\n"), false, true},
		"error: incomplete header":                   {"State Recv-Q Send-Q Local Address:Port\n", false, true},
		"error: unreadable row after valid listener": {header + listener + "truncated\n", false, true},
		"error: unknown state":                       {header + "ESTAB 0 0 *:18764 *:*\n", false, true},
		"error: invalid queue":                       {header + "LISTEN ? 0 *:18764 *:*\n", false, true},
		"error: unfiltered row":                      {header + "LISTEN 0 0 *:80 *:*\n", false, true},
		"error: invalid peer":                        {header + "LISTEN 0 0 *:18764 invalid\n", false, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			busy, err := ssListener(tc.table, "18764")
			if diff := gocmp.Diff([2]bool{tc.busy, tc.failed}, [2]bool{busy, err != nil}); diff != "" {
				t.Errorf("listener and failure (-want +got):\n%s; err=%v", diff, err)
			}
		})
	}
}

func TestAccountHome(t *testing.T) {
	const darwin = "name: venue\npassword: *\nuid: 501\ngid: 20\ndir: /Users/venue\nshell: /bin/sh\ngecos: Venue User\n\n"
	const linux = "venue:x:501:20::/home/venue:/bin/sh\n"
	tests := map[string]struct{ platform, record, want string }{
		"success: complete Darwin record":      {"darwin", darwin, "/Users/venue"},
		"success: complete Linux record":       {"linux", linux, "/home/venue"},
		"error: Darwin directory continuation": {"darwin", strings.ReplaceAll(darwin, "/Users/venue", "/Users/venue\nbackup"), ""},
		"error: Darwin unknown field":          {"darwin", strings.ReplaceAll(darwin, "gecos:", "extra:"), ""},
		"error: Darwin repeated field":         {"darwin", strings.ReplaceAll(darwin, "dir: ", "dir: \ndir: "), ""},
		"error: Darwin record boundary":        {"darwin", strings.ReplaceAll(darwin, "\ndir:", "\n\ndir:"), ""},
		"error: Darwin second record":          {"darwin", darwin + darwin, ""},
		"error: Darwin different UID":          {"darwin", strings.ReplaceAll(darwin, "uid: 501", "uid: 502"), ""},
		"error: Linux record continuation":     {"linux", strings.ReplaceAll(linux, "venue:x", "venue\n:x"), ""},
		"error: Linux second record":           {"linux", linux + linux, ""},
		"error: Linux different UID":           {"linux", strings.ReplaceAll(linux, ":501:", ":502:"), ""},
		"error: Linux missing field":           {"linux", strings.ReplaceAll(linux, "::", ":"), ""},
		"error: Linux truncated record":        {"linux", strings.TrimSuffix(linux, "\n"), ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := accountHome(tc.platform, tc.record, "501")
			if diff := gocmp.Diff(tc.want, got); diff != "" || (err != nil) != (tc.want == "") {
				t.Errorf("home (-want +got):\n%s; err=%v", diff, err)
			}
		})
	}
}
