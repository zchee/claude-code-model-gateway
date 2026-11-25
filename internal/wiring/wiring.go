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
	"errors"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
)

// Pins supplies the current pin and picker entries and all values the gateway owns for each key.
type Pins struct {
	Current map[string]string
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

// EffectiveBaseURL reads definitions in process, project-local, project-shared, user order.
func (s Store) EffectiveBaseURL() (Resolution, error) {
	return Resolution{}, errors.New("settings resolution not implemented")
}

// UnsafeProcessOverride returns the effective definition only for a process-level HTTPS override of the compatibility host.
func (r Resolution) UnsafeProcessOverride() *Resolution { return nil }

// BypassedWiring returns the highest-precedence gateway file definition hidden by a foreign process URL.
func (s Store) BypassedWiring(r Resolution) *Definition { return nil }

// Wiring names the writable scope and the source of an observed gateway mode.
type Wiring struct{ Scope, Mode, Source, File string }

// WiredMode prefers file-backed wiring even when an environment definition shadows the file.
func (s Store) WiredMode() (*Wiring, error) { return nil, errors.New("wiring mode not implemented") }

// IsWired reports whether the effective base URL selects a gateway endpoint.
func (s Store) IsWired() (bool, error) { return false, errors.New("wiring check not implemented") }

// EnvBlock returns the static and pin entries for the selected transport.
func (s Store) EnvBlock(mode string) map[string]string { return nil }

// WriteEnv writes or removes gateway-owned settings in the specified scope and verifies the result.
func (s Store) WriteEnv(scope, mode string, remove bool) error {
	return errors.New("settings write not implemented")
}

// SyncReadinessHooks edits only the owned hook entries in the supplied settings document.
func (s Store) SyncReadinessHooks(document []byte, enabled bool) ([]byte, error) {
	return nil, errors.New("readiness hooks not implemented")
}

// ReadinessHookPresent reports whether both prompt and startup have an owned readiness hook.
func (s Store) ReadinessHookPresent(document []byte) bool { return false }

// MigrateReadinessHooks updates registered settings and existing wired user settings.
func (s Store) MigrateReadinessHooks() error {
	return errors.New("readiness migration not implemented")
}

// MigrateLegacy moves only gateway-owned entries from shared project settings to local settings.
func (s Store) MigrateLegacy() (bool, error) {
	return false, errors.New("legacy migration not implemented")
}

// Health describes the compatibility observations supplied by the gateway health check.
type Health struct {
	HostsDetected, Port80Bound bool
	HostsLine                  string
}

// ModeResult records the verified mode, health observations and whether settings changed.
type ModeResult struct {
	Mode    string
	Compat  Health
	Changed bool
}

// SyncMode migrates eligible settings and verifies a mode change against the supplied health observations.
func (s Store) SyncMode(health *Health, required string) (ModeResult, error) {
	return ModeResult{}, errors.New("mode sync not implemented")
}

// Registration identifies a locally wired project and its settings.
type Registration struct{ Project, File, Value string }

// Pruned describes a registry directory that no longer exists.
type Pruned struct{ Project, Reason string }

// Projects reads and deduplicates registrations. A writable scan also repairs an existing registry.
func (s Store) Projects(readOnly bool) ([]Registration, []Pruned, error) {
	return nil, nil, errors.New("registry scan not implemented")
}

// Disagreement describes a missing or stale registered pin. Stale is nil for an absent value.
type Disagreement struct {
	Registration
	Key, Expected string
	Stale         any
}

// PinDisagreements compares even missing pin keys without rewriting the registry or settings.
func (s Store) PinDisagreements() ([]Disagreement, []Pruned, error) {
	return nil, nil, errors.New("pin inspection not implemented")
}

// Reconcile lists projects wired to the other gateway URL and removes only their owned entries on confirmation.
func (s Store) Reconcile(target string, confirm bool) (conflicting, reconciled []Registration, err error) {
	return nil, nil, errors.New("wiring reconciliation not implemented")
}

// RecordProject records the project only if its local settings are wired.
func (s Store) RecordProject() error { return errors.New("project registration not implemented") }

// Skipped identifies a settings file or key a synchronization could not update.
type Skipped struct{ File, Key, Reason string }

// Changes lists updated settings files and files skipped by a sync in input order.
type Changes struct {
	Changed []string
	Skipped []Skipped
}

// SyncPins updates registered projects without replacing user-owned pins or choices.
func (s Store) SyncPins() (Changes, error) { return Changes{}, errors.New("pin sync not implemented") }

// Repair removes orphaned gateway entries and records wired projects missing from the registry.
func (s Store) Repair(report func(string)) error {
	return errors.New("settings repair not implemented")
}

// SyncWindow updates only a missing or previously owned cap. Nil next selects the full window.
func SyncWindow(files []string, owned, next *int) Changes { return Changes{} }

// CleanModelCache normalizes owned model IDs only in valid gateway caches. The function returns the canonical ID and whether it is gateway-owned.
func (s Store) CleanModelCache(file string, canonical func(string) (string, bool)) bool { return false }
