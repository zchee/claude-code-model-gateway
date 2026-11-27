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

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"unicode/utf8"
)

// IdentityNonceHeader carries the client's fresh supervisor identity challenge.
const IdentityNonceHeader = "X-Model-Gateway-Identity-Nonce"

// IdentityNonceBytes is the number of independent random bytes in a supervisor identity challenge.
const IdentityNonceBytes = 32

// supervisorIdentityDomain separates this role, endpoint and wire format from other control messages.
const supervisorIdentityDomain = "model-gateway/supervisor/GET /internal/identity/v1"

// ErrInvalidIdentityNonce denotes a challenge that is not canonical lowercase hex of the required length.
var ErrInvalidIdentityNonce = errors.New("invalid supervisor identity nonce")

// These errors name only the kind of input that was refused: an error never repeats the token, the challenge or
// the generation, in any spelling.
var (
	errInvalidIdentityKey        = errors.New("invalid local control token for the supervisor identity proof")
	errInvalidIdentityGeneration = errors.New("invalid supervisor startup generation")
)

// SignSupervisorIdentity returns the lowercase hexadecimal MAC for a validated challenge and startup generation.
// A valid proof shows only that the listener holds this state root's control token; it does not
// tell the supervisor from another process that holds the same token.
func SignSupervisorIdentity(token, nonce, generation string) (string, error) {
	sum, err := supervisorIdentityMAC(token, nonce, generation)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum), nil
}

// VerifySupervisorIdentity reports whether a canonical proof authenticates this challenge and startup generation.
// A true result shows only that the listener holds this state root's control token; it does not
// tell the supervisor from another process that holds the same token.
func VerifySupervisorIdentity(token, nonce, generation, proof string) bool {
	got, ok := canonicalHex(proof, sha256.Size)
	if !ok {
		return false
	}
	want, err := supervisorIdentityMAC(token, nonce, generation)
	// hmac.Equal compares in constant time, so the time taken says nothing about how much of a forged proof matched.
	return err == nil && hmac.Equal(got, want)
}

// supervisorIdentityMAC validates the inputs and returns the HMAC-SHA-256, keyed with the decoded token bytes, of
// the domain, one NUL byte, the decoded challenge bytes and the generation's bytes exactly as given. The domain and
// the challenge have fixed lengths, so the generation is the rest of the message whatever bytes it holds.
func supervisorIdentityMAC(token, nonce, generation string) ([]byte, error) {
	challenge, ok := canonicalHex(nonce, IdentityNonceBytes)
	if !ok {
		return nil, ErrInvalidIdentityNonce
	}
	key, ok := canonicalHex(token, tokenBytes)
	if !ok {
		return nil, errInvalidIdentityKey
	}
	if generation == "" || !utf8.ValidString(generation) {
		return nil, errInvalidIdentityGeneration
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(supervisorIdentityDomain))
	mac.Write([]byte{0})
	mac.Write(challenge)
	mac.Write([]byte(generation))
	return mac.Sum(nil), nil
}

// canonicalHex decodes s only when it is exactly size bytes spelled as lowercase hexadecimal. Every other spelling
// of the same bytes (uppercase, a prefix, white space) is refused before decoding, so it can never verify.
func canonicalHex(s string, size int) ([]byte, bool) {
	if !lowerHex(s, size) {
		return nil, false
	}
	decoded, err := hex.DecodeString(s)
	return decoded, err == nil
}
