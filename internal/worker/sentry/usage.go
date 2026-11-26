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
	"encoding/json/jsontext"
	"strconv"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// MergeUsage applies one stream event to accumulated usage, replacing supplied fields rather than summing them.
func MergeUsage(previous, event jsontext.Value) jsontext.Value {
	v, err := jsonx.DecodeWire(event)
	if err != nil {
		return previous
	}
	if text(member(v, "type")) == "message_start" {
		v = member(v, "message")
	}
	usage := member(v, "usage")
	if usage.Kind != '{' && usage.Kind != '[' {
		return previous
	}
	prev, _ := jsonx.DecodeWire(previous)
	// EncodeWire retains the first position of each key and supplies its last value.
	out := jsonx.WireValue{Kind: '{', Members: append(spread(prev), spread(usage)...)}
	merged, _ := jsonx.EncodeWire(out) // Both spreads came from documents within the wire depth limit.
	return merged
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
