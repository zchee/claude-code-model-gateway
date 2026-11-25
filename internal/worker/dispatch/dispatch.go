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
	"encoding/json/jsontext"
	"time"
)

// Route is the model and optional effort and ticket from a briefing marker.
type Route struct {
	Model  string
	Effort *string
	Ticket *string
}

// FromMessages returns the sole user-authored route, or nil, and the number of qualifying markers.
func FromMessages(messages jsontext.Value) (*Route, int) { return nil, 0 }

// CachedRoute preserves the difference between an omitted effort and a null effort.
// A route from a marker has EffortMissing false, even when its Effort is nil.
type CachedRoute struct {
	Route
	EffortMissing bool
}

// Cache remembers session routes in least-recently-used order.
type Cache struct{}

// NewCache loads a cache using the supplied clock, idle lifetime in milliseconds and session limit.
// An empty path disables persistence.
func NewCache(path string, ttlMillis float64, maxSessions int, now func() time.Time) *Cache {
	return &Cache{}
}

// Set remembers a valid route for a nonempty identity; disk writes are best effort.
func (c *Cache) Set(key string, route CachedRoute) {}

// Get returns and refreshes a live route, or returns nil for a missing or expired route.
func (c *Cache) Get(key string) *CachedRoute { return nil }
