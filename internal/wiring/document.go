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
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

const utf8BOM = "\xef\xbb\xbf"

// stripLeadingBOM removes at most one leading UTF-8 byte-order mark before decoding a settings document's
// semantic view. The authored bytes keep any leading BOM so in-place edits preserve it.
func stripLeadingBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte(utf8BOM))
}

// document is a parsed settings file: the authored bytes, which every edit splices, and the value JSON.parse
// would produce from them (after removing at most one leading UTF-8 BOM), which every decision reads.
type document struct {
	data []byte
	root jsonx.WireValue
}

func parseDocument(data []byte) (document, error) {
	root, err := jsonx.DecodeWire(stripLeadingBOM(data))
	if err != nil {
		return document{}, err
	}
	return document{data: bytes.Clone(data), root: root}, nil
}

// edit splices the edits into the authored bytes and re-reads the result, so later decisions see the new state.
func (d *document) edit(edits ...jsonx.Edit) error {
	if len(edits) == 0 {
		return nil
	}
	data, err := jsonx.EditSettings(d.data, edits...)
	if err != nil {
		return err
	}
	root, err := jsonx.DecodeWire(stripLeadingBOM(data))
	if err != nil {
		return err
	}
	d.data, d.root = data, root
	return nil
}

// env returns the settings env member as JSON.parse resolves it.
func (d document) env() (jsonx.WireValue, bool) { return member(d.root, "env") }

// envValue reads settings.env?.[key]. Only an object env has the gateway's fixed key names as properties.
func (d document) envValue(key string) (jsonx.WireValue, bool) {
	env, ok := d.env()
	if !ok {
		return jsonx.WireValue{}, false
	}
	return member(env, key)
}

// readDocument mirrors the source's try { JSON.parse(readFileSync(file)) } catch {} readers, bounded to regular
// files so special files never block the readiness path. Any failure means no document.
func readDocument(file string) (document, bool) {
	data, _, err := readRegularFile(file)
	if err != nil {
		return document{}, false
	}
	doc, err := parseDocument(data)
	return doc, err == nil
}

// readDocumentSnapshot reads a regular settings file once, returning both its parsed view and the FileInfo of
// the opened descriptor. Missing files return fs.ErrNotExist unwrapped so callers can distinguish them.
func readDocumentSnapshot(file string) (document, fs.FileInfo, error) {
	data, info, err := readRegularFile(file)
	if err != nil {
		return document{}, nil, err
	}
	doc, err := parseDocument(data)
	if err != nil {
		return document{}, info, fmt.Errorf("Could not read %s: %w", file, err) //nolint:staticcheck // The message is the source's user-facing wording.
	}
	return doc, info, nil
}

// readSettingsForWrite treats a missing file as an empty object and names the file in every other failure
// (lib/settings-wiring.js:76-83).
func readSettingsForWrite(file string) (document, error) {
	data, _, err := readRegularFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		data, err = []byte("{}"), nil
	}
	if err != nil {
		return document{}, fmt.Errorf("Could not read %s: %w", file, err) //nolint:staticcheck // The message is the source's user-facing wording.
	}
	doc, err := parseDocument(data)
	if err != nil {
		return document{}, fmt.Errorf("Could not read %s: %w", file, err) //nolint:staticcheck // The message is the source's user-facing wording.
	}
	return doc, nil
}

// errUnchanged tells the settings replacement that a recomputed edit has nothing to write or is no longer
// eligible, so a missing file is not created and no hooks are edited.
var errUnchanged = errors.New("settings unchanged")

// missingDocument is what the settings replacement hands an edit in place of a file that does not exist.
const missingDocument = "{}"

// testSeamInfo derives existence and identity when a caller overrides Store.Replace, whose signature carries
// only bytes. Production writes take the atomicfile.ReplaceSettingsInfo path directly with no extra lookup.
func testSeamInfo(file string, old []byte) (fs.FileInfo, bool, error) {
	info, err := os.Stat(file)
	if err == nil {
		return info, true, nil
	}
	if string(old) != missingDocument {
		return nil, true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		if linfo, lerr := os.Lstat(file); lerr == nil {
			return linfo, true, nil
		}
		return nil, false, nil
	}
	return nil, false, err
}

// replaceInfo runs change on the current bytes and opened-file info of file inside the settings replacement.
// change may run more than once when another writer modifies the file concurrently. It reports whether the file
// was written.
//
// An existing file is edited in place and keeps every byte the edits do not name, including any leading UTF-8
// BOM and trailing bytes after the closing brace. A file this write creates has no authored bytes: it is written
// as the source writes settings, JSON.stringify(settings, null, 2) followed by one newline
// (lib/settings-wiring.js:107-112). Production writes get existence directly from atomicfile.ReplaceSettingsInfo
// on the same opened target that the replacement verifies before commit.
func (s Store) replaceInfo(file string, change func(*document, fs.FileInfo) error) (bool, error) {
	apply := func(old []byte, info fs.FileInfo, existed bool) ([]byte, error) {
		doc, err := parseDocument(old)
		if err != nil {
			return nil, fmt.Errorf("Could not read %s: %w", file, err) //nolint:staticcheck // The message is the source's user-facing wording.
		}
		if doc.root.Kind != '{' {
			return nil, fmt.Errorf("Could not read %s: settings must be a JSON object", file) //nolint:staticcheck // The message is the source's user-facing wording.
		}
		if err := change(&doc, info); err != nil {
			return nil, err
		}
		if bytes.Equal(doc.data, old) {
			return nil, errUnchanged
		}
		if existed {
			return doc.data, nil
		}
		return jsonx.EncodeState(doc.root)
	}
	var err error
	if s.Replace == nil {
		err = atomicfile.ReplaceSettingsInfo(file, func(old []byte, info fs.FileInfo) ([]byte, error) {
			return apply(old, info, info != nil)
		})
	} else {
		err = s.Replace(file, func(old []byte) ([]byte, error) {
			info, existed, statErr := testSeamInfo(file, old)
			if statErr != nil {
				return nil, statErr
			}
			return apply(old, info, existed)
		})
	}
	if errors.Is(err, errUnchanged) {
		return false, nil
	}
	return err == nil, err
}

// writeSettings is the source's writeSettings: the caller's edits, then the readiness hooks re-derived from the
// base URL the edited file holds (lib/settings-wiring.js:107-112). Returning errUnchanged from change aborts
// the transaction without editing hooks or writing the file.
func (s Store) writeSettings(file string, change func(*document) error) (bool, error) {
	return s.writeSettingsInfo(file, func(doc *document, _ fs.FileInfo) error {
		if change == nil {
			return nil
		}
		return change(doc)
	})
}

func (s Store) writeSettingsInfo(file string, change func(*document, fs.FileInfo) error) (bool, error) {
	forms, err := s.hookForms()
	if err != nil {
		return false, err
	}
	return s.replaceInfo(file, func(doc *document, info fs.FileInfo) error {
		if change != nil {
			if err := change(doc, info); err != nil {
				return err
			}
		}
		enabled := s.ours(doc.envValue(baseURLKey))
		if err := forms.checkDuplicates(*doc, enabled); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		edits, err := forms.sync(doc.root, enabled)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		return doc.edit(edits...)
	})
}

func decodeRawKey(raw []byte) (string, bool) {
	v, err := jsonx.DecodeWire(raw)
	if err != nil || v.Kind != '"' {
		return "", false
	}
	return wtf8(v.Text), true
}

// checkRawDuplicates inspects raw JSON text for duplicate member names on parentKey at the root and, when
// childKeys is non-empty and parentKey holds a JSON object, for duplicate member names among childKeys inside
// that object. Checking raw text catches duplicates before parent-delete coalescing or equal-value shortcuts.
func checkRawDuplicates(data []byte, parentKey string, childKeys map[string]bool) error {
	src := stripLeadingBOM(data)
	d := jsontext.NewDecoder(bytes.NewReader(src), jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))
	if d.PeekKind() != '{' {
		return nil
	}
	if _, err := d.ReadToken(); err != nil {
		return nil
	}
	parentCount := 0
	var parentVal []byte
	for d.PeekKind() != '}' {
		rawKey, err := d.ReadValue()
		if err != nil {
			return nil
		}
		k, ok := decodeRawKey(rawKey)
		rawVal, err := d.ReadValue()
		if err != nil {
			return nil
		}
		if !ok || k != parentKey {
			continue
		}
		parentCount++
		if parentCount > 1 {
			return fmt.Errorf("settings %s: duplicate owned key %q", pointer(parentKey), parentKey)
		}
		parentVal = bytes.Clone(rawVal)
	}
	if parentCount != 1 || len(childKeys) == 0 || len(parentVal) == 0 || parentVal[0] != '{' {
		return nil
	}
	cd := jsontext.NewDecoder(bytes.NewReader(parentVal), jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))
	if _, err := cd.ReadToken(); err != nil {
		return nil
	}
	seen := make(map[string]int, len(childKeys))
	for cd.PeekKind() != '}' {
		rawKey, err := cd.ReadValue()
		if err != nil {
			return nil
		}
		k, ok := decodeRawKey(rawKey)
		if _, err := cd.ReadValue(); err != nil {
			return nil
		}
		if !ok || !childKeys[k] {
			continue
		}
		seen[k]++
		if seen[k] > 1 {
			return fmt.Errorf("settings %s: duplicate owned key %q", pointer(parentKey, k), k)
		}
	}
	return nil
}

// checkRawOwnedDuplicates refuses a raw env object where a watched key appears more than once and at least one
// occurrence matches isOwned. This prevents a later personal value from masking an earlier owned duplicate.
func checkRawOwnedDuplicates(data []byte, keys map[string]bool, isOwned func(string, jsonx.WireValue) bool) error {
	src := stripLeadingBOM(data)
	d := jsontext.NewDecoder(bytes.NewReader(src), jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))
	if d.PeekKind() != '{' {
		return nil
	}
	if _, err := d.ReadToken(); err != nil {
		return nil
	}
	envCount := 0
	var envVal []byte
	for d.PeekKind() != '}' {
		rawKey, err := d.ReadValue()
		if err != nil {
			return nil
		}
		k, ok := decodeRawKey(rawKey)
		rawVal, err := d.ReadValue()
		if err != nil {
			return nil
		}
		if ok && k == "env" {
			envCount++
			envVal = bytes.Clone(rawVal)
		}
	}
	if envCount == 0 || len(envVal) == 0 || envVal[0] != '{' {
		return nil
	}
	if envCount > 1 {
		// Check whether any occurrence of env has an owned value; if so, duplicate env is ambiguous.
		if hasAnyOwnedInRaw(src, keys, isOwned) {
			return fmt.Errorf("settings %s: duplicate owned key %q", pointer("env"), "env")
		}
		return nil
	}
	cd := jsontext.NewDecoder(bytes.NewReader(envVal), jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))
	if _, err := cd.ReadToken(); err != nil {
		return nil
	}
	counts := make(map[string]int, len(keys))
	owned := make(map[string]bool, len(keys))
	for cd.PeekKind() != '}' {
		rawKey, err := cd.ReadValue()
		if err != nil {
			return nil
		}
		k, ok := decodeRawKey(rawKey)
		rawVal, err := cd.ReadValue()
		if err != nil {
			return nil
		}
		if !ok || !keys[k] {
			continue
		}
		counts[k]++
		if !owned[k] {
			if v, err := jsonx.DecodeWire(rawVal); err == nil && isOwned(k, v) {
				owned[k] = true
			}
		}
		if counts[k] > 1 && owned[k] {
			return fmt.Errorf("settings %s: duplicate owned key %q", pointer("env", k), k)
		}
	}
	return nil
}

func hasAnyOwnedInRaw(src []byte, keys map[string]bool, isOwned func(string, jsonx.WireValue) bool) bool {
	d := jsontext.NewDecoder(bytes.NewReader(src), jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))
	if d.PeekKind() != '{' {
		return false
	}
	if _, err := d.ReadToken(); err != nil {
		return false
	}
	for d.PeekKind() != '}' {
		rawKey, err := d.ReadValue()
		if err != nil {
			return false
		}
		k, ok := decodeRawKey(rawKey)
		rawVal, err := d.ReadValue()
		if err != nil {
			return false
		}
		if !ok || k != "env" || len(rawVal) == 0 || rawVal[0] != '{' {
			continue
		}
		cd := jsontext.NewDecoder(bytes.NewReader(bytes.Clone(rawVal)), jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))
		if _, err := cd.ReadToken(); err != nil {
			continue
		}
		for cd.PeekKind() != '}' {
			ek, err := cd.ReadValue()
			if err != nil {
				break
			}
			name, ok := decodeRawKey(ek)
			ev, err := cd.ReadValue()
			if err != nil {
				break
			}
			if !ok || !keys[name] {
				continue
			}
			if v, err := jsonx.DecodeWire(ev); err == nil && isOwned(name, v) {
				return true
			}
		}
	}
	return false
}

func exists(file string) bool {
	_, err := os.Stat(file)
	return err == nil
}

// member returns an object member by name. The decoder already resolved duplicate names as JSON.parse does.
func member(v jsonx.WireValue, name string) (jsonx.WireValue, bool) {
	if v.Kind != '{' {
		return jsonx.WireValue{}, false
	}
	units := utf16.Encode([]rune(name))
	for _, m := range v.Members {
		if slices.Equal(m.Name, units) {
			return m.Value, true
		}
	}
	return jsonx.WireValue{}, false
}

// wtf8 spells UTF-16 text as a Go string without losing an unpaired surrogate: such a unit keeps its own
// three-byte encoding, so the result never equals a well-formed string and two texts compare equal exactly when
// JavaScript's === would.
func wtf8(units []uint16) string {
	b := make([]byte, 0, len(units))
	for i := 0; i < len(units); i++ {
		r := rune(units[i])
		if !utf16.IsSurrogate(r) {
			b = utf8.AppendRune(b, r)
			continue
		}
		if i+1 < len(units) {
			if pair := utf16.DecodeRune(r, rune(units[i+1])); pair != utf8.RuneError {
				b = utf8.AppendRune(b, pair)
				i++
				continue
			}
		}
		b = append(b, 0xe0|byte(r>>12), 0x80|byte(r>>6&0x3f), 0x80|byte(r&0x3f))
	}
	return string(b)
}

// wellFormed returns the text as a Go string only when it has no unpaired surrogate.
func wellFormed(units []uint16) (string, bool) {
	s := wtf8(units)
	return s, utf8.ValidString(s)
}

func isString(v jsonx.WireValue, want string) bool { return v.Kind == '"' && wtf8(v.Text) == want }

func text(s string) jsonx.WireValue { return jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune(s))} }

func object(members ...jsonx.Member) jsonx.WireValue {
	return jsonx.WireValue{Kind: '{', Members: members}
}

func field(name string, value jsonx.WireValue) jsonx.Member {
	return jsonx.Member{Name: utf16.Encode([]rune(name)), Value: value}
}

func encode(v jsonx.WireValue) (jsontext.Value, error) {
	data, err := jsonx.EncodeWire(v)
	return jsontext.Value(data), err
}

func pointer(tokens ...string) jsontext.Pointer {
	var p jsontext.Pointer
	for _, token := range tokens {
		p = p.AppendToken(token)
	}
	return p
}

func set(value jsonx.WireValue, tokens ...string) (jsonx.Edit, error) {
	data, err := encode(value)
	return jsonx.Edit{Path: pointer(tokens...), Value: data}, err
}

func remove(tokens ...string) jsonx.Edit { return jsonx.Edit{Path: pointer(tokens...)} }

// truthy applies JavaScript's ToBoolean to a parsed JSON value.
func truthy(v jsonx.WireValue) bool {
	switch v.Kind {
	case 'n', 'f':
		return false
	case '0':
		return v.Number != 0 && !math.IsNaN(v.Number)
	case '"':
		return len(v.Text) > 0
	default:
		return true
	}
}

// keyCount is Object.keys(value).length for a parsed JSON value.
func keyCount(v jsonx.WireValue) int {
	switch v.Kind {
	case '{':
		return len(v.Members)
	case '[':
		return len(v.Elements)
	case '"':
		return len(v.Text)
	default:
		return 0
	}
}

// jsString is JavaScript's String(value) for a parsed JSON value, spelled as wtf8 does.
func jsString(v jsonx.WireValue) string {
	switch v.Kind {
	case '"':
		return wtf8(v.Text)
	case '0':
		return numberString(v.Number)
	case 't':
		return "true"
	case 'f':
		return "false"
	case 'n':
		return "null"
	case '[':
		parts := make([]string, len(v.Elements))
		for i, e := range v.Elements {
			if e.Kind != 'n' {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// numberString is Number.prototype.toString for a float64; JSON's spelling of a finite number is the same text.
func numberString(n float64) string {
	switch {
	case math.IsNaN(n):
		return "NaN"
	case math.IsInf(n, 1):
		return "Infinity"
	case math.IsInf(n, -1):
		return "-Infinity"
	}
	data, err := jsonx.EncodeWire(jsonx.WireValue{Kind: '0', Number: n})
	if err != nil {
		return "NaN"
	}
	return string(data)
}

// strictEqual is JavaScript's === between two optional parsed values. Two objects or arrays are never identical.
func strictEqual(a jsonx.WireValue, aok bool, b jsonx.WireValue, bok bool) bool {
	if !aok || !bok {
		return aok == bok
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case '"':
		return wtf8(a.Text) == wtf8(b.Text)
	case '0':
		return a.Number == b.Number
	case 'n', 't', 'f':
		return true
	default:
		return false
	}
}

// hasUnpairedSurrogate reports whether v or any nested member name or value contains an unpaired UTF-16 surrogate.
func hasUnpairedSurrogate(v jsonx.WireValue) bool {
	switch v.Kind {
	case '"':
		_, ok := wellFormed(v.Text)
		return !ok
	case '[':
		return slices.ContainsFunc(v.Elements, hasUnpairedSurrogate)
	case '{':
		for _, m := range v.Members {
			if _, ok := wellFormed(m.Name); !ok || hasUnpairedSurrogate(m.Value) {
				return true
			}
		}
	}
	return false
}

// plain converts a parsed value into the Go values a report carries: string, float64, bool, nil, []any or
// map[string]any for well-formed values, or the lossless jsonx.WireValue when an unpaired UTF-16 surrogate is
// present so neither string content nor distinct surrogate object keys are lost before serialization.
func plain(v jsonx.WireValue) any {
	if hasUnpairedSurrogate(v) {
		return v
	}
	switch v.Kind {
	case '"':
		return string(utf16.Decode(v.Text))
	case '0':
		return v.Number
	case 't':
		return true
	case 'f':
		return false
	case '[':
		out := make([]any, len(v.Elements))
		for i, e := range v.Elements {
			out[i] = plain(e)
		}
		return out
	case '{':
		out := make(map[string]any, len(v.Members))
		for _, m := range v.Members {
			out[string(utf16.Decode(m.Name))] = plain(m.Value)
		}
		return out
	default:
		return nil
	}
}
