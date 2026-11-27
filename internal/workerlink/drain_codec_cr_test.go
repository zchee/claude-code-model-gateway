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

package workerlink_test

import (
	"bufio"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// TestDecodeDrainCarriageReturn checks where a carriage return may stand in a drain line. Only a carriage return
// immediately before the terminating LF is refused, because it makes the line a CRLF line. Anywhere else outside a
// string it is JSON white space like a space or a tab: between tokens, before the object, and after the object when
// something other than the LF follows it. A raw carriage return inside a string is a control character, which JSON
// refuses there.
func TestDecodeDrainCarriageReturn(t *testing.T) {
	body := strings.TrimSuffix(frame("draining", "g1"), "\n")
	draining := workerlink.DrainMessage{Type: workerlink.Draining, Generation: "g1"}
	tests := map[string]struct {
		input string
		want  workerlink.DrainMessage
		err   error
	}{
		"success: carriage return between tokens": {
			input: `{"type":"draining",` + "\r" + `"generation":"g1"}` + "\n",
			want:  draining,
		},
		"success: carriage return with other white space between tokens": {
			input: "{\r \"type\"\t\r:\"draining\",\"generation\" \r :\"g1\"}\n",
			want:  draining,
		},
		"success: carriage return before the object": {
			input: "\r" + body + "\n",
			want:  draining,
		},
		"success: carriage return after the object, followed by a space before the line feed": {
			input: body + "\r \n",
			want:  draining,
		},
		"error: carriage return immediately before the line feed": {
			input: body + "\r\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: carriage return immediately before the line feed after a space": {
			input: body + " \r\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: two carriage returns before the line feed": {
			input: body + "\r\r\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: raw carriage return inside the generation": {
			input: `{"type":"draining","generation":"g` + "\r" + `1"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
		"error: raw carriage return inside a member name": {
			input: `{"ty` + "\r" + `pe":"draining","generation":"g1"}` + "\n",
			err:   workerlink.ErrDrainMessage,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := workerlink.DecodeDrain(bufio.NewReader(strings.NewReader(tt.input)))
			checkCodecError(t, err, tt.err)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("message (-want +got):\n%s", diff)
			}
		})
	}
}
