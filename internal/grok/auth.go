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

package grok

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
)

// DefaultRefreshSkew is the lead time before token expiry at which a refresh is triggered.
const DefaultRefreshSkew = 5 * time.Minute

var (
	// ErrAuthMissing reports a missing or unreadable Grok CLI auth file.
	ErrAuthMissing = errors.New("Grok CLI auth is missing. Run `grok` and log in again.") //nolint:staticcheck // The message is the source's user-facing wording.
	// ErrAuthInvalid reports a Grok CLI auth file without a valid x.ai access key entry.
	ErrAuthInvalid = errors.New("Grok CLI auth is invalid. Run `grok` and log in again.") //nolint:staticcheck // The message is the source's user-facing wording.
	// ErrAuthCannotRefresh reports an auth entry that lacks refresh credentials or cannot refresh in shadow mode.
	ErrAuthCannotRefresh = errors.New("Grok CLI auth cannot refresh. Run `grok` and log in again.") //nolint:staticcheck // The message is the source's user-facing wording.
	// ErrAuthRefreshFailed reports a failed OIDC discovery or token refresh exchange.
	ErrAuthRefreshFailed = errors.New("Grok CLI auth refresh failed. Run `grok` and log in again.") //nolint:staticcheck // The message is the source's user-facing wording.

	errNotImplemented = errors.New("not implemented")
)

// CheckAuth verifies that file contains a parseable Grok CLI auth object with a non-empty
// access key under an https://auth.x.ai:: entry, without returning any credential.
func CheckAuth(file string) error {
	return errNotImplemented
}

// Readiness reports Grok CLI authentication readiness using check.
// When check returns nil, Grok is ready; otherwise an error whose message contains "invalid"
// (ASCII case-insensitive) maps to "auth-invalid" and any other error maps to "auth-missing".
// A nil check reports "auth-missing" with the unavailable fallback message.
func Readiness(check func() error) catalog.Readiness {
	return catalog.Readiness{}
}

// AuthOptions configures an AuthStore without reading ambient environment or files.
type AuthOptions struct {
	// File is the clean, absolute path to the Grok CLI auth.json file.
	File string
	// Transport performs OIDC discovery and OAuth token refresh requests.
	// If nil, http.DefaultTransport is used.
	Transport http.RoundTripper
	// Now returns the current time for expiry checks and refreshed expiry timestamps.
	// If nil, time.Now is used.
	Now func() time.Time
	// Skew is the refresh lead window before expires_at. Zero or negative uses DefaultRefreshSkew.
	Skew time.Duration
	// Shadow disables token refresh and auth file writes.
	Shadow bool
}

// AuthStore reads and refreshes Grok CLI credentials with single-flight deduplication.
// It is safe for concurrent use and must not be copied after first use.
type AuthStore struct{}

// NewAuthStore constructs an AuthStore for the configured auth file and transport.
func NewAuthStore(opts AuthOptions) *AuthStore {
	return &AuthStore{}
}

// AccessToken returns a valid Grok access token, refreshing once across concurrent callers
// when the stored token is expired, unparseable, or within the refresh skew window.
// In shadow mode it never refreshes or writes the auth file.
func (s *AuthStore) AccessToken(ctx context.Context) (string, error) {
	return "", errNotImplemented
}

// Refresh exchanges the stored refresh_token for a new access token, persists the updated auth file
// atomically with mode 0600, and deduplicates concurrent refreshes.
// In shadow mode it returns ErrAuthCannotRefresh without network requests or disk writes.
func (s *AuthStore) Refresh(ctx context.Context) (string, error) {
	return "", errNotImplemented
}
