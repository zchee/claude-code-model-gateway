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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestDarwinListener(t *testing.T) {
	const header = "Active Internet connections (including servers)\nProto Recv-Q Send-Q Local Address Foreign Address (state)\n"
	tests := map[string]struct {
		table        string
		busy, failed bool
	}{
		"success: no listeners":           {table: header},
		"success: IPv4 wildcard":          {table: header + "tcp4 0 0 *.18764 *.* LISTEN", busy: true},
		"success: IPv6 interface":         {table: header + "tcp6 0 0 fe80::1%lo0.18764 *.* LISTEN", busy: true},
		"success: another listener":       {table: header + "tcp46 0 0 *.18765 *.* LISTEN"},
		"success: established connection": {table: header + "tcp4 0 0 127.0.0.1.18764 127.0.0.1.4321 ESTABLISHED"},
		"error: missing header":           {failed: true},
		"error: malformed row":            {table: header + "truncated", failed: true},
		"error: unknown endpoint":         {table: header + "tcp4 0 0 unknown *.* LISTEN", failed: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			busy, err := darwinListener(tc.table, "18764")
			if diff := gocmp.Diff([2]bool{tc.busy, tc.failed}, [2]bool{busy, err != nil}); diff != "" {
				t.Errorf("listener and failure (-want +got):\n%s; err=%v", diff, err)
			}
		})
	}
}
