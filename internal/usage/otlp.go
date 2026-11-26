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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

type logAttribute struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func otlpValue(value any) (map[string]any, error) {
	v := reflect.ValueOf(value)
	var n float64
	switch v.Kind() {
	case reflect.Bool:
		return map[string]any{"boolValue": v.Bool()}, nil
	case reflect.String:
		return map[string]any{"stringValue": v.String()}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n = float64(v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n = float64(v.Uint())
	case reflect.Float32, reflect.Float64:
		n = v.Float()
	default:
		return nil, errors.New("usage: unsupported attribute type")
	}
	if math.IsInf(n, 0) || math.IsNaN(n) {
		return map[string]any{"doubleValue": nil}, nil
	}
	if n == math.Trunc(n) {
		raw, e := jsonx.EncodeWire(jsonx.WireValue{Kind: '0', Number: n})
		if e != nil {
			return nil, errors.New("usage: cannot encode integer")
		}
		return map[string]any{"intValue": string(raw)}, nil
	}
	return map[string]any{"doubleValue": n}, nil
}

// OTLP encodes this record as an OTLP/HTTP JSON log export payload.
func (r Record) OTLP() (jsontext.Value, error) {
	attrs := make([]logAttribute, 0, len(r.Attributes))
	for _, k := range slices.Sorted(maps.Keys(r.Attributes)) {
		if r.Attributes[k] == nil {
			continue
		}
		v, e := otlpValue(r.Attributes[k])
		if e != nil {
			return nil, e
		}
		attrs = append(attrs, logAttribute{k, v})
	}
	stamp := "0"
	if ms := r.ObservedAt.UnixMilli(); ms != 0 {
		stamp = strconv.FormatInt(ms, 10) + "000000"
	}
	log := map[string]any{"timeUnixNano": stamp, "observedTimeUnixNano": stamp, "eventName": r.EventName, "body": map[string]string{"stringValue": r.EventName}, "attributes": attrs}
	if r.TraceID != "" {
		log["traceId"] = r.TraceID
	}
	if r.SpanID != "" {
		log["spanId"] = r.SpanID
	}
	scope := map[string]any{"scope": map[string]string{"name": "eigenwise.codex-gateway.usage", "version": "1"}, "logRecords": []any{log}}
	resource := map[string]any{"attributes": []logAttribute{{"service.name", map[string]any{"stringValue": "codex-gateway"}}}}
	data, e := json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{"resource": resource, "scopeLogs": []any{scope}}}}, json.Deterministic(true))
	if e != nil {
		return nil, errors.New("usage: cannot encode log record")
	}
	return data, nil
}
