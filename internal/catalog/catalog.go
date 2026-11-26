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

// Package catalog builds the model catalog consumed by Sidequest.
package catalog

import (
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

const (
	prefix      = "claude-"
	grokPrefix  = "claude-grok-"
	codexPrefix = "claude-codex-"
	dispatchID  = "claude-codex-auto"
	longWindow  = "[1m]"
	maxSlugLen  = 32

	// JavaScript's dot excludes CR and Unicode line and paragraph separators as well as LF.
	codexLabelPattern = `^([0-9]+(?:\.[0-9]+)?)(?:-([^\n\r\x{2028}\x{2029}]+))?$`
)

// Policy describes how the gateway serves and advertises an input model ID.
// A zero window means no window is advertised. An unresolved policy is the zero
// value: an eligible legacy ID keeps its original client spelling.
type Policy struct {
	BackendID, Provider, Transport, ClientID string
	Window                                   int
	WindowNote                               string
}

// Readiness is the public summary; diagnostic checks are not part of the catalog.
type Readiness struct {
	Ready   bool   `json:"ready"`
	State   string `json:"state"`
	Message string `json:"message"`
}

// Checks records the gateway checks that every proxy provider shares with Codex.
type Checks struct {
	ProxyBinary, ProxyModels, ShimRunning, ServingVersionMatches bool
}

// ReadinessSource provides observations not already supplied by the caller.
type ReadinessSource interface {
	// Probe reports provider-specific readiness. Grok does not share the proxy checks.
	Probe(ctx context.Context, provider string) Readiness
	// SharedFailureMessage describes a failed shared check using the caller's installation paths.
	SharedFailureMessage(state string) string
}

// Inputs supplies observed readiness, model policy and writer metadata without ambient reads.
// Providers overrides individual diagnoses; Codex and SharedChecks are the flat readiness input.
type Inputs struct {
	Policy       func(id string) Policy
	Providers    map[string]Readiness
	Codex        *Readiness
	SharedChecks *Checks
	Fallback     ReadinessSource
	UpdatedAt    time.Time
	Version      string
}

// Model is one discoverable routing choice.
type Model struct {
	Slug              string `json:"slug"`
	ID                string `json:"id"`
	Label             string `json:"label"`
	Provider          string `json:"provider"`
	ContextWindow     int    `json:"contextWindow,omitzero"`
	ContextWindowNote string `json:"contextWindowNote,omitzero"`
}

// Catalog is the provider-generic document read by Sidequest.
type Catalog struct {
	SchemaVersion int    `json:"schemaVersion"`
	Source        string `json:"source"`
	UpdatedAt     string `json:"updatedAt"`
	// WrittenBy is the port's non-empty writer version, never JSON null.
	WrittenBy string `json:"writtenBy"`
	// Providers publishes canonical JavaScript array-index keys first in ascending numeric order.
	// Other provider names follow in the order of their first retained row in Models.
	Providers      map[string]Readiness `json:"providers"`
	CodexReadiness *Readiness           `json:"codexReadiness"`
	Models         []Model              `json:"models"`
}

// ParseIDs reads the raw data array from a shim model listing in discovery order.
// It ignores rows with missing, non-string, or ill-formed Unicode IDs.
func ParseIDs(rows jsontext.Value) ([]string, error) {
	var entries []jsontext.Value
	if err := json.Unmarshal(rows, &entries, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		var row struct {
			ID jsontext.Value `json:"id"`
		}
		// Keep the ID raw until strict decoding so it cannot alias a literal U+FFFD.
		if err := json.Unmarshal(entry, &row, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil || row.ID.Kind() != '"' {
			continue
		}
		var id string
		if err := json.Unmarshal(row.ID, &id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Build constructs a catalog in discovery order from supplied policy observations.
// On label validation failure it returns the zero Catalog, which cannot be serialized.
// Use Prepare if the caller needs the validation error or must resolve window notes.
func Build(ctx context.Context, ids []string, inputs Inputs) Catalog {
	models, err := prepareModels(ids, inputs.Policy, nil)
	if err != nil {
		return Catalog{}
	}
	return build(ctx, inputs, models)
}

func prepareModels(ids []string, observe func(string) Policy, note func(id, provider string) (string, error)) ([]Model, error) {
	models := make([]Model, 0, len(ids))
	type pendingNote struct {
		id    string
		index int
	}
	var pendingNotes []pendingNote
	used := make(map[string]struct{}, len(ids))
	pattern := regexp.MustCompile(codexLabelPattern)
	lower := cases.Lower(language.Und)
	for _, id := range ids {
		if !candidate(id) {
			continue
		}
		var p Policy
		if observe != nil {
			p = observe(id)
		}
		provider, base := route(id, p)
		if provider == "" {
			continue
		}
		label, err := modelLabel(pattern, provider, base)
		if err != nil {
			return nil, err
		}
		model := Model{Slug: slugFor(lower, provider, base, used), ID: cmp.Or(p.ClientID, id), Label: label, Provider: provider}
		if p.Window != 0 {
			model.ContextWindow, model.ContextWindowNote = p.Window, p.WindowNote
			if note != nil {
				pendingNotes = append(pendingNotes, pendingNote{id, len(models)})
			}
		}
		models = append(models, model)
	}
	// All labels must be valid before formatting the first window note.
	for _, pending := range pendingNotes {
		m := &models[pending.index]
		text, err := note(pending.id, m.Provider)
		if err != nil {
			return nil, err
		}
		m.ContextWindowNote = text
	}
	return models, nil
}

func build(ctx context.Context, inputs Inputs, models []Model) Catalog {
	catalog := Catalog{
		SchemaVersion: 4,
		Source:        "model-gateway",
		UpdatedAt:     isoTimestamp(inputs.UpdatedAt),
		WrittenBy:     cmp.Or(inputs.Version, "0.0.0-dev"),
		Providers:     make(map[string]Readiness),
		Models:        models,
	}
	for _, model := range models {
		if _, exists := catalog.Providers[model.Provider]; !exists {
			catalog.Providers[model.Provider] = readinessFor(ctx, model.Provider, inputs)
		}
	}
	if codex, ok := catalog.Providers["codex"]; ok {
		catalog.CodexReadiness = new(codex)
	}
	return catalog
}

func candidate(id string) bool {
	return strings.HasPrefix(id, prefix) && utf8.ValidString(id) && strings.TrimSuffix(id, longWindow) != dispatchID
}

func route(id string, p Policy) (provider, base string) {
	if !utf8.ValidString(p.ClientID) {
		return "", ""
	}
	bare := strings.TrimSuffix(id, longWindow)
	switch {
	case strings.HasPrefix(bare, codexPrefix):
		base := strings.TrimPrefix(bare, codexPrefix)
		if strings.HasPrefix(base, "gpt-") {
			return "codex", base
		}
	case p.Transport == "proxy" && (p.Provider != "codex" || p.BackendID != "auto"):
		if utf8.ValidString(p.Provider) && utf8.ValidString(p.BackendID) {
			return p.Provider, p.BackendID
		}
	case strings.HasPrefix(id, grokPrefix):
		return "grok", strings.TrimPrefix(bare, prefix)
	}
	return "", ""
}

// LabelError identifies a Codex model whose label suffix has an empty word.
type LabelError struct {
	Base string
}

// Error describes why the model cannot be labeled.
func (e *LabelError) Error() string {
	return fmt.Sprintf("cannot label Codex model %q: empty suffix word", e.Base)
}

func modelLabel(pattern *regexp.Regexp, provider, base string) (string, error) {
	switch provider {
	case "grok":
		if rest, ok := strings.CutPrefix(base, "grok-"); ok {
			base = "Grok " + rest
		}
		return strings.ReplaceAll(base, "-", " "), nil
	case "codex":
		rest := strings.TrimPrefix(base, "gpt-")
		parts := pattern.FindStringSubmatch(rest)
		if parts == nil {
			return "GPT-" + strings.ReplaceAll(rest, "-", " "), nil
		}
		label := "GPT-" + parts[1]
		if parts[2] == "" {
			return label, nil
		}
		upper := cases.Upper(language.Und)
		var out strings.Builder
		out.WriteString(label)
		for word := range strings.SplitSeq(parts[2], "-") {
			if word == "" {
				return "", &LabelError{Base: base}
			}
			out.WriteByte(' ')
			_, size := utf8.DecodeRuneInString(word)
			// The source uppercases one UTF-16 code unit, not a surrogate pair.
			if size == utf8.UTFMax {
				out.WriteString(word)
			} else {
				out.WriteString(upper.String(word[:size]))
				out.WriteString(word[size:])
			}
		}
		return out.String(), nil
	default:
		return proxymodels.DisplayName(base, provider), nil
	}
}

func slugFor(lower cases.Caser, provider, base string, used map[string]struct{}) string {
	if rest, ok := strings.CutPrefix(base, provider+"-"); ok {
		base = rest
	} else if rest, ok := strings.CutPrefix(base, provider+"/"); ok {
		base = rest
	}
	text := strings.TrimSuffix(lower.String(provider+"-"+base), longWindow)
	var slug strings.Builder
	slug.Grow(len(text))
	separator := false
	for i := range len(text) {
		c := text[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			if separator && slug.Len() != 0 {
				slug.WriteByte('-')
			}
			slug.WriteByte(c)
			separator = false
		} else {
			// A dot, a hyphen and any run of other bytes become one separator.
			separator = true
		}
	}
	s := cmp.Or(slug.String(), "x")
	if len(s) > maxSlugLen {
		sum := sha1.Sum([]byte(s))
		s = fmt.Sprintf("%s-%x", s[:maxSlugLen-7], sum[:3])
	}
	unique := s
	for n := 2; ; n++ {
		if _, exists := used[unique]; !exists {
			used[unique] = struct{}{}
			return unique
		}
		suffix := "-" + strconv.Itoa(n)
		unique = s[:min(len(s), max(1, maxSlugLen-len(suffix)))] + suffix
	}
}

func isoTimestamp(t time.Time) string {
	t = t.UTC()
	if year := t.Year(); year < 0 || year > 9999 {
		return fmt.Sprintf("%+07d%s", year, t.Format("-01-02T15:04:05.000Z"))
	}
	return t.Format("2006-01-02T15:04:05.000Z")
}

func readinessFor(ctx context.Context, provider string, inputs Inputs) Readiness {
	if result, ok := inputs.Providers[provider]; ok {
		return result
	}
	const unavailable = "Readiness is unavailable."
	if provider == "codex" {
		if inputs.Codex != nil {
			return *inputs.Codex
		}
		if inputs.SharedChecks != nil || inputs.Providers != nil {
			return Readiness{State: "unavailable", Message: unavailable}
		}
	} else if provider == "grok" {
		if inputs.Fallback != nil {
			return inputs.Fallback.Probe(ctx, provider)
		}
	} else if checks := inputs.SharedChecks; checks != nil {
		var state string
		switch {
		case !checks.ProxyBinary:
			state = "binary-missing"
		case !checks.ProxyModels:
			state = "proxy-down"
		case !checks.ShimRunning:
			state = "shim-down"
		case !checks.ServingVersionMatches:
			state = "serving-version-mismatch"
		}
		if state != "" {
			message := unavailable
			if inputs.Fallback != nil {
				message = inputs.Fallback.SharedFailureMessage(state)
			}
			return Readiness{State: state, Message: message}
		}
		if inputs.Fallback != nil {
			return inputs.Fallback.Probe(ctx, provider)
		}
	}
	return Readiness{State: "unavailable", Message: provider + " readiness is unavailable."}
}

// MarshalJSON writes the catalog in source order and rejects the zero-schema result of a failed build.
func (c Catalog) MarshalJSON() ([]byte, error) {
	if c.SchemaVersion == 0 {
		return nil, errors.New("cannot serialize a catalog without a schema version")
	}
	member := func(name string, value jsonx.WireValue) jsonx.Member {
		return jsonx.Member{Name: utf16.Encode([]rune(name)), Value: value}
	}
	text := func(value string) jsonx.WireValue {
		return jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune(value))}
	}
	providers := jsonx.WireValue{Kind: '{', Members: make([]jsonx.Member, 0, len(c.Providers))}
	seen := make(map[string]struct{}, len(c.Providers))
	for _, model := range c.Models {
		if _, ok := seen[model.Provider]; ok {
			continue
		}
		if r, ok := c.Providers[model.Provider]; ok {
			seen[model.Provider] = struct{}{}
			ready := jsonx.WireValue{Kind: 'f'}
			if r.Ready {
				ready.Kind = 't'
			}
			providers.Members = append(providers.Members, member(model.Provider, jsonx.WireValue{Kind: '{', Members: []jsonx.Member{
				member("ready", ready), member("state", text(r.State)), member("message", text(r.Message)),
			}}))
		}
	}
	return json.Marshal(struct {
		SchemaVersion  int             `json:"schemaVersion"`
		Source         string          `json:"source"`
		UpdatedAt      string          `json:"updatedAt"`
		WrittenBy      string          `json:"writtenBy"`
		Providers      jsonx.WireValue `json:"providers"`
		CodexReadiness *Readiness      `json:"codexReadiness"`
		Models         []Model         `json:"models"`
	}{c.SchemaVersion, c.Source, c.UpdatedAt, c.WrittenBy, providers, c.CodexReadiness, c.Models})
}
