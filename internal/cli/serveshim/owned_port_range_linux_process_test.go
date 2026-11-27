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

//go:build process && linux

package serveshim_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ephemeralPortRange returns the system's ephemeral port range, from which Linux gives port numbers to sockets that
// bind port zero or connect without binding.
func ephemeralPortRange() (low, high int, err error) {
	const path = "/proc/sys/net/ipv4/ip_local_port_range"
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("%s holds %q, not two port numbers", path, data)
	}
	low, lowErr := strconv.Atoi(fields[0])
	high, highErr := strconv.Atoi(fields[1])
	if lowErr != nil || highErr != nil {
		return 0, 0, fmt.Errorf("%s holds %q, not two port numbers", path, data)
	}
	// No reordering: the kernel refuses to set a range whose first port is above its second, so low <= high here.
	return low, high, nil
}
