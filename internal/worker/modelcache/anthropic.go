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

package modelcache

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// catalogHeaderNames are the incoming headers a catalog request may carry (lib/anthropic-catalog.js:12).
var catalogHeaderNames = []string{"authorization", "x-api-key", "anthropic-version", "anthropic-beta", "anthropic-workspace-id", "user-agent"}

// emptyRows is an empty row array.
var emptyRows = jsontext.Value("[]")

// catalogFlight is the single in-flight Anthropic catalog refresh. rows is written before done closes.
type catalogFlight struct {
	done chan struct{}
	rows jsontext.Value
}

// ModelsResponse builds the GET /v1/models JSON response body using allowed credential headers from incoming.
// When serving stale cached rows immediately, any background refresh runs under the worker context from Start rather than ctx.
func (c *Cache) ModelsResponse(ctx context.Context, incoming http.Header) (jsontext.Value, error) {
	if err := c.awaitFirstListing(ctx); err != nil {
		return nil, err
	}
	anthropicRows, count, flight := c.anthropicRows(incoming)
	// A cold cache waits for the shared refresh; the request context ends only this wait
	// (lib/anthropic-catalog.js:96-104).
	if flight != nil && count == 0 {
		select {
		case <-flight.done:
			anthropicRows = flight.rows
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	current := c.current.Load()
	resolver := current.snapshot.Resolver()
	combined, err := concatRows(anthropiccatalog.WithCached(emptyRows, anthropicRows, resolver), current.rows)
	if err != nil {
		return nil, err
	}
	data := anthropiccatalog.WithPrevious(combined, c.serverRows(), c.currentPins())
	if len(data) == 0 {
		return nil, errors.New("modelcache: cannot encode the model listing")
	}
	body := make([]byte, 0, len(data)+32)
	body = append(body, `{"data":`...)
	body = append(body, data...)
	body = append(body, `,"has_more":false}`...)
	return body, nil
}

// awaitFirstListing holds an answer given after Start until the first proxy listing has been
// refreshed, so an answer just after startup does not list the built-in Codex defaults while the
// listing is still on its way. The wait ends at the latest when the listing's own timeout after
// Start has passed, and a failed or timed-out listing leaves the fallback to answer. Once the first
// refresh ended or that bound passed, no answer waits; before Start none does. The request context
// ends only this wait. The source answers at once from the fallback; this wait is a deliberate
// difference.
func (c *Cache) awaitFirstListing(ctx context.Context) error {
	c.mu.Lock()
	started, deadline := c.started, c.firstDeadline
	c.mu.Unlock()
	if !started {
		return nil
	}
	select {
	case <-c.firstListing:
		return nil
	default:
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-c.firstListing:
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// TriggerAnthropicRefresh initiates a non-blocking Anthropic catalog refresh under the worker context from Start when stale and credentials are present.
func (c *Cache) TriggerAnthropicRefresh(incoming http.Header) {
	_, _, _ = c.anthropicRows(incoming)
}

// anthropicRows returns the cached catalog rows and their count, and starts or joins a refresh when
// the cache is stale and the request carries credentials. The read and the election are one
// operation: a refresh clears its flight only after its write, so a caller that finds no flight reads
// what the last refresh wrote and never starts a redundant one from an older snapshot.
func (c *Cache) anthropicRows(incoming http.Header) (jsontext.Value, int, *catalogFlight) {
	headers := catalogHeaders(incoming)
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	cached := anthropiccatalog.Read(c.catalogPath())
	rows, count := catalogData(cached)
	if cached.Fresh(c.clock()) || c.anthropicBase == nil {
		return rows, count, nil
	}
	if headers.Get("Authorization") == "" && headers.Get("X-Api-Key") == "" {
		return rows, count, nil
	}
	if c.flight != nil {
		return rows, count, c.flight
	}
	if !c.register() {
		return rows, count, nil
	}
	flight := &catalogFlight{done: make(chan struct{})}
	c.flight = flight
	go func() {
		defer c.wg.Done()
		flight.rows = c.refreshCatalog(headers, cached)
		c.catalogMu.Lock()
		c.flight = nil
		c.catalogMu.Unlock()
		close(flight.done)
	}()
	return rows, count, flight
}

// register admits one background catalog refresh and adds it to the wait group in one step under
// c.mu, the gate Wait crosses before joining. A refresh admitted before shutdown is therefore counted
// before Wait joins, and none is admitted after the worker context was cancelled or Wait was called.
// Nothing under the gate does I/O, waits, or calls a method of a context.
func (c *Cache) register() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.liveLocked() {
		return false
	}
	c.wg.Add(1)
	return true
}

func (c *Cache) catalogPath() string {
	if c.stateDir == "" {
		return ""
	}
	return filepath.Join(c.stateDir, "anthropic-models.json")
}

// admitted reports whether a running catalog refresh may still write: neither the worker context
// from Start nor the catalog lifetime has ended.
func (c *Cache) admitted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveLocked()
}

// liveLocked reports, with c.mu held, that neither the worker context nor the catalog lifetime ended.
// It only polls the Done channels captured outside the lock; a nil workerDone (before Start) never
// fires.
func (c *Cache) liveLocked() bool {
	select {
	case <-c.lifeDone:
		return false
	case <-c.workerDone:
		return false
	default:
		return true
	}
}

// refreshCatalog fetches every page and records the outcome against the snapshot that triggered it.
// It runs under the catalog lifetime, which worker shutdown cancels. No write is admitted once the
// worker context ended (lib/anthropic-catalog.js:83-95).
func (c *Cache) refreshCatalog(headers http.Header, cached anthropiccatalog.Snapshot) jsontext.Value {
	path := c.catalogPath()
	rows, failure := c.fetchCatalog(c.life, headers)
	cachedRows, _ := catalogData(cached)
	if !c.admitted() {
		return cachedRows
	}
	now := c.clock()
	if failure == "" {
		if err := anthropiccatalog.WriteSuccess(path, rows, now); err == nil {
			if c.admitted() {
				c.syncDiscovery(true)
			}
			saved, _ := catalogData(anthropiccatalog.Read(path))
			return saved
		}
		failure = "catalog write failed"
		if !c.admitted() {
			return cachedRows
		}
	}
	if err := anthropiccatalog.WriteFailure(path, cached, failure, now); err != nil {
		c.logf("model-gateway: could not record the Anthropic catalog refresh failure (%s)", errorClass(err))
	}
	return cachedRows
}

// fetchCatalog follows has_more for at most MaxAnthropicPages pages. A failure is one of the source's
// classified messages and never carries response text (lib/anthropic-catalog.js:59-70).
func (c *Cache) fetchCatalog(ctx context.Context, headers http.Header) (jsontext.Value, string) {
	all := jsonx.WireValue{Kind: '['}
	afterID, paged := "", false
	for range MaxAnthropicPages {
		query := "limit=1000"
		if paged {
			query += "&after_id=" + formEncode(afterID)
		}
		page, failure := c.fetchCatalogPage(ctx, query, headers)
		if failure != "" {
			return nil, failure
		}
		data := member(page, "data")
		if data.Kind != '[' {
			return nil, "invalid catalog data"
		}
		// Each page keeps only its valid rows before the next one is read (lib/anthropic-catalog.js:63).
		all.Elements = append(all.Elements, slices.DeleteFunc(data.Elements, invalidCatalogRow)...)
		if !truthy(member(page, "has_more")) {
			encoded, err := jsonx.EncodeWire(all)
			if err != nil {
				return nil, "invalid catalog data"
			}
			return encoded, ""
		}
		last := member(page, "last_id")
		if last.Kind != '"' {
			break
		}
		next := string(utf16.Decode(last.Text))
		if paged && next == afterID {
			break
		}
		afterID, paged = next, true
	}
	return nil, "catalog pagination limit"
}

// fetchCatalogPage requests one page under its own deadline. The request follows no redirect, as the
// source's http.get does, so credentials never leave the configured upstream.
func (c *Cache) fetchCatalogPage(ctx context.Context, query string, headers http.Header) (jsonx.WireValue, string) {
	pageCtx, cancel := context.WithTimeout(ctx, AnthropicPageTimeout)
	defer cancel()
	target := c.anthropicBase.ResolveReference(&url.URL{Path: "/v1/models", RawQuery: query})
	req, err := http.NewRequestWithContext(pageCtx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return jsonx.WireValue{}, "catalog request failed"
	}
	req.Header = headers.Clone()
	if _, ok := req.Header["User-Agent"]; !ok {
		// An empty value keeps the client from adding its own User-Agent.
		req.Header["User-Agent"] = []string{""}
	}
	resp, err := c.catalogClient.Do(req)
	if err != nil {
		return jsonx.WireValue{}, pageFailure(pageCtx)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return jsonx.WireValue{}, "upstream HTTP " + strconv.Itoa(resp.StatusCode)
	}
	body, tooLarge, err := readCatalogPage(resp.Body)
	switch {
	case tooLarge:
		return jsonx.WireValue{}, "catalog too large"
	case err != nil:
		return jsonx.WireValue{}, pageFailure(pageCtx)
	}
	page, err := jsonx.DecodeWire(body)
	if err != nil {
		return jsonx.WireValue{}, "invalid catalog JSON"
	}
	return page, ""
}

// pageFailure classifies a transport failure without its error text, which can name addresses.
func pageFailure(pageCtx context.Context) string {
	if errors.Is(pageCtx.Err(), context.DeadlineExceeded) {
		return "catalog timeout"
	}
	return "catalog request failed"
}

// catalogHeaders keeps the allowed incoming headers under their lowercase names, with the source's
// default API version. Values are copied, so later changes by the caller never reach a refresh.
// Repeated values merge as Node merges them: authorization and user-agent keep the first value, the
// others join with ", " (lib/anthropic-catalog.js:14-18).
func catalogHeaders(incoming http.Header) http.Header {
	headers := http.Header{"Anthropic-Version": {"2023-06-01"}}
	for _, name := range slices.Sorted(maps.Keys(incoming)) {
		lower := strings.ToLower(name)
		values := incoming[name]
		if !slices.Contains(catalogHeaderNames, lower) || len(values) == 0 {
			continue
		}
		value := strings.Join(values, ", ")
		if lower == "authorization" || lower == "user-agent" {
			value = values[0]
		}
		headers[http.CanonicalHeaderKey(lower)] = []string{value}
	}
	return headers
}

// catalogData returns the data rows of a normalized catalog snapshot and their count.
func catalogData(snapshot anthropiccatalog.Snapshot) (jsontext.Value, int) {
	obj, err := jsonx.DecodeWire(snapshot.Object)
	if err != nil {
		return emptyRows, 0
	}
	data := member(obj, "data")
	if data.Kind != '[' {
		return emptyRows, 0
	}
	encoded, err := jsonx.EncodeWire(data)
	if err != nil {
		return emptyRows, 0
	}
	return encoded, len(data.Elements)
}

// concatRows joins two row arrays without merging rows that share an ID.
func concatRows(first, second jsontext.Value) (jsontext.Value, error) {
	a, err := jsonx.DecodeWire(first)
	if err != nil {
		return nil, err
	}
	b, err := jsonx.DecodeWire(second)
	if err != nil {
		return nil, err
	}
	a.Elements = append(a.Elements, b.Elements...)
	return jsonx.EncodeWire(a)
}

// member returns the last member named name, as JSON.parse keeps the last duplicate.
func member(obj jsonx.WireValue, name string) jsonx.WireValue {
	key := utf16.Encode([]rune(name))
	for _, m := range slices.Backward(obj.Members) {
		if slices.Equal(m.Name, key) {
			return m.Value
		}
	}
	return jsonx.WireValue{}
}

// truthy is JavaScript's Boolean conversion of a decoded JSON value; an absent value is false.
func truthy(v jsonx.WireValue) bool {
	switch v.Kind {
	case 't', '{', '[':
		return true
	case '"':
		return len(v.Text) > 0
	case '0':
		return v.Number != 0 && !math.IsNaN(v.Number)
	}
	return false
}

// formEncode serializes a URLSearchParams value: UTF-8 bytes percent-encoded except the
// application/x-www-form-urlencoded safe set, and a space as '+'.
func formEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
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
	return b.String()
}

// readCatalogPage decodes a page body as the source's UTF-8 string decoder does, one U+FFFD for each
// maximal ill-formed subsequence, and counts the decoded UTF-16 code units as they arrive. It stops at
// the first unit over the bound and reports the page too large, whatever the rest of the body would
// have done (lib/anthropic-catalog.js:43-47). The returned text is valid UTF-8.
func readCatalogPage(body io.Reader) ([]byte, bool, error) {
	decoder := transform.NewReader(body, unicode.UTF8.NewDecoder())
	buf := make([]byte, 32<<10)
	var text []byte
	units, counted := 0, 0
	for {
		n, err := decoder.Read(buf)
		text = append(text, buf[:n]...)
		for counted < len(text) && utf8.FullRune(text[counted:]) {
			r, size := utf8.DecodeRune(text[counted:])
			units++
			if r >= 0x10000 {
				units++
			}
			counted += size
		}
		if units > MaxAnthropicPageUTF16Units {
			return nil, true, nil
		}
		switch {
		case errors.Is(err, io.EOF):
			return text, false, nil
		case err != nil:
			return nil, false, err
		}
	}
}

// invalidCatalogRow reports a row the source's validCatalogRows drops: one whose id is not a string
// "claude-" followed by at least one of [a-z0-9._-], or whose display_name is not a string. An id
// holding an unpaired surrogate is invalid, as the catalog store also treats it.
func invalidCatalogRow(row jsonx.WireValue) bool {
	id := member(row, "id")
	if id.Kind != '"' || member(row, "display_name").Kind != '"' {
		return true
	}
	for i := 0; i < len(id.Text); i++ {
		u := rune(id.Text[i])
		if !utf16.IsSurrogate(u) {
			continue
		}
		i++
		if i == len(id.Text) || utf16.DecodeRune(u, rune(id.Text[i])) == utf8.RuneError {
			return true
		}
	}
	rest, ok := strings.CutPrefix(string(utf16.Decode(id.Text)), "claude-")
	if !ok || rest == "" {
		return true
	}
	return strings.ContainsFunc(rest, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune("._-", r)
	})
}
