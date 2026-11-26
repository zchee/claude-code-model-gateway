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
	"errors"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

func wireString(s string) jsonx.WireValue {
	return jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune(s))}
}

func member(key string, v jsonx.WireValue) jsonx.Member {
	return jsonx.Member{Name: utf16.Encode([]rune(key)), Value: v}
}
func object(m ...jsonx.Member) jsonx.WireValue   { return jsonx.WireValue{Kind: '{', Members: m} }
func array(v ...jsonx.WireValue) jsonx.WireValue { return jsonx.WireValue{Kind: '[', Elements: v} }
func stringValue(s string) jsonx.WireValue       { return object(member("stringValue", wireString(s))) }

func otlpValue(value any) (jsonx.WireValue, error) {
	v := reflect.ValueOf(value)
	var n float64
	switch v.Kind() {
	case reflect.Bool:
		kind := jsontext.Kind('f')
		if v.Bool() {
			kind = 't'
		}
		return object(member("boolValue", jsonx.WireValue{Kind: kind})), nil
	case reflect.String:
		return stringValue(v.String()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n = float64(v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n = float64(v.Uint())
	case reflect.Float32, reflect.Float64:
		n = v.Float()
	default:
		return jsonx.WireValue{}, errors.New("usage: unsupported attribute type")
	}
	number := jsonx.WireValue{Kind: '0', Number: n}
	if !math.IsInf(n, 0) && n == math.Trunc(n) {
		raw, _ := jsonx.EncodeWire(number)
		return object(member("intValue", wireString(string(raw)))), nil
	}
	return object(member("doubleValue", number)), nil
}

// OTLP encodes this record as an OTLP/HTTP JSON log export payload.
func (r Record) OTLP() (jsontext.Value, error) {
	order := r.order
	if len(order) == 0 { // Records created from a bare map have no source insertion order.
		order = slices.Sorted(maps.Keys(r.Attributes))
	}
	attrs := make([]jsonx.WireValue, 0, len(order))
	for _, key := range order {
		val := r.Attributes[key]
		if val == nil {
			continue
		}
		v, err := otlpValue(val)
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, object(member("key", wireString(key)), member("value", v)))
	}
	stamp := "0"
	if ms := r.ObservedAt.UnixMilli(); ms != 0 {
		stamp = strconv.FormatInt(ms, 10) + "000000"
	}
	log := object(
		member("timeUnixNano", wireString(stamp)),
		member("observedTimeUnixNano", wireString(stamp)),
		member("eventName", wireString(r.EventName)),
		member("body", stringValue(r.EventName)),
		member("attributes", array(attrs...)),
	)
	if r.TraceID != "" {
		log.Members = append(log.Members, member("traceId", wireString(r.TraceID)))
	}
	if r.SpanID != "" {
		log.Members = append(log.Members, member("spanId", wireString(r.SpanID)))
	}
	service := object(member("key", wireString("service.name")), member("value", stringValue("codex-gateway")))
	resource := object(member("attributes", array(service)))
	scope := object(member("name", wireString("eigenwise.codex-gateway.usage")), member("version", wireString("1")))
	scopeLogs := array(object(member("scope", scope), member("logRecords", array(log))))
	payload := object(member("resourceLogs", array(object(member("resource", resource), member("scopeLogs", scopeLogs)))))
	data, err := jsonx.EncodeWire(payload)
	if err != nil {
		return nil, errors.New("usage: cannot encode log record")
	}
	return data, nil
}
