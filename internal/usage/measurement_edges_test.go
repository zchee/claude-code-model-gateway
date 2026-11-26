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

package usage

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

func TestTimestampFamilies(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want float64
		ok   bool
	}{
		"success: numeric seconds":                      {raw: "0.125", want: 125, ok: true},
		"success: numeric milliseconds":                 {raw: "1000000000000", want: 1e12, ok: true},
		"success: UTC timestamp":                        {raw: "1970-01-01T00:00:01.234Z", want: 1234, ok: true},
		"success: lowercase RFC separators":             {raw: "1970-01-01t00:00:01.234z", want: 1234, ok: true},
		"success: UTC offset":                           {raw: "1970-01-01t01:00:01+01:00", want: 1000, ok: true},
		"success: date only":                            {raw: "1970-01-02", want: 86400000, ok: true},
		"success: space separated datetime":             {raw: "1970-01-01 00:00:01", want: 1000, ok: true},
		"success: ISO datetime":                         {raw: "1970-01-01T00:00:01", want: 1000, ok: true},
		"success: RFC numeric offset":                   {raw: "Thu, 01 Jan 1970 01:00:01 +0100", want: 1000, ok: true},
		"success: RFC short year":                       {raw: "01 Jan 70 01:00 +0100", ok: true},
		"success: HTTP date":                            {raw: "Thu, 01 Jan 1970 00:00:01 GMT", want: 1000, ok: true},
		"success: obsolete HTTP date":                   {raw: "Thursday, 01-Jan-70 00:00:01 GMT", want: 1000, ok: true},
		"success: asctime":                              {raw: "Thu Jan  1 00:00:01 1970", want: 1000, ok: true},
		"success: BOM trimmed":                          {raw: string(rune(0xfeff)) + "1", want: 1000, ok: true},
		"error: next line is not ECMAScript whitespace": {raw: string(rune(0x85)) + "1"},
		"error: unsupported local spelling":             {raw: "January 1st, 1970"},
		"error: unsupported named zone":                 {raw: "Thu, 01 Jan 1970 00:00:00 XYZ"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			n, ok := timestamp(tt.raw)
			if ok != tt.ok || n != tt.want {
				t.Errorf("timestamp = (%v,%v), want (%v,%v)", n, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestServerCountOrderAcrossMerge(t *testing.T) {
	tests := map[string]struct {
		first, next, merged string
		sum                 float64
	}{
		"success: integer properties precede later integer and string properties": {
			first: `{"server_tool_use":{"2":1,"0":9007199254740992}}`, next: `{"server_tool_use":{"1":1}}`,
			merged: `{"server_tool_use":{"0":9007199254740992,"1":1,"2":1}}`, sum: 9007199254740992,
		},
		"success: non-index keys keep insertion rather than alphabetical order": {
			first: `{"server_tool_use":{"z":1,"a":9007199254740992}}`, next: `{"server_tool_use":{"z":1,"q":1}}`,
			merged: `{"server_tool_use":{"z":1,"a":9007199254740992,"q":1}}`, sum: 9007199254740992,
		},
		"success: small counts before large one survive rounding": {
			first: `{"server_tool_use":{"z":1,"q":1}}`, next: `{"server_tool_use":{"a":9007199254740992}}`,
			merged: `{"server_tool_use":{"z":1,"q":1,"a":9007199254740992}}`, sum: 9007199254740994,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			v := MergeWireUsage(wire(t, tt.first), wire(t, tt.next))
			raw, err := jsonx.EncodeWire(v)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.merged, string(raw)); diff != "" {
				t.Error(diff)
			}
			v = MergeWireUsage(v, wire(t, `{"output_tokens":0}`))
			r := onlyEvent(t, NewBuilder(options(t)).Build(Request{WireUsage: v, StatusCode: 200}), "gateway.token.usage")
			if diff := gocmp.Diff(tt.sum, r.Attributes["server_tool_use_count"]); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestSafeHighWater(t *testing.T) {
	tests := map[string]struct {
		seed string
		want int64
	}{
		"success: skip overflow in irrelevant member": {seed: `{"value":1234,"extra":1e400}`, want: 1234},
		"success: ignore fractional count":            {seed: `{"value":1234.5}`, want: 2},
		"success: ignore numeric string":              {seed: `{"value":"1234"}`, want: 2},
		"success: ignore unsafe count":                {seed: `{"value":9007199254740992}`, want: 2},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "cw.json"), []byte(tt.seed), 0o600); err != nil {
				t.Fatal(err)
			}
			h := NewHighWater(dir, options(t).Now)
			if diff := gocmp.Diff(new(tt.want), h.Record("s", 2)); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestLargeAllocation(t *testing.T) {
	tests := map[string]struct {
		total int64
		want  []int64
	}{
		"success: unsafe total saturates":  {total: 1 << 53, want: []int64{maxSafeInteger, 0}},
		"success: maximum int64 saturates": {total: math.MaxInt64, want: []int64{maxSafeInteger, 0}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, allocateLargestRemainder(tt.total, []int64{1, 0})); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestSnapshotWithClockRollback(t *testing.T) {
	tests := map[string]struct{ first, second, last time.Duration }{"success: older identity behind a fresh front entry expires": {first: 20 * time.Second, second: 0, last: 15 * time.Second}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			o := options(t)
			o.SnapshotTTL = 10 * time.Second
			stamp := time.Unix(0, int64(tt.first))
			o.Now = func() time.Time { return stamp }
			b := NewBuilder(o)
			observe(t, b, exactBase, "s", "front", "", "", 1)
			stamp = time.Unix(0, int64(tt.second))
			observe(t, b, exactBase, "s", "back", "", "", 2)
			stamp = time.Unix(0, int64(tt.last))
			if got := events(observe(t, b, exactNext, "s", "back", "", "", 4), toolEvent); len(got) != 0 {
				t.Fatalf("expired identity produced %d derived records", len(got))
			}
		})
	}
}
