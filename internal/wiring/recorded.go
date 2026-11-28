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
	"errors"
	"fmt"
	"io/fs"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// RecordedProjects lists unique absolute recorded project paths without requiring local wiring or repairing the registry.
// A missing registry gives an empty list. Paths keep their recorded order and the first spelling of each project,
// where two spellings name the same project when their registry keys are equal. Unlike the repairing reader, an
// unreadable registry, invalid JSON, a root that is not an object or a projects member that is not a list is an
// error; elements that are not well-formed absolute path strings are skipped. Nothing is written.
func (s Store) RecordedProjects() ([]string, error) {
	file, err := s.stateFile(registryName)
	if err != nil {
		return nil, err
	}
	data, _, err := readRegularFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read project registry: %w", err)
	}
	root, err := jsonx.DecodeWire(data)
	if err != nil {
		return nil, fmt.Errorf("read project registry %s: %s", file, sanitizeJSONError(err))
	}
	// member refuses a root that is not an object; the decoder already let the last duplicate member win.
	list, ok := member(root, "projects")
	if !ok || list.Kind != '[' {
		return nil, fmt.Errorf("read project registry %s: no projects list", file)
	}
	seen := make(map[string]bool, len(list.Elements))
	projects := make([]string, 0, len(list.Elements))
	for _, entry := range list.Elements {
		p, ok := projectPath(entry)
		if !ok {
			continue
		}
		key := registryKey(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		projects = append(projects, p)
	}
	return projects, nil
}
