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

// Package anthropiccatalog persists upstream catalog snapshots and merges picker rows.
package anthropiccatalog

import (
	"cmp"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/pins"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

// Snapshot holds the normalized catalog in member order and its numeric update time.
// UpdatedAt is in milliseconds and may be infinite even though JSON serializes it as null.
type Snapshot struct {
	Object    jsontext.Value
	UpdatedAt float64
}

// Fresh reports whether the supplied time is less than one hour after the last update.
func (s Snapshot) Fresh(at time.Time) bool {
	return float64(at.UnixMilli())-s.UpdatedAt < float64(time.Hour.Milliseconds())
}

func member(name string, value jsonx.WireValue) jsonx.Member {
	return jsonx.Member{Name: utf16.Encode([]rune(name)), Value: value}
}

func field(obj jsonx.WireValue, name string) jsonx.WireValue {
	key := utf16.Encode([]rune(name))
	for _, m := range obj.Members {
		if slices.Equal(m.Name, key) {
			return m.Value
		}
	}
	return jsonx.WireValue{}
}

func text(s string) jsonx.WireValue    { return jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune(s))} }
func number(n float64) jsonx.WireValue { return jsonx.WireValue{Kind: '0', Number: n} }

func idString(v jsonx.WireValue) (string, bool) {
	if v.Kind != '"' {
		return "", false
	}
	for i := 0; i < len(v.Text); i++ {
		u := rune(v.Text[i])
		if !utf16.IsSurrogate(u) {
			continue
		}
		i++
		if i == len(v.Text) || utf16.DecodeRune(u, rune(v.Text[i])) == '�' {
			return "", false
		}
	}
	return string(utf16.Decode(v.Text)), true
}

func validRows(rows jsonx.WireValue) (jsonx.WireValue, error) {
	if rows.Kind != '[' {
		return jsonx.WireValue{}, errors.New("invalid catalog data")
	}
	rows.Elements = slices.DeleteFunc(rows.Elements, func(row jsonx.WireValue) bool {
		id, ok := idString(field(row, "id"))
		rest, prefixed := strings.CutPrefix(id, "claude-")
		if !ok || !prefixed || rest == "" || field(row, "display_name").Kind != '"' {
			return true
		}
		return strings.ContainsFunc(rest, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune("._-", r)
		})
	})
	return rows, nil
}

// numberInput converts only the timestamp to config.JSNumber's decoded JSON types.
// A JSON object that shadows toString has no callable primitive conversion.
func numberInput(v jsonx.WireValue) (any, bool) {
	switch v.Kind {
	case '"':
		return string(utf16.Decode(v.Text)), true
	case '0':
		return v.Number, true
	case 't', 'f':
		return v.Kind == 't', true
	case '{':
		return map[string]any{}, field(v, "toString").Kind == 0
	case '[':
		a := make([]any, len(v.Elements))
		for i, el := range v.Elements {
			var ok bool
			if a[i], ok = numberInput(el); !ok {
				return nil, false
			}
		}
		return a, true
	default:
		return nil, true
	}
}

// readRegular reads path, following symlinks, only when the opened handle is a regular file. The open
// never waits on a FIFO, so a reader that holds a lock across this read is never parked by one.
func readRegular(path string) ([]byte, error) {
	f, err := openForRead(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("anthropiccatalog: not a regular file")
	}
	return io.ReadAll(f)
}

// Read returns a normalized cache object, retaining unknown fields and their ordering.
// A missing, non-regular or malformed cache supplies empty data and a zero update time.
func Read(path string) Snapshot {
	empty := Snapshot{Object: jsontext.Value(`{"data":[],"updatedAt":0}`)}
	data, err := readRegular(path)
	if err != nil {
		return empty
	}
	obj, err := jsonx.DecodeWire(data)
	if err != nil {
		return empty
	}
	rows, err := validRows(field(obj, "data"))
	if err != nil {
		return empty
	}
	stamp, ok := numberInput(field(obj, "updatedAt"))
	if !ok {
		return empty
	}
	n := config.JSNumber(stamp)
	if math.IsNaN(n) || n == 0 {
		n = 0
	}
	obj.Members = append(obj.Members, member("data", rows), member("updatedAt", number(n)))
	encoded, err := jsonx.EncodeWire(obj)
	if err != nil {
		return empty
	}
	return Snapshot{Object: encoded, UpdatedAt: n}
}

// WriteSuccess stores valid upstream rows with the supplied refresh time and clears old errors.
func WriteSuccess(path string, rows jsontext.Value, at time.Time) error {
	v, err := jsonx.DecodeWire(rows)
	if err != nil {
		return errors.New("invalid catalog data")
	}
	v, err = validRows(v)
	if err != nil {
		return err
	}
	return write(path, jsonx.WireValue{Kind: '{', Members: []jsonx.Member{member("data", v), member("updatedAt", number(float64(at.UnixMilli()))), member("error", jsonx.WireValue{Kind: 'n'})}})
}

// WriteFailure records the failed attempt against the snapshot that triggered the refresh.
// The caller must supply only classified, payload-free text in message.
func WriteFailure(path string, snapshot Snapshot, message string, at time.Time) error {
	obj, err := jsonx.DecodeWire(snapshot.Object)
	if err != nil || obj.Kind != '{' {
		return errors.New("invalid catalog snapshot")
	}
	obj.Members = append(obj.Members, member("error", text(message)), member("attemptedAt", number(float64(at.UnixMilli()))))
	return write(path, obj)
}

func write(path string, obj jsonx.WireValue) error {
	data, err := jsonx.EncodeState(obj)
	if err != nil {
		return err
	}
	parent, name := filepath.Split(path)
	parent = cmp.Or(parent, ".")
	dir, err := atomicfile.OpenDir(parent)
	if errors.Is(err, fs.ErrNotExist) {
		// No reader can watch a directory that does not exist yet, so the write that creates it
		// also creates its staging subdirectory. In an existing directory the cache creates that
		// subdirectory when it starts, and a write never does.
		if err = atomicfile.CreateStagingDir(parent, 0o777); err == nil {
			dir, err = atomicfile.OpenDir(parent)
		}
	}
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	// The catalog is a cache the next refresh fetches again, so it is not synced. It is staged out
	// of the view of a directory watcher and renamed into place, so a reader woken by the change
	// finds the new content.
	return dir.WriteFile(name, data, 0o600, atomicfile.SkipSync(), atomicfile.PrivateStaging())
}

// WithCached puts the advertised cached rows before models, which lose on an ID collision.
func WithCached(models, cachedRows jsontext.Value, resolver policy.Resolver) jsontext.Value {
	return merge(models, cachedRows, "id", func(row jsonx.WireValue) (jsonx.WireValue, string, bool) {
		id, ok := idString(field(row, "id"))
		if !ok {
			return jsonx.WireValue{}, "", false
		}
		id = resolver.ClientID(id)
		row.Members = append(row.Members, member("id", text(id)))
		return row, id, true
	})
}

// WithPrevious prepends relabelled older server rows, replacing any models with those IDs.
// Server rows are the entries of Claude Code's additionalModelOptionsCache array.
func WithPrevious(models, serverRows jsontext.Value, currentPins map[string]string) jsontext.Value {
	return merge(models, serverRows, "value", func(row jsonx.WireValue) (jsonx.WireValue, string, bool) {
		id, ok := idString(field(row, "value"))
		if !ok {
			return jsonx.WireValue{}, "", false
		}
		v, ok := pins.ParseVersion(id)
		if !ok {
			return jsonx.WireValue{}, "", false
		}
		if sign, _ := pins.Compare(id, currentPins[v.Family]); sign != -1 {
			return jsonx.WireValue{}, "", false
		}
		return jsonx.WireValue{Kind: '{', Members: []jsonx.Member{member("id", text(id)), member("display_name", text(pins.DisplayName(id)+" (previous)"))}}, id, true
	})
}

// Wire decoding preserves escaped surrogates but replaces invalid UTF-8. When the
// document contains invalid bytes, check the winning identifier token before decoding.
func decodeRows(raw jsontext.Value, key string) jsonx.WireValue {
	if utf8.Valid(raw) {
		v, _ := jsonx.DecodeWire(raw)
		return v
	}
	opts := json.JoinOptions(jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	var entries []jsontext.Value
	if err := json.Unmarshal(raw, &entries, opts); err != nil {
		return jsonx.WireValue{}
	}
	rows := jsonx.WireValue{Kind: '[', Elements: make([]jsonx.WireValue, 0, len(entries))}
	for _, entry := range entries {
		if entry.Kind() == '{' && !utf8.Valid(entry) {
			var fields struct {
				ID    jsontext.Value `json:"id"`
				Value jsontext.Value `json:"value"`
			}
			if err := json.Unmarshal(entry, &fields, opts); err != nil {
				continue
			}
			id := fields.ID
			if key == "value" {
				id = fields.Value
			}
			if id.Kind() == '"' && !utf8.Valid(id) {
				continue
			}
		}
		if row, err := jsonx.DecodeWire(entry); err == nil {
			rows.Elements = append(rows.Elements, row)
		}
	}
	return rows
}

func merge(models, preferred jsontext.Value, preferredKey string, advertise func(jsonx.WireValue) (jsonx.WireValue, string, bool)) jsontext.Value {
	first := decodeRows(preferred, preferredKey)
	rest := decodeRows(models, "id")
	result := jsonx.WireValue{Kind: '[', Elements: make([]jsonx.WireValue, 0, len(first.Elements)+len(rest.Elements))}
	ids := make(map[string]bool, len(first.Elements))
	for _, row := range first.Elements {
		if r, id, ok := advertise(row); ok {
			result.Elements = append(result.Elements, r)
			ids[id] = true
		}
	}
	for _, row := range rest.Elements {
		v := field(row, "id")
		if v.Kind == '"' {
			if id, ok := idString(v); !ok || ids[id] {
				continue
			}
		}
		result.Elements = append(result.Elements, row)
	}
	encoded, _ := jsonx.EncodeWire(result)
	return encoded
}
