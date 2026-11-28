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

package ownership

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// kill is the kill system call that a numeric handle makes; the package's tests replace it to record its calls.
var kill = unix.Kill

// numericProcess signals a process by its number. Nothing ties the number to the process that was sampled: if that
// process exits and the number is reused between the last sample and the signal, the signal reaches the new process.
// The two samples taken immediately before every signal only narrow that window.
type numericProcess int

func (p numericProcess) signal(sig syscall.Signal) error {
	err := kill(int(p), sig)
	if errors.Is(err, unix.ESRCH) {
		return errProcessGone
	}
	return err
}

func (numericProcess) close() error { return nil }
