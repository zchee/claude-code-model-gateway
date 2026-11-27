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

package controlauth_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
)

// identityDomain is written out here so that no expected proof is derived from the code under test.
const identityDomain = "model-gateway/supervisor/GET /internal/identity/v1"

// Fixture keys and challenges, all canonical lowercase hex of 32 bytes.
const (
	sequentialKey   = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	sequentialNonce = "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
)

// mac returns the lowercase hex HMAC-SHA-256 of the concatenated parts.
func mac(key []byte, parts ...[]byte) string {
	h := hmac.New(sha256.New, key)
	for _, part := range parts {
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// decodedOrRaw decodes hex when it can, so that a noncanonical spelling of valid bytes yields the proof of those bytes.
func decodedOrRaw(s string) []byte {
	if b, err := hex.DecodeString(s); err == nil {
		return b
	}
	return []byte(s)
}

// referenceProof follows the published layout: key = decoded token; message = domain, NUL, decoded nonce, generation bytes.
func referenceProof(token, nonce, generation string) string {
	return mac(decodedOrRaw(token), []byte(identityDomain), []byte{0}, decodedOrRaw(nonce), []byte(generation))
}

// sign calls the signer and fails the test, not the binary, if it panics.
func sign(t testing.TB, token, nonce, generation string) (proof string, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("SignSupervisorIdentity panicked: %v", p)
		}
	}()
	return controlauth.SignSupervisorIdentity(token, nonce, generation)
}

// verify calls the verifier and fails the test, not the binary, if it panics.
func verify(t testing.TB, token, nonce, generation, proof string) (ok bool) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("VerifySupervisorIdentity panicked: %v", p)
		}
	}()
	return controlauth.VerifySupervisorIdentity(token, nonce, generation, proof)
}

// leaks reports which of the secret or caller-supplied inputs an error message repeats.
// Inputs shorter than four bytes are skipped: they can occur in any English message.
func leaks(err error, inputs ...string) []string {
	var found []string
	for _, input := range inputs {
		if len(input) >= 4 && strings.Contains(err.Error(), input) {
			found = append(found, input)
		}
	}
	return found
}

func TestIdentityDeclarations(t *testing.T) {
	if diff := gocmp.Diff("X-Model-Gateway-Identity-Nonce", controlauth.IdentityNonceHeader); diff != "" {
		t.Errorf("nonce header (-want +got):\n%s", diff)
	}
	// The server reads the header through net/http, which canonicalizes field names.
	if diff := gocmp.Diff(http.CanonicalHeaderKey(controlauth.IdentityNonceHeader), controlauth.IdentityNonceHeader); diff != "" {
		t.Errorf("nonce header is not in canonical form (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(32, controlauth.IdentityNonceBytes); diff != "" {
		t.Errorf("nonce size (-want +got):\n%s", diff)
	}
	if controlauth.ErrInvalidIdentityNonce == nil {
		t.Fatal("missing invalid-nonce sentinel")
	}
}

func TestSupervisorIdentityKnownAnswers(t *testing.T) {
	// Each pinned value was computed outside Go (openssl dgst -sha256 -mac HMAC, and Python's hmac module)
	// from the same layout; the in-test reference must agree with it before the signer is compared.
	tests := map[string]struct {
		token, nonce, generation, pinned string
	}{
		"success: ascii generation": {
			token: sequentialKey, nonce: sequentialNonce, generation: "g",
			pinned: "40f744be50b34693471c847603cce7210ca5e77a093da1816ed54e9651af8960",
		},
		"success: multibyte utf-8 generation": {
			token: strings.Repeat("ab", 32), nonce: strings.Repeat("cd", 32), generation: "世代-1",
			pinned: "6269bfea11a28f9d7b2c0f79c127a6d28516d9e83344138b91b3f30ab40ea426",
		},
		"success: all-ones key and all-zero challenge": {
			token: strings.Repeat("ff", 32), nonce: strings.Repeat("00", 32), generation: "2026-11-27T00:00:00.000Z/4f1c",
			pinned: "156011eb711570bd78109f45bbc3ed8db2bb1206d3bb035873c0bd4cb5d58229",
		},
		"success: long generation": {
			token: sequentialKey, nonce: sequentialNonce, generation: strings.Repeat("generation-", 24),
		},
		"success: generation with separators and spaces": {
			token: sequentialKey, nonce: sequentialNonce, generation: " a/b:c d ",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			want := referenceProof(tt.token, tt.nonce, tt.generation)
			if tt.pinned != "" {
				if diff := gocmp.Diff(tt.pinned, want); diff != "" {
					t.Fatalf("in-test reference disagrees with the externally computed vector (-want +got):\n%s", diff)
				}
			}
			got, err := sign(t, tt.token, tt.nonce, tt.generation)
			if err != nil {
				t.Fatalf("SignSupervisorIdentity: %v", err)
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Fatalf("proof (-want +got):\n%s", diff)
			}
			again, err := sign(t, tt.token, tt.nonce, tt.generation)
			if err != nil || again != got {
				t.Fatalf("second signature = %q, %v; want the same proof %q", again, err, got)
			}
			if !verify(t, tt.token, tt.nonce, tt.generation, got) {
				t.Fatal("verifier rejected the signer's own proof")
			}
			if !verify(t, tt.token, tt.nonce, tt.generation, want) {
				t.Fatal("verifier rejected the independently computed proof")
			}
		})
	}
}

func TestSupervisorIdentityBinding(t *testing.T) {
	const generation = "g-1"
	proof, err := sign(t, sequentialKey, sequentialNonce, generation)
	if err != nil {
		t.Fatalf("SignSupervisorIdentity: %v", err)
	}
	otherToken := sequentialKey[:63] + "e"
	otherNonce := "3" + sequentialNonce[1:]
	tests := map[string]struct {
		token, nonce, generation string
	}{
		"error: different token":                       {otherToken, sequentialNonce, generation},
		"error: different nonce":                       {sequentialKey, otherNonce, generation},
		"error: different generation":                  {sequentialKey, sequentialNonce, "g-2"},
		"error: generation prefix":                     {sequentialKey, sequentialNonce, "g-"},
		"error: generation with trailing newline":      {sequentialKey, sequentialNonce, generation + "\n"},
		"error: generation with trailing NUL":          {sequentialKey, sequentialNonce, generation + "\x00"},
		"error: generation with different case":        {sequentialKey, sequentialNonce, "G-1"},
		"error: token and nonce exchanged":             {sequentialNonce, sequentialKey, generation},
		"error: nonce moved into the generation":       {sequentialKey, sequentialNonce[:62] + "00", sequentialNonce[62:] + generation},
		"error: different token, nonce and generation": {otherToken, otherNonce, "g-2"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if verify(t, tt.token, tt.nonce, tt.generation, proof) {
				t.Fatal("a proof for one token, challenge and generation verified for another")
			}
			// A control character in the generation is valid UTF-8, but whether the signer accepts it is left open.
			if strings.ContainsAny(tt.generation, "\x00\n") {
				return
			}
			// The changed inputs are still valid, so they must sign to a different proof.
			other, err := sign(t, tt.token, tt.nonce, tt.generation)
			if err != nil {
				t.Fatalf("SignSupervisorIdentity(changed inputs): %v", err)
			}
			if other == proof {
				t.Fatal("changed inputs produced the same proof")
			}
		})
	}
}

func TestSupervisorIdentityDomainSeparation(t *testing.T) {
	const generation = "g-1"
	key, nonce := decodedOrRaw(sequentialKey), decodedOrRaw(sequentialNonce)
	proof, err := sign(t, sequentialKey, sequentialNonce, generation)
	if err != nil {
		t.Fatalf("SignSupervisorIdentity: %v", err)
	}
	// Each forgery is a MAC under this root's key that some other message layout or protocol could produce.
	tests := map[string]struct{ forged string }{
		"error: worker role":                 {mac(key, []byte("model-gateway/worker/GET /internal/identity/v1"), []byte{0}, nonce, []byte(generation))},
		"error: stop endpoint":               {mac(key, []byte("model-gateway/supervisor/POST /internal/stop/v1"), []byte{0}, nonce, []byte(generation))},
		"error: drain endpoint":              {mac(key, []byte("model-gateway/supervisor/POST /drain/v1"), []byte{0}, nonce, []byte(generation))},
		"error: other version":               {mac(key, []byte("model-gateway/supervisor/GET /internal/identity/v2"), []byte{0}, nonce, []byte(generation))},
		"error: no domain":                   {mac(key, nonce, []byte(generation))},
		"error: domain without separator":    {mac(key, []byte(identityDomain), nonce, []byte(generation))},
		"error: newline separator":           {mac(key, []byte(identityDomain), []byte{'\n'}, nonce, []byte(generation))},
		"error: trailing separator":          {mac(key, []byte(identityDomain), []byte{0}, nonce, []byte(generation), []byte{0})},
		"error: trailing newline":            {mac(key, []byte(identityDomain), []byte{0}, nonce, []byte(generation), []byte{'\n'})},
		"error: generation before nonce":     {mac(key, []byte(identityDomain), []byte{0}, []byte(generation), nonce)},
		"error: nonce as hex text":           {mac(key, []byte(identityDomain), []byte{0}, []byte(sequentialNonce), []byte(generation))},
		"error: token text as key":           {mac([]byte(sequentialKey), []byte(identityDomain), []byte{0}, nonce, []byte(generation))},
		"error: plain hash without key":      {hex.EncodeToString(sha256Sum([]byte(identityDomain), []byte{0}, nonce, []byte(generation)))},
		"error: proof is the token":          {sequentialKey},
		"error: proof is the nonce":          {sequentialNonce},
		"error: hash of the token":           {hex.EncodeToString(sha256Sum([]byte(sequentialKey)))},
		"error: mac of the generation alone": {mac(key, []byte(generation))},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.forged == proof {
				t.Fatal("the signer's proof equals a MAC of a different message layout or domain")
			}
			if verify(t, sequentialKey, sequentialNonce, generation, tt.forged) {
				t.Fatal("verifier accepted a MAC that was not made over this endpoint's domain-separated message")
			}
		})
	}
}

func sha256Sum(parts ...[]byte) []byte {
	h := sha256.New()
	for _, part := range parts {
		h.Write(part)
	}
	return h.Sum(nil)
}

func TestIdentityNonceForms(t *testing.T) {
	const generation = "g"
	tests := map[string]struct{ nonce string }{
		"error: absent":                   {""},
		"error: one byte short":           {sequentialNonce[:62]},
		"error: odd length short":         {sequentialNonce[:63]},
		"error: odd length long":          {sequentialNonce + "a"},
		"error: one byte long":            {sequentialNonce + "40"},
		"error: twice the size":           {sequentialNonce + sequentialNonce},
		"error: uppercase":                {strings.ToUpper(sequentialNonce)},
		"error: one uppercase digit":      {sequentialNonce[:63] + "F"},
		"error: non-hex letter":           {sequentialNonce[:63] + "g"},
		"error: leading space":            {" " + sequentialNonce[1:]},
		"error: trailing newline":         {sequentialNonce[:63] + "\n"},
		"error: padded with whitespace":   {" " + sequentialNonce + " "},
		"error: hex prefix":               {"0x" + sequentialNonce[2:]},
		"error: base64 of 32 bytes":       {base64.StdEncoding.EncodeToString(decodedOrRaw(sequentialNonce))},
		"error: 64 bytes with multibyte":  {sequentialNonce[:62] + "é"},
		"error: embedded NUL":             {sequentialNonce[:63] + "\x00"},
		"error: raw 32 bytes, not hex":    {string(decodedOrRaw(sequentialNonce))},
		"error: url-safe base64 32 bytes": {base64.RawURLEncoding.EncodeToString(decodedOrRaw(sequentialNonce))},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			proof, err := sign(t, sequentialKey, tt.nonce, generation)
			if !errors.Is(err, controlauth.ErrInvalidIdentityNonce) {
				t.Fatalf("SignSupervisorIdentity error = %v, want ErrInvalidIdentityNonce", err)
			}
			if proof != "" {
				t.Fatalf("SignSupervisorIdentity returned %q with an invalid challenge, want no proof", proof)
			}
			if found := leaks(err, sequentialKey, tt.nonce); len(found) != 0 {
				t.Fatalf("error %q repeats inputs %q", err, found)
			}
			// A noncanonical spelling of valid bytes must not verify, even with the proof of those bytes.
			if verify(t, sequentialKey, tt.nonce, generation, referenceProof(sequentialKey, tt.nonce, generation)) {
				t.Fatal("verifier accepted an invalid challenge")
			}
		})
	}
}

func TestIdentitySignerInputs(t *testing.T) {
	tests := map[string]struct {
		token, generation string
	}{
		"error: absent token":                {"", "g"},
		"error: short token":                 {sequentialKey[:62], "g"},
		"error: long token":                  {sequentialKey + "20", "g"},
		"error: uppercase token":             {strings.ToUpper(sequentialKey), "g"},
		"error: non-hex token":               {strings.Repeat("g", 64), "g"},
		"error: token with trailing newline": {sequentialKey[:63] + "\n", "g"},
		"error: empty generation":            {sequentialKey, ""},
		"error: invalid utf-8 generation":    {sequentialKey, "gen-1\xff"},
		"error: truncated utf-8 generation":  {sequentialKey, "gen-1\xc3"},
		"error: encoded surrogate":           {sequentialKey, "gen-1\xed\xa0\x80"},
		"error: overlong encoding":           {sequentialKey, "gen-1\xc0\xaf"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			proof, err := sign(t, tt.token, sequentialNonce, tt.generation)
			if err == nil {
				t.Fatalf("SignSupervisorIdentity returned %q without an error", proof)
			}
			if proof != "" {
				t.Fatalf("SignSupervisorIdentity returned %q with an error, want no proof", proof)
			}
			if found := leaks(err, tt.token, sequentialNonce, tt.generation); len(found) != 0 {
				t.Fatalf("error %q repeats inputs %q", err, found)
			}
			if verify(t, tt.token, sequentialNonce, tt.generation, referenceProof(tt.token, sequentialNonce, tt.generation)) {
				t.Fatal("verifier accepted a proof for inputs the signer refuses")
			}
		})
	}
}

func TestIdentityProofForms(t *testing.T) {
	const generation = "g"
	valid := referenceProof(sequentialKey, sequentialNonce, generation)
	tests := map[string]struct {
		proof string
		want  bool
	}{
		"success: canonical proof":       {valid, true},
		"error: uppercase proof":         {strings.ToUpper(valid), false},
		"error: one uppercase letter":    {upperFirstLetter(t, valid), false},
		"error: absent":                  {"", false},
		"error: one byte short":          {valid[:62], false},
		"error: odd length":              {valid[:63], false},
		"error: one byte long":           {valid + "00", false},
		"error: trailing newline":        {valid + "\n", false},
		"error: leading space":           {" " + valid, false},
		"error: hex prefix":              {"0x" + valid, false},
		"error: non-hex character":       {"g" + valid[1:], false},
		"error: base64 of the mac":       {base64.StdEncoding.EncodeToString(decodedOrRaw(valid)), false},
		"error: raw mac bytes":           {string(decodedOrRaw(valid)), false},
		"error: zero mac":                {strings.Repeat("0", 64), false},
		"error: last bit flipped":        {flipLastBit(valid), false},
		"error: first byte changed":      {flipFirstByte(valid), false},
		"error: proof repeated":          {valid + valid, false},
		"error: half of the proof":       {valid[:32], false},
		"error: 64 bytes with multibyte": {valid[:62] + "é", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, verify(t, sequentialKey, sequentialNonce, generation, tt.proof)); diff != "" {
				t.Fatalf("verified (-want +got):\n%s", diff)
			}
		})
	}
}

func upperFirstLetter(t testing.TB, proof string) string {
	t.Helper()
	i := strings.IndexAny(proof, "abcdef")
	if i < 0 {
		t.Fatalf("fixture proof %q has no hex letter", proof)
	}
	return proof[:i] + strings.ToUpper(proof[i:i+1]) + proof[i+1:]
}

func flipLastBit(proof string) string {
	b := decodedOrRaw(proof)
	b[len(b)-1] ^= 1
	return hex.EncodeToString(b)
}

func flipFirstByte(proof string) string {
	b := decodedOrRaw(proof)
	b[0] ^= 0xff
	return hex.EncodeToString(b)
}
