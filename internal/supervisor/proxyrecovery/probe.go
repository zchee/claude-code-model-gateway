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

package proxyrecovery

import "context"

// ModelsProbe returns a probe that asks the proxy on the loopback port for its model list on a fresh connection and
// reports whether it answered 200 with a complete body within two seconds. The two seconds bound the whole call: the
// head, every redirect and the body. Only 301, 302, 303, 307 and 308 are followed, at most ten requests are sent,
// and a location with user information is refused. A body over 1 MiB of encoded body bytes, as sent and before any
// decoding, is no answer.
func ModelsProbe(port uint16) func(context.Context) bool { panic("not implemented") }
