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

// handleTTerminateTrace is the guard's trace of an open and one terminate of the held child: macOS has only the
// number, so the open makes no call and the terminate is a kill.
var handleTTerminateTrace = map[string][]string{"number": {"kill(held, terminated)"}}

// handleTOpenTrace is the guard's trace of an open alone: none.
var handleTOpenTrace = map[string][]string{"number": nil}

// handleTDelivery is the one delivery a terminate must make: a kill of the held number.
var handleTDelivery = map[string]string{"number": "kill(held, terminated)"}

// handleTDeliveries are the successful deliveries of the only backend the guard can forward on this platform.
var handleTDeliveries = []string{"kill(held, terminated)"}
