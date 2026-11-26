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

package modelcache

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"syscall"
)

// maxFileBytes bounds a read of models.json, the Claude global config or the discovery cache.
const maxFileBytes = 16 << 20

var (
	errTooLarge   = errors.New("modelcache: content exceeds its size bound")
	errNotRegular = errors.New("modelcache: not a regular file")
)

// readLimited reads r to its end, failing once it would exceed limit bytes.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	return data, nil
}

// readSmallFile reads a regular file, following symlinks, up to maxFileBytes. The opened file is
// checked again so a replacement after the first check is never read as a regular file.
func readSmallFile(path string) ([]byte, error) {
	if path == "" {
		return nil, fs.ErrNotExist
	}
	if info, err := os.Stat(path); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, errNotRegular
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, errNotRegular
	}
	return readLimited(f, maxFileBytes)
}

// lastMember returns the raw bytes of the last top-level member named name in the JSON object doc,
// as JSON.parse keeps the last duplicate. Raw bytes keep invalid UTF-8 and escaped surrogates for
// the strict readers downstream. A document JSON.parse would reject has no members.
func lastMember(doc []byte, name string) (jsontext.Value, bool) {
	dec := jsontext.NewDecoder(bytes.NewReader(doc), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return nil, false
	}
	var found jsontext.Value
	for dec.PeekKind() != '}' {
		key, err := dec.ReadToken()
		if err != nil {
			return nil, false
		}
		// A token is valid only until the next decoder call.
		matches := key.String() == name
		value, err := dec.ReadValue()
		if err != nil {
			return nil, false
		}
		if matches {
			found = slices.Clone(value)
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, false
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return found, found != nil
}

// serverRows returns the raw additionalModelOptionsCache array of the Claude global config, or an
// empty array (lib/previous-picker-rows.js:19-24).
func (c *Cache) serverRows() jsontext.Value {
	data, err := readSmallFile(c.globalConfig)
	if err != nil {
		return emptyRows
	}
	rows, ok := lastMember(data, "additionalModelOptionsCache")
	if !ok || rows.Kind() != '[' {
		return emptyRows
	}
	return rows
}

func (c *Cache) currentPins() map[string]string {
	if c.opts.CurrentPins == nil {
		return nil
	}
	return c.opts.CurrentPins()
}

// errorClass names an error for a log line without its text, which can carry paths or payloads.
func errorClass(err error) string {
	var errno syscall.Errno
	switch {
	case errors.As(err, &errno):
		return errno.Error()
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, errTooLarge):
		return "too large"
	case errors.Is(err, errNotRegular):
		return "not a regular file"
	}
	return "failed"
}
