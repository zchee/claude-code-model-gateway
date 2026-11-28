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

//go:build (process && (linux || darwin)) || !unix

package ownership_test

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
)

// handleTOpened is what one OpenHandle call returned.
type handleTOpened struct {
	handle *ownership.Handle
	err    error
}

// handleTOpenRaw opens a handle on pid and fails the row instead of the binary when OpenHandle panics. It registers
// no cleanup: a live row closes the handle itself, at its place in the row's ending sequence, before the child is
// reaped.
func handleTOpenRaw(t testing.TB, pid int) (*ownership.Handle, error) {
	t.Helper()
	r := noPanic(t, "OpenHandle", func() handleTOpened {
		h, err := ownership.OpenHandle(pid)
		return handleTOpened{h, err}
	})
	return r.handle, r.err
}

// handleTMatch says which of the candidate sentinels err matches, so that a row compares one literal per call.
func handleTMatch(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, fs.ErrInvalid):
		return "fs.ErrInvalid"
	case errors.Is(err, errors.ErrUnsupported):
		return "errors.ErrUnsupported"
	case errors.Is(err, os.ErrProcessDone):
		return "os.ErrProcessDone"
	case errors.Is(err, os.ErrClosed):
		return "os.ErrClosed"
	case errors.Is(err, os.ErrPermission):
		return "os.ErrPermission"
	}
	return "unexpected: " + err.Error()
}
