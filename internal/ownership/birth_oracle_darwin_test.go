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

package ownership_test

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// kernelBirth reads the start of pid from the kernel's process record without the package under test, truncated
// to the second as the legacy start text is.
func kernelBirth(t *testing.T, pid int) time.Time {
	t.Helper()
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		t.Fatalf("kernel record of %d: %v", pid, err)
	}
	return time.Unix(int64(p.Proc.P_starttime.Sec), 0)
}
