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

// Package upstreamstate persists the evidence and expiry of Codex dispatch refusals.
package upstreamstate

import (
	"errors"
	"net/http"
	"time"
)

// Store uses a gateway state directory and an injected clock.
type Store struct {
	Dir string
	Now func() time.Time
}

// Blocked describes an upstream rejection; only rate limits have an expiry.
type Blocked struct {
	State       string   `json:"state"`
	ObservedAt  string   `json:"observedAt"`
	StatusCode  int      `json:"statusCode"`
	Evidence    string   `json:"evidence"`
	ExpiresAt   string   `json:"expiresAt,omitzero"`
	ExpiresAtMs *float64 `json:"expiresAtMs,omitzero"`
}

// Unavailable describes a transient upstream failure and its recovery window.
type Unavailable struct {
	State        string `json:"state"`
	ObservedAt   string `json:"observedAt"`
	ObservedAtMs int64  `json:"observedAtMs"`
	ExpiresAt    string `json:"expiresAt"`
	StatusCode   int    `json:"statusCode"`
}

// SetBlocked persists the upstream rejection and returns the recorded state.
func (s Store) SetBlocked(statusCode int, evidence string, headers http.Header) (*Blocked, error) {
	return nil, errors.New("SetBlocked not implemented")
}

// ReadBlocked returns an active block, or nil for missing, unreadable or expired state.
func (s Store) ReadBlocked() *Blocked { return nil }

// ClearBlocked removes the recorded rejection if present.
func (s Store) ClearBlocked() {}

// SetUnavailable records a transient failure unless a block is active.
func (s Store) SetUnavailable(statusCode int) (*Unavailable, error) {
	return nil, errors.New("SetUnavailable not implemented")
}

// ReadUnavailable returns an active transient failure, or nil if none can be read.
func (s Store) ReadUnavailable() *Unavailable { return nil }

// ClearUnavailable removes the recorded transient failure if present.
func (s Store) ClearUnavailable() {}

// ReadinessMessage describes the block and how to resume dispatch using the stable binary.
func (b Blocked) ReadinessMessage(commandPath string) string { return "" }

// ReadinessMessage describes when the transient failure occurred and when it lifts.
func (u Unavailable) ReadinessMessage() string { return "" }
