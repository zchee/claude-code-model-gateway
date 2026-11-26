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
		"success: numeric seconds":                            {raw: "0.125", want: 125, ok: true},
		"success: numeric milliseconds":                       {raw: "1000000000000", want: 1e12, ok: true},
		"error: zone-less calendar time is not a measurement": {raw: "1970-01-01T00:00:01"},
		"error: space-separated date-time":                    {raw: "1970-01-01 00:00:01"},
		"error: RFC 822 short date":                           {raw: "01 Jan 70 01:00 +0100"},
		"error: asctime":                                      {raw: "Thu Jan  1 00:00:01 1970"},
		"success: BOM trimmed":                                {raw: string(rune(0xfeff)) + "1", want: 1000, ok: true},
		"error: next line is not ECMAScript whitespace":       {raw: string(rune(0x85)) + "1"},
		"error: unsupported local spelling":                   {raw: "January 1st, 1970"},
		"error: unsupported named zone":                       {raw: "Thu, 01 Jan 1970 00:00:00 XYZ"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			n, ok := timestamp(tt.raw)
			if diff := gocmp.Diff(tt.ok, ok); diff != "" {
				t.Errorf("availability (-want +got): %s", diff)
			}
			if diff := gocmp.Diff(tt.want, n); diff != "" {
				t.Errorf("milliseconds (-want +got): %s", diff)
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

func TestUnsafeHighWaterObservation(t *testing.T) {
	tests := map[string]struct{ size int64 }{
		"error: first integer outside safe range": {size: 1 << 53},
		"error: maximum int64":                    {size: math.MaxInt64},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			h := NewHighWater(dir, options(t).Now)
			if diff := gocmp.Diff((*int64)(nil), h.Record("s", tt.size)); diff != "" {
				t.Fatal(diff)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(0, len(entries)); diff != "" {
				t.Errorf("files created (-want +got): %s", diff)
			}
			if diff := gocmp.Diff(map[string]int64{}, h.maxima); diff != "" {
				t.Errorf("cached maxima (-want +got): %s", diff)
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
	tests := map[string]struct{ refreshFront bool }{
		"success: older identity behind a fresh front entry expires":                       {},
		"success: accessing the front exposes an expired identity before another rollback": {refreshFront: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			o := options(t)
			o.SnapshotTTL = 10 * time.Second
			stamp := time.Unix(20, 0)
			o.Now = func() time.Time { return stamp }
			b := NewBuilder(o)
			observe(t, b, exactBase, "s", "front", "", "", 1)
			stamp = time.Unix(0, 0)
			observe(t, b, exactBase, "s", "back", "", "", 2)
			stamp = time.Unix(15, 0)
			if tt.refreshFront {
				observe(t, b, exactBase, "s", "front", "", "", 1)
				stamp = time.Unix(5, 0)
			}
			got := events(observe(t, b, exactNext, "s", "back", "", "", 4), toolEvent)
			if diff := gocmp.Diff(0, len(got)); diff != "" {
				t.Errorf("derived records (-want +got): %s", diff)
			}
		})
	}
}
