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

package wiring

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

const (
	baseURLKey    = "ANTHROPIC_BASE_URL"
	compatBaseURL = "http://api.anthropic.com"
	compatHost    = "api.anthropic.com"
	discoveryKey  = "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"
	// socketKey is a retired setting that every explicit write removes (lib/commands.js:1191).
	socketKey = "ANTHROPIC_UNIX_SOCKET"
	// The retired compact window is owned only with this value (lib/runtime.js:551).
	legacyKey   = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	legacyValue = "950000"
)

// staticBlock is STATIC_ENV_BLOCK in source order (lib/runtime.js:522-527).
func staticBlock() [4]Entry {
	return [...]Entry{
		{discoveryKey, "1"},
		{"CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK", "1"},
		{"ENABLE_TOOL_SEARCH", "true"},
		{"CLAUDE_CODE_MAX_OUTPUT_TOKENS", "64000"},
	}
}

const fableAliasKey = "ANTHROPIC_DEFAULT_FABLE_MODEL"

// isAliasKey reports whether key is one of the values of PIN_ALIASES (lib/runtime.js:528-532).
func isAliasKey(key string) bool {
	switch key {
	case "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", fableAliasKey:
		return true
	}
	return false
}

// isFableOptionKey reports whether key is one of the values of FABLE_OPTION_KEYS (lib/pins.js:394-398).
func isFableOptionKey(key string) bool {
	switch key {
	case "ANTHROPIC_CUSTOM_MODEL_OPTION", "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME", "ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION":
		return true
	}
	return false
}

// writableScope is WRITABLE_SCOPE_BY_SOURCE (lib/settings-wiring.js:451).
func writableScope(source string) string {
	if source == "user" {
		return "user"
	}
	return "project"
}

// defaultBaseURL interpolates Number(CODEX_GATEWAY_PORT || 18764) as JavaScript does (lib/runtime.js:24,46).
func (s Store) defaultBaseURL() string {
	return "http://127.0.0.1:" + numberString(config.PublicPort.Get(s.Lookup))
}

func (s Store) oursString(value string) bool {
	return value == s.defaultBaseURL() || value == compatBaseURL
}

// ours is ourBaseUrls().includes(value) for an optional parsed value (lib/pins.js:476).
func (s Store) ours(v jsonx.WireValue, ok bool) bool {
	return ok && v.Kind == '"' && s.oursString(wtf8(v.Text))
}

func (s Store) modeFor(value string) string {
	switch value {
	case compatBaseURL:
		return "compat"
	case s.defaultBaseURL():
		return "default"
	}
	return ""
}

func (s Store) projectDir() (string, error) {
	if !filepath.IsAbs(s.Project) {
		return "", fmt.Errorf("project directory %q is not absolute", s.Project)
	}
	return filepath.Clean(s.Project), nil
}

// settingsPath maps a scope to its file as the source does; any other scope name is the user file
// (lib/settings-wiring.js:30-36).
func (s Store) settingsPath(scope string) (string, error) {
	switch scope {
	case "project", "project-local":
		dir, err := s.projectDir()
		return filepath.Join(dir, ".claude", "settings.local.json"), err
	case "legacy-project", "project-shared":
		dir, err := s.projectDir()
		return filepath.Join(dir, ".claude", "settings.json"), err
	}
	return s.Paths.UserSettingsPath()
}

func (s Store) stateFile(name string) (string, error) {
	dir, err := s.Paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// EffectiveBaseURL reads definitions in process, project-local, project-shared, user order.
func (s Store) EffectiveBaseURL() (Resolution, error) {
	var defs []Definition
	if value := config.AnthropicBaseURL.Get(s.Lookup); value != nil {
		defs = append(defs, Definition{Source: "env", Value: *value})
	}
	for _, source := range [...]string{"project-local", "project-shared", "user"} {
		file, err := s.settingsPath(source)
		if err != nil {
			return Resolution{}, err
		}
		doc, ok := readDocument(file)
		if !ok {
			continue
		}
		// A value with an unpaired surrogate still takes its place in the precedence order; it can never
		// equal a gateway URL.
		if v, ok := doc.envValue(baseURLKey); ok && v.Kind == '"' {
			defs = append(defs, Definition{Source: source, File: file, Value: wtf8(v.Text)})
		}
	}
	if len(defs) == 0 {
		return Resolution{Shadowed: []Definition{}}, nil
	}
	winner := defs[0]
	return Resolution{Winner: &winner, Shadowed: append([]Definition{}, defs[1:]...)}, nil
}

func (r Resolution) clone() *Resolution {
	out := &Resolution{Shadowed: append([]Definition{}, r.Shadowed...)}
	if r.Winner != nil {
		out.Winner = new(*r.Winner)
	}
	return out
}

// UnsafeProcessOverride returns the effective definition only for a process-level HTTPS override of the compatibility host.
func (r Resolution) UnsafeProcessOverride() *Resolution {
	if r.Winner == nil || r.Winner.Source != "env" || !compatHTTPSURL(r.Winner.Value) {
		return nil
	}
	return r.clone()
}

// compatHTTPSURL reports whether WHATWG URL parsing gives protocol https:, hostname api.anthropic.com and the
// default port (lib/settings-wiring.js:61-69). Hosts are compared after percent-decoding and ASCII
// lowercasing; a host with non-ASCII characters is not mapped through IDNA and never matches.
func compatHTTPSURL(raw string) bool {
	s := strings.TrimFunc(raw, func(r rune) bool { return r <= 0x20 })
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok || !strings.EqualFold(scheme, "https") {
		return false
	}
	rest = strings.TrimLeft(rest, `/\`)
	if end := strings.IndexAny(rest, `/\?#`); end >= 0 {
		rest = rest[:end]
	}
	if at := strings.LastIndexByte(rest, '@'); at >= 0 {
		rest = rest[at+1:]
	}
	host := rest
	if colon := strings.LastIndexByte(rest, ':'); colon >= 0 && !strings.Contains(rest[colon:], "]") {
		host = rest[:colon]
		// The default port serializes as no port at all; any other valid port, 0 included, is kept.
		if port := rest[colon+1:]; port != "" {
			if strings.Trim(port, "0123456789") != "" || strings.TrimLeft(port, "0") != "443" {
				return false
			}
		}
	}
	decoded, err := percentDecode(host)
	if err != nil {
		return false
	}
	return strings.ToLower(decoded) == compatHost
}

func percentDecode(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", errors.New("truncated percent escape")
		}
		n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			return "", err
		}
		b.WriteByte(byte(n))
		i += 2
	}
	return b.String(), nil
}

// BypassedWiring returns the highest-precedence gateway file definition hidden by a foreign process URL.
func (s Store) BypassedWiring(r Resolution) *Definition {
	if r.Winner == nil || r.Winner.Source != "env" || s.oursString(r.Winner.Value) {
		return nil
	}
	for _, d := range r.Shadowed {
		if s.oursString(d.Value) {
			return new(d)
		}
	}
	return nil
}

// WiredMode prefers file-backed wiring even when an environment definition shadows the file.
func (s Store) WiredMode() (*Wiring, error) {
	r, err := s.EffectiveBaseURL()
	if err != nil || r.Winner == nil {
		return nil, err
	}
	for _, d := range append([]Definition{*r.Winner}, r.Shadowed...) {
		if mode := s.modeFor(d.Value); mode != "" && d.File != "" {
			return &Wiring{Scope: writableScope(d.Source), Mode: mode, Source: d.Source, File: d.File}, nil
		}
	}
	if r.Winner.Source == "env" {
		if mode := s.modeFor(r.Winner.Value); mode != "" {
			return &Wiring{Mode: mode, Source: "env"}, nil
		}
	}
	return nil, nil
}

// IsWired reports whether the effective base URL selects a gateway endpoint.
func (s Store) IsWired() (bool, error) {
	r, err := s.EffectiveBaseURL()
	return err == nil && r.Winner != nil && s.oursString(r.Winner.Value), err
}

// expectedPins is pinEnvBlock() as an object would hold it: a repeated key keeps its first position and last value.
func (s Store) expectedPins() []Entry {
	out := make([]Entry, 0, len(s.Pins.Current))
	for _, e := range s.Pins.Current {
		if i := slices.IndexFunc(out, func(o Entry) bool { return o.Key == e.Key }); i >= 0 {
			out[i].Value = e.Value
			continue
		}
		out = append(out, e)
	}
	return out
}

// EnvBlock returns the static and pin entries for the selected transport.
func (s Store) EnvBlock(mode string) []Entry {
	url := s.defaultBaseURL()
	if mode == "compat" {
		url = compatBaseURL
	}
	static := staticBlock()
	out := append([]Entry{{baseURLKey, url}}, static[:]...)
	return append(out, s.expectedPins()...)
}

func staticValue(key string) (string, bool) {
	if key == legacyKey {
		return legacyValue, true
	}
	for _, e := range staticBlock() {
		if e.Key == key {
			return e.Value, true
		}
	}
	return "", false
}

// ownedEntries is ownedGatewayEnvEntries: our base URL, a pin value the gateway could have written, or a static
// key holding its static value, all compared through String(value) (lib/settings-wiring.js:116-124).
func (s Store) ownedEntries(env jsonx.WireValue) []jsonx.Member {
	if env.Kind != '{' {
		return nil
	}
	var owned []jsonx.Member
	for _, m := range env.Members {
		key := wtf8(m.Name)
		value := jsString(m.Value)
		static, isStatic := staticValue(key)
		if (key == baseURLKey && s.ours(m.Value, true)) || slices.Contains(s.Pins.Owned[key], value) || (isStatic && value == static) {
			owned = append(owned, m)
		}
	}
	return owned
}

// removeOwned is removeGatewayOwnedSettings, also removing the extra keys named: it deletes the owned entries and
// an emptied env, and returns the pin keys that still hold a user's value (lib/settings-wiring.js:126-131).
func (s Store) removeOwned(doc *document, extra ...string) ([]string, error) {
	var before []string
	for _, e := range s.expectedPins() {
		if _, ok := doc.envValue(e.Key); ok {
			before = append(before, e.Key)
		}
	}
	env, has := doc.env()
	if !has {
		return nil, nil
	}
	if env.Kind != '{' {
		if truthy(env) && keyCount(env) == 0 {
			return nil, doc.edit(remove("env"))
		}
		return nil, nil
	}
	drop := map[string]bool{}
	for _, m := range s.ownedEntries(env) {
		drop[wtf8(m.Name)] = true
	}
	for _, key := range extra {
		drop[key] = true
	}
	var edits []jsonx.Edit
	for _, m := range env.Members {
		if name := wtf8(m.Name); drop[name] {
			edits = append(edits, remove("env", name))
		}
	}
	if len(edits) == len(env.Members) && len(edits) > 0 {
		edits = []jsonx.Edit{remove("env")}
	}
	if err := doc.edit(edits...); err != nil {
		return nil, err
	}
	var preserved []string
	for _, key := range before {
		if _, ok := doc.envValue(key); ok {
			preserved = append(preserved, key)
		}
	}
	return preserved, nil
}

// assignEnv is Object.assign(settings.env, block) followed by removal of the retired keys (lib/commands.js:1190-1198).
func assignEnv(doc *document, file string, block []Entry) error {
	env, has := doc.env()
	if !has || !truthy(env) {
		members := make([]jsonx.Member, len(block))
		for i, e := range block {
			members[i] = field(e.Key, text(e.Value))
		}
		e, err := set(object(members...), "env")
		if err != nil {
			return err
		}
		return doc.edit(e)
	}
	if env.Kind != '{' {
		return fmt.Errorf("settings env in %s is not an object", file)
	}
	var edits []jsonx.Edit
	if _, ok := member(env, socketKey); ok {
		edits = append(edits, remove("env", socketKey))
	}
	for _, e := range block {
		// Leave an equal value as written so its authored spelling survives.
		if current, ok := member(env, e.Key); ok && isString(current, e.Value) {
			continue
		}
		edit, err := set(text(e.Value), "env", e.Key)
		if err != nil {
			return err
		}
		edits = append(edits, edit)
	}
	if current, ok := member(env, legacyKey); ok && jsString(current) == legacyValue {
		edits = append(edits, remove("env", legacyKey))
	}
	return doc.edit(edits...)
}

// WriteEnv writes or removes gateway-owned settings in the specified scope and verifies the result.
func (s Store) WriteEnv(scope, mode string, remove bool) error {
	_, err := s.writeEnv(scope, mode, remove)
	return err
}

func (s Store) recordIfProject(scope string) error {
	if scope == "project" {
		return s.RecordProject()
	}
	return nil
}

// writeEnv follows lib/commands.js:1177-1218 and returns the verified mode. Publishing the stable launcher, which
// the source does here, belongs to installation.
func (s Store) writeEnv(scope, mode string, removeAll bool) (string, error) {
	effective := mode
	if !removeAll {
		// A committed project settings.json with gateway keys outranks the user file, so migrate it even for a
		// user-scope write.
		m, err := s.migrateLegacy()
		if err != nil {
			return "", err
		}
		effective = cmp.Or(m.mode, mode)
	}
	file, err := s.settingsPath(scope)
	if err != nil {
		return "", err
	}
	if removeAll && !exists(file) {
		return "", s.recordIfProject(scope)
	}
	current, err := readSettingsForWrite(file)
	if err != nil {
		return "", err
	}
	forms, err := s.hookForms()
	if err != nil {
		return "", err
	}
	block := s.EnvBlock(effective)
	var final document
	change := func(doc *document) error {
		edits, err := forms.sync(doc.root, !removeAll)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if err := doc.edit(edits...); err != nil {
			return err
		}
		if !removeAll {
			err = assignEnv(doc, file, block)
		} else if env, has := doc.env(); has && !truthy(env) {
			// settings.env || {} turns a falsy env into an empty object, which removal then deletes.
			err = doc.edit(remove("env"))
		} else {
			_, err = s.removeOwned(doc, socketKey)
		}
		final = *doc
		return err
	}
	if removeAll {
		probe := current
		if err := change(&probe); err != nil {
			return "", err
		}
		if bytes.Equal(probe.data, current.data) {
			return "", s.recordIfProject(scope)
		}
	} else if cache, err := s.Paths.DiscoveryCachePath(); err == nil {
		// Advisory, as in the source: a cache that cannot be cleaned never fails the write.
		_, _ = s.CleanModelCache(cache, nil)
	}
	if _, err := s.writeSettings(file, change); err != nil {
		return "", err
	}
	verified, err := readSettingsForWrite(file)
	if err != nil {
		return "", err
	}
	got, gotOK := verified.envValue(baseURLKey)
	want, wantOK := text(block[0].Value), true
	if removeAll {
		want, wantOK = final.envValue(baseURLKey)
	}
	if !strictEqual(got, gotOK, want, wantOK) {
		return "", fmt.Errorf("Could not verify gateway settings in %s", file) //nolint:staticcheck // The message is the source's user-facing wording.
	}
	verifiedMode := "default"
	if gotOK && isString(got, compatBaseURL) {
		verifiedMode = "compat"
	}
	return verifiedMode, s.recordIfProject(scope)
}

// SyncMode migrates eligible settings and verifies a mode change against the supplied health observations.
func (s Store) SyncMode(health *Health, required string) (ModeResult, error) {
	var compat Health
	desired := "default"
	if health != nil {
		compat = *health
		if compat.HostsDetected && compat.Port80Bound {
			desired = "compat"
		}
	}
	if required != "" && desired != required {
		return ModeResult{Mode: desired, Compat: compat}, nil
	}
	if _, err := s.migrateLegacy(); err != nil {
		return ModeResult{}, err
	}
	current, err := s.WiredMode()
	if err != nil {
		return ModeResult{}, err
	}
	// An unwired project is written in the default scope; a definition only the environment holds has no
	// writable scope (lib/commands.js:1226-1235).
	scope, observed := "project", "default"
	if current != nil {
		scope, observed = current.Scope, current.Mode
	}
	if scope == "" || desired == observed {
		return ModeResult{Mode: observed, Compat: compat}, nil
	}
	mode, err := s.writeEnv(scope, desired, false)
	if err != nil {
		return ModeResult{}, err
	}
	return ModeResult{Mode: mode, Compat: compat, Changed: true}, nil
}

type migration struct {
	migrated bool
	mode     string
}

// MigrateLegacy moves only gateway-owned entries from shared project settings to local settings.
func (s Store) MigrateLegacy() (bool, error) {
	m, err := s.migrateLegacy()
	return m.migrated, err
}

// migrateLegacy follows lib/settings-wiring.js:361-402. It refuses when the shared project file is, lexically or
// after resolving links, the user file, or when that identity cannot be decided.
func (s Store) migrateLegacy() (migration, error) {
	legacyFile, err := s.settingsPath("legacy-project")
	if err != nil {
		return migration{}, err
	}
	userFile, err := s.settingsPath("user")
	if err != nil {
		return migration{}, err
	}
	if !exists(legacyFile) || filepath.Clean(legacyFile) == filepath.Clean(userFile) {
		return migration{}, nil
	}
	resolved, err := filepath.EvalSymlinks(legacyFile)
	if err != nil {
		return migration{}, nil
	}
	real, err := filepath.EvalSymlinks(userFile)
	switch {
	case err == nil && real == resolved:
		return migration{}, nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		// A user file whose identity cannot be decided, such as a link loop, may be the same file.
		return migration{}, nil
	}
	legacy, err := readSettingsForWrite(legacyFile)
	if err != nil {
		return migration{}, err
	}
	if !s.ours(legacy.envValue(baseURLKey)) {
		return migration{}, nil
	}
	env, _ := legacy.env()
	entries := s.ownedEntries(env)
	var drop []string
	for _, m := range entries {
		drop = append(drop, wtf8(m.Name))
	}
	if v, ok := member(env, legacyKey); ok && jsString(v) == legacyValue && !slices.Contains(drop, legacyKey) {
		drop = append(drop, legacyKey)
	}
	if len(drop) == 0 {
		return migration{}, nil
	}
	if len(entries) > 0 {
		localFile, err := s.settingsPath("project")
		if err != nil {
			return migration{}, err
		}
		if _, err := s.writeSettings(localFile, func(doc *document) error { return copyMissing(doc, localFile, entries) }); err != nil {
			return migration{}, err
		}
	}
	if _, err := s.writeSettings(legacyFile, func(doc *document) error { return removeEnvKeys(doc, drop) }); err != nil {
		return migration{}, err
	}
	m := migration{migrated: true}
	for _, e := range entries {
		if wtf8(e.Name) == baseURLKey {
			m.mode = s.modeFor(wtf8(e.Value.Text))
		}
	}
	return m, nil
}

// copyMissing sets each entry the local env does not already define.
func copyMissing(doc *document, file string, entries []jsonx.Member) error {
	env, has := doc.env()
	if !has || !truthy(env) {
		e, err := set(object(entries...), "env")
		if err != nil {
			return err
		}
		return doc.edit(e)
	}
	if env.Kind != '{' {
		return fmt.Errorf("settings env in %s is not an object", file)
	}
	var edits []jsonx.Edit
	for _, m := range entries {
		name := wtf8(m.Name)
		if _, ok := member(env, name); ok {
			continue
		}
		e, err := set(m.Value, "env", name)
		if err != nil {
			return err
		}
		edits = append(edits, e)
	}
	return doc.edit(edits...)
}

// removeEnvKeys deletes the named env members and an env left empty.
func removeEnvKeys(doc *document, keys []string) error {
	env, _ := doc.env()
	if env.Kind != '{' {
		return nil
	}
	var edits []jsonx.Edit
	for _, m := range env.Members {
		if name := wtf8(m.Name); slices.Contains(keys, name) {
			edits = append(edits, remove("env", name))
		}
	}
	if len(edits) > 0 && len(edits) == len(env.Members) {
		edits = []jsonx.Edit{remove("env")}
	}
	return doc.edit(edits...)
}

// CleanLegacyEnv migrates legacy wiring and removes the stale compact-window key from gateway settings.
func (s Store) CleanLegacyEnv() error {
	if _, err := s.migrateLegacy(); err != nil {
		return err
	}
	for _, scope := range [...]string{"user", "project"} {
		file, err := s.settingsPath(scope)
		if err != nil {
			return err
		}
		doc, ok := readDocument(file)
		if !ok {
			continue
		}
		env, has := doc.env()
		if !has || !truthy(env) || !s.ours(doc.envValue(baseURLKey)) {
			continue
		}
		if v, ok := member(env, legacyKey); !ok || jsString(v) != legacyValue {
			continue
		}
		if _, err := s.writeSettings(file, func(doc *document) error {
			env, _ := doc.env()
			if v, ok := member(env, legacyKey); !ok || jsString(v) != legacyValue {
				return nil
			}
			return removeEnvKeys(doc, []string{legacyKey})
		}); err != nil {
			return err
		}
	}
	return nil
}

// RetireModeConfig removes the obsolete scope selection file, returning whether it was removed.
func (s Store) RetireModeConfig() bool {
	file, err := s.stateFile("wiring.json")
	// fs.rmSync without recursion removes a file or link but never a directory (lib/settings-wiring.js:23-28).
	return err == nil && syscall.Unlink(file) == nil
}

// SyncReadinessHooks edits only the owned hook entries in the supplied settings document.
func (s Store) SyncReadinessHooks(document []byte, enabled bool) ([]byte, error) {
	forms, err := s.hookForms()
	if err != nil {
		return nil, err
	}
	doc, err := parseDocument(document)
	if err != nil {
		return nil, err
	}
	edits, err := forms.sync(doc.root, enabled)
	if err != nil {
		return nil, err
	}
	if err := doc.edit(edits...); err != nil {
		return nil, err
	}
	return doc.data, nil
}

// ReadinessHookPresent reports whether both prompt and startup have an owned readiness hook.
func (s Store) ReadinessHookPresent(document []byte) bool {
	forms, err := s.hookForms()
	if err != nil {
		return false
	}
	doc, err := parseDocument(document)
	return err == nil && forms.present(doc.root)
}

// MigrateReadinessHooks updates registered settings and existing wired user settings.
func (s Store) MigrateReadinessHooks() error {
	wirings, _, err := s.report(false)
	if err != nil {
		return err
	}
	user, err := s.settingsPath("user")
	if err != nil {
		return err
	}
	forms, err := s.hookForms()
	if err != nil {
		return err
	}
	files := make([]string, 0, len(wirings)+1)
	for _, w := range wirings {
		files = append(files, w.File)
	}
	for _, file := range append(files, user) {
		doc, err := readSettingsForWrite(file)
		if err != nil {
			return err
		}
		if !s.ours(doc.envValue(baseURLKey)) {
			continue
		}
		edits, err := forms.sync(doc.root, true)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if len(edits) == 0 {
			continue
		}
		if _, err := s.writeSettings(file, nil); err != nil {
			return err
		}
	}
	return nil
}

type issue struct{ file, kind, message string }

// Repair removes orphaned gateway entries and records wired projects missing from the registry.
func (s Store) Repair(report func(string)) error {
	if report == nil {
		report = func(string) {}
	}
	local, err := s.settingsPath("project")
	if err != nil {
		return err
	}
	var issues []issue
	for _, scope := range [...]string{"project", "project-shared", "user"} {
		file, err := s.settingsPath(scope)
		if err != nil {
			return err
		}
		if !exists(file) {
			continue
		}
		doc, err := readSettingsForWrite(file)
		if err != nil {
			return err
		}
		env, _ := doc.env()
		if v, ok := member(env, baseURLKey); ok {
			if file != local || !s.ours(v, true) {
				continue
			}
			registered, err := s.registered()
			if err != nil {
				return err
			}
			if !registered {
				issues = append(issues, issue{file: file, kind: "unregistered"})
			}
			continue
		}
		if message, ok := orphanMessage(s.ownedEntries(env)); ok {
			issues = append(issues, issue{file: file, kind: "orphan", message: message})
		}
	}
	for _, is := range issues {
		if is.kind == "unregistered" {
			if err := s.RecordProject(); err != nil {
				return err
			}
			report("model-gateway: registered wired project " + s.Project + " for pin sync")
			continue
		}
		var preserved []string
		if _, err := s.writeSettings(is.file, func(doc *document) (err error) {
			preserved, err = s.removeOwned(doc)
			return err
		}); err != nil {
			return err
		}
		report("model-gateway: repaired " + is.message + " in " + is.file + "; preserved user pins: " + cmp.Or(strings.Join(preserved, ", "), "none"))
	}
	return nil
}

// orphanMessage describes leftover entries that still name the discovery switch or an alias pin
// (lib/settings-wiring.js:146-150).
func orphanMessage(entries []jsonx.Member) (string, bool) {
	var aliases []string
	found := false
	for _, m := range entries {
		key := wtf8(m.Name)
		isAlias := isAliasKey(key)
		found = found || isAlias || key == discoveryKey
		if isAlias {
			aliases = append(aliases, jsString(m.Value))
		}
	}
	return "leftover gateway settings: aliases pinned to " + cmp.Or(strings.Join(aliases, ", "), "none") + ", no gateway routing", found
}

// SyncWindow updates only a missing or previously owned cap. Nil next selects the full window.
func (s Store) SyncWindow(files []string, owned, next *int) (Changes, error) {
	var r Changes
	for _, file := range files {
		doc, err := readSettingsForWrite(file)
		if err != nil {
			r.Skipped = append(r.Skipped, Skipped{File: file, Reason: err.Error()})
			continue
		}
		current, has := member(doc.root, "autoCompactWindow")
		if has && !windowMatches(current, owned) && !windowMatches(current, next) {
			r.Skipped = append(r.Skipped, Skipped{File: file, Reason: "autoCompactWindow " + jsString(current) + " is not gateway-owned"})
			continue
		}
		if !windowChanges(current, has, next) {
			continue
		}
		changed, err := s.writeSettings(file, func(doc *document) error {
			current, has := member(doc.root, "autoCompactWindow")
			if !windowChanges(current, has, next) {
				return nil
			}
			if next == nil {
				return doc.edit(remove("autoCompactWindow"))
			}
			e, err := set(jsonx.WireValue{Kind: '0', Number: float64(*next)}, "autoCompactWindow")
			if err != nil {
				return err
			}
			return doc.edit(e)
		})
		if err != nil {
			return r, err
		}
		if changed {
			r.Changed = append(r.Changed, file)
		}
	}
	return r, nil
}

// windowMatches is current === value. A nil window, owned or next, is the string "full" the source passes for the
// full window: owned is the previously saved Claude value, a token count or "full" (lib/context-window.js:99-101;
// lib/commands.js:978-998).
func windowMatches(current jsonx.WireValue, value *int) bool {
	if value == nil {
		return isString(current, "full")
	}
	return current.Kind == '0' && current.Number == float64(*value)
}

// windowChanges reports whether applying next changes the member (lib/context-window.js:103-108).
func windowChanges(current jsonx.WireValue, has bool, next *int) bool {
	if next == nil {
		return has
	}
	return !has || current.Kind != '0' || current.Number != float64(*next)
}

// CleanModelCache normalizes owned model IDs in valid gateway caches. A nil canonical function
// selects the production policy adapter configured by Lookup and Paths.
func (s Store) CleanModelCache(file string, canonical func(string) (string, bool)) (bool, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return false, nil
	}
	cache, err := jsonx.DecodeWire(data)
	if err != nil {
		return false, nil
	}
	baseURL, ok := member(cache, "baseUrl")
	if !s.ours(baseURL, ok) {
		return false, nil
	}
	models, ok := member(cache, "models")
	if !ok || models.Kind != '[' {
		return false, nil
	}
	if canonical == nil {
		if canonical, err = s.productionCanonical(); err != nil {
			return false, err
		}
	}
	changed := false
	for i, model := range models.Elements {
		for j, m := range model.Members {
			if model.Kind != '{' || wtf8(m.Name) != "id" || m.Value.Kind != '"' {
				continue
			}
			id, ok := wellFormed(m.Value.Text)
			if !ok {
				continue
			}
			next, gateway := canonical(id)
			if !gateway || next == id {
				continue
			}
			models.Elements[i].Members[j].Value = text(next)
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	for i, m := range cache.Members {
		if wtf8(m.Name) == "models" {
			cache.Members[i].Value = models
		}
	}
	out, err := jsonx.EncodeState(cache)
	if err != nil {
		return false, err
	}
	if err := atomicfile.WriteFile(file, out, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// productionCanonical resolves model spellings with the saved and configured context windows and no listed
// proxy routes, which is what a command process holds (lib/runtime.js:506-512; lib/pins.js:16-28,54-56).
func (s Store) productionCanonical() (func(string) (string, bool), error) {
	file, err := s.stateFile("context-window.json")
	if err != nil {
		return nil, err
	}
	windows, err := policy.ReadWindows(file, s.Lookup)
	if err != nil {
		return nil, err
	}
	r := policy.NewResolver(policy.ResolverInputs{Windows: windows})
	return func(id string) (string, bool) { return r.ClientID(id), gatewayModelID(r, id) }, nil
}

// gatewayModelID is isGatewayModelId: a proxy route or the Grok prefix (lib/pins.js:16-28,54-56).
func gatewayModelID(r policy.Resolver, id string) bool {
	bare := strings.TrimSuffix(id, "[1m]")
	if bare == "claude-codex-auto" {
		return true
	}
	if rest, ok := strings.CutPrefix(bare, "claude-codex-"); ok {
		return strings.HasPrefix(rest, "gpt-")
	}
	if strings.HasPrefix(bare, "claude-") {
		if m := r.Resolve(bare); m != nil && m.Transport == "proxy" {
			return true
		}
	}
	return strings.HasPrefix(id, "claude-grok-")
}
