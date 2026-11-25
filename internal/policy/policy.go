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

// Package policy resolves gateway model identities, windows and compaction limits.
package policy

import (
	"encoding/json/jsontext"
	"errors"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

// Model describes a table row or a resolved model. Zero window sizes mean unknown.
type Model struct {
	Backend, Transport, BackendID                         string
	BackendWindow, AdvertisedWindow                       int64
	Measurement, PickerAlias, PickerAliasTemplate, Sentry string
	CompactHeadroom                                       int64
}

// Rows returns the window table in declaration order, independent of caller mutations.
func Rows() []Model { return nil }

// Resolver uses a route snapshot and observed server windows without querying running servers.
// LMStudioWindows is keyed by the server's model key, without the provider prefix.
// Nil Windows selects the gateway defaults; zero live windows indicate no measurement.
type Resolver struct {
	Routes          proxymodels.Table
	Windows         Windows
	LocalWindow     int64
	LocalPort       int
	LMStudioURL     string
	LMStudioWindows map[string]int64
}

// BackendID resolves a picker spelling to the backend ID, including any service tier.
func (Resolver) BackendID(id string) string { return "" }

// WindowModelID returns the ID used to share a window between Codex service tiers.
func (Resolver) WindowModelID(id string) string { return "" }

// Resolve returns the model's uncapped policy, or nil for an unrecognized spelling.
func (Resolver) Resolve(id string) *Model { return nil }

// ClientID returns the model spelling to advertise with this resolver's caps.
func (Resolver) ClientID(id string) string { return "" }

// AdvertisedWindow returns the capped client window, or zero when it is unknown.
func (Resolver) AdvertisedWindow(id string) int64 { return 0 }

// CodexWindow returns the advertised window or the Codex fallback when unknown.
func (Resolver) CodexWindow(id string) int64 { return 0 }

// Sentry describes the earliest candidate and the backend window it protects.
type Sentry struct {
	BackendWindow  int64
	CompactTrigger float64
	Source         string
}

// EffectiveSentry selects the lowest trigger, or nil when the model has no sentry.
// A zero cap or compactAt indicates no limit. A nonpositive or nonfinite env value is ignored.
func EffectiveSentry(model *Model, env float64, cap, compactAt int64) (*Sentry, error) {
	return nil, errors.New("sentry not implemented")
}

// CompactTrigger returns the capped direct maximum or the cap less its reserve; zero means absent.
func CompactTrigger(cap, compactAt int64) int64 { return 0 }

// BillingNote describes Codex input billing for the cap and direct compaction maximum.
func BillingNote(cap, compactAt int64) string { return "" }

// Setting is a resolved backend window, its source and any saved direct compaction maximum.
type Setting struct {
	Value     config.ContextWindow
	Source    string
	CompactAt int64
}

// Windows contains the Claude, Codex and Grok settings.
type Windows map[string]Setting

// Cap returns the backend's numeric cap or zero for an uncapped backend.
func (Windows) Cap(backend string) int64 { return 0 }

// ReadWindows resolves saved settings, the supplied environment and backend defaults, in that order.
func ReadWindows(path string, lookup config.LookupEnv) (Windows, error) {
	return nil, errors.New("window reader not implemented")
}

// WriteWindows validates the persisted JSON object before creating its directory and writing it atomically.
func WriteWindows(path string, values jsontext.Value) error {
	return errors.New("window writer not implemented")
}

// Saved returns only the explicitly saved values, in gateway serialization order.
func (Windows) Saved() jsontext.Value { return nil }

// Update changes one window or compactAt field. Zero tokens means full or cap, respectively.
type Update struct {
	Backend, Field string
	Tokens         int64
}

// ParseUpdate validates a flag and its optional argument; nil means no following token.
func ParseUpdate(flag string, value *string) (Update, error) {
	return Update{}, errors.New("window parser not implemented")
}

// ParseCompactAt parses a JSON string or number as a positive safe integer; zero means invalid.
func ParseCompactAt(value jsontext.Value) int64 { return 0 }

// Apply updates a saved settings object, deleting the last compactAt object when emptied.
func (Update) Apply(saved jsontext.Value) (jsontext.Value, error) {
	return nil, errors.New("window update not implemented")
}

// AutoCompact describes a native window restriction and its configuration source.
type AutoCompact struct {
	Window int64
	Source string
}

// Report formats the effective Claude, Codex and Grok windows, in that order.
func (Windows) Report(auto *AutoCompact, lookup config.LookupEnv) []string { return nil }

// WindowNote formats compaction and billing information for a resolved backend row.
func WindowNote(backend string, cap, compactAt int64, model *Model, lookup config.LookupEnv) string {
	return ""
}
