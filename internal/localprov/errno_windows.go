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

package localprov

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// errnoNames are the Winsock errors a probe reports by the POSIX name the source's fetch helper uses.
var errnoNames = map[syscall.Errno]string{
	windows.WSAECONNREFUSED: "ECONNREFUSED",
	windows.WSAECONNRESET:   "ECONNRESET",
	windows.WSAECONNABORTED: "ECONNABORTED",
	windows.WSAEHOSTUNREACH: "EHOSTUNREACH",
	windows.WSAENETUNREACH:  "ENETUNREACH",
	windows.WSAETIMEDOUT:    "ETIMEDOUT",
}
