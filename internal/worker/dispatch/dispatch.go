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

// Package dispatch recognizes conversation route markers and remembers session routes.
package dispatch

import (
	"container/list"
	"encoding/json/jsontext"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

const (
	modelPattern  = `[a-z0-9][a-z0-9.-]{0,63}`
	effortPattern = `low|medium|high|xhigh|max`
	ticketPattern = `[A-Za-z][A-Za-z0-9_-]{0,63}`
)

var (
	markerRE = regexp.MustCompile(`\[sidequest-route model=(` + modelPattern + `)(?: effort=(` + effortPattern + `))?(?: ticket=(` + ticketPattern + `))?\]`)
	modelRE  = regexp.MustCompile(`^` + modelPattern + `$`)
	effortRE = regexp.MustCompile(`^(?:` + effortPattern + `)$`)
	ticketRE = regexp.MustCompile(`^` + ticketPattern + `$`)
)

// Route is the model and optional effort and ticket from a briefing marker.
type Route struct {
	Model  string
	Effort *string
	Ticket *string
}

// FromMessages returns the sole user-authored route, or nil, and the number of qualifying markers.
func FromMessages(messages jsontext.Value) (*Route, int) {
	root, err := jsonx.DecodeWire(messages)
	if err != nil || root.Kind != '[' {
		return nil, 0
	}
	var route *Route
	var count int
	scan := func(text string) {
		for {
			match := markerRE.FindStringSubmatchIndex(text)
			if match == nil {
				return
			}
			count++
			if count == 1 {
				route = &Route{Model: strings.Clone(text[match[2]:match[3]])}
				if match[4] >= 0 {
					route.Effort = new(strings.Clone(text[match[4]:match[5]]))
				}
				if match[6] >= 0 {
					route.Ticket = new(strings.Clone(text[match[6]:match[7]]))
				}
			}
			text = text[match[1]:]
		}
	}
	for _, message := range root.Elements {
		if string(utf16.Decode(property(message, "role").Text)) != "user" {
			continue
		}
		content := property(message, "content")
		switch content.Kind {
		case '"':
			scan(string(utf16.Decode(content.Text)))
		case '[':
			for _, block := range content.Elements {
				if string(utf16.Decode(property(block, "type").Text)) == "text" {
					text := property(block, "text")
					if text.Kind == '"' {
						scan(string(utf16.Decode(text.Text)))
					}
				}
			}
		}
	}
	if count != 1 {
		return nil, count
	}
	return route, count
}

func property(object jsonx.WireValue, name string) jsonx.WireValue {
	for _, member := range object.Members {
		if len(member.Name) == len(name) && string(utf16.Decode(member.Name)) == name {
			return member.Value
		}
	}
	return jsonx.WireValue{}
}

// CachedRoute preserves the difference between an omitted effort and a null effort.
// A route from a marker has EffortMissing false, even when its Effort is nil.
type CachedRoute struct {
	Route
	EffortMissing bool
}

type cacheEntry struct {
	key        string   // Canonical JSON string used for key equality.
	keyText    []uint16 // The original key, including any unpaired surrogate loaded from disk.
	route      CachedRoute
	lastUsedAt float64
}

// Cache remembers session routes in least-recently-used order.
type Cache struct {
	mu          sync.Mutex
	path        string
	ttlMillis   float64
	maxSessions int
	now         func() time.Time
	byKey       map[string]*list.Element
	lru         list.List
}

// NewCache loads a cache using the supplied clock, idle lifetime in milliseconds and session limit.
// The limit bounds live entries, not the size of the file read at startup.
// An empty path disables persistence.
func NewCache(path string, ttlMillis float64, maxSessions int, now func() time.Time) *Cache {
	c := &Cache{path: path, ttlMillis: ttlMillis, maxSessions: max(maxSessions, 0), now: now, byKey: make(map[string]*list.Element)}
	c.load()
	return c
}

// Set remembers a valid route for a nonempty UTF-8 key; disk writes are best effort.
func (c *Cache) Set(key string, route CachedRoute) {
	if key == "" || !validRoute(route) {
		return
	}
	quoted, err := jsontext.AppendQuote(nil, key)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := float64(c.now().UnixMilli())
	c.prune(now)
	entry := cacheEntry{key: string(quoted), keyText: utf16.Encode([]rune(key)), route: cloneRoute(route), lastUsedAt: now}
	if el := c.byKey[entry.key]; el != nil {
		el.Value = entry
		c.lru.MoveToBack(el)
	} else {
		c.byKey[entry.key] = c.lru.PushBack(entry)
	}
	c.prune(now)
	c.persist()
}

// Get returns and refreshes a live route for a nonempty UTF-8 key.
// It returns nil for an invalid key or a missing or expired route.
func (c *Cache) Get(key string) *CachedRoute {
	if key == "" {
		return nil
	}
	quoted, err := jsontext.AppendQuote(nil, key)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.byKey[string(quoted)]
	if el == nil {
		return nil
	}
	entry := el.Value.(cacheEntry)
	now := float64(c.now().UnixMilli())
	if c.expired(entry, now) {
		delete(c.byKey, entry.key)
		c.lru.Remove(el)
		c.persist()
		return nil
	}
	entry.lastUsedAt = now
	el.Value = entry
	c.lru.MoveToBack(el)
	c.persist()
	return new(cloneRoute(entry.route))
}

func validRoute(route CachedRoute) bool {
	return modelRE.MatchString(route.Model) &&
		(route.Effort == nil || !route.EffortMissing && effortRE.MatchString(*route.Effort)) &&
		(route.Ticket == nil || ticketRE.MatchString(*route.Ticket))
}

func cloneRoute(route CachedRoute) CachedRoute {
	if route.Effort != nil {
		route.Effort = new(*route.Effort)
	}
	if route.Ticket != nil {
		route.Ticket = new(*route.Ticket)
	}
	return route
}

func (c *Cache) expired(entry cacheEntry, now float64) bool {
	return now-entry.lastUsedAt >= c.ttlMillis
}

func (c *Cache) prune(now float64) {
	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		if entry := el.Value.(cacheEntry); c.expired(entry, now) {
			delete(c.byKey, entry.key)
			c.lru.Remove(el)
		}
		el = next
	}
	for c.lru.Len() > c.maxSessions {
		el := c.lru.Front()
		delete(c.byKey, el.Value.(cacheEntry).key)
		c.lru.Remove(el)
	}
}

func (c *Cache) load() {
	if c.path == "" {
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	root, err := jsonx.DecodeWire(data)
	if err != nil {
		return
	}
	version, rows := property(root, "version"), property(root, "routes")
	if version.Kind != '0' || version.Number != 1 || rows.Kind != '[' {
		return
	}
	// Resolve duplicate keys before expiry and eviction: the first valid row fixes
	// the position and the last valid row supplies the value.
	positions := make(map[string]int)
	var entries []cacheEntry
	for _, row := range rows.Elements {
		entry, ok := decodeEntry(row)
		if !ok {
			continue
		}
		if pos, ok := positions[entry.key]; ok {
			entries[pos] = entry
		} else {
			positions[entry.key] = len(entries)
			entries = append(entries, entry)
		}
	}
	now := float64(c.now().UnixMilli())
	entries = slices.DeleteFunc(entries, func(entry cacheEntry) bool { return c.expired(entry, now) })
	survivors := entries[max(len(entries)-c.maxSessions, 0):]
	c.byKey = make(map[string]*list.Element, len(survivors))
	for _, entry := range survivors {
		c.byKey[entry.key] = c.lru.PushBack(entry)
	}
	c.persist()
}

func decodeEntry(row jsonx.WireValue) (cacheEntry, bool) {
	if len(row.Elements) < 2 || row.Elements[0].Kind != '"' {
		return cacheEntry{}, false
	}
	raw := row.Elements[1]
	model, effort := property(raw, "model"), property(raw, "effort")
	ticket, used := property(raw, "ticket"), property(raw, "lastUsedAt")
	if model.Kind != '"' || used.Kind != '0' || math.IsInf(used.Number, 0) {
		return cacheEntry{}, false
	}
	if effort.Kind != 0 && effort.Kind != 'n' && effort.Kind != '"' ||
		ticket.Kind != 0 && ticket.Kind != 'n' && ticket.Kind != '"' {
		return cacheEntry{}, false
	}
	route := CachedRoute{Model: string(utf16.Decode(model.Text)), EffortMissing: effort.Kind == 0}
	if effort.Kind == '"' {
		route.Effort = new(string(utf16.Decode(effort.Text)))
	}
	if ticket.Kind == '"' {
		route.Ticket = new(string(utf16.Decode(ticket.Text)))
	}
	if !validRoute(route) {
		return cacheEntry{}, false
	}
	key, err := jsonx.EncodeWire(row.Elements[0])
	return cacheEntry{key: string(key), keyText: row.Elements[0].Text, route: route, lastUsedAt: used.Number}, err == nil
}

func stateMember(name string, value jsonx.WireValue) jsonx.Member {
	return jsonx.Member{Name: utf16.Encode([]rune(name)), Value: value}
}

func stateString(value *string) jsonx.WireValue {
	if value == nil {
		return jsonx.WireValue{Kind: 'n'}
	}
	return jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune(*value))}
}

func (c *Cache) persist() {
	if c.path == "" {
		return
	}
	routes := make([]jsonx.WireValue, 0, c.lru.Len())
	for el := c.lru.Front(); el != nil; el = el.Next() {
		entry := el.Value.(cacheEntry)
		members := make([]jsonx.Member, 0, 4)
		members = append(members, stateMember("model", stateString(&entry.route.Model)))
		if !entry.route.EffortMissing {
			members = append(members, stateMember("effort", stateString(entry.route.Effort)))
		}
		members = append(members,
			stateMember("ticket", stateString(entry.route.Ticket)),
			stateMember("lastUsedAt", jsonx.WireValue{Kind: '0', Number: entry.lastUsedAt}),
		)
		routes = append(routes, jsonx.WireValue{Kind: '[', Elements: []jsonx.WireValue{
			{Kind: '"', Text: entry.keyText},
			{Kind: '{', Members: members},
		}})
	}
	data, err := jsonx.EncodeWire(jsonx.WireValue{Kind: '{', Members: []jsonx.Member{
		stateMember("version", jsonx.WireValue{Kind: '0', Number: 1}),
		stateMember("routes", jsonx.WireValue{Kind: '[', Elements: routes}),
	}})
	if err != nil {
		return
	}
	// Do not clean the parent separately: a symlink followed by .. is resolved by the filesystem.
	parent, _ := filepath.Split(c.path)
	if parent != "" && os.MkdirAll(parent, 0o777) != nil {
		return
	}
	_ = atomicfile.WriteFile(c.path, append(data, '\n'), 0o600)
}
