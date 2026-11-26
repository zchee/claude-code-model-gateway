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

// Package install reads installed-plugin registries and compares installed versions.
package install

import (
	"cmp"
	"encoding/json/jsontext"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// MinProxyVersion is the oldest supported proxy version.
const MinProxyVersion = "0.1.14"

var (
	threeParts = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)
	nonDigits  = regexp.MustCompile(`[^0-9]+`)
	separators = regexp.MustCompile(`[\\/]+`)
)

// ParseVersion extracts the first three-part numeric version in text.
func ParseVersion(text string) ([3]float64, bool) {
	var parts [3]float64
	version := threeParts.FindString(text)
	if version == "" {
		return parts, false
	}
	i := 0
	for part := range strings.SplitSeq(version, ".") {
		parts[i], _ = strconv.ParseFloat(part, 64)
		i++
	}
	return parts, true
}

// Older reports whether the first parsed version is older than the second.
func Older(first, second [3]float64) bool {
	for i, part := range first {
		if part != second[i] {
			return part < second[i]
		}
	}
	return false
}

// RegistryDirectories returns the parent directories of the resolved registry files in search order.
func RegistryDirectories(registries []string) []string {
	var dirs []string
	for _, file := range registries {
		if !filepath.IsAbs(file) || filepath.Clean(file) != file {
			continue
		}
		if dir := filepath.Dir(file); !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// ReadEntries reads matching installs in registry order. A bare name matches any marketplace.
func ReadEntries(registries []string, pluginID string) []map[string]any {
	entries := []map[string]any{}
	exact := strings.ContainsRune(pluginID, '@')
	prefix := pluginID + "@"
	for _, file := range registries {
		if !filepath.IsAbs(file) || filepath.Clean(file) != file {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		root, err := jsonx.DecodeWire(data)
		if err != nil {
			continue
		}
		for _, member := range root.Members {
			if string(utf16.Decode(member.Name)) != "plugins" {
				continue
			}
			for _, plugin := range member.Value.Members {
				if !wellFormedUTF16(plugin.Name) {
					continue
				}
				key := string(utf16.Decode(plugin.Name))
				if exact && key != pluginID || !exact && !strings.HasPrefix(key, prefix) {
					continue
				}
				for _, item := range plugin.Value.Elements {
					if entry := registryEntry(item); entry != nil {
						entry["registry"] = file
						entries = append(entries, entry)
					}
				}
			}
			break
		}
	}
	return entries
}

func registryEntry(item jsonx.WireValue) map[string]any {
	value := jsonValue(item)
	if !truthy(value) {
		return nil
	}
	if entry, ok := value.(map[string]any); ok {
		return entry
	}
	// JavaScript spreads array elements and individual UTF-16 string units as indexed properties.
	entry := make(map[string]any, len(item.Elements)+len(item.Text)+1)
	elements, _ := value.([]any)
	for i, element := range elements {
		entry[strconv.Itoa(i)] = element
	}
	for i := range item.Text {
		entry[strconv.Itoa(i)] = string(utf16.Decode(item.Text[i : i+1]))
	}
	return entry
}

func jsonValue(value jsonx.WireValue) any {
	switch value.Kind {
	case '{':
		object := make(map[string]any, len(value.Members))
		for _, member := range value.Members {
			if wellFormedUTF16(member.Name) {
				object[string(utf16.Decode(member.Name))] = jsonValue(member.Value)
			}
		}
		return object
	case '[':
		array := make([]any, len(value.Elements))
		for i, element := range value.Elements {
			array[i] = jsonValue(element)
		}
		return array
	case '"':
		return string(utf16.Decode(value.Text))
	case '0':
		return value.Number
	case 't':
		return true
	case 'f':
		return false
	default:
		return nil
	}
}

func wellFormedUTF16(units []uint16) bool {
	for i := 0; i < len(units); i++ {
		if r := rune(units[i]); utf16.IsSurrogate(r) {
			if i+1 == len(units) || utf16.DecodeRune(r, rune(units[i+1])) == utf8.RuneError {
				return false
			}
			i++
		}
	}
	return true
}

// CompareEntries orders registry entries newest first by numeric version parts, then by update text.
func CompareEntries(left, right map[string]any) int {
	a := nonDigits.Split(jsString(left["version"]), -1)
	b := nonDigits.Split(jsString(right["version"]), -1)
	for i := range max(len(a), len(b)) {
		leftPart := versionPart(a, i)
		rightPart := versionPart(b, i)
		if math.IsInf(leftPart, 1) && math.IsInf(rightPart, 1) {
			// The source comparator's subtraction yields NaN, which sort treats as a tie.
			return 0
		}
		if order := cmp.Compare(rightPart, leftPart); order != 0 {
			return order
		}
	}
	return collate.New(language.Und).CompareString(updateText(right), updateText(left))
}

func versionPart(parts []string, i int) float64 {
	if i >= len(parts) {
		return 0
	}
	n, _ := strconv.ParseFloat(parts[i], 64)
	return n
}

func updateText(entry map[string]any) string {
	if value := entry["lastUpdated"]; truthy(value) {
		return jsString(value)
	}
	return ""
}

func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0 && !math.IsNaN(v)
	default:
		return true
	}
}

func jsString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		switch {
		case math.IsInf(v, -1):
			return "-Infinity"
		case math.IsInf(v, 1):
			return "Infinity"
		case v == 0:
			return "0"
		default:
			return string(jsontext.AppendFloat(nil, v, 64))
		}
	case []any:
		var text strings.Builder
		for i, element := range v {
			if i > 0 {
				text.WriteByte(',')
			}
			text.WriteString(jsString(element))
		}
		return text.String()
	default:
		return "[object Object]"
	}
}

// Scope reports the model-gateway installation scope across the supplied registry files.
func Scope(registries []string) string {
	entries := ReadEntries(registries, "model-gateway")
	if len(entries) == 0 {
		return "unknown"
	}
	for _, entry := range entries {
		if entry["scope"] == "user" {
			return "user"
		}
	}
	return "project-only"
}

// IsCacheParent reports whether directory is a plugin cache parent.
func IsCacheParent(directory string, registryDirs []string) bool {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return false
	}
	if asciiEqualFold(filepath.Base(directory), "plugins") {
		return true
	}
	physical := normalPath(physicalPath(directory))
	for _, registryDir := range registryDirs {
		if filepath.IsAbs(registryDir) && filepath.Clean(registryDir) == registryDir && normalPath(physicalPath(registryDir)) == physical {
			return true
		}
	}
	return false
}

// CacheIdentity returns the common identity of sibling version directories, or the empty string.
// The identity is only a comparison key, not authority to signal a process or delete a path.
func CacheIdentity(installRoot string, registryDirs []string) string {
	if !filepath.IsAbs(installRoot) || filepath.Clean(installRoot) != installRoot {
		return ""
	}
	root := physicalPath(installRoot)
	version := filepath.Base(root)
	if threeParts.FindString(version) != version {
		return ""
	}
	pluginDir := filepath.Dir(root)
	cacheDir := filepath.Dir(filepath.Dir(pluginDir))
	parent := filepath.Dir(cacheDir)
	if !asciiEqualFold(filepath.Base(pluginDir), "model-gateway") || !asciiEqualFold(filepath.Base(cacheDir), "cache") || !IsCacheParent(parent, registryDirs) {
		return ""
	}
	// Even if the version leaf is absent, its parent may resolve through a symlink.
	return normalPath(physicalPath(pluginDir))
}

func physicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	// Some callers compare candidates that have not landed on disk yet.
	return path
}

func normalPath(path string) string {
	return cases.Lower(language.Und).String(separators.ReplaceAllString(path, "/"))
}

func asciiEqualFold(text, lower string) bool {
	if len(text) != len(lower) {
		return false
	}
	for i := range len(text) {
		b := text[i]
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if b != lower[i] {
			return false
		}
	}
	return true
}
