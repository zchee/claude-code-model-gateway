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
	"fmt"
	"net/http"
	"strconv"
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

// canaryGeneration is a startup generation that no ordinary error text contains, so that an error which repeats
// it can be told apart from one that merely mentions a generation.
const canaryGeneration = "startup-canary-q7x41z"

// The text "café-1" in its two Unicode spellings, written as bytes: composed with U+00E9, and decomposed as "e"
// followed by U+0301 COMBINING ACUTE ACCENT. Normalization (NFC or NFD) turns one into the other; the proof signs the
// generation's bytes as given, so the two have different proofs.
const (
	composedGeneration   = "caf\xc3\xa9-1"
	decomposedGeneration = "cafe\xcc\x81-1"
)

// Proofs of composedGeneration and decomposedGeneration under sequentialKey and sequentialNonce, each computed outside
// Go twice, with Python's hmac module and with openssl dgst -sha256 -mac HMAC, from the published layout.
const (
	composedProof   = "474c7b2a2ed7c1c71638975021ece09c4b0432e0be5553c7e11bc2c9b8499455"
	decomposedProof = "11d883c3d6a388fa09b12002657f34ccf0599824b3613a02b16a40cd43d9d8f1"
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

// minLeakInput is a guard on the fixtures, not a proof of uniqueness: an input shorter than this is too likely to
// occur in ordinary error text by chance, so a fixture may not use one. A longer input can still occur in ordinary
// text (an input "identity" would match a message about the identity proof); the fixtures choose distinctive values
// such as canaryGeneration so that a match means a repetition.
const minLeakInput = 8

// leakForms returns the spellings in which an error message could repeat input: as it is, escaped as Go
// quoting escapes it (%q, %+q, strconv), and as the hex encoding of its bytes (%x).
func leakForms(input string) []string {
	unquote := func(quoted string) string { return quoted[1 : len(quoted)-1] }
	return []string{
		input,
		unquote(strconv.Quote(input)),
		unquote(strconv.QuoteToASCII(input)),
		hex.EncodeToString([]byte(input)),
	}
}

// leaks reports which of the secret or caller-supplied inputs an error message repeats, in any form of leakForms
// and in any letter case (an uppercase token repeated in lowercase is still the token). An empty input cannot be
// repeated; a nonempty input shorter than minLeakInput fails the test as a fixture error, because no row may go
// unchecked. A match is a repetition only because the fixture inputs are distinctive; see minLeakInput.
func leaks(t testing.TB, err error, inputs ...string) []string {
	t.Helper()
	message := strings.ToLower(err.Error())
	var found []string
	for _, input := range inputs {
		if input == "" {
			continue
		}
		if len(input) < minLeakInput {
			t.Fatalf("fixture input %q is too short to tell its repetition from ordinary text", input)
		}
		for _, form := range leakForms(input) {
			if strings.Contains(message, strings.ToLower(form)) {
				found = append(found, input)
				break
			}
		}
	}
	return found
}

// TestLeakCheck checks the repetition check the error tests rely on, against messages this test builds itself.
func TestLeakCheck(t *testing.T) {
	const invalid = canaryGeneration + "\xff"
	tests := map[string]struct {
		err   error
		input string
		want  []string
	}{
		"success: message without the input":   {err: errors.New("invalid supervisor identity nonce"), input: sequentialNonce},
		"success: empty input is not repeated": {err: errors.New("invalid supervisor identity nonce"), input: ""},
		"error: input as it is":                {err: fmt.Errorf("bad nonce %s", sequentialNonce), input: sequentialNonce, want: []string{sequentialNonce}},
		"error: input in another case":         {err: fmt.Errorf("bad token %s", strings.ToLower(sequentialKey)), input: strings.ToUpper(sequentialKey), want: []string{strings.ToUpper(sequentialKey)}},
		"error: input quoted":                  {err: fmt.Errorf("bad generation %q", invalid), input: invalid, want: []string{invalid}},
		"error: input quoted as ascii":         {err: fmt.Errorf("bad generation %+q", "世代-"+canaryGeneration), input: "世代-" + canaryGeneration, want: []string{"世代-" + canaryGeneration}},
		"error: input escaped with a newline":  {err: fmt.Errorf("bad token %q", sequentialKey[:63]+"\n"), input: sequentialKey[:63] + "\n", want: []string{sequentialKey[:63] + "\n"}},
		"error: input as hex of its bytes":     {err: fmt.Errorf("bad generation %x", canaryGeneration), input: canaryGeneration, want: []string{canaryGeneration}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, leaks(t, tt.err, tt.input)); diff != "" {
				t.Fatalf("repeated inputs (-want +got):\n%s", diff)
			}
		})
	}
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
		// The nonce has a fixed length and the generation is the rest of the message, so a NUL or a line feed
		// inside the generation leaves the layout unambiguous; such a generation is signed like any other.
		"success: generation containing a NUL": {
			token: sequentialKey, nonce: sequentialNonce, generation: "g\x00-1",
			pinned: "d9ff780459e4fbab87919ac64821ead0e2f89c9f881a36f0a93a471f073ddfde",
		},
		"success: generation containing a line feed": {
			token: sequentialKey, nonce: sequentialNonce, generation: "g\n-1",
			pinned: "5deb023c24c52610bb0efee8eed1b3d21cffad4663f744e37ce393fedabdae15",
		},
		"success: composed unicode generation": {
			token: sequentialKey, nonce: sequentialNonce, generation: composedGeneration, pinned: composedProof,
		},
		"success: decomposed unicode generation": {
			token: sequentialKey, nonce: sequentialNonce, generation: decomposedGeneration, pinned: decomposedProof,
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
			// The changed inputs are still valid, a NUL or a line feed in the generation included, so they must
			// sign to a different proof.
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

// TestSupervisorIdentityExactGenerationBytes shows that the generation is signed and verified as the exact bytes given,
// without Unicode normalization: two spellings of the same text have different proofs, and neither verifies the other.
func TestSupervisorIdentityExactGenerationBytes(t *testing.T) {
	// The bytes the external computation signed, so that the fixtures cannot drift from the reference values.
	spelled := []string{hex.EncodeToString([]byte(composedGeneration)), hex.EncodeToString([]byte(decomposedGeneration))}
	if diff := gocmp.Diff([]string{"636166c3a92d31", "63616665cc812d31"}, spelled); diff != "" {
		t.Fatalf("fixture generation bytes (-want +got):\n%s", diff)
	}
	tests := map[string]struct {
		signed, signedProof, presented, presentedProof string
	}{
		"error: composed proof presented for the decomposed generation": {
			signed: composedGeneration, signedProof: composedProof, presented: decomposedGeneration, presentedProof: decomposedProof,
		},
		"error: decomposed proof presented for the composed generation": {
			signed: decomposedGeneration, signedProof: decomposedProof, presented: composedGeneration, presentedProof: composedProof,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.signedProof, referenceProof(sequentialKey, sequentialNonce, tt.signed)); diff != "" {
				t.Fatalf("in-test reference disagrees with the externally computed vector (-want +got):\n%s", diff)
			}
			got, err := sign(t, sequentialKey, sequentialNonce, tt.signed)
			if err != nil {
				t.Fatalf("SignSupervisorIdentity: %v", err)
			}
			if diff := gocmp.Diff(tt.signedProof, got); diff != "" {
				t.Fatalf("proof of the generation as given (-want +got):\n%s", diff)
			}
			// A normalizing signer would give the other spelling the same proof.
			other, err := sign(t, sequentialKey, sequentialNonce, tt.presented)
			if err != nil {
				t.Fatalf("SignSupervisorIdentity(other spelling): %v", err)
			}
			if diff := gocmp.Diff(tt.presentedProof, other); diff != "" {
				t.Fatalf("proof of the other spelling (-want +got):\n%s", diff)
			}
			// A normalizing verifier would accept a proof made for the other spelling.
			if verify(t, sequentialKey, sequentialNonce, tt.presented, tt.signedProof) {
				t.Fatal("verifier accepted the proof of one Unicode spelling for the other")
			}
			if !verify(t, sequentialKey, sequentialNonce, tt.presented, tt.presentedProof) {
				t.Fatal("verifier rejected the proof of the spelling presented")
			}
		})
	}
}

// TestSupervisorIdentityDomainSeparation tries named samples of other domains and other layouts of the signed
// fields. It does not prove that no other message yields the same MAC: what verification enforces is equality with
// the one canonical MAC of this endpoint's message.
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
	const generation = canaryGeneration
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
			if found := leaks(t, err, sequentialKey, tt.nonce, generation); len(found) != 0 {
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
		"error: absent token":                {"", canaryGeneration},
		"error: short token":                 {sequentialKey[:62], canaryGeneration},
		"error: long token":                  {sequentialKey + "20", canaryGeneration},
		"error: uppercase token":             {strings.ToUpper(sequentialKey), canaryGeneration},
		"error: non-hex token":               {strings.Repeat("g", 64), canaryGeneration},
		"error: token with trailing newline": {sequentialKey[:63] + "\n", canaryGeneration},
		"error: empty generation":            {sequentialKey, ""},
		"error: invalid utf-8 generation":    {sequentialKey, canaryGeneration + "\xff"},
		"error: truncated utf-8 generation":  {sequentialKey, canaryGeneration + "\xc3"},
		"error: encoded surrogate":           {sequentialKey, canaryGeneration + "\xed\xa0\x80"},
		"error: overlong encoding":           {sequentialKey, canaryGeneration + "\xc0\xaf"},
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
			// The canary on its own catches an error that repeats only the valid part of an invalid generation.
			if found := leaks(t, err, tt.token, sequentialNonce, tt.generation, canaryGeneration); len(found) != 0 {
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
