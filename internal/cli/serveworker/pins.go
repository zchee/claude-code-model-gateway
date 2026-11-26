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

package serveworker

import (
	"os"
	"path/filepath"
	"slices"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/pins"
)

// pinAliases are the alias families a pin exists for (lib/runtime.js:528-551).
var pinAliases = [...]string{"opus", "sonnet", "fable"}

// currentPins returns the effective pin value of each alias family, computed from the saved pin
// overrides and the saved detection cache on every call, as the source's currentPinValues does
// (lib/previous-picker-rows.js:26-28, lib/pins.js:366-380). It never probes the claude CLI and writes
// nothing. Only the values are used, so the catalog candidates and the retired history, which feed
// the newest-pin field alone, are not read.
func currentPins(state string) map[string]string {
	in := pins.Inputs{
		Overrides: readPinOverrides(filepath.Join(state, "pins.json")),
		Cache:     readDetectedPins(filepath.Join(state, "detected-pins.json")),
	}
	out := make(map[string]string, len(pinAliases))
	for alias, pin := range in.Effective() {
		out[alias] = pin.Value
	}
	return out
}

// readPinOverrides follows readPinOverrides (lib/pins.js:346-354): a file that is missing, unparsable
// or not an object holds no override, and each alias keeps only a usable pin.
func readPinOverrides(path string) map[string]string {
	saved, ok := readStateObject(path)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(pinAliases))
	for _, alias := range pinAliases {
		if value, ok := stringMember(saved, alias); ok && pins.ValidOverride(value) {
			out[alias] = value
		}
	}
	return out
}

// readDetectedPins follows readDetectedPinCache (lib/pins.js:79-93): the CLI version must be a string;
// each detected pin and each saved catalog pin is normalized for its alias and dropped when it is not
// a pin of that alias; a saved catalog pin is also dropped when it does not outrank the alias's
// shipped pin (validatedCatalogPins, lib/pins.js:123-127); an alias's detection version defaults to
// the CLI version. A file that is missing, unparsable or not an object yields an empty cache.
func readDetectedPins(path string) pins.Cache {
	saved, ok := readStateObject(path)
	if !ok {
		return pins.Cache{}
	}
	cache := pins.Cache{
		Detected:    map[string]string{},
		DetectedFor: map[string]string{},
		CatalogPins: map[string]string{},
	}
	cache.CLIVersion, _ = stringMember(saved, "cliVersion")
	detected, detectedFor, catalog := member(saved, "pins"), member(saved, "detectedFor"), member(saved, "catalogPins")
	for _, alias := range pinAliases {
		if value, ok := stringMember(detected, alias); ok {
			if pin, ok := pins.NormalizeDetected(alias, value); ok {
				cache.Detected[alias] = pin
				cache.DetectedFor[alias] = cache.CLIVersion
				if version, ok := stringMember(detectedFor, alias); ok {
					cache.DetectedFor[alias] = version
				}
			}
		}
		if value, ok := stringMember(catalog, alias); ok {
			if pin, ok := pins.NormalizeDetected(alias, value); ok && pins.OutranksShipped(alias, pin) {
				cache.CatalogPins[alias] = pin
			}
		}
	}
	return cache
}

// readStateObject reads a gateway state file as JSON.parse does and returns its object, or false when
// the file is missing or unreadable, does not parse, or is not an object.
func readStateObject(path string) (jsonx.WireValue, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return jsonx.WireValue{}, false
	}
	v, err := jsonx.DecodeWire(data)
	if err != nil || v.Kind != '{' {
		return jsonx.WireValue{}, false
	}
	return v, true
}

// member returns the last member of an object with the given name, as JSON.parse keeps the last
// duplicate, or the zero value.
func member(v jsonx.WireValue, name string) jsonx.WireValue {
	if v.Kind != '{' {
		return jsonx.WireValue{}
	}
	want := utf16.Encode([]rune(name))
	for _, m := range slices.Backward(v.Members) {
		if slices.Equal(m.Name, want) {
			return m.Value
		}
	}
	return jsonx.WireValue{}
}

// stringMember returns a string member of an object.
func stringMember(v jsonx.WireValue, name string) (string, bool) {
	m := member(v, name)
	if m.Kind != '"' {
		return "", false
	}
	return string(utf16.Decode(m.Text)), true
}
