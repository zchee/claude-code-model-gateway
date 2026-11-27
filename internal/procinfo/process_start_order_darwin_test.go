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

//go:build process

package procinfo

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// kernelStart returns the kernel's recorded start of pid in microseconds,
// read here without the package so that it can check it.
func kernelStart(pid int) (int64, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if int(info.Proc.P_pid) != pid {
		return 0, fmt.Errorf("kern.proc.pid %d returned pid %d", pid, info.Proc.P_pid)
	}
	start := info.Proc.P_starttime
	return start.Sec*1_000_000 + int64(start.Usec), nil
}
