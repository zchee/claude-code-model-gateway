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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

const registryName = "wired-projects.json"

// recorded is recordedWiringProjects: the listed entries, whether the file must be rewritten because it is
// unreadable or holds no list, and whether it exists (lib/settings-wiring.js:206-215).
type recorded struct {
	projects        []jsonx.WireValue
	rewrite, exists bool
}

func readRegistry(file string) (recorded, error) {
	data, _, err := readRegularFile(file)
	if err != nil {
		if errors.Is(err, errSnapshotTooLarge) {
			return recorded{}, err
		}
		missing := errors.Is(err, fs.ErrNotExist)
		return recorded{rewrite: !missing, exists: !missing}, nil
	}
	root, err := jsonx.DecodeWire(data)
	if err != nil {
		return recorded{rewrite: true, exists: true}, nil
	}
	projects, ok := member(root, "projects")
	if !ok || projects.Kind != '[' {
		return recorded{rewrite: true, exists: true}, nil
	}
	return recorded{projects: projects.Elements, exists: true}, nil
}

// writeRegistry replaces the registry atomically with mode 0600, creating the state directory as the source's
// mkdirSync does (lib/settings-wiring.js:201-204).
func writeRegistry(file string, projects []string) error {
	dir, err := atomicfile.OpenDirCreate(filepath.Dir(file), 0o777)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	data, err := jsonx.EncodeState(struct {
		Projects []string `json:"projects"`
	}{append([]string{}, projects...)})
	if err != nil {
		return err
	}
	return dir.WriteFile(filepath.Base(file), data, 0o600)
}

// normalize is Node's path.normalize: lexical cleaning that keeps one trailing separator.
func normalize(p string) string {
	clean := filepath.Clean(p)
	if strings.HasSuffix(p, string(filepath.Separator)) && !strings.HasSuffix(clean, string(filepath.Separator)) {
		clean += string(filepath.Separator)
	}
	return clean
}

// registryKey identifies a project directory by the real path of its nearest existing ancestor, with any
// missing components appended, falling back to the lexical path (lib/settings-wiring.js:178-199).
func registryKey(project string) string {
	resolved := filepath.Clean(project)
	var missing []string
	existing := resolved
	for !exists(existing) {
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		missing = append([]string{filepath.Base(existing)}, missing...)
		existing = parent
	}
	key := resolved
	if real, err := filepath.EvalSymlinks(existing); err == nil {
		key = filepath.Join(append([]string{real}, missing...)...)
	}
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}

// projectPath returns a registry entry that is an absolute path. A path with an unpaired surrogate cannot name
// a file without being altered, so such an entry is ignored with the other unusable ones.
func projectPath(entry jsonx.WireValue) (string, bool) {
	if entry.Kind != '"' {
		return "", false
	}
	p, ok := wellFormed(entry.Text)
	return p, ok && filepath.IsAbs(p)
}

// registeredWiring evaluates one registry entry (lib/settings-wiring.js:217-242). It returns a wiring, a pruned
// directory, neither for an entry that is unusable, already seen, or no longer wired, or an error if the project's
// settings file exceeds the size bound.
func (s Store) registeredWiring(entry jsonx.WireValue, seen map[string]bool) (*Registration, *Pruned, error) {
	p, ok := projectPath(entry)
	if !ok {
		return nil, nil, nil
	}
	project := normalize(p)
	key := registryKey(project)
	if seen[key] {
		return nil, nil, nil
	}
	if info, err := os.Stat(project); err != nil || !info.IsDir() {
		return nil, &Pruned{Project: project, Reason: "directory is missing"}, nil
	}
	file := filepath.Join(project, ".claude", "settings.local.json")
	doc, ok, err := readDocument(file)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, nil
	}
	value, ok := doc.envValue(baseURLKey)
	if !s.ours(value, ok) {
		return nil, nil, nil
	}
	seen[key] = true
	return &Registration{Project: project, File: file, Value: wtf8(value.Text)}, nil, nil
}

// report is registeredProjectWiringReport: unless readOnly, an existing registry is rewritten with the surviving
// projects when it was unusable or its list changed (lib/settings-wiring.js:244-253).
func (s Store) report(readOnly bool) ([]Registration, []Pruned, error) {
	file, err := s.stateFile(registryName)
	if err != nil {
		return nil, nil, err
	}
	rec, err := readRegistry(file)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	var wirings []Registration
	var pruned []Pruned
	for _, entry := range rec.projects {
		w, p, err := s.registeredWiring(entry, seen)
		if err != nil {
			return nil, nil, err
		}
		if w != nil {
			wirings = append(wirings, *w)
		}
		if p != nil {
			pruned = append(pruned, *p)
		}
	}
	if !readOnly && rec.exists {
		projects := projectsOf(wirings)
		if rec.rewrite || !sameProjects(rec.projects, projects) {
			if err := writeRegistry(file, projects); err != nil {
				return nil, nil, err
			}
		}
	}
	return wirings, pruned, nil
}

func projectsOf(wirings []Registration) []string {
	projects := make([]string, len(wirings))
	for i, w := range wirings {
		projects[i] = w.Project
	}
	return projects
}

// sameProjects compares the JSON text of the recorded list and the surviving list, as the source does.
func sameProjects(recordedList []jsonx.WireValue, projects []string) bool {
	next := jsonx.WireValue{Kind: '[', Elements: make([]jsonx.WireValue, len(projects))}
	for i, p := range projects {
		next.Elements[i] = text(p)
	}
	a, err := jsonx.EncodeWire(jsonx.WireValue{Kind: '[', Elements: recordedList})
	if err != nil {
		return false
	}
	b, err := jsonx.EncodeWire(next)
	return err == nil && bytes.Equal(a, b)
}

// Projects reads and deduplicates registrations. A writable scan also repairs an existing registry.
// Concurrent updates to the project registry are last-writer-wins, matching the source
// (lib/settings-wiring.js:201-253,317-331).
func (s Store) Projects(readOnly bool) ([]Registration, []Pruned, error) {
	return s.report(readOnly)
}

// registered reports whether the working project is listed, comparing canonical paths without reading any
// project's settings (lib/settings-wiring.js:152-156).
func (s Store) registered() (bool, error) {
	project, err := s.projectDir()
	if err != nil {
		return false, err
	}
	file, err := s.stateFile(registryName)
	if err != nil {
		return false, err
	}
	rec, err := readRegistry(file)
	if err != nil {
		return false, err
	}
	key := registryKey(project)
	for _, entry := range rec.projects {
		if p, ok := projectPath(entry); ok && registryKey(p) == key {
			return true, nil
		}
	}
	return false, nil
}

// RecordProject records the project only if its local settings are wired.
// Concurrent updates to the project registry are last-writer-wins, matching the source
// (lib/settings-wiring.js:201-253,317-331).
func (s Store) RecordProject() error {
	project, err := s.projectDir()
	if err != nil {
		return err
	}
	file := filepath.Join(project, ".claude", "settings.local.json")
	doc, ok, err := readDocument(file)
	if err != nil {
		return err
	}
	if !ok || !s.ours(doc.envValue(baseURLKey)) {
		// The source still refreshes the registry here (lib/settings-wiring.js:317-323).
		_, _, err := s.report(false)
		return err
	}
	wirings, _, err := s.report(false)
	if err != nil {
		return err
	}
	key := registryKey(project)
	if slices.ContainsFunc(wirings, func(w Registration) bool { return registryKey(w.Project) == key }) {
		return nil
	}
	registry, err := s.stateFile(registryName)
	if err != nil {
		return err
	}
	if err := writeRegistry(registry, append(projectsOf(wirings), project)); err != nil {
		return fmt.Errorf("record wired project %s: %w", project, err)
	}
	return nil
}

// pinKeys lists the pin keys in traversal order.
func (s Store) pinKeys() []string {
	pins := s.expectedPins()
	keys := make([]string, len(pins))
	for i, e := range pins {
		keys[i] = e.Key
	}
	return keys
}

// firstUnowned is pinValuesNotOwned: the first pin key present with a value the gateway could not have written.
// A key without owned values owns nothing (lib/settings-wiring.js:259-263).
func (s Store) firstUnowned(doc document) (string, jsonx.WireValue, bool) {
	for _, key := range s.pinKeys() {
		if v, ok := doc.envValue(key); ok && !slices.Contains(s.Pins.Owned[key], jsString(v)) {
			return key, v, true
		}
	}
	return "", jsonx.WireValue{}, false
}

// stalePins is stalePinsIn: only pins the project holds are updated, except that a project holding the Fable
// alias also receives the Fable picker row (lib/settings-wiring.js:265-275).
func (s Store) stalePins(doc document) []Entry {
	_, addsFableOption := doc.envValue(fableAliasKey)
	var stale []Entry
	for _, e := range s.expectedPins() {
		current, ok := doc.envValue(e.Key)
		if (ok || addsFableOption && isFableOptionKey(e.Key)) && (!ok || !isString(current, e.Value)) {
			stale = append(stale, e)
		}
	}
	return stale
}

func applyPins(doc *document, stale []Entry) error {
	edits := make([]jsonx.Edit, 0, len(stale))
	for _, e := range stale {
		edit, err := set(text(e.Value), "env", e.Key)
		if err != nil {
			return err
		}
		edits = append(edits, edit)
	}
	return doc.edit(edits...)
}

// SyncPins updates registered projects without replacing user-owned pins or choices.
func (s Store) SyncPins() (Changes, error) {
	wirings, pruned, err := s.report(false)
	if err != nil {
		return Changes{}, err
	}
	r := Changes{Pruned: pruned}
	for _, w := range wirings {
		doc, err := readSettingsForWrite(w.File)
		if err != nil {
			if errors.Is(err, errSnapshotTooLarge) {
				return r, err
			}
			r.Skipped = append(r.Skipped, Skipped{File: w.File, Value: w.Value, Reason: err.Error()})
			continue
		}
		if key, v, ok := s.firstUnowned(doc); ok {
			r.Skipped = append(r.Skipped, Skipped{File: w.File, Key: key, Value: plain(v), Reason: "pin value is not gateway-owned"})
			continue
		}
		if err := s.checkOwnedDuplicates(doc.data); err != nil {
			return r, fmt.Errorf("%s: %w", w.File, err)
		}
		if len(s.stalePins(doc)) == 0 {
			continue
		}
		var skipped *Skipped
		changed, err := s.writeSettingsInfo(w.File, func(doc *document, info fs.FileInfo) error {
			skipped = nil
			if info == nil || !s.ours(doc.envValue(baseURLKey)) {
				return errUnchanged
			}
			if key, v, unowned := s.firstUnowned(*doc); unowned {
				skipped = &Skipped{File: w.File, Key: key, Value: plain(v), Reason: "pin value is not gateway-owned"}
				return errUnchanged
			}
			stale := s.stalePins(*doc)
			if len(stale) == 0 {
				return errUnchanged
			}
			return applyPins(doc, stale)
		})
		if err != nil {
			return r, err
		}
		if skipped != nil {
			r.Skipped = append(r.Skipped, *skipped)
		}
		if changed {
			r.Changed = append(r.Changed, w.File)
		}
	}
	return r, nil
}

// PinDisagreements compares even missing pin keys without rewriting the registry or settings.
func (s Store) PinDisagreements() ([]Disagreement, []Pruned, error) {
	wirings, pruned, err := s.report(true)
	if err != nil {
		return nil, nil, err
	}
	var out []Disagreement
	for _, w := range wirings {
		doc, err := readSettingsForWrite(w.File)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range s.expectedPins() {
			v, ok := doc.envValue(e.Key)
			if ok && isString(v, e.Value) {
				continue
			}
			d := Disagreement{Registration: w, Key: e.Key, Expected: e.Value, Present: ok}
			if ok {
				d.Stale = plain(v)
			}
			out = append(out, d)
		}
	}
	return out, pruned, nil
}

// Reconcile lists projects wired to a conflicting gateway URL and removes only their owned entries on confirmation.
// Only the configured default gateway URL is accepted as the target; the retired compatibility URL is rejected
// before scanning or rewriting the project registry, while legacy entries remain recognized as owned.
func (s Store) Reconcile(target string, confirm bool) (conflicting, reconciled []Registration, err error) {
	if target == compatBaseURL {
		return nil, nil, fmt.Errorf("Cannot reconcile project wiring to retired compatibility base URL: %s", target) //nolint:staticcheck // Matches the source's user-facing wording for target errors.
	}
	if target != s.defaultBaseURL() {
		return nil, nil, fmt.Errorf("Cannot reconcile project wiring to unknown base URL: %s", target) //nolint:staticcheck // The message is the source's user-facing wording.
	}
	wirings, _, err := s.report(false)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range wirings {
		if w.Value != target {
			conflicting = append(conflicting, w)
		}
	}
	if !confirm {
		return conflicting, nil, nil
	}
	for _, w := range conflicting {
		removed, err := s.removeOwnedWiring(w.File, target)
		if err != nil {
			return conflicting, reconciled, err
		}
		if removed {
			reconciled = append(reconciled, w)
		}
	}
	if _, _, err := s.report(false); err != nil {
		return conflicting, reconciled, err
	}
	return conflicting, reconciled, nil
}

// removeOwnedWiring removes the owned entries of a project wired to a conflicting owned URL (lib/settings-wiring.js:333-342).
func (s Store) removeOwnedWiring(file, target string) (bool, error) {
	doc, err := readSettingsForWrite(file)
	if err != nil {
		return false, err
	}
	value, ok := doc.envValue(baseURLKey)
	if !s.ours(value, ok) || wtf8(value.Text) == target {
		return false, nil
	}
	env, _ := doc.env()
	if len(s.ownedEntries(env)) == 0 {
		return false, nil
	}
	return s.writeSettingsInfo(file, func(doc *document, info fs.FileInfo) error {
		if info == nil {
			return errUnchanged
		}
		value, ok := doc.envValue(baseURLKey)
		if !s.ours(value, ok) || wtf8(value.Text) == target {
			return errUnchanged
		}
		env, _ := doc.env()
		if len(s.ownedEntries(env)) == 0 {
			return errUnchanged
		}
		_, err := s.removeOwned(doc, false)
		return err
	})
}
