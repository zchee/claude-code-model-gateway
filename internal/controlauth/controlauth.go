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
	"errors"
	"io"
	"net/http"
)

// ReadToken returns the validated token, or an empty string if the file is absent.
func ReadToken(file string) (string, error) { return "", errors.New("ReadToken not implemented") }

// EnsureToken reads the token or exclusively creates a private token from entropy.
func EnsureToken(file string, entropy io.Reader) (string, error) {
	return "", errors.New("EnsureToken not implemented")
}

// RequestHeaders returns the control header, or empty headers for a legacy installation.
func RequestHeaders(file string) (http.Header, error) {
	return nil, errors.New("RequestHeaders not implemented")
}

// LocalRequestAllowed reports whether the request has an allowed Host and no Origin.
func LocalRequestAllowed(req *http.Request, hosts []string) bool { return false }

// Authenticated reports whether a lifecycle request is authorized for the listener.
func Authenticated(req *http.Request, token string, ports []int, compatibilityListener bool) bool {
	return false
}
