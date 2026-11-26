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

import (
	"cmp"
	"encoding/json/jsontext"
	"maps"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

const (
	optionKey         = "ANTHROPIC_CUSTOM_MODEL_OPTION"
	optionName        = optionKey + "_NAME"
	optionDescription = optionKey + "_DESCRIPTION"
)

type alias struct {
	name, key, shipped string
	retired            []string
}

func aliases() [3]alias {
	return [3]alias{
		{"opus", "ANTHROPIC_DEFAULT_OPUS_MODEL", "claude-opus-5-5[1m]", []string{"claude-opus-5[1m]", "claude-opus-4-8[1m]"}},
		{"sonnet", "ANTHROPIC_DEFAULT_SONNET_MODEL", "claude-sonnet-5-5[1m]", []string{"claude-sonnet-5[1m]"}},
		{"fable", "ANTHROPIC_DEFAULT_FABLE_MODEL", "claude-fable-5-1[1m]", nil},
	}
}

// Version contains the lowercased family and numeric fields of a concrete model ID.
type Version struct {
	Family       string
	Major, Minor float64
}

// ParseVersion parses a canonical pin, treating an omitted minor as zero.
func ParseVersion(id string) (Version, bool) {
	if !utf8.ValidString(id) {
		return Version{}, false
	}
	rest, ok := strings.CutPrefix(strings.TrimSuffix(id, "[1m]"), "claude-")
	family, fields, hasMajor := strings.Cut(rest, "-")
	if !ok || !hasMajor {
		return Version{}, false
	}
	major, minor, hasMinor := strings.Cut(fields, "-")
	// Longer decimal integers stringify in exponential notation, so none round-trip.
	if len(major) > 21 || len(minor) > 21 || strings.ContainsRune(minor, '-') {
		return Version{}, false
	}
	if !hasMinor {
		minor = "0"
	}
	v := Version{Family: cases.Lower(language.Und).String(family), Major: config.JSParseInt(major, 10), Minor: config.JSParseInt(minor, 10)}
	if numberText(v.Major) != major || numberText(v.Minor) != minor {
		return Version{}, false
	}
	return v, true
}

func numberText(n float64) string {
	if math.IsNaN(n) {
		return "NaN"
	}
	if math.IsInf(n, 1) {
		return "Infinity"
	}
	return string(jsontext.AppendFloat(nil, n, 64))
}

// Compare returns the sign of the version difference, or false for unrelated or invalid IDs.
// Unicode lowercasing uses bounded context: for long families or runs of combining
// marks after a capital sigma, spellings JavaScript equates may compare as unrelated.
func Compare(first, second string) (float64, bool) {
	a, ok := ParseVersion(first)
	b, valid := ParseVersion(second)
	if !ok || !valid || a.Family != b.Family {
		return 0, false
	}
	diff := a.Major - b.Major
	if diff == 0 || math.IsNaN(diff) {
		diff = a.Minor - b.Minor
	}
	if diff == 0 || math.IsNaN(diff) {
		return diff, true
	}
	return math.Copysign(1, diff), true
}

// DisplayName formats a concrete model number, retaining an explicitly written zero minor.
func DisplayName(id string) string {
	v, ok := ParseVersion(id)
	if !ok || v.Family == "" {
		return id
	}
	r, size := utf8.DecodeRuneInString(v.Family)
	family := v.Family
	// JavaScript indexes the first UTF-16 unit, not the first code point.
	if r <= 0xffff {
		family = cases.Upper(language.Und).String(family[:size]) + family[size:]
	}
	name := family + " " + numberText(v.Major)
	if strings.Count(id, "-") == 3 {
		name += "." + numberText(v.Minor)
	}
	return name
}

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
func (in Inputs) Effective() map[string]Pin {
	families := aliases()
	byName := make(map[string]alias, len(families))
	result := make(map[string]Pin, len(families))
	for _, a := range families {
		byName[a.name] = a
		p := Pin{Default: a.shipped, Shipped: a.shipped}
		if in.Cache.CLIVersion != "" && in.Cache.DetectedFor[a.name] == in.Cache.CLIVersion {
			p.Detected = in.Cache.Detected[a.name]
			if a.accepts(p.Detected) {
				p.Default = p.Detected
			}
		}
		saved := in.Cache.CatalogPins[a.name]
		if sign, _ := Compare(saved, p.Default); sign == 1 {
			p.Default = saved
		}
		if value := in.Overrides[a.name]; ValidOverride(value) {
			p.Override = value
		}
		p.Value = cmp.Or(p.Override, p.Default)
		switch {
		case p.Override != "":
			p.Source = "override"
		case p.Default == saved:
			p.Source = "catalog"
		case p.Default == p.Detected:
			p.Source = "detected"
		default:
			p.Source = "shipped"
		}
		result[a.name] = p
	}
	for _, row := range in.Catalog {
		if row.Lifecycle == "deprecated" || row.Lifecycle == "retired" {
			continue
		}
		v, ok := ParseVersion(row.ID)
		if !ok {
			continue
		}
		a, ok := byName[v.Family]
		if !ok {
			continue
		}
		pin, ok := NormalizeDetected(a.name, row.ID)
		if !ok || !a.accepts(pin) || slices.Contains(in.Cache.Retired[a.name], pin) {
			continue
		}
		p := result[a.name]
		if sign, _ := Compare(pin, p.Newest); p.Newest == "" || sign == 1 {
			p.Newest = pin
			result[a.name] = p
		}
	}
	return result
}

// OutranksShipped reports whether pin may stand in for the shipped pin of the named alias family, as
// the source's detectedPinOutranksShipped decides (lib/pins.js:119-121): pin is not one of the
// family's retired shipped pins and does not rank below its shipped pin. An empty pin or an unknown
// family is never eligible.
func OutranksShipped(family, pin string) bool {
	for _, a := range aliases() {
		if a.name == family {
			return a.accepts(pin)
		}
	}
	return false
}

func (a alias) accepts(pin string) bool {
	if pin == "" || slices.Contains(a.retired, pin) {
		return false
	}
	sign, _ := Compare(pin, a.shipped)
	return sign != -1
}

// FollowCatalog returns the updated pin fields and whether to persist them.
// The caller retains detection metadata such as the last probe timestamp.
func (in Inputs) FollowCatalog() (Cache, bool) {
	c := in.Cache
	c.Detected = maps.Clone(c.Detected)
	c.DetectedFor = maps.Clone(c.DetectedFor)
	c.CatalogPins = make(map[string]string, 3)
	c.Retired = make(map[string][]string, 3)
	effective := in.Effective()
	for _, a := range aliases() {
		history := slices.Clone(in.Cache.Retired[a.name])
		p := effective[a.name]
		saved := in.Cache.CatalogPins[a.name]
		if sign, _ := Compare(p.Newest, p.Default); sign == 1 {
			history = slices.DeleteFunc(history, func(old string) bool { return old == p.Default })
			history = slices.Insert(history[:min(15, len(history))], 0, p.Default)
			saved = p.Newest
		}
		c.Retired[a.name] = history
		if saved != "" {
			c.CatalogPins[a.name] = saved
		}
	}
	return c, len(c.CatalogPins) != 0
}

// NormalizeDetected validates a CLI result for an alias and normalizes its context suffix.
func NormalizeDetected(alias, value string) (string, bool) {
	if strings.Contains(value, "[") {
		value = strings.NewReplacer("[1M]", "", "[1m]", "").Replace(value)
	}
	id := config.JSTrim(value)
	if len(id) <= len("claude-") || !strings.EqualFold(id[:len("claude-")], "claude-") {
		return "", false
	}
	for i := len("claude-"); i < len(id); i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i == len("claude-") || c != '-' && c != '.' && c != '_' {
			return "", false
		}
	}
	for part := range strings.SplitSeq(id, "-") {
		if strings.EqualFold(part, alias) {
			return id + "[1m]", true
		}
	}
	return "", false
}

// ValidOverride reports whether a string is a usable explicit pin.
func ValidOverride(value string) bool {
	return value != "" && utf8.ValidString(value) && !strings.ContainsFunc(value, func(r rune) bool {
		return r <= 0x1f || strings.ContainsRune("\x7f\"'`$\\;&|<>(){}", r) || config.JSTrim(string(r)) == ""
	})
}

// Entry is one setting in installation order.
type Entry struct{ Key, Value string }

// PinEnv returns the model defaults and custom Fable picker option in settings order.
func (in Inputs) PinEnv() []Entry {
	effective := in.Effective()
	entries := make([]Entry, 0, 6)
	for _, a := range aliases() {
		entries = append(entries, Entry{a.key, effective[a.name].Value})
	}
	fable := effective["fable"].Value
	return append(entries, Entry{optionKey, fable}, Entry{optionName, "Fable"}, Entry{optionDescription, description(fable)})
}

func description(id string) string { return DisplayName(id) + " · current Fable, through the gateway" }

// EnvBlock returns the complete settings block using the injected default gateway URL. The port has no
// remote-control compatibility listener, so every mode, the retired "compat" included, writes that URL.
func (in Inputs) EnvBlock(_, defaultURL string) []Entry {
	return append([]Entry{
		{"ANTHROPIC_BASE_URL", defaultURL},
		{"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "1"},
		{"CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK", "1"},
		{"ENABLE_TOOL_SEARCH", "true"},
		{"CLAUDE_CODE_MAX_OUTPUT_TOKENS", "64000"},
	}, in.PinEnv()...)
}

// Owned lists values the gateway recognizes under each pin setting key.
// The sets are unordered; iterate PinEnv for settings order.
func (in Inputs) Owned() map[string]map[string]bool {
	owned := map[string]map[string]bool{optionName: {"Fable": true}, optionDescription: {}}
	for _, a := range aliases() {
		values := map[string]bool{a.shipped: true}
		for _, p := range a.retired {
			values[p] = true
		}
		for _, p := range in.Cache.Retired[a.name] {
			values[p] = true
		}
		values[in.Cache.Detected[a.name]] = true
		values[in.Cache.CatalogPins[a.name]] = true
		if p := in.Overrides[a.name]; ValidOverride(p) {
			values[p] = true
		}
		delete(values, "")
		owned[a.key] = values
		if a.name == "fable" {
			owned[optionKey] = maps.Clone(values)
			for p := range values {
				owned[optionDescription][description(p)] = true
			}
		}
	}
	return owned
}

// Update describes one stale gateway-written setting.
type Update struct{ Key, From, To string }

// Stale returns changes only for present, owned string values, in pin-setting order.
func (in Inputs) Stale(env map[string]any) []Update {
	owned := in.Owned()
	var updates []Update
	for _, entry := range in.PinEnv() {
		if old, ok := env[entry.Key].(string); ok && old != entry.Value && owned[entry.Key][old] {
			updates = append(updates, Update{entry.Key, old, entry.Value})
		}
	}
	return updates
}
