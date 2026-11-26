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

package policy

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Setting is a resolved backend window, its source and any saved direct compaction maximum.
type Setting struct {
	Value     config.ContextWindow
	Source    string
	CompactAt int64
}

// Windows contains the Claude, Codex and Grok settings.
type Windows map[string]Setting

// Cap returns the backend's numeric cap or zero for an uncapped backend.
func (w Windows) Cap(backend string) int64 {
	if w == nil && backend == "codex" {
		return defaultCodexCap
	}
	return int64(w[backend].Value.Tokens)
}

func validBackend(backend string) bool {
	return backend == "claude" || backend == "codex" || backend == "grok"
}

func windowMin(backend string) int64 {
	if backend == "claude" {
		return 100000
	}
	return 100000 + capMargin
}

func parseWindow(backend string, value any) config.ContextWindow {
	if value == "full" {
		return config.ContextWindow{Full: true}
	}
	n := config.JSNumber(value)
	if n >= float64(windowMin(backend)) && n <= clientLimit && n == math.Trunc(n) {
		return config.ContextWindow{Tokens: int(n)}
	}
	return config.ContextWindow{}
}

func positiveSafe(n float64) int64 {
	if n > 0 && n <= maxSafe && n == math.Trunc(n) {
		return int64(n)
	}
	return 0
}

// ParseCompactAt parses a JSON string or number as a positive safe integer; zero means invalid.
func ParseCompactAt(value jsontext.Value) int64 {
	v, err := jsonx.DecodeWire(value)
	if err != nil {
		return 0
	}
	switch v.Kind {
	case '"':
		return positiveSafe(config.JSNumber(string(utf16.Decode(v.Text))))
	case '0':
		return positiveSafe(v.Number)
	default:
		return 0
	}
}

func member(o jsonx.WireValue, name string) jsonx.WireValue {
	for _, m := range o.Members {
		if string(utf16.Decode(m.Name)) == name {
			return m.Value
		}
	}
	return jsonx.WireValue{}
}

func unsupportedCompact(backend string) error {
	return fmt.Errorf("unsupported compactAt.%s: direct compaction maxima are supported for Codex and Grok only; use --claude <tokens|full> for the native window", backend)
}

func validateCompact(v jsonx.WireValue) error {
	// Object.entries boxes primitives. Only a nonempty string or array contributes an indexed entry.
	if len(v.Text) > 0 || len(v.Elements) > 0 {
		return unsupportedCompact("0")
	}
	if v.Kind == 'n' {
		return errors.New("invalid compactAt: must not be null")
	}
	for _, m := range v.Members {
		backend := string(utf16.Decode(m.Name))
		if backend != "codex" && backend != "grok" {
			return unsupportedCompact(backend)
		}
		if m.Value.Kind != '0' || positiveSafe(m.Value.Number) == 0 {
			return fmt.Errorf("invalid compactAt.%s: use a positive whole token count", backend)
		}
	}
	return nil
}

// JavaScript array coercion stringifies every element, even if the result cannot be numeric.
// For JSON objects, an own toString is never callable and valueOf cannot return a primitive.
func canCoerceWindow(v jsonx.WireValue) bool {
	if v.Kind == '{' {
		return member(v, "toString").Kind == 0
	}
	for _, element := range v.Elements {
		if !canCoerceWindow(element) {
			return false
		}
	}
	return true
}

func savedWindow(backend string, v jsonx.WireValue) (config.ContextWindow, error) {
	if !canCoerceWindow(v) {
		return config.ContextWindow{}, fmt.Errorf("cannot convert %s context window to a number", backend)
	}
	if v.Kind == '"' {
		return parseWindow(backend, string(utf16.Decode(v.Text))), nil
	}
	// Only singleton arrays can stringify to a numeric spelling within the allowed range.
	for v.Kind == '[' && len(v.Elements) == 1 {
		v = v.Elements[0]
	}
	switch v.Kind {
	case '0':
		return parseWindow(backend, v.Number), nil
	case '"':
		return parseWindow(backend, config.JSNumber(string(utf16.Decode(v.Text)))), nil
	default:
		return config.ContextWindow{}, nil
	}
}

func checkWindowsPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("context window path must be clean and absolute: %s", path)
	}
	return nil
}

// ReadWindows resolves saved settings, the supplied environment and backend defaults, in that order.
// The caller must supply a clean, absolute path.
func ReadWindows(path string, lookup config.LookupEnv) (Windows, error) {
	if err := checkWindowsPath(path); err != nil {
		return nil, err
	}
	// A missing, unreadable or malformed file has no saved settings. The depth limit is an explicit refusal.
	var root jsonx.WireValue
	if data, err := os.ReadFile(path); err == nil {
		root, err = jsonx.DecodeWire(data)
		if depth, ok := errors.AsType[*jsonx.WireDepthError](err); ok {
			return nil, depth
		}
	}
	compact := member(root, "compactAt")
	if err := validateCompact(compact); err != nil {
		return nil, err
	}
	w := Windows{}
	for _, backend := range []string{"claude", "codex", "grok"} {
		value, err := savedWindow(backend, member(root, backend))
		if err != nil {
			return nil, err
		}
		source := "saved"
		if value == (config.ContextWindow{}) && backend == "codex" {
			value = config.CodexContextWindow.Get(lookup)
			source = "CODEX_GATEWAY_CONTEXT_WINDOW"
		}
		if value == (config.ContextWindow{}) {
			source = "default"
			if backend == "codex" {
				value.Tokens = defaultCodexCap
			} else {
				value.Full = true
			}
		}
		w[backend] = Setting{Value: value, Source: source, CompactAt: int64(member(compact, backend).Number)}
	}
	return w, nil
}

// WriteWindows validates the persisted JSON object before creating its directory and writing it atomically.
// The caller must supply a clean, absolute path.
func WriteWindows(path string, values jsontext.Value) error {
	if err := checkWindowsPath(path); err != nil {
		return err
	}
	root, err := jsonx.DecodeWire(values)
	if err != nil {
		return err
	}
	if root.Kind != '{' {
		return errors.New("context windows must be a JSON object")
	}
	if err := validateCompact(member(root, "compactAt")); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return jsonx.WriteState(path, root, 0o666)
}

// Update changes one window or compactAt field. Zero tokens means full or cap, respectively.
type Update struct {
	Backend, Field string
	Tokens         int64
}

// ParseUpdate validates a flag and its optional argument; nil means no following token.
func ParseUpdate(flag string, value *string) (Update, error) {
	const usage = "context-window expects --claude, --codex, or --grok followed by a token count or full; --codex-compact-at or --grok-compact-at accepts tokens or cap"
	raw := "undefined"
	if value != nil {
		raw = *value
	}
	backend, ok := strings.CutPrefix(flag, "--")
	if !ok {
		return Update{}, errors.New(usage)
	}
	if name, compact := strings.CutSuffix(backend, "-compact-at"); compact {
		if name != "codex" && name != "grok" {
			return Update{}, unsupportedCompact(name)
		}
		tokens := positiveSafe(config.JSNumber(raw))
		if tokens == 0 && raw != "cap" {
			return Update{}, fmt.Errorf(`invalid %s compact-at "%s": use cap or a positive whole token count`, name, raw)
		}
		return Update{Backend: name, Field: "compactAt", Tokens: tokens}, nil
	}
	if !validBackend(backend) {
		return Update{}, errors.New(usage)
	}
	v := parseWindow(backend, raw)
	if v == (config.ContextWindow{}) {
		return Update{}, fmt.Errorf(`invalid %s context window "%s": use full or a whole number of tokens from %d to %d`, backend, raw, windowMin(backend), clientLimit)
	}
	return Update{Backend: backend, Field: "window", Tokens: int64(v.Tokens)}, nil
}

// setMember preserves insertion order; a zero kind deletes the property.
func setMember(obj *jsonx.WireValue, key string, value jsonx.WireValue) {
	name := utf16.Encode([]rune(key))
	for i, m := range obj.Members {
		if slices.Equal(m.Name, name) {
			if value.Kind == 0 {
				obj.Members = slices.Delete(obj.Members, i, i+1)
			} else {
				obj.Members[i].Value = value
			}
			return
		}
	}
	if value.Kind != 0 {
		obj.Members = append(obj.Members, jsonx.Member{Name: name, Value: value})
	}
}

func windowJSON(tokens int64) jsonx.WireValue {
	if tokens == 0 {
		return jsonx.WireValue{Kind: '"', Text: []uint16{'f', 'u', 'l', 'l'}}
	}
	return jsonx.WireValue{Kind: '0', Number: float64(tokens)}
}

// Saved returns only the explicitly saved values, in gateway serialization order.
func (w Windows) Saved() jsontext.Value {
	root := jsonx.WireValue{Kind: '{'}
	compact := jsonx.WireValue{Kind: '{'}
	for _, backend := range []string{"claude", "codex", "grok"} {
		setting := w[backend]
		if setting.Source == "saved" {
			setMember(&root, backend, windowJSON(int64(setting.Value.Tokens)))
		}
		if backend != "claude" && setting.CompactAt != 0 {
			setMember(&compact, backend, windowJSON(setting.CompactAt))
		}
	}
	if len(compact.Members) != 0 {
		setMember(&root, "compactAt", compact)
	}
	data, _ := jsonx.EncodeWire(root) // The tree contains only known scalar fields and two object levels.
	return data
}

// Apply updates a saved settings object, deleting the last compactAt object when emptied.
func (u Update) Apply(saved jsontext.Value) (jsontext.Value, error) {
	obj, err := jsonx.DecodeWire(saved)
	if err != nil {
		return nil, err
	}
	if obj.Kind != '{' {
		return nil, errors.New("context windows must be a JSON object")
	}
	if u.Field == "window" {
		setMember(&obj, u.Backend, windowJSON(u.Tokens))
	} else {
		compact := member(obj, "compactAt")
		if err := validateCompact(compact); err != nil {
			return nil, err
		}
		compact.Kind = '{'
		v := jsonx.WireValue{}
		if u.Tokens != 0 {
			v = windowJSON(u.Tokens)
		}
		setMember(&compact, u.Backend, v)
		if len(compact.Members) == 0 {
			compact = jsonx.WireValue{}
		}
		setMember(&obj, "compactAt", compact)
	}
	return jsonx.EncodeWire(obj)
}
