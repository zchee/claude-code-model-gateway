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

// Package wiring manages the gateway's entries in Claude settings and project registrations.
package wiring

import (
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
)

// Entry is one environment entry in gateway settings order.
type Entry struct{ Key, Value string }

// Pins supplies alias and picker entries in source order, with their historical owned values.
// Current determines traversal order; Owned is only used for per-key lookup.
type Pins struct {
	Current []Entry
	Owned   map[string][]string
}

// Store resolves project and user settings without consulting the process environment.
// Replace, when non-nil, wraps the settings replacement operation for a file transaction.
type Store struct {
	Paths   paths.Resolver
	Project string
	Lookup  config.LookupEnv
	Pins    Pins
	Replace func(string, func([]byte) ([]byte, error)) error
}

// Definition identifies a base URL and the file or environment that supplied it.
type Definition struct{ Source, File, Value string }

// Resolution lists the winning definition and those it shadows, highest precedence first.
type Resolution struct {
	Winner   *Definition
	Shadowed []Definition
}

// Wiring names the writable scope and the source of an observed gateway mode.
type Wiring struct{ Scope, Mode, Source, File string }

// Health describes the compatibility observations supplied by the gateway health check.
type Health struct {
	HostsDetected, Port80Bound bool
	HostsLine, Reason          string
}

// ModeResult records the verified mode, health observations and whether settings changed.
type ModeResult struct {
	Mode    string
	Compat  Health
	Changed bool
}

// Registration identifies a locally wired project and its settings.
type Registration struct{ Project, File, Value string }

// Pruned describes a registry directory that no longer exists.
type Pruned struct{ Project, Reason string }

// Disagreement describes a registered pin. Present differentiates JSON null from an absent key.
// Values containing unpaired UTF-16 surrogates are retained as jsonx.WireValue until serialization.
type Disagreement struct {
	Registration
	Key, Expected string
	Stale         any
	Present       bool
}

// Skipped identifies a settings file or key a synchronization could not update.
// Values containing unpaired UTF-16 surrogates are retained as jsonx.WireValue until serialization.
type Skipped struct {
	File, Key, Reason string
	Value             any
}

// Changes lists updated settings files, skipped files and missing registered projects in input order.
type Changes struct {
	Changed []string
	Skipped []Skipped
	Pruned  []Pruned
}
