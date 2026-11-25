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

// MinProxyVersion is the oldest supported proxy version.
const MinProxyVersion = ""

// ParseVersion extracts the first three-part numeric version in text.
func ParseVersion(text string) ([3]float64, bool) { return [3]float64{}, false }

// Older reports whether the first parsed version is older than the second.
func Older(first, second [3]float64) bool { return false }

// RegistryDirectories returns the parent directories of the resolved registry files in search order.
func RegistryDirectories(registries []string) []string { return nil }

// ReadEntries reads matching installs in registry order. A bare name matches any marketplace.
func ReadEntries(registries []string, pluginID string) []map[string]any { return nil }

// CompareEntries orders registry entries newest first by numeric version parts, then by update text.
func CompareEntries(left, right map[string]any) int { return 0 }

// Scope reports the model-gateway installation scope across the supplied registry files.
func Scope(registries []string) string { return "" }

// IsCacheParent reports whether directory is a plugin cache parent.
func IsCacheParent(directory string, registryDirs []string) bool { return false }

// CacheIdentity returns the common identity of sibling version directories, or the empty string.
func CacheIdentity(installRoot string, registryDirs []string) string { return "" }
