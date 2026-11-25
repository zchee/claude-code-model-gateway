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

//go:build linux && process

package procinfo

import "golang.org/x/sys/unix"

// Linux uses procfs directly. Only the process tests run the command supervisor.
func leaderExited(pid int) (bool, error) {
	var status unix.Siginfo
	err := unix.Waitid(unix.P_PID, pid, &status, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
	return status.Signo != 0, err
}

func signalCommandGroup(group int, signal unix.Signal) error { return unix.Kill(group, signal) }
