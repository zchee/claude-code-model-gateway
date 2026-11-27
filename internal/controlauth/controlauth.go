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

// Package controlauth authorizes local gateway lifecycle requests.
package controlauth

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

const (
	controlHeader = "X-Model-Gateway-Control-Token"
	tokenBytes    = 32
)

func validToken(token string) bool { return lowerHex(token, tokenBytes) }

// lowerHex reports whether s spells exactly size bytes in lowercase hexadecimal. Token loading, the identity
// challenge and the identity proof all use it, so the spellings they accept cannot drift apart.
func lowerHex(s string, size int) bool {
	if len(s) != hex.EncodedLen(size) {
		return false
	}
	for i := range len(s) {
		if b := s[i]; (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

// ReadToken returns the validated token, or an empty string if the file is absent.
func ReadToken(file string) (string, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Match String.trim on token files shared with the JavaScript launcher.
	token := strings.TrimFunc(string(data), func(r rune) bool {
		return unicode.Is(unicode.Zs, r) || strings.ContainsRune("\t\n\v\f\r", r) || r == 0x2028 || r == 0x2029 || r == 0xFEFF
	})
	if !validToken(token) {
		return "", errors.New("model-gateway: invalid local control token; refusing lifecycle requests")
	}
	return token, nil
}

// EnsureToken reads the token or exclusively creates a private token from entropy.
// The file path must be clean and absolute so parent creation resolves the same path.
func EnsureToken(file string, entropy io.Reader) (string, error) {
	if !filepath.IsAbs(file) || filepath.Clean(file) != file {
		return "", errors.New("control token path must be clean and absolute")
	}
	token, err := ReadToken(file)
	if err != nil || token != "" {
		return token, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		return "", err
	}
	var random [tokenBytes]byte
	if entropy == nil {
		return "", errors.New("missing control token entropy source")
	}
	if _, err := io.ReadFull(entropy, random[:]); err != nil {
		return "", errors.New("could not read control token entropy")
	}
	data := make([]byte, hex.EncodedLen(len(random))+1)
	hex.Encode(data, random[:])
	data[len(data)-1] = '\n'
	if err := atomicfile.CreateExclusive(file, data, 0o600); err != nil {
		if _, cleanupFailed := errors.AsType[*atomicfile.StageCleanupError](err); cleanupFailed || !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return ReadToken(file)
}

// RequestHeaders returns the control header, or empty headers for a legacy installation.
func RequestHeaders(file string) (http.Header, error) {
	token, err := ReadToken(file)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	if token != "" {
		headers.Set(controlHeader, token)
	}
	return headers, nil
}

// LocalRequestAllowed reports whether the request has an allowed Host and no Origin.
// The caller must put the Host header as received in req.Host, not the authority
// of an absolute-form request target. req.Header must retain every Origin
// occurrence, including an Origin with an empty value.
func LocalRequestAllowed(req *http.Request, hosts []string) bool {
	return req.Host != "" && slices.Contains(hosts, req.Host) && !hasOrigin(req.Header)
}

func hasOrigin(headers http.Header) bool {
	for key := range headers {
		if strings.EqualFold(key, "Origin") {
			return true
		}
	}
	return false
}

// Authenticated reports whether a lifecycle request is authorized for the listener.
// In addition to the header requirements of [LocalRequestAllowed], req.Header
// must come from an HTTP parser: field names must be ASCII and repeated values
// must be in received order so the first Content-Type can be selected. Retain
// its exact value, without media type normalization.
// ports must contain the listener's actual local port, not a port provided by
// the request. The caller must load a valid, non-empty token before serving.
func Authenticated(req *http.Request, token string, ports []int, compatibilityListener bool) bool {
	if compatibilityListener || req.Method != http.MethodPost || !validToken(token) || hasOrigin(req.Header) {
		return false
	}
	if !slices.ContainsFunc(ports, func(port int) bool {
		return port > 0 && req.Host == "127.0.0.1:"+strconv.Itoa(port)
	}) {
		return false
	}

	const contentType = "Content-Type"
	var typeKey string
	var supplied []string
	for key, values := range req.Header {
		if len(values) == 0 {
			continue
		}
		switch {
		case strings.EqualFold(key, contentType):
			// A hand-built Header with separate spellings has no wire order.
			if typeKey == "" || key == contentType || typeKey != contentType && key < typeKey {
				typeKey = key
			}
		case strings.EqualFold(key, controlHeader):
			if supplied != nil || len(values) != 1 {
				return false
			}
			supplied = values
		}
	}
	return typeKey != "" && req.Header[typeKey][0] == "application/json" && supplied != nil && validToken(supplied[0]) &&
		subtle.ConstantTimeCompare([]byte(supplied[0]), []byte(token)) == 1
}
