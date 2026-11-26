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
	"container/list"
	"math"
	"strconv"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

type cacheEntry[K comparable, V any] struct {
	key   K
	value V
	at    time.Time
}

type recent[K comparable, V any] struct {
	byKey map[K]*list.Element
	order list.List
}

func (r *recent[K, V]) pop(e *list.Element) {
	delete(r.byKey, e.Value.(cacheEntry[K, V]).key)
	r.order.Remove(e)
}

func (r *recent[K, V]) put(k K, v V, at time.Time, limit int) {
	if r.byKey == nil {
		r.byKey = make(map[K]*list.Element)
	}
	if old := r.byKey[k]; old != nil {
		r.pop(old)
	}
	r.byKey[k] = r.order.PushBack(cacheEntry[K, V]{k, v, at})
	if len(r.byKey) > limit {
		r.pop(r.order.Front())
	}
}

type blockKey struct {
	id    string
	bytes int64
}
type block struct {
	blockKey
	toolID, toolName string
}
type (
	identity struct{ session, agent, parent string }
	snapshot struct {
		blocks []block
		tokens float64
	}
)

func messageBlocks(body jsonx.WireValue) []block {
	messages := field(body, "messages").Elements
	names := make(map[string]string)
	for _, m := range messages {
		for _, v := range field(m, "content").Elements {
			if text(v, "type") == "tool_use" {
				id, name := safeID(text(v, "id")), safeID(text(v, "name"))
				if id != "" && name != "" {
					names[id] = name
				}
			}
		}
	}
	var blocks []block
	for i, m := range messages {
		content := field(m, "content")
		entries := content.Elements
		if content.Kind != '[' && content.Kind != 0 {
			entries = []jsonx.WireValue{content}
		}
		for j, v := range entries {
			kind := safeID(text(v, "type"))
			if kind == "" {
				kind = "content"
				if v.Kind == '"' {
					kind = "text"
				}
			}
			toolID := ""
			if isResult(kind) {
				toolID = safeID(text(v, "tool_use_id"))
			}
			id := safeID(text(v, "id"))
			if id == "" {
				id = toolID
			}
			if id != "" {
				id = kind + ":" + id
			} else {
				id = strconv.Itoa(i) + ":" + strconv.Itoa(j) + ":" + kind
			}
			blocks = append(blocks, block{blockKey{id, byteSize(v)}, toolID, names[toolID]})
		}
	}
	return blocks
}

func (b *Builder) toolRecords(r Record, key identity, body jsonx.WireValue) []Record {
	if key.session == "" {
		return nil
	}
	if key.agent != "" {
		key.parent = ""
	}
	total, ok := numeric(r.Attributes["context_tokens"])
	if !ok {
		return nil
	}
	now := r.ObservedAt
	cache := &b.snapshots
	for front := cache.order.Front(); front != nil; front = cache.order.Front() {
		if now.Sub(front.Value.(cacheEntry[identity, snapshot]).at) < b.options.SnapshotTTL {
			break
		}
		cache.pop(front)
	}
	prev := cache.byKey[key]
	cur := snapshot{messageBlocks(body), math.Floor(total)}
	cache.put(key, cur, now, b.options.MaxIdentities)
	if prev == nil {
		return nil
	}
	old := prev.Value.(cacheEntry[identity, snapshot]).value
	delta := cur.tokens - old.tokens
	if delta < 0 {
		return nil
	}
	remaining := make(map[blockKey]int, len(old.blocks))
	for _, v := range old.blocks {
		remaining[v.blockKey]++
	}
	var added []block
	var weights []int64
	for _, v := range cur.blocks {
		if remaining[v.blockKey] > 0 {
			remaining[v.blockKey]--
			continue
		}
		added = append(added, v)
		weights = append(weights, v.bytes)
	}
	var records []Record
	allocations := shares(delta, weights)
	quality := "estimate"
	if len(added) == 1 {
		allocations[0] = delta
		quality = "derived_exact"
	}
	for i, v := range added {
		if v.toolID == "" {
			continue
		}
		rec := b.derived(r, "gateway.tool_result.usage", "gateway-tool-result-")
		rec.Attributes["tool_use_id"] = v.toolID
		rec.Attributes["tool_name"] = nullable(v.toolName)
		rec.Attributes["tool_result_tokens"] = allocations[i]
		rec.Attributes["tool_result_tokens_unit"] = "tokens"
		rec.Attributes["tool_result_tokens_quality"] = quality
		records = append(records, rec)
	}
	return records
}
