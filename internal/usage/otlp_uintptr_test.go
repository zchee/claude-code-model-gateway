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
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
)

// uintptrHandle is a named attribute type whose underlying kind is uintptr.
type uintptrHandle uintptr

// uintptrAttrValues are the attribute values of the uintptr encoding tests, with the decimal
// digits each must keep on the wire. 2^53+1 is the first integer a binary64 cannot hold, and the
// largest value lies above the signed 64-bit range.
var uintptrAttrValues = []struct {
	key    string
	value  uint64
	digits string
}{
	{"zero", 0, "0"},
	{"small", 7, "7"},
	{"above_float_precision", 1<<53 + 1, "9007199254740993"},
	{"above_signed_range", 1<<63 + 1, "9223372036854775809"},
	{"largest", 1<<64 - 1, "18446744073709551615"},
}

// uintptrRecord builds a record holding every uintptrAttrValues entry, converted by conv.
func uintptrRecord(t *testing.T, conv func(uint64) any) Record {
	t.Helper()
	attrs := make(map[string]any, len(uintptrAttrValues))
	for _, v := range uintptrAttrValues {
		if uint64(uintptr(v.value)) != v.value {
			t.Fatalf("uintptr cannot hold %d on this platform; the test needs a 64-bit uintptr", v.value)
		}
		attrs[v.key] = conv(v.value)
	}
	return Record{EventName: "gateway.token.usage", ObservedAt: time.Unix(123, 0), Attributes: attrs}
}

// uintptrExportedAttrs decodes the attributes of the single log record of an OTLP payload.
func uintptrExportedAttrs(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var envelope struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					Attributes []map[string]any `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("unmarshal OTLP payload: %v\n%s", err, payload)
	}
	if len(envelope.ResourceLogs) != 1 || len(envelope.ResourceLogs[0].ScopeLogs) != 1 || len(envelope.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
		t.Fatalf("unexpected OTLP envelope structure: %s", payload)
	}
	return envelope.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes
}

// wantUintptrAttrs is the exported attribute list of a uintptrRecord: a record built from a bare
// map exports its keys in sorted order, each value as an intValue string of its decimal digits.
func wantUintptrAttrs() []map[string]any {
	digits := make(map[string]string, len(uintptrAttrValues))
	for _, v := range uintptrAttrValues {
		digits[v.key] = v.digits
	}
	keys := []string{"above_float_precision", "above_signed_range", "largest", "small", "zero"}
	want := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		want = append(want, map[string]any{"key": k, "value": map[string]any{"intValue": digits[k]}})
	}
	return want
}

// TestOTLPUintptrAttributesExact checks that attribute values of kind uintptr, plain and named,
// are encoded as their exact decimal digits, in the same wire form as uint64 values, and that
// the record is encoded rather than refused.
func TestOTLPUintptrAttributesExact(t *testing.T) {
	tests := map[string]struct {
		conv func(uint64) any
	}{
		"success: plain uintptr values keep their digits": {
			conv: func(n uint64) any { return uintptr(n) },
		},
		"success: named uintptr values keep their digits": {
			conv: func(n uint64) any { return uintptrHandle(n) },
		},
		"success: uint64 values keep their digits": {
			conv: func(n uint64) any { return n },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			payload, err := uintptrRecord(t, tt.conv).OTLP()
			if err != nil {
				t.Fatalf("OTLP() error = %v, want the record encoded", err)
			}
			if diff := gocmp.Diff(wantUintptrAttrs(), uintptrExportedAttrs(t, payload)); diff != "" {
				t.Errorf("exported attributes (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEmitterExportsUintptrAttributes checks that the emitter posts a record holding uintptr
// attribute values to its collector, with each value's exact digits, instead of dropping it.
func TestEmitterExportsUintptrAttributes(t *testing.T) {
	tests := map[string]struct {
		conv func(uint64) any
	}{
		"success: plain uintptr record reaches the collector": {
			conv: func(n uint64) any { return uintptr(n) },
		},
		"success: named uintptr record reaches the collector": {
			conv: func(n uint64) any { return uintptrHandle(n) },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			// The pipe collector runs the export bound on virtual time, so the single post is
			// delivered however loaded the host is.
			synctest.Test(t, func(t *testing.T) {
				collector := &pipeCollector{}
				e := collector.emitter(NewBuilder(options(t)))
				e.schedule(uintptrRecord(t, tt.conv))
				e.Wait()

				payloads := collector.received(t)
				if len(payloads) != 1 {
					t.Fatalf("collector received %d payloads, want 1", len(payloads))
				}
				if diff := gocmp.Diff(wantUintptrAttrs(), uintptrExportedAttrs(t, payloads[0])); diff != "" {
					t.Errorf("exported attributes (-want +got):\n%s", diff)
				}
			})
		})
	}
}
