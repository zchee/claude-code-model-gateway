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

package ownership

import "sync"

// Handle names one process for its lifetime where the system can, for one terminate request.
//
// On Linux it is a pidfd, which cannot reach a process that reused the number. Where pidfd_open answers ENOSYS, and on
// macOS and the BSDs, it is the number itself: a number reused between the caller's last sample and the signal is
// signalled.
//
// A nonzero Handle returned by OpenHandle must not be copied. It is used through the pointer OpenHandle returns, and
// every copy of that pointer shares one close state; the mutex it holds makes go vet's copylocks check report a copy
// of the value. The zero Handle is closed.
type Handle struct {
	// mu is held across the signal and across the release, so that a release never overlaps a signal in flight.
	mu sync.Mutex
	// process is the reference the signal goes through; nil for the zero Handle and once the Handle is closed.
	process processHandle
}

// OpenHandle opens a handle on the positive process number pid.
//
// A pid that is not positive is refused with an error matching fs.ErrInvalid on every platform, before anything is
// opened. On a platform without Unix signals a positive pid is refused with an error matching errors.ErrUnsupported.
// Where the handle is a pidfd and pidfd_open reports that the process no longer exists (ESRCH), the error matches
// os.ErrProcessDone. A numeric handle is opened without looking at the process, so a successful open alone establishes
// no liveness. No handle is returned beside an error.
func OpenHandle(pid int) (*Handle, error) {
	panic("not implemented")
}

// Terminate sends SIGTERM through the handle.
//
// A process that is gone is reported with an error matching os.ErrProcessDone. A zero or closed handle sends nothing
// and returns an error matching os.ErrClosed. No other signal is offered.
func (h *Handle) Terminate() error {
	panic("not implemented")
}

// Close releases the handle.
//
// A Close that races a Terminate in flight waits for it, so a released descriptor is never signalled through. A second
// Close returns nil and releases nothing twice; Close of the zero Handle returns nil.
func (h *Handle) Close() error {
	panic("not implemented")
}
