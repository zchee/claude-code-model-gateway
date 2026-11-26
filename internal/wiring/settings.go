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
	// socketKey is a retired setting that explicit writes remove by key (lib/commands.js:1191) and
	// explicit removals delete only when value-matched to a gateway-owned socket path.
	socketKey         = "ANTHROPIC_UNIX_SOCKET"
	windowsSocketPipe = `\\.\pipe\model-gateway`
	// The retired compact window is owned only with this value (lib/runtime.js:551).
	legacyKey   = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	legacyValue = "950000"
	// The retired global context cap is removed only during explicit writes when value-matched
	// (lib/commands.js:148-151,1196-1199).
	legacyMaxContextKey   = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	legacyMaxContextValue = "920000"
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
		doc, ok, err := readDocument(file)
		if err != nil {
			return Resolution{}, err
		}
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

// asciiEqualFold compares two strings with ASCII-only case folding so non-ASCII Unicode characters
// (such as Kelvin sign U+212A, dotted capital I U+0130, dotless i U+0131 or long s U+017F) never fold to ASCII.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// compatHTTPSURL reports whether WHATWG URL parsing gives protocol https:, hostname api.anthropic.com and the
// default port (lib/settings-wiring.js:61-69). Scheme and host are compared with ASCII-only case folding after
// percent-decoding the host; non-ASCII characters are not mapped through IDNA or Unicode folding and never match.
func compatHTTPSURL(raw string) bool {
	s := strings.TrimFunc(raw, func(r rune) bool { return r <= 0x20 })
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok || !asciiEqualFold(scheme, "https") {
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
	return asciiEqualFold(decoded, compatHost)
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
// Every mode selects the gateway's own URL: the compatibility URL is recognized as owned but never written,
// because the port has no listener that could serve it.
func (s Store) EnvBlock(string) []Entry {
	static := staticBlock()
	out := append([]Entry{{baseURLKey, s.defaultBaseURL()}}, static[:]...)
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

func (s Store) isOwnedEnvMember(key string, v jsonx.WireValue) bool {
	if key == baseURLKey && s.ours(v, true) {
		return true
	}
	value := jsString(v)
	if slices.Contains(s.Pins.Owned[key], value) {
		return true
	}
	static, isStatic := staticValue(key)
	return isStatic && value == static
}

// ownedEntries is ownedGatewayEnvEntries: our base URL, a pin value the gateway could have written, or a static
// key holding its static value, all compared through String(value) (lib/settings-wiring.js:116-124).
func (s Store) ownedEntries(env jsonx.WireValue) []jsonx.Member {
	if env.Kind != '{' {
		return nil
	}
	var owned []jsonx.Member
	for _, m := range env.Members {
		if s.isOwnedEnvMember(wtf8(m.Name), m.Value) {
			owned = append(owned, m)
		}
	}
	return owned
}

// removeOwned is removeGatewayOwnedSettings, also removing the extra keys named: it deletes the owned entries and
// an emptied env (and a present empty env when deleteEmptyEnv is true), and returns the pin keys that still hold
// a user's value (lib/settings-wiring.js:126-131). File-wide raw duplicate validation has already run in replaceInfo.
func (s Store) removeOwned(doc *document, deleteEmptyEnv bool, extra ...string) ([]string, error) {
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
	if len(edits) == len(env.Members) && (len(edits) > 0 || deleteEmptyEnv) {
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

// assignEnv is Object.assign(settings.env, block) followed by removal of the retired keys (lib/commands.js:1190-1199).
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
	if current, ok := member(env, legacyMaxContextKey); ok && jsString(current) == legacyMaxContextValue {
		edits = append(edits, remove("env", legacyMaxContextKey))
	}
	return doc.edit(edits...)
}

// ownedSocketValues returns every ANTHROPIC_UNIX_SOCKET value the gateway itself could have written:
// the store's resolved socket path, the source's default gateway.sock under the gateway home and
// state directory (lib/runtime.js:16,47-49), any CODEX_GATEWAY_SOCKET_PATH override, and the Windows
// named pipe.
func (s Store) ownedSocketValues() []string {
	vals := []string{windowsSocketPipe}
	add := func(v string) {
		if v != "" && !slices.Contains(vals, v) {
			vals = append(vals, v)
		}
	}
	if v, err := s.Paths.SocketPath(); err == nil {
		add(v)
	}
	if home, err := s.Paths.GatewayHome(); err == nil {
		add(filepath.Join(home, "gateway.sock"))
	}
	if dir, err := s.Paths.StateDir(); err == nil {
		add(filepath.Join(dir, "gateway.sock"))
	}
	if v := config.SocketPath.Get(s.Lookup); v != "" {
		add(v)
	}
	return vals
}

func (s Store) isOwnedSocket(v jsonx.WireValue, ok bool) bool {
	return ok && v.Kind == '"' && slices.Contains(s.ownedSocketValues(), wtf8(v.Text))
}

// WriteEnv writes or removes gateway-owned settings in the specified scope and verifies the result.
// An explicit write replaces the retired compatibility URL with the gateway URL and deletes
// ANTHROPIC_UNIX_SOCKET by key (lib/commands.js:1191); requesting the "compat" mode for a write is
// an error, because the port cannot serve that URL.
//
// Explicit removal deletes ANTHROPIC_UNIX_SOCKET only when its value matches a gateway-owned socket
// path (preserving any other value silently). For the project scope, explicit removal clears
// value-matched gateway-owned entries from both project settings files (local first, then shared),
// comparing the opened identity of each project file against the user settings file before any edit
// so user settings are never modified: a direct target aliasing the user file is left untouched and
// returns an error naming the file, while a secondary shared file aliasing the user file during
// "project" removal is left untouched and returns nil. A project file that is a symbolic link to
// some other regular file is edited through the link, matching the source.
func (s Store) WriteEnv(scope, mode string, remove bool) error {
	if !remove && mode == "compat" {
		return errors.New("the remote-control compatibility mode is not supported; wire the default gateway URL")
	}
	return s.writeEnv(scope, remove)
}

func isProjectScope(scope string) bool {
	switch scope {
	case "project", "project-local", "project-shared", "legacy-project":
		return true
	}
	return false
}

func (s Store) recordIfProject(scope string) error {
	if scope == "project" {
		return s.RecordProject()
	}
	return nil
}

// writeEnv follows lib/commands.js:1177-1218 for the default mode only. Publishing the stable launcher, which the
// source does here, belongs to installation.
func (s Store) writeEnv(scope string, removeAll bool) error {
	if removeAll {
		return s.removeEnv(scope)
	}
	// A committed project settings.json with gateway keys outranks the user file, so migrate it even for a
	// user-scope write. This write is explicit, so a migrated compatibility URL becomes the gateway URL.
	if _, err := s.migrateLegacy(true); err != nil {
		return err
	}
	file, err := s.settingsPath(scope)
	if err != nil {
		return err
	}
	if _, err := readSettingsForWrite(file); err != nil {
		return err
	}
	forms, err := s.hookForms()
	if err != nil {
		return err
	}
	block := s.EnvBlock("default")
	if cache, err := s.Paths.DiscoveryCachePath(); err == nil {
		// Advisory, as in the source: a cache that cannot be cleaned never fails the write.
		_, _ = s.CleanModelCache(cache, nil)
	}
	change := func(doc *document) error {
		edits, err := forms.sync(doc.root, true)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if err := doc.edit(edits...); err != nil {
			return err
		}
		return assignEnv(doc, file, block)
	}
	if _, err := s.writeSettings(file, change); err != nil {
		return err
	}
	verified, err := readSettingsForWrite(file)
	if err != nil {
		return err
	}
	got, gotOK := verified.envValue(baseURLKey)
	if !strictEqual(got, gotOK, text(block[0].Value), true) {
		return fmt.Errorf("Could not verify gateway settings in %s", file) //nolint:staticcheck // The message is the source's user-facing wording.
	}
	return s.recordIfProject(scope)
}

func (s Store) removeEnv(scope string) error {
	file, err := s.settingsPath(scope)
	if err != nil {
		return err
	}
	forms, err := s.hookForms()
	if err != nil {
		return err
	}
	if err := s.removeFromTargetFile(scope, file, forms); err != nil {
		return err
	}
	if scope == "project" {
		if err := s.removeFromSharedProjectFile(forms); err != nil {
			return err
		}
	}
	return s.recordIfProject(scope)
}

// removeFromTargetFile removes gateway-owned entries, a value-matched gateway-owned socket entry, readiness hooks
// and any present empty env object from the explicitly targeted settings file. For project scopes it verifies on
// the opened file identity that the target does not alias user settings before any edit and on every retry. A
// missing file is never created.
func (s Store) removeFromTargetFile(scope, file string, forms hookForms) error {
	var userFile string
	checkUser := isProjectScope(scope)
	if checkUser {
		var err error
		userFile, err = s.settingsPath("user")
		if err != nil {
			return err
		}
		if filepath.Clean(file) == filepath.Clean(userFile) {
			return fmt.Errorf("%s: refuses to edit project settings aliasing user settings", file)
		}
	}
	_, targetInfo, err := readDocumentSnapshot(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if checkUser && userAliasState(targetInfo, userFile) {
		return fmt.Errorf("%s: refuses to edit project settings aliasing user settings", file)
	}
	var final document
	changed, err := s.writeSettingsInfo(file, func(doc *document, openInfo fs.FileInfo) error {
		if openInfo == nil {
			return errUnchanged
		}
		if checkUser && userAliasState(openInfo, userFile) {
			return fmt.Errorf("%s: refuses to edit project settings aliasing user settings", file)
		}
		edits, err := forms.sync(doc.root, false)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if err := doc.edit(edits...); err != nil {
			return err
		}
		if env, has := doc.env(); has && !truthy(env) {
			// settings.env || {} turns a falsy env into an empty object, which removal then deletes.
			err = doc.edit(remove("env"))
		} else {
			var extra []string
			if s.isOwnedSocket(doc.envValue(socketKey)) {
				extra = []string{socketKey}
			}
			_, err = s.removeOwned(doc, true, extra...)
		}
		final = *doc
		return err
	})
	if err != nil || !changed {
		return err
	}
	verified, err := readSettingsForWrite(file)
	if err != nil {
		return err
	}
	got, gotOK := verified.envValue(baseURLKey)
	want, wantOK := final.envValue(baseURLKey)
	if !strictEqual(got, gotOK, want, wantOK) {
		return fmt.Errorf("Could not verify gateway settings in %s", file) //nolint:staticcheck // The message is the source's user-facing wording.
	}
	return nil
}

// removeFromSharedProjectFile clears value-matched gateway-owned entries and owned readiness hooks from the
// project's shared .claude/settings.json during explicit project removal, deciding on the current state of the
// shared path (so hard-linked local/shared files are both cleaned) while leaving user settings and unrelated
// shared files untouched.
func (s Store) removeFromSharedProjectFile(forms hookForms) error {
	sharedFile, err := s.settingsPath("project-shared")
	if err != nil {
		return err
	}
	userFile, err := s.settingsPath("user")
	if err != nil {
		return err
	}
	if filepath.Clean(sharedFile) == filepath.Clean(userFile) {
		return nil
	}
	_, sharedInfo, err := readDocumentSnapshot(sharedFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if sharedInfo != nil && userAliasState(sharedInfo, userFile) {
		return nil
	}
	if err != nil {
		return err
	}
	var final document
	changed, err := s.writeSettingsInfo(sharedFile, func(doc *document, openInfo fs.FileInfo) error {
		if openInfo == nil {
			return errUnchanged
		}
		if userAliasState(openInfo, userFile) {
			return errUnchanged
		}
		env, _ := doc.env()
		hasOwnedEnv := len(s.ownedEntries(env)) > 0
		hasOwnedSocket := s.isOwnedSocket(doc.envValue(socketKey))
		hasOwnedHooks := forms.hasAnyOwned(doc.root)
		if !hasOwnedEnv && !hasOwnedSocket && !hasOwnedHooks {
			return errUnchanged
		}
		if hasOwnedHooks {
			edits, err := forms.sync(doc.root, false)
			if err != nil {
				return fmt.Errorf("%s: %w", sharedFile, err)
			}
			if err := doc.edit(edits...); err != nil {
				return err
			}
		}
		if hasOwnedEnv || hasOwnedSocket {
			var extra []string
			if hasOwnedSocket {
				extra = []string{socketKey}
			}
			if _, err := s.removeOwned(doc, false, extra...); err != nil {
				return err
			}
		}
		final = *doc
		return nil
	})
	if err != nil || !changed {
		return err
	}
	verified, err := readSettingsForWrite(sharedFile)
	if err != nil {
		return err
	}
	got, gotOK := verified.envValue(baseURLKey)
	want, wantOK := final.envValue(baseURLKey)
	if !strictEqual(got, gotOK, want, wantOK) {
		return fmt.Errorf("Could not verify gateway settings in %s", sharedFile) //nolint:staticcheck // The message is the source's user-facing wording.
	}
	return nil
}

func sameFile(a, b fs.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b)
}

// userAliasState reports whether targetInfo refers to the same opened file as userFile, or whether
// userFile's identity cannot be established (such as a symlink loop or non-regular file). It checks
// file identity without reading userFile's contents, so an unrelated oversized user file never blocks
// project removal.
func userAliasState(targetInfo fs.FileInfo, userFile string) bool {
	userInfo, err := statRegularFile(userFile)
	switch {
	case err == nil:
		return sameFile(targetInfo, userInfo)
	case errors.Is(err, fs.ErrNotExist):
		return false
	default:
		return true
	}
}

// SyncMode migrates eligible settings and verifies a mode change against the supplied health observations.
// The port has no remote-control compatibility listener, so it never switches a project between the gateway URL
// and the compatibility URL (the source's lib/commands.js:1226-1253): it runs the automatic legacy migration and
// reports the observed mode unchanged. Requiring the "compat" mode is an error.
func (s Store) SyncMode(health *Health, required string) (ModeResult, error) {
	var compat Health
	if health != nil {
		compat = *health
	}
	if required == "compat" {
		return ModeResult{}, errors.New("the remote-control compatibility mode is not supported")
	}
	if _, err := s.migrateLegacy(false); err != nil {
		return ModeResult{}, err
	}
	current, err := s.WiredMode()
	if err != nil {
		return ModeResult{}, err
	}
	observed := "default"
	if current != nil {
		observed = current.Mode
	}
	return ModeResult{Mode: observed, Compat: compat}, nil
}

// MigrateLegacy moves only gateway-owned entries from shared project settings to local settings.
// It is the automatic migration: shared settings wired to the compatibility URL are left untouched,
// and no edit is made if moving entries would change the effective base URL or if source and destination alias.
// Migration executes as two separate single-file atomic replacements (copy to local, then delete from shared),
// not a two-file transaction.
func (s Store) MigrateLegacy() (bool, error) {
	return s.migrateLegacy(false)
}

type baseURLState struct {
	present bool
	value   string
}

func docBaseURL(doc document, ok bool) baseURLState {
	if !ok {
		return baseURLState{}
	}
	v, has := doc.envValue(baseURLKey)
	if !has || v.Kind != '"' {
		return baseURLState{}
	}
	return baseURLState{present: true, value: wtf8(v.Text)}
}

func effectiveBaseURLState(lookup config.LookupEnv, local, shared, user baseURLState) baseURLState {
	if value := config.AnthropicBaseURL.Get(lookup); value != nil {
		return baseURLState{present: true, value: *value}
	}
	for _, candidate := range [...]baseURLState{local, shared, user} {
		if candidate.present {
			return candidate
		}
	}
	return baseURLState{}
}

func plannedLocalBaseURL(localDoc document, localOK bool, entries []jsonx.Member) baseURLState {
	if localOK {
		if env, has := localDoc.env(); has && truthy(env) {
			if env.Kind == '{' {
				if v, defined := member(env, baseURLKey); defined {
					// copyMissing skips any already-defined key, including null or non-string values.
					if v.Kind == '"' {
						return baseURLState{present: true, value: wtf8(v.Text)}
					}
					return baseURLState{}
				}
			}
		}
	}
	for _, m := range entries {
		if wtf8(m.Name) == baseURLKey && m.Value.Kind == '"' {
			return baseURLState{present: true, value: wtf8(m.Value.Text)}
		}
	}
	return docBaseURL(localDoc, localOK)
}

func effectiveBaseURLPreserved(lookup config.LookupEnv, beforeLocal, afterLocal, beforeShared, afterShared, user baseURLState) bool {
	before := effectiveBaseURLState(lookup, beforeLocal, beforeShared, user)
	after := effectiveBaseURLState(lookup, afterLocal, afterShared, user)
	if before != after {
		return false
	}
	// Also keep the persisted file-backed effective URL unchanged even when a transient process env shadows files.
	beforeFiles := effectiveBaseURLState(nil, beforeLocal, beforeShared, user)
	afterFiles := effectiveBaseURLState(nil, afterLocal, afterShared, user)
	return beforeFiles == afterFiles
}

func docBaseURLAfterDrop(doc document, drop []string) baseURLState {
	if slices.Contains(drop, baseURLKey) {
		return baseURLState{}
	}
	return docBaseURL(doc, true)
}

func readOptionalUserDoc(userFile string, legacyInfo fs.FileInfo) (doc document, userInfo fs.FileInfo, ok, aliasOrUncertain bool, err error) {
	data, info, readErr := readRegularFile(userFile)
	switch {
	case errors.Is(readErr, fs.ErrNotExist):
		return document{}, nil, false, false, nil
	case errors.Is(readErr, errSnapshotTooLarge):
		return document{}, nil, false, false, readErr
	case readErr != nil:
		return document{}, nil, false, true, nil
	}
	if sameFile(legacyInfo, info) {
		return document{}, info, false, true, nil
	}
	parsed, parseErr := parseDocument(data)
	return parsed, info, parseErr == nil, false, nil
}

func (s Store) migrateLegacy(explicit bool) (bool, error) {
	migrated, _, err := s.migrateLegacyStatus(explicit)
	return migrated, err
}

// migrateLegacyStatus follows lib/settings-wiring.js:361-402. It decides whether the shared project file is
// gateway-wired before reading the user or local files, and refuses when the shared or local file aliases the
// user file, when shared and local alias each other (compared on the opened file descriptors), or when user
// identity cannot be decided. A shared file wired to the compatibility URL moves only in an explicit write, which
// copies the gateway URL in its place. Automatic migration (!explicit) makes no edit if the planned changes would
// alter the effective base URL. Migration runs as two separate single-file atomic replacements rather than a
// two-file transaction: the destination copy callback re-verifies the same opened source file and identical bytes
// (along with alias and effective-URL guards), and the source deletion callback re-verifies the same opened source
// file, non-aliasing current local and user files, and each copied entry's original value before dropping it.
// declined is true when safety guards refused a migration so callers such as CleanLegacyEnv can stop without
// making further edits in that run.
func (s Store) migrateLegacyStatus(explicit bool) (migrated, declined bool, err error) {
	legacyFile, err := s.settingsPath("legacy-project")
	if err != nil {
		return false, false, err
	}
	userFile, err := s.settingsPath("user")
	if err != nil {
		return false, false, err
	}
	if filepath.Clean(legacyFile) == filepath.Clean(userFile) {
		return false, false, nil
	}
	legacy, legacyInfo, err := readDocumentSnapshot(legacyFile)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	base, ok := legacy.envValue(baseURLKey)
	if !s.ours(base, ok) {
		return false, false, nil
	}
	retired := isString(base, compatBaseURL)
	if retired && !explicit {
		return false, false, nil
	}
	env, _ := legacy.env()
	entries := s.ownedEntries(env)
	origValues := make(map[string]jsonx.WireValue, len(entries))
	for _, m := range entries {
		origValues[wtf8(m.Name)] = m.Value
	}
	if retired {
		entries = slices.Clone(entries)
		for i, m := range entries {
			if wtf8(m.Name) == baseURLKey {
				entries[i].Value = text(s.defaultBaseURL())
			}
		}
	}
	var drop []string
	for _, m := range entries {
		drop = append(drop, wtf8(m.Name))
	}
	hasLegacyWindow := false
	if v, ok := member(env, legacyKey); ok && jsString(v) == legacyValue {
		hasLegacyWindow = true
		if !slices.Contains(drop, legacyKey) {
			drop = append(drop, legacyKey)
		}
	}
	if len(drop) == 0 {
		return false, false, nil
	}
	userDoc, userInfo, userOK, skipUser, err := readOptionalUserDoc(userFile, legacyInfo)
	if err != nil {
		return false, false, err
	}
	if skipUser {
		return false, true, nil
	}
	localFile, err := s.settingsPath("project")
	if err != nil {
		return false, false, err
	}
	localData, localInfo, localErr := readRegularFile(localFile)
	switch {
	case localErr == nil:
		if sameFile(legacyInfo, localInfo) || sameFile(localInfo, userInfo) {
			return false, true, nil
		}
	case !errors.Is(localErr, fs.ErrNotExist):
		return false, false, formatReadError(localFile, localErr)
	}
	if err := s.checkOwnedDuplicates(legacy.data); err != nil {
		return false, false, fmt.Errorf("%s: %w", legacyFile, err)
	}
	var localDoc document
	localExists := localErr == nil
	if localExists {
		localDoc, err = parseDocument(localData)
		if err != nil {
			return false, false, formatParseError(localFile, err)
		}
		if err := s.checkOwnedDuplicates(localDoc.data); err != nil {
			return false, false, fmt.Errorf("%s: %w", localFile, err)
		}
	}
	if !explicit && !effectiveBaseURLPreserved(s.Lookup, docBaseURL(localDoc, localExists), plannedLocalBaseURL(localDoc, localExists, entries), docBaseURL(legacy, true), docBaseURLAfterDrop(legacy, drop), docBaseURL(userDoc, userOK)) {
		return false, true, nil
	}
	if len(entries) > 0 {
		aborted := false
		if _, err := s.writeSettingsInfo(localFile, func(doc *document, info fs.FileInfo) error {
			aborted = false
			curLegacyData, curLegacyInfo, curLegacyErr := readRegularFile(legacyFile)
			if errors.Is(curLegacyErr, errSnapshotTooLarge) {
				return curLegacyErr
			}
			if curLegacyErr != nil || !sameFile(curLegacyInfo, legacyInfo) || !bytes.Equal(curLegacyData, legacy.data) {
				aborted = true
				return errUnchanged
			}
			if info != nil && sameFile(info, curLegacyInfo) {
				aborted = true
				return errUnchanged
			}
			curUserDoc, curUserInfo, curUserOK, curSkipUser, curUserErr := readOptionalUserDoc(userFile, curLegacyInfo)
			if curUserErr != nil {
				return curUserErr
			}
			if curSkipUser || (info != nil && sameFile(info, curUserInfo)) {
				aborted = true
				return errUnchanged
			}
			if !explicit && !effectiveBaseURLPreserved(s.Lookup, docBaseURL(*doc, info != nil), plannedLocalBaseURL(*doc, info != nil, entries), docBaseURL(legacy, true), docBaseURLAfterDrop(legacy, drop), docBaseURL(curUserDoc, curUserOK)) {
				aborted = true
				return errUnchanged
			}
			return copyMissing(doc, localFile, entries)
		}); err != nil {
			return false, false, err
		}
		if aborted {
			return false, true, nil
		}
	}
	aborted := false
	changed, err := s.writeSettingsInfo(legacyFile, func(doc *document, info fs.FileInfo) error {
		aborted = false
		if info == nil || !sameFile(info, legacyInfo) {
			aborted = true
			return errUnchanged
		}
		curUserDoc, curUserInfo, curUserOK, curSkipUser, curUserErr := readOptionalUserDoc(userFile, info)
		if curUserErr != nil {
			return curUserErr
		}
		if curSkipUser {
			aborted = true
			return errUnchanged
		}
		curLocalData, curLocalInfo, curLocalErr := readRegularFile(localFile)
		if errors.Is(curLocalErr, errSnapshotTooLarge) {
			return curLocalErr
		}
		if curLocalErr != nil || sameFile(info, curLocalInfo) || sameFile(curLocalInfo, curUserInfo) {
			aborted = true
			return errUnchanged
		}
		curLocalDoc, perr := parseDocument(curLocalData)
		if perr != nil {
			aborted = true
			return errUnchanged
		}
		for key := range origValues {
			if _, present := curLocalDoc.envValue(key); !present {
				aborted = true
				return errUnchanged
			}
		}
		curBase, curBaseOK := doc.envValue(baseURLKey)
		if !s.ours(curBase, curBaseOK) || !strictEqual(curBase, curBaseOK, base, true) || (isString(curBase, compatBaseURL) && !explicit) {
			aborted = true
			return errUnchanged
		}
		curEnv, _ := doc.env()
		var dropNow []string
		for _, m := range s.ownedEntries(curEnv) {
			name := wtf8(m.Name)
			origVal, wasCopied := origValues[name]
			if wasCopied && strictEqual(m.Value, true, origVal, true) {
				dropNow = append(dropNow, name)
			}
		}
		if hasLegacyWindow {
			if v, ok := member(curEnv, legacyKey); ok && jsString(v) == legacyValue && !slices.Contains(dropNow, legacyKey) {
				dropNow = append(dropNow, legacyKey)
			}
		}
		if len(dropNow) == 0 {
			aborted = true
			return errUnchanged
		}
		if !explicit && !effectiveBaseURLPreserved(s.Lookup, docBaseURL(curLocalDoc, true), docBaseURL(curLocalDoc, true), docBaseURL(*doc, true), docBaseURLAfterDrop(*doc, dropNow), docBaseURL(curUserDoc, curUserOK)) {
			aborted = true
			return errUnchanged
		}
		return removeEnvKeys(doc, dropNow)
	})
	if err != nil {
		return false, false, err
	}
	if aborted {
		return false, true, nil
	}
	return changed, false, nil
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
// When automatic migration declines to edit in order to preserve the effective endpoint or avoid an alias,
// CleanLegacyEnv stops without modifying any settings file in that run.
func (s Store) CleanLegacyEnv() error {
	_, declined, err := s.migrateLegacyStatus(false)
	if err != nil || declined {
		return err
	}
	for _, scope := range [...]string{"user", "project"} {
		file, err := s.settingsPath(scope)
		if err != nil {
			return err
		}
		doc, ok, err := readDocument(file)
		if err != nil {
			return err
		}
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
		if _, err := s.writeSettingsInfo(file, func(doc *document, info fs.FileInfo) error {
			if info == nil {
				return errUnchanged
			}
			env, has := doc.env()
			if !has || !truthy(env) || !s.ours(doc.envValue(baseURLKey)) {
				return errUnchanged
			}
			if v, ok := member(env, legacyKey); !ok || jsString(v) != legacyValue {
				return errUnchanged
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
		return nil, errors.New(sanitizeJSONError(err))
	}
	if err := s.checkOwnedDuplicates(doc.data); err != nil {
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
		if err := s.checkOwnedDuplicates(doc.data); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		edits, err := forms.sync(doc.root, true)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if len(edits) == 0 {
			continue
		}
		if _, err := s.writeSettingsInfo(file, func(doc *document, info fs.FileInfo) error {
			if info == nil || !s.ours(doc.envValue(baseURLKey)) {
				return errUnchanged
			}
			return nil
		}); err != nil {
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
		doc, _, err := readDocumentSnapshot(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
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
		var msg string
		changed, err := s.writeSettingsInfo(is.file, func(doc *document, info fs.FileInfo) error {
			preserved = nil
			msg = ""
			if info == nil {
				return errUnchanged
			}
			env, _ := doc.env()
			if _, ok := member(env, baseURLKey); ok {
				return errUnchanged
			}
			var ok bool
			msg, ok = orphanMessage(s.ownedEntries(env))
			if !ok {
				return errUnchanged
			}
			var err error
			preserved, err = s.removeOwned(doc, false)
			return err
		})
		if err != nil {
			return err
		}
		if changed {
			report("model-gateway: repaired " + msg + " in " + is.file + "; preserved user pins: " + cmp.Or(strings.Join(preserved, ", "), "none"))
		}
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

// windowSkipReason formats the reason for skipping a non-owned autoCompactWindow without echoing arbitrary
// authored strings or composite values into diagnostic text.
func windowSkipReason(current jsonx.WireValue) string {
	if current.Kind == '0' {
		return "autoCompactWindow " + numberString(current.Number) + " is not gateway-owned"
	}
	return "autoCompactWindow is not gateway-owned"
}

// SyncWindow updates only a missing or previously owned cap. Nil next selects the full window.
func (s Store) SyncWindow(files []string, owned, next *int) (Changes, error) {
	var r Changes
	for _, file := range files {
		doc, err := readSettingsForWrite(file)
		if err != nil {
			if errors.Is(err, errSnapshotTooLarge) {
				return r, err
			}
			r.Skipped = append(r.Skipped, Skipped{File: file, Reason: err.Error()})
			continue
		}
		current, has := member(doc.root, "autoCompactWindow")
		if has && !windowMatches(current, owned) && !windowMatches(current, next) {
			r.Skipped = append(r.Skipped, Skipped{File: file, Reason: windowSkipReason(current)})
			continue
		}
		if err := s.checkOwnedDuplicates(doc.data); err != nil {
			return r, fmt.Errorf("%s: %w", file, err)
		}
		if !windowChanges(current, has, next) {
			continue
		}
		var skipped *Skipped
		changed, err := s.writeSettings(file, func(doc *document) error {
			skipped = nil
			current, has := member(doc.root, "autoCompactWindow")
			if has && !windowMatches(current, owned) && !windowMatches(current, next) {
				skipped = &Skipped{File: file, Reason: windowSkipReason(current)}
				return errUnchanged
			}
			if !windowChanges(current, has, next) {
				return errUnchanged
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
		if skipped != nil {
			r.Skipped = append(r.Skipped, *skipped)
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
// selects the production policy adapter configured by Lookup and Paths. Write failures are advisory
// and return false with a nil error, matching lib/settings-wiring.js:438-440.
func (s Store) CleanModelCache(file string, canonical func(string) (string, bool)) (bool, error) {
	data, _, err := readRegularFile(file)
	if err != nil {
		if errors.Is(err, errSnapshotTooLarge) {
			return false, err
		}
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
		return false, nil
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
