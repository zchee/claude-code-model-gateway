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
	"net"
)

// WithBeforeRemove returns s with hook set to run inside its RemoveIf between the match and the unlink.
func WithBeforeRemove(s Store, hook func()) Store {
	s.beforeRemove = hook
	return s
}

// SetStopSelfDial makes StopSelf open its connections through dial and returns a function that restores the
// default. It is a package global: only serial tests that join everything they start may use it.
func SetStopSelfDial(dial func(ctx context.Context, network, address string) (net.Conn, error)) (restore func()) {
	previous := stopSelfDial
	stopSelfDial = dial
	return func() { stopSelfDial = previous }
}
