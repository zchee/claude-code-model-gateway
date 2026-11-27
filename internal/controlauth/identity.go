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

package controlauth

import "errors"

// IdentityNonceHeader carries the client's fresh supervisor identity challenge.
const IdentityNonceHeader = "X-Model-Gateway-Identity-Nonce"

// IdentityNonceBytes is the number of independent random bytes in a supervisor identity challenge.
const IdentityNonceBytes = 32

// ErrInvalidIdentityNonce denotes a challenge that is not canonical lowercase hex of the required length.
var ErrInvalidIdentityNonce = errors.New("invalid supervisor identity nonce")

// SignSupervisorIdentity returns the lowercase hexadecimal MAC for a validated challenge and startup generation.
// A valid proof shows only that the listener holds this state root's control token; it does not
// tell the supervisor from another process that holds the same token.
func SignSupervisorIdentity(token, nonce, generation string) (string, error) {
	panic("not implemented")
}

// VerifySupervisorIdentity reports whether a canonical proof authenticates this challenge and startup generation.
// A true result shows only that the listener holds this state root's control token; it does not
// tell the supervisor from another process that holds the same token.
func VerifySupervisorIdentity(token, nonce, generation, proof string) bool {
	panic("not implemented")
}
