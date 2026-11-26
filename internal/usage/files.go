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

package usage

import (
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// HighWater tracks the largest request body for each session in a state directory.
type HighWater struct {
	directory string
	now       func() time.Time
	mu        sync.Mutex
	maxima    map[string]int64
}

// NewHighWater creates a tracker for the supplied directory using the supplied clock.
func NewHighWater(directory string, now func() time.Time) *HighWater {
	if now == nil {
		now = time.Now
	}
	return &HighWater{directory: directory, now: now, maxima: make(map[string]int64)}
}

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,255}$`)

// Record persists a new maximum and returns the known value, or nil if unavailable.
func (h *HighWater) Record(session string, size int64) *int64 {
	if size < 0 || !sessionPattern.MatchString(session) || !filepath.IsAbs(h.directory) || filepath.Clean(h.directory) != h.directory {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	previous, known := h.maxima[session]
	if known && size <= previous {
		return new(previous)
	}
	name := base64.RawURLEncoding.EncodeToString([]byte(session)) + ".json"
	dir, err := atomicfile.OpenDir(h.directory)
	if errors.Is(err, fs.ErrNotExist) && size > 0 {
		if err = os.MkdirAll(h.directory, 0o700); err == nil {
			dir, err = atomicfile.OpenDir(h.directory)
		}
	}
	if err == nil {
		defer func() { _ = dir.Close() }()
		if !known {
			if data, e := dir.ReadFile(name); e == nil {
				var state map[string]any
				if json.Unmarshal(data, &state, jsontext.AllowDuplicateNames(true)) == nil {
					if n, ok := numeric(state["value"]); ok && n < 1<<63 {
						previous = int64(n)
					}
				}
			}
		}
	}
	h.maxima[session] = previous
	if size <= previous {
		return new(previous)
	}
	if err == nil {
		state := struct {
			Value      int64  `json:"value"`
			ObservedAt string `json:"observed_at"`
		}{size, h.now().UTC().Format("2006-01-02T15:04:05.000Z")}
		data, e := jsonx.EncodeState(state)
		compact := jsontext.Value(data)
		if e == nil && compact.Compact() == nil && dir.WriteFile(name, compact, 0o600) == nil {
			h.maxima[session] = size
			return new(size)
		}
	}
	if previous > 0 {
		return new(previous)
	}
	return nil
}

const jsWhitespace = `[\t\n\v\f\r\p{Zs}\x{FEFF}\x{2028}\x{2029}]*`

var cwdMember = regexp.MustCompile(`"cwd"` + jsWhitespace + `:` + jsWhitespace + `("(?:\\[^\n\r\x{2028}\x{2029}]|[^"\\])*")`)

func (b *Builder) projectName(session string) string {
	if session == "" || !filepath.IsAbs(b.options.ProjectsDir) || filepath.Clean(b.options.ProjectsDir) != b.options.ProjectsDir {
		return ""
	}
	if e := b.projects.byKey[session]; e != nil {
		b.projects.order.MoveToBack(e)
		return e.Value.(cacheEntry[string, string]).value
	}
	root, err := os.OpenRoot(b.options.ProjectsDir)
	if err != nil {
		return ""
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		f, e := root.Open(filepath.Join(entry.Name(), session+".jsonl"))
		if e != nil {
			continue
		}
		cwd := transcriptCWD(f)
		_ = f.Close()
		if name := projectFromCWD(cwd); name != "" {
			b.projects.put(session, name, time.Time{}, 500)
			return name
		}
	}
	return ""
}

func transcriptCWD(f *os.File) string {
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		return ""
	}
	data, e := io.ReadAll(io.LimitReader(f, 64<<10))
	if e != nil {
		return ""
	}
	m := cwdMember.FindSubmatch(data)
	if m == nil {
		return ""
	}
	var cwd string
	if json.Unmarshal(m[1], &cwd) != nil {
		return ""
	}
	return cwd
}

func projectFromCWD(cwd string) string {
	if !filepath.IsAbs(cwd) {
		return ""
	}
	path := filepath.Clean(cwd)
	for p := path; ; p = filepath.Dir(p) {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			rest, _ := filepath.Rel(p, path)
			path = filepath.Join(resolved, rest)
			break
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	for p := path; ; p = filepath.Dir(p) {
		marker, err := os.Open(filepath.Join(p, ".git"))
		if err == nil {
			info, e := marker.Stat()
			if e == nil {
				if info.IsDir() {
					path = p
				} else if info.Mode().IsRegular() {
					if data, e := io.ReadAll(marker); e == nil {
						if repo := worktreeRoot(p, string(data)); repo != "" {
							path = repo
						}
					}
				}
			}
			_ = marker.Close()
			break
		}
		if !errors.Is(err, fs.ErrNotExist) || p == filepath.Dir(p) {
			break
		}
	}
	label := sanitizeLabel(filepath.Base(path), false)
	return safeID(label[:min(64, len(label))])
}

func worktreeRoot(dir, pointer string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(pointer), "gitdir:")
	if !ok {
		return ""
	}
	resolve := func(base, s string) string {
		if filepath.IsAbs(s) {
			return filepath.Clean(s)
		}
		return filepath.Join(base, s)
	}
	p := resolve(dir, strings.TrimSpace(rest))
	data, err := os.ReadFile(filepath.Join(p, "commondir"))
	if err == nil {
		p = resolve(p, strings.TrimSpace(string(data)))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	for filepath.Base(p) != ".git" {
		if p == filepath.Dir(p) {
			return ""
		}
		p = filepath.Dir(p)
	}
	root := filepath.Dir(p)
	if info, e := os.Stat(root); e == nil && info.IsDir() {
		return root
	}
	return ""
}
