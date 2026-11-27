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

//go:build process && darwin

package serveshim_test

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ephemeralPortRange returns the system's ephemeral port range, from which macOS gives port numbers to sockets that
// bind port zero or connect without binding. The two sysctls may be in either order: XNU accepts a first port above the
// last and then allocates downward, so the range runs from the smaller to the larger of the two.
func ephemeralPortRange() (low, high int, err error) {
	first, err := unix.SysctlUint32("net.inet.ip.portrange.first")
	if err != nil {
		return 0, 0, fmt.Errorf("sysctl net.inet.ip.portrange.first: %w", err)
	}
	last, err := unix.SysctlUint32("net.inet.ip.portrange.last")
	if err != nil {
		return 0, 0, fmt.Errorf("sysctl net.inet.ip.portrange.last: %w", err)
	}
	return int(min(first, last)), int(max(first, last)), nil
}
