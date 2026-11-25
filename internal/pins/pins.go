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

// Package pins selects the model for each picker alias and identifies settings it owns.
package pins

// Version contains the lowercased family and numeric fields of a concrete model ID.
type Version struct {
	Family       string
	Major, Minor float64
}

// ParseVersion parses a canonical pin, treating an omitted minor as zero.
func ParseVersion(id string) (Version, bool) { return Version{}, false }

// Compare returns the sign of the version difference, or false for unrelated or invalid IDs.
func Compare(first, second string) (float64, bool) { return 0, false }

// Cache holds normalized pin history from prior detections and catalog refreshes.
type Cache struct {
	CLIVersion                         string
	Detected, DetectedFor, CatalogPins map[string]string
	Retired                            map[string][]string
}

// Candidate is the part of an upstream catalog row needed for pin selection.
type Candidate struct{ ID, Lifecycle string }

// Inputs combines the detection snapshot, saved overrides and currently visible models.
type Inputs struct {
	Cache     Cache
	Overrides map[string]string
	Catalog   []Candidate
}

// Pin distinguishes the live value from its default and the independent candidate sources.
type Pin struct{ Default, Shipped, Detected, Override, Newest, Value, Source string }

// Effective returns the selected values and their provenance, keyed by alias.
func (Inputs) Effective() map[string]Pin { return nil }

// FollowCatalog returns the updated pin fields and whether to persist them.
// The caller retains detection metadata such as the last probe timestamp.
func (Inputs) FollowCatalog() (Cache, bool) { return Cache{}, false }

// NormalizeDetected validates a CLI result for an alias and normalizes its context suffix.
func NormalizeDetected(alias, value string) (string, bool) { return "", false }

// ValidOverride reports whether a string is a usable explicit pin.
func ValidOverride(value string) bool { return false }

// Entry is one setting in installation order.
type Entry struct{ Key, Value string }

// PinEnv returns the model defaults and custom Fable picker option in settings order.
func (Inputs) PinEnv() []Entry { return nil }

// EnvBlock returns the complete settings block using the injected default gateway URL.
func (Inputs) EnvBlock(mode, defaultURL string) []Entry { return nil }

// Owned lists values the gateway recognizes under each pin setting key.
// The sets are unordered; iterate PinEnv for settings order.
func (Inputs) Owned() map[string]map[string]bool { return nil }

// Update describes one stale gateway-written setting.
type Update struct{ Key, From, To string }

// Stale returns changes only for present, owned string values, in pin-setting order.
func (Inputs) Stale(env map[string]any) []Update { return nil }
