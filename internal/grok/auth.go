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
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
	"github.com/zchee/claude-code-model-gateway/internal/jsdate"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
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
)

const (
	// authRequestTimeout bounds one discovery or exchange request, its body included. The source
	// sets no limit there; without one a stalled issuer would hold every waiting caller.
	authRequestTimeout = 30 * time.Second
	// maxAuthBody is the largest discovery or exchange response body that is read.
	maxAuthBody = 1 << 20
)

var (
	// errAuthRequestEnded reports a discovery or exchange request stopped by its deadline or by the
	// end of the refresh it belongs to.
	errAuthRequestEnded = errors.New("grok: auth request ended before its response")
	// errAuthBodyTooLarge reports a discovery or exchange response body above maxAuthBody.
	errAuthBodyTooLarge = errors.New("grok: auth response body too large")
)

// authEntryPrefix starts the name of the auth file member that holds the x.ai credentials.
const authEntryPrefix = "https://auth.x.ai::"

// unavailableMessage is the readiness message when no auth check is available.
const unavailableMessage = "Grok CLI auth is unavailable. Run `grok` and log in again."

// authFile is a parsed auth file and the position of its first x.ai entry.
type authFile struct {
	root  jsonx.WireValue
	entry int
}

func (a *authFile) value() jsonx.WireValue { return a.root.Members[a.entry].Value }

// readAuth follows readGrokAuth (lib/grok-backend.js:34-50): the first member whose name starts
// with the x.ai prefix and whose value is an object or array must hold a non-empty string key.
// Errors are the bare sentinels, so they carry nothing from the file.
func readAuth(file string) (*authFile, error) {
	root, ok := readJSONFile(file)
	if !ok {
		return nil, ErrAuthMissing
	}
	if root.Kind == '{' {
		for i, m := range root.Members {
			if !hasUnitsPrefix(m.Name, authEntryPrefix) || m.Value.Kind != '{' && m.Value.Kind != '[' {
				continue
			}
			if key := get(m.Value, "key"); key.Kind != '"' || len(key.Text) == 0 {
				return nil, ErrAuthInvalid
			}
			return &authFile{root: root, entry: i}, nil
		}
	}
	return nil, ErrAuthInvalid
}

func hasUnitsPrefix(name []uint16, prefix string) bool {
	return len(name) >= len(prefix) && nameIs(name[:len(prefix)], prefix)
}

// nonEmptyString reports the text of a non-empty string value.
func nonEmptyString(v jsonx.WireValue) (string, bool) {
	if v.Kind != '"' || len(v.Text) == 0 {
		return "", false
	}
	return string(utf16.Decode(v.Text)), true
}

// expiresAtMillis follows lib/grok-backend.js:52-59: a string is a date, anything else a number of
// seconds below 1e11 and of milliseconds from there on. Where the source's Number() would throw,
// the expiry is unverifiable, as an unparseable date is.
func expiresAtMillis(v jsonx.WireValue) float64 {
	if v.Kind == '"' {
		if ms, ok := jsdate.Parse(string(utf16.Decode(v.Text))); ok {
			return float64(ms)
		}
		return math.NaN()
	}
	if coercionThrows(v) {
		return math.NaN()
	}
	n := jsNumber(v)
	if n < 100000000000 {
		return n * 1000
	}
	return n
}

func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }

// CheckAuth verifies that file contains a parseable Grok CLI auth object with a non-empty
// access key under an https://auth.x.ai:: entry, without returning any credential,
// refreshing tokens, or writing to disk.
func CheckAuth(file string) error {
	_, err := readAuth(file)
	return err
}

// Readiness reports Grok CLI authentication readiness using check without refreshing tokens.
// When check returns nil, Grok is ready; otherwise an error whose message contains "invalid"
// (ASCII case-insensitive) maps to "auth-invalid" and any other error maps to "auth-missing".
// A nil check reports "auth-missing" with the unavailable fallback message.
func Readiness(check func() error) catalog.Readiness {
	if check == nil {
		return catalog.Readiness{State: "auth-missing", Message: unavailableMessage}
	}
	err := check()
	if err == nil {
		return catalog.Readiness{Ready: true, State: "ready", Message: "Grok CLI auth is present."}
	}
	message := err.Error()
	state := "auth-missing"
	if strings.Contains(asciiLower(message), "invalid") {
		state = "auth-invalid"
	}
	return catalog.Readiness{State: state, Message: message}
}

// asciiLower folds only ASCII letters, as a JavaScript regular expression with the i flag and
// without the u flag matches the ASCII pattern "invalid".
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// AuthOptions configures an AuthStore without reading ambient environment or files.
type AuthOptions struct {
	// File is the clean, absolute path to the Grok CLI auth.json file.
	File string
	// Transport performs OIDC discovery and OAuth token refresh requests.
	// Callers must supply a non-nil Transport isolated from the inference client's lifetime;
	// nil is rejected with an error and http.DefaultTransport is never used. Each request ends
	// after 30 seconds only when Transport honours the request's context.
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
type AuthStore struct {
	file      string
	transport http.RoundTripper
	now       func() time.Time
	skew      time.Duration
	shadow    bool

	mu     sync.Mutex
	flight *flight // the refresh in progress, if any; guarded by mu
}

// flight is one refresh shared by every caller waiting for it. Only one flight exists at a time,
// so two exchanges never run at once on one refresh token.
type flight struct {
	done   chan struct{} // closed after the result fields are set
	cancel context.CancelFunc

	// Guarded by AuthStore.mu.
	waiters   int
	abandoned bool // every waiter left before the exchange was sent, and the flight's context was ended
	committed bool // the exchange request is built and goes to the transport next; waiters no longer end it

	// Written before done is closed and read only after it.
	token     string
	exchanged bool // token came from an exchange rather than from the file
	err       error
}

// NewAuthStore constructs an AuthStore for the configured auth file and transport.
// It returns an error when opts.Transport is nil.
func NewAuthStore(opts AuthOptions) (*AuthStore, error) {
	if opts.Transport == nil {
		return nil, errors.New("grok: auth store requires an explicit transport")
	}
	s := &AuthStore{file: opts.File, transport: opts.Transport, now: opts.Now, skew: opts.Skew, shadow: opts.Shadow}
	if s.now == nil {
		s.now = time.Now
	}
	if s.skew <= 0 {
		s.skew = DefaultRefreshSkew
	}
	return s, nil
}

func (s *AuthStore) nowMillis() float64 { return float64(s.now().UnixMilli()) }

// AccessToken returns a valid Grok access token, refreshing once across concurrent callers
// when the stored token is expired, unparseable, or within the refresh skew window.
// In shadow mode it uses a stored token until its real expiry (even inside the refresh skew
// window), refuses expired or unverifiable tokens with ErrAuthCannotRefresh, and never
// refreshes or writes the auth file.
//
// When ctx has already ended, AccessToken returns ctx's error before it reads the auth file, so
// before it would return a fresh stored token and before any shadow-mode refusal; it starts and
// joins no refresh. ctx is checked again under the store's lock before the caller enrolls in a
// refresh. A caller that stops waiting gets ctx's error at once and does not fail the others.
//
// Outside shadow mode, a caller whose stored token is fresh still joins a refresh whose exchange
// has already gone to the transport and returns that refresh's result, because the issuer may have
// rotated the stored credentials. A refresh that has not reached its exchange does not delay it.
func (s *AuthStore) AccessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	current, err := readAuth(s.file)
	if err != nil {
		return "", err
	}
	expires := expiresAtMillis(get(current.value(), "expires_at"))
	if s.shadow {
		if !finite(expires) || expires <= s.nowMillis() {
			return "", ErrAuthCannotRefresh
		}
		return keyOf(current), nil
	}
	if s.expiresSoon(expires) {
		return s.shared(ctx, false)
	}
	s.mu.Lock()
	f := s.flight
	if f == nil || !f.committed {
		s.mu.Unlock()
		return keyOf(current), nil
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return "", err
	}
	f.waiters++
	s.mu.Unlock()
	token, _, err := s.wait(ctx, f)
	return token, err
}

// expiresSoon compares with the whole skew, so a skew below a millisecond still counts.
func (s *AuthStore) expiresSoon(expires float64) bool {
	return !finite(expires) || expires <= s.nowMillis()+float64(s.skew)/float64(time.Millisecond)
}

func keyOf(a *authFile) string {
	key, _ := nonEmptyString(get(a.value(), "key")) // readAuth checked it.
	return key
}

// Refresh exchanges the stored refresh_token for a new access token, persists the updated auth file
// atomically with mode 0600, and deduplicates concurrent refreshes. The key it returns always comes
// from an exchange: when it joins a refresh in flight that found the stored token fresh and
// exchanged nothing, it runs one exchange of its own afterwards.
// When ctx has already ended, Refresh returns ctx's error before anything else, the shadow-mode
// refusal included, and ctx is checked again under the store's lock before the caller enrolls in
// a refresh. In shadow mode it returns ErrAuthCannotRefresh before reading the auth file or
// starting a refresh.
func (s *AuthStore) Refresh(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.shadow {
		return "", ErrAuthCannotRefresh
	}
	return s.shared(ctx, true)
}

// shared waits for the refresh in flight, starting one when there is none. A caller whose ctx has
// ended, checked under s.mu, starts and joins nothing. Each caller stops waiting when its own ctx
// ends; the refresh keeps running for the others. When the last waiter leaves before the exchange
// was sent, the refresh's own context ends; once the exchange was sent, the refresh reads the reply
// and stores the new credential although nobody waits for it, because the issuer may already have
// rotated the refresh token. A caller arriving at an abandoned refresh waits for it to stop, then
// starts a new one. A forced caller accepts only a key that an exchange produced. A caller whose
// ctx has ended when it finds the refresh finished gets ctx's error, not the refresh's result.
func (s *AuthStore) shared(ctx context.Context, forced bool) (string, error) {
	for {
		s.mu.Lock()
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return "", err
		}
		f := s.flight
		if f != nil && f.abandoned {
			s.mu.Unlock()
			select {
			case <-f.done:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		if f == nil {
			f = s.start(ctx, forced)
		} else {
			f.waiters++
		}
		s.mu.Unlock()

		token, exchanged, err := s.wait(ctx, f)
		if err != nil {
			return "", err
		}
		if forced && !exchanged {
			continue
		}
		return token, nil
	}
}

// wait waits for f, in which the caller is enrolled, to finish, and returns its result. When ctx
// ends first, the caller leaves f and gets ctx's error; the last waiter leaving a flight that has
// not committed its exchange ends it. When both have happened by the time the wait returns, ctx's
// error wins: the caller has stopped waiting, so it gets no result.
func (s *AuthStore) wait(ctx context.Context, f *flight) (string, bool, error) {
	select {
	case <-f.done:
		// f has ended and left s.flight, so a departing caller has nothing to release.
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		return f.token, f.exchanged, f.err
	case <-ctx.Done():
		s.mu.Lock()
		if f.waiters--; f.waiters == 0 && !f.committed {
			f.abandoned = true
			f.cancel()
		}
		s.mu.Unlock()
		return "", false, ctx.Err()
	}
}

// start begins a flight with one waiter. The flight's context keeps ctx's values but not its
// cancellation, so the first caller leaving does not fail the others. s.mu must be held.
func (s *AuthStore) start(ctx context.Context, forced bool) *flight {
	work, cancel := context.WithCancel(context.WithoutCancel(ctx))
	f := &flight{done: make(chan struct{}), cancel: cancel, waiters: 1}
	s.flight = f
	go func() {
		token, exchanged, err := s.run(work, f, forced)
		s.mu.Lock()
		f.token, f.exchanged, f.err = token, exchanged, err
		// A flight stays in s.flight until it ends, so no other flight can have replaced it.
		s.flight = nil
		s.mu.Unlock()
		cancel()
		close(f.done)
	}()
	return f
}

// run is the work of one flight. The file is read inside the flight: a refresh that finished
// while a caller read the file has already rotated the refresh token, so an unforced flight
// exchanges only when the token it finds is still expiring.
func (s *AuthStore) run(ctx context.Context, f *flight, forced bool) (token string, exchanged bool, err error) {
	current, err := readAuth(s.file)
	if err != nil {
		return "", false, err
	}
	if !forced && !s.expiresSoon(expiresAtMillis(get(current.value(), "expires_at"))) {
		return keyOf(current), false, nil
	}
	token, err = s.refresh(ctx, f, current)
	return token, err == nil, err
}

// commit decides, in one step under s.mu, whether f's exchange is sent: it reports false when every
// waiter had already left, and otherwise marks f past the point where its waiters can end it, so
// the exchange runs to its end under its own deadline. requestJSON calls it with the request built,
// immediately before the transport call, and s.mu is not held across that call. The instant between
// this decision and the first byte on the wire counts as sent.
func (s *AuthStore) commit(f *flight) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.abandoned {
		return false
	}
	f.committed = true
	return true
}

// refresh follows refreshGrokAuth (lib/grok-backend.js:83-113). Discovery follows the flight's
// context; the exchange does not, so a refresh token the issuer rotated is always stored. The
// exchange is committed to, or dropped because every waiter left, at the transport call.
func (s *AuthStore) refresh(ctx context.Context, f *flight, current *authFile) (string, error) {
	entry := current.value()
	refreshToken, ok := nonEmptyString(get(entry, "refresh_token"))
	if !ok {
		return "", ErrAuthCannotRefresh
	}
	clientID, ok := nonEmptyString(get(entry, "oidc_client_id"))
	if !ok {
		return "", ErrAuthCannotRefresh
	}
	endpoint, err := s.tokenEndpoint(ctx, entry)
	if err != nil {
		return "", ErrAuthRefreshFailed
	}
	form := formEncode([][2]string{{"grant_type", "refresh_token"}, {"refresh_token", refreshToken}, {"client_id", clientID}})
	status, body, err := s.requestJSON(context.WithoutCancel(ctx), f, http.MethodPost, endpoint, form)
	if err != nil || status < 200 || status >= 300 {
		return "", ErrAuthRefreshFailed
	}
	accessToken := get(body, "access_token")
	if accessToken.Kind != '"' || len(accessToken.Text) == 0 {
		return "", ErrAuthRefreshFailed
	}
	setMember(&entry, "key", accessToken)
	if rotated := get(body, "refresh_token"); rotated.Kind == '"' && len(rotated.Text) > 0 {
		setMember(&entry, "refresh_token", rotated)
	}
	// Where the source's Number() would throw, the lifetime is unusable and expires_at stays.
	if lifetime := get(body, "expires_in"); !coercionThrows(lifetime) {
		if seconds := jsNumber(lifetime); finite(seconds) && seconds > 0 {
			setMember(&entry, "expires_at", num(s.nowMillis()+seconds*1000))
		}
	}
	root := current.root
	root.Members = slices.Clone(root.Members)
	root.Members[current.entry].Value = entry
	if err := jsonx.WriteState(s.file, root, 0o600); err != nil {
		return "", ErrAuthRefreshFailed
	}
	return string(utf16.Decode(accessToken.Text)), nil
}

// tokenEndpoint follows oidcTokenEndpoint (lib/grok-backend.js:70-81): the issuer's discovery
// document names the endpoint, and a failed discovery falls back to the conventional path. A
// discovery stopped by its deadline or by the end of the refresh, or one whose body is too large,
// is an error instead: the refresh fails without an exchange.
func (s *AuthStore) tokenEndpoint(ctx context.Context, entry jsonx.WireValue) (string, error) {
	issuer := DefaultIssuer
	if custom, ok := nonEmptyString(get(entry, "oidc_issuer")); ok {
		issuer = strings.TrimSuffix(custom, "/")
	}
	status, body, err := s.requestJSON(ctx, nil, http.MethodGet, issuer+"/.well-known/openid-configuration", "")
	if errors.Is(err, errAuthRequestEnded) || errors.Is(err, errAuthBodyTooLarge) {
		return "", err
	}
	if err == nil && status >= 200 && status < 300 {
		if endpoint := get(body, "token_endpoint"); endpoint.Kind == '"' {
			return string(utf16.Decode(endpoint.Text)), nil
		}
	}
	if issuer == DefaultIssuer {
		return DefaultTokenEndpoint, nil
	}
	return issuer + "/oauth/token", nil
}

// requestJSON follows the source's requestJson (lib/grok-backend.js:393-408): it sends no headers
// besides the form content type, and a body that is not JSON reads as null. Unlike the source, the
// request and its body read end after authRequestTimeout (errAuthRequestEnded), and a body above
// maxAuthBody is refused (errAuthBodyTooLarge) whether or not the read also failed. The deadline
// holds only with a transport that honours the request's context.
//
// For an exchange, f is the flight it belongs to: the request is built first, then commit decides
// whether it is sent, as the last step before the transport call. A flight every waiter has left
// sends nothing and gets errAuthRequestEnded. Discovery passes a nil f and is not committed.
func (s *AuthStore) requestJSON(ctx context.Context, f *flight, method, target, form string) (int, jsonx.WireValue, error) {
	ctx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(form)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, jsonx.WireValue{}, err
	}
	// An empty User-Agent entry stops net/http from sending its own; the source sends none.
	req.Header["User-Agent"] = []string{""}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if f != nil && !s.commit(f) {
		return 0, jsonx.WireValue{}, errAuthRequestEnded
	}
	resp, err := s.transport.RoundTrip(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, jsonx.WireValue{}, errAuthRequestEnded
		}
		return 0, jsonx.WireValue{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthBody+1))
	// A reader may return data and an error together; the bound is decided on the data alone.
	if len(raw) > maxAuthBody {
		return 0, jsonx.WireValue{}, errAuthBodyTooLarge
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, jsonx.WireValue{}, errAuthRequestEnded
		}
		return 0, jsonx.WireValue{}, err
	}
	parsed, err := jsonx.DecodeWire(raw)
	if err != nil {
		parsed = null()
	}
	return resp.StatusCode, parsed, nil
}

// formEncode is URLSearchParams.toString(): the application/x-www-form-urlencoded serializer
// keeps the pairs in order, leaves ASCII alphanumerics and *-._ as they are, writes a space as +
// and percent-encodes every other UTF-8 byte.
func formEncode(pairs [][2]string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	write := func(s string) {
		for i := range len(s) {
			switch c := s[i]; {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
				b.WriteByte(c)
			case c == ' ':
				b.WriteByte('+')
			default:
				b.WriteByte('%')
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			}
		}
	}
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		write(p[0])
		b.WriteByte('=')
		write(p[1])
	}
	return b.String()
}

// setMember assigns obj[name] as JavaScript does: an existing member keeps its position and a new
// one is appended. obj gets its own member slice, so the parsed tree it came from is unchanged.
func setMember(obj *jsonx.WireValue, name string, v jsonx.WireValue) {
	members := slices.Clone(obj.Members)
	for i := range members {
		if nameIs(members[i].Name, name) {
			members[i].Value = v
			obj.Members = members
			return
		}
	}
	obj.Members = append(members, member(name, v))
}
