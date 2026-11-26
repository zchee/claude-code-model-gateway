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

//go:build unix

package telemetry

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// errorCode returns the system error name of a failure, as the source prints error.code,
// so a diagnostic never repeats the path or any request data.
func errorCode(err error) string {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		if name := unix.ErrnoName(errno); name != "" {
			return name
		}
	}
	return "unknown error"
}
