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
	json "encoding/json/v2"
	"math"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestOTLPIntegerAttributesExact checks that an integer attribute value of any Go integer type is
// exported as the exact decimal of that value. A binary64 holds every integer only up to 2^53, so
// an unsigned value sent through a float would lose its last digits: 9007199254740993 would be
// exported as 9007199254740992. The kind of the value decides, not its declared type, so named
// integer types and uintptr are checked as well as the predeclared ones.
func TestOTLPIntegerAttributesExact(t *testing.T) {
	type namedUint64 uint64
	type namedUint16 uint16
	type namedUintptr uintptr
	tests := map[string]struct {
		value any
		want  string
	}{
		"success: unsigned 64-bit value one above 2^53": {
			value: uint64(9007199254740993),
			want:  "9007199254740993",
		},
		"success: unsigned 64-bit value at the largest signed 64-bit integer": {
			value: uint64(math.MaxInt64),
			want:  "9223372036854775807",
		},
		"success: unsigned int value one above 2^53": {
			value: uint(9007199254740993),
			want:  "9007199254740993",
		},
		"success: unsigned 64-bit value three above 2^53": {
			value: uint64(1<<53 + 3),
			want:  "9007199254740995",
		},
		"success: largest unsigned 32-bit value": {
			value: uint32(math.MaxUint32),
			want:  "4294967295",
		},
		"success: largest unsigned 8-bit value": {
			value: uint8(math.MaxUint8),
			want:  "255",
		},
		// The largest 64-bit unsigned value lies above the range of a signed 64-bit integer, which
		// is the range of an OTLP integer value; it still keeps every one of its digits.
		"success: largest unsigned 64-bit value": {
			value: uint64(math.MaxUint64),
			want:  "18446744073709551615",
		},
		"success: named type of kind uint64 one above 2^53": {
			value: namedUint64(9007199254740993),
			want:  "9007199254740993",
		},
		"success: named type of kind uint16 at its largest value": {
			value: namedUint16(math.MaxUint16),
			want:  "65535",
		},
		"success: largest unsigned 16-bit value": {
			value: uint16(math.MaxUint16),
			want:  "65535",
		},
		"success: uintptr value one above 2^53": {
			value: uintptr(9007199254740993),
			want:  "9007199254740993",
		},
		"success: named type of kind uintptr at the largest 64-bit unsigned value": {
			value: namedUintptr(math.MaxUint64),
			want:  "18446744073709551615",
		},
		"success: unsigned zero": {
			value: uint64(0),
			want:  "0",
		},
		"success: signed 64-bit value one above 2^53": {
			value: int64(9007199254740993),
			want:  "9007199254740993",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			record := Record{EventName: "gateway.token.usage", ObservedAt: time.Unix(123, 0), Attributes: map[string]any{"request_sequence": tt.value}}
			payload, err := record.OTLP()
			if err != nil {
				t.Fatalf("OTLP: %v", err)
			}
			var doc struct {
				ResourceLogs []struct {
					ScopeLogs []struct {
						LogRecords []struct {
							Attributes []struct {
								Key   string         `json:"key"`
								Value map[string]any `json:"value"`
							} `json:"attributes"`
						} `json:"logRecords"`
					} `json:"scopeLogs"`
				} `json:"resourceLogs"`
			}
			if err := json.Unmarshal(payload, &doc); err != nil {
				t.Fatalf("decode %s: %v", payload, err)
			}
			if len(doc.ResourceLogs) != 1 || len(doc.ResourceLogs[0].ScopeLogs) != 1 || len(doc.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
				t.Fatalf("invalid OTLP log envelope: %s", payload)
			}
			attrs := doc.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes
			if len(attrs) != 1 || attrs[0].Key != "request_sequence" {
				t.Fatalf("attributes of %s, want request_sequence alone", payload)
			}
			if diff := gocmp.Diff(map[string]any{"intValue": tt.want}, attrs[0].Value); diff != "" {
				t.Errorf("exported value of %T %v (-want +got):\n%s", tt.value, tt.value, diff)
			}
		})
	}
}
