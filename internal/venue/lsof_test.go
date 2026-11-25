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
	"io/fs"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestInspectLsof(t *testing.T) {
	const witness = "p26313\nf4\nn127.0.0.1:5934\n"
	tests := map[string]struct {
		output                string
		runErr                error
		first, second, failed bool
	}{
		"success: canary alone":                        {output: witness},
		"success: IPv4 loopback":                       {output: witness + "p9\nf10\nn127.0.0.1:18764\n", first: true},
		"success: IPv4 wildcard":                       {output: witness + "p9\nf10\nn0.0.0.0:18765\n", second: true},
		"success: IPv6 loopback":                       {output: witness + "p9\nf10\nn[::1]:18765\n", second: true},
		"success: IPv6 wildcard":                       {output: witness + "p9\nf10\nn[::]:18764\n", first: true},
		"success: both wildcard listeners":             {output: witness + "p9\nf10\nn*:18764\nf11\nn*:18765\n", first: true, second: true},
		"error: empty listing":                         {failed: true},
		"error: another PID owns matching port":        {output: "p263130\nf4\nn127.0.0.1:5934\n", failed: true},
		"error: port has same suffix":                  {output: "p26313\nf4\nn127.0.0.1:55934\n", failed: true},
		"error: truncated file after canary":           {output: witness + "f5\n", failed: true},
		"error: missing descriptor":                    {output: witness + "p9\nn*:18764\n", failed: true},
		"error: invalid descriptor":                    {output: witness + "fnot-a-number\nn*:18764\n", failed: true},
		"error: hostname in numeric listing":           {output: witness + "f5\nnlocalhost:18764\n", failed: true},
		"error: remote connection in listener listing": {output: witness + "f5\nn127.0.0.1:5000->127.0.0.1:18764\n", failed: true},
		"error: unknown field after canary":            {output: witness + "TST=UNKNOWN\n", failed: true},
		"error: unterminated line":                     {output: witness + "f5\nn*:18764", failed: true},
		"error: tool failure despite canary":           {output: witness, runErr: fs.ErrPermission, failed: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			busy, err := inspectLsof("26313", "5934", func(string, ...string) (string, error) { return tc.output, tc.runErr })
			if diff := gocmp.Diff([3]bool{tc.first, tc.second, tc.failed}, [3]bool{busy["18764"], busy["18765"], err != nil}); diff != "" {
				t.Errorf("port results and failure (-want +got):\n%s; err=%v", diff, err)
			}
		})
	}
}
