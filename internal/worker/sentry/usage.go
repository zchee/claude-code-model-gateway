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

package sentry

import (
	"cmp"
	"encoding/json/jsontext"
	"math"
	"slices"
	"strconv"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// MergeUsage applies one stream event to accumulated usage, replacing supplied fields rather than summing them.
func MergeUsage(previous, event jsontext.Value) jsontext.Value {
	v, err := jsonx.DecodeWire(event)
	if err != nil {
		return previous
	}
	prev, _ := jsonx.DecodeWire(previous)
	merged, ok := mergeUsage(prev, v)
	if !ok {
		return previous
	}
	out, _ := jsonx.EncodeWire(merged) // Both spreads came from documents within the wire depth limit.
	return out
}

// mergeUsage reports false when the event supplies no object or array usage to spread.
func mergeUsage(previous, event jsonx.WireValue) (jsonx.WireValue, bool) {
	if text(member(event, "type")) == "message_start" {
		event = member(event, "message")
	}
	usage := member(event, "usage")
	if usage.Kind != '{' && usage.Kind != '[' {
		return jsonx.WireValue{}, false
	}
	// A new slice: normalizing in place must not write into the caller's members.
	return jsonx.WireValue{Kind: '{', Members: orderedMembers(slices.Concat(spread(previous), spread(usage)))}, true
}

// orderedMembers applies the decoder's object normalization in place, so a merged value equals
// the decoding of its encoding: the first occurrence of a name keeps its position, the last
// supplies its value, and array-index names sort before all others in numeric order.
func orderedMembers(members []jsonx.Member) []jsonx.Member {
	if len(members) < 2 {
		return members
	}
	positions := make(map[string]int, len(members))
	out := members[:0]
	for _, m := range members {
		// Compare exact code units: decoding to runes would merge distinct lone surrogates.
		key := make([]byte, 0, 2*len(m.Name))
		for _, unit := range m.Name {
			key = append(key, byte(unit>>8), byte(unit))
		}
		if pos, ok := positions[string(key)]; ok {
			out[pos].Value = m.Value
			continue
		}
		positions[string(key)] = len(out)
		out = append(out, m)
	}
	clear(members[len(out):])
	slices.SortStableFunc(out, func(a, b jsonx.Member) int { return cmp.Compare(propertyIndex(a.Name), propertyIndex(b.Name)) })
	return out
}

// propertyIndex returns a JavaScript array index, or math.MaxUint32 for any other name.
func propertyIndex(s []uint16) uint32 {
	if len(s) == 0 || len(s) > 10 || len(s) > 1 && s[0] == '0' {
		return math.MaxUint32
	}
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return math.MaxUint32
		}
		n = n*10 + uint64(c-'0')
	}
	return uint32(min(n, math.MaxUint32))
}

func spread(v jsonx.WireValue) []jsonx.Member {
	if v.Kind == '{' {
		return v.Members
	}
	// Object spread boxes strings, exposing one UTF-16 code unit per index.
	n := max(len(v.Elements), len(v.Text))
	members := make([]jsonx.Member, n)
	for i := range n {
		members[i].Name = wireString(strconv.Itoa(i)).Text
		if v.Kind == '"' {
			members[i].Value = jsonx.WireValue{Kind: '"', Text: v.Text[i : i+1]}
		} else {
			members[i].Value = v.Elements[i]
		}
	}
	return members
}
