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
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// HighWater tracks the largest request body for each session in a state directory.
type HighWater struct {
	directory string
	now       func() time.Time
	mu        sync.Mutex
	maxima    map[string]int64
}

// NewHighWater creates a tracker for a clean absolute directory. It panics if now is nil.
func NewHighWater(directory string, now func() time.Time) *HighWater {
	if now == nil {
		panic("usage: missing high-water clock")
	}
	return &HighWater{directory: directory, now: now, maxima: make(map[string]int64)}
}

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,255}$`)

// Record persists a new maximum and returns the known value, or nil if unavailable.
func (h *HighWater) Record(session string, size int64) *int64 {
	if size < 0 || size > maxSafeInteger || !sessionPattern.MatchString(session) || !filepath.IsAbs(h.directory) || filepath.Clean(h.directory) != h.directory {
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
				if state, err := jsonx.DecodeWire(data); err == nil {
					v := field(state, "value")
					if v.Kind == '0' && v.Number >= 0 && v.Number <= maxSafeInteger && v.Number == math.Trunc(v.Number) {
						previous = int64(v.Number)
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
		f, e := root.OpenFile(filepath.Join(entry.Name(), session+".jsonl"), readFlags, 0)
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

const maxProjectLookupBytes = 64 << 10

func transcriptCWD(f *os.File) string {
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		return ""
	}
	data, e := io.ReadAll(io.LimitReader(f, maxProjectLookupBytes))
	if e != nil {
		return ""
	}
	m := cwdMember.FindSubmatch(data)
	if m == nil {
		return ""
	}
	var cwd string
	// Only the captured value must be valid Unicode; unrelated transcript fields need not be decodable.
	if !utf8.Valid(m[1]) || json.Unmarshal(m[1], &cwd) != nil {
		return ""
	}
	return cwd
}

func readGitPointer(f *os.File, info fs.FileInfo) (string, bool) {
	if !info.Mode().IsRegular() {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxProjectLookupBytes+1))
	if err != nil || len(data) > maxProjectLookupBytes {
		return "", false
	}
	return string(data), true
}

// projectFromCWD follows .git and commondir symlinks, but accepts only bounded regular-file pointers; root confinement applies only to transcripts.
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
		marker, err := os.OpenFile(filepath.Join(p, ".git"), readFlags, 0)
		if err == nil {
			info, e := marker.Stat()
			if e == nil {
				if info.IsDir() {
					path = p
				} else if data, ok := readGitPointer(marker, info); ok {
					if repo := worktreeRoot(p, data); repo != "" {
						path = repo
					}
				}
			}
			_ = marker.Close()
			if e != nil || info.IsDir() || info.Mode().IsRegular() || p == filepath.Dir(p) {
				break
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) || p == filepath.Dir(p) {
			break
		}
	}
	label := sanitizeLabel(filepath.Base(path), false)
	return safeID(label[:min(64, len(label))])
}

func worktreeRoot(dir, pointer string) string {
	rest, ok := strings.CutPrefix(config.JSTrim(pointer), "gitdir:")
	if !ok {
		return ""
	}
	resolve := func(base, s string) string {
		if filepath.IsAbs(s) {
			return filepath.Clean(s)
		}
		return filepath.Join(base, s)
	}
	p := resolve(dir, config.JSTrim(rest))
	common, err := os.OpenFile(filepath.Join(p, "commondir"), readFlags, 0)
	if err == nil {
		defer func() { _ = common.Close() }()
		info, err := common.Stat()
		if err != nil {
			return ""
		}
		if info.Mode().IsRegular() {
			data, ok := readGitPointer(common, info)
			if !ok {
				return ""
			}
			p = resolve(p, config.JSTrim(data))
		}
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
