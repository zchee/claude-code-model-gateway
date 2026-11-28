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

package modeldata

import "encoding/json/jsontext"

// Read returns the normalized native catalog object and update timestamp, or a regular-file I/O error.
func Read(path string) (jsontext.Value, float64, error) { panic("not implemented") }

// Version contains the lowercased family and numeric fields of a concrete model ID.
type Version struct {
	Family       string
	Major, Minor float64
}

// ParseVersion parses a canonical pin, treating an omitted minor as zero.
func ParseVersion(id string) (Version, bool) { panic("not implemented") }

// Compare returns the sign of a version difference for comparable IDs.
func Compare(first, second string) (float64, bool) { panic("not implemented") }

// DisplayName formats a concrete model number, retaining an explicit zero minor.
func DisplayName(id string) string { panic("not implemented") }
