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

//go:build unix && !darwin

package workerlink

// endedPeerEvidence gathers nothing: only XNU's EINVAL from getpeername is read as a local stream socket whose peer
// may have ended (see peerstate_darwin.go). On the other systems the import refuses that EINVAL as it came.
func endedPeerEvidence(int, error) (peerEvidence, bool) { return peerEvidence{}, false }
