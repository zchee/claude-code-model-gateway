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

import (
	"context"
	"errors"
	"net"
)

// stopSelfDial, when the package's own tests set it, opens every connection StopSelf makes, the stop connection and
// the health connection alike, instead of a loopback TCP dial. Nothing else sets it.
var stopSelfDial func(ctx context.Context, network, address string) (net.Conn, error)

// ErrUnproven means the listener did not prove that it holds this state root's control token. Only the identity
// challenge went over the control connection to it, and no credential was sent to it at all; the diagnostic health
// request StopSelf may send afterwards carries none either. Its error text is the refusal to print.
var ErrUnproven = errors.New("the listener could not be identified as this model-gateway install")

// ErrStopUnacknowledged means a proven supervisor did not acknowledge the stop. The stop is not sent again; the
// caller confirms the exit separately.
var ErrStopUnacknowledged = errors.New("the supervisor did not acknowledge the stop")

// StopSelf asks the supervisor on the loopback port to stop itself. On one connection it sends a fresh identity
// challenge, verifies the proof against token, and only then sends the authenticated stop on that same connection.
// A nil error means the stop was acknowledged, not that the supervisor has exited. The whole exchange is bounded by
// the context, or by five seconds when the context has no deadline.
//
// When the listener is not proven, the error matches ErrUnproven and its text is the refusal to print. Only then,
// StopSelf sends one diagnostic GET /healthz on a separate connection; it carries neither the token nor a
// challenge, and healthPID is the supervisorPid that health document reports, or zero when none was reported. That
// number is a diagnostic only and never an authority to signal.
func StopSelf(ctx context.Context, port uint16, token string) (healthPID int, err error) {
	panic("not implemented")
}
