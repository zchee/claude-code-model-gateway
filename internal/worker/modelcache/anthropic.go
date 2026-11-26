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
	"unicode/utf16"
	"unicode/utf8"

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

// TriggerAnthropicRefresh initiates a non-blocking Anthropic catalog refresh under the worker context from Start when stale and credentials are present.
func (c *Cache) TriggerAnthropicRefresh(incoming http.Header) {
	_, _, _ = c.anthropicRows(incoming)
}

// anthropicRows returns the cached catalog rows and their count, and starts or joins a refresh when
// the cache is stale and the request carries credentials.
func (c *Cache) anthropicRows(incoming http.Header) (jsontext.Value, int, *catalogFlight) {
	cached := anthropiccatalog.Read(c.catalogPath())
	rows, count := catalogData(cached)
	if cached.Fresh(c.clock()) || c.anthropicBase == nil {
		return rows, count, nil
	}
	headers := catalogHeaders(incoming)
	if headers.Get("Authorization") == "" && headers.Get("X-Api-Key") == "" {
		return rows, count, nil
	}
	return rows, count, c.startCatalogRefresh(headers, cached)
}

func (c *Cache) catalogPath() string {
	if c.stateDir == "" {
		return ""
	}
	return filepath.Join(c.stateDir, "anthropic-models.json")
}

// startCatalogRefresh returns the in-flight refresh, or starts one under the worker context. Before
// Start the refresh runs to its own end. After the worker context ends nothing new starts, so no
// goroutine races Wait.
func (c *Cache) startCatalogRefresh(headers http.Header, cached anthropiccatalog.Snapshot) *catalogFlight {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.flight != nil {
		return c.flight
	}
	ctx := c.worker
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil
	}
	flight := &catalogFlight{done: make(chan struct{})}
	c.flight = flight
	c.wg.Go(func() {
		flight.rows = c.refreshCatalog(ctx, headers, cached)
		c.mu.Lock()
		c.flight = nil
		c.mu.Unlock()
		close(flight.done)
	})
	return flight
}

// refreshCatalog fetches every page and records the outcome against the snapshot that triggered it.
// Worker shutdown records nothing (lib/anthropic-catalog.js:83-95).
func (c *Cache) refreshCatalog(ctx context.Context, headers http.Header, cached anthropiccatalog.Snapshot) jsontext.Value {
	path := c.catalogPath()
	rows, failure := c.fetchCatalog(ctx, headers)
	cachedRows, _ := catalogData(cached)
	if ctx.Err() != nil {
		return cachedRows
	}
	now := c.clock()
	if failure == "" {
		if err := anthropiccatalog.WriteSuccess(path, rows, now); err == nil {
			c.syncDiscovery(true)
			saved, _ := catalogData(anthropiccatalog.Read(path))
			return saved
		}
		failure = "catalog write failed"
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
		all.Elements = append(all.Elements, data.Elements...)
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
	client := *c.opts.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return jsonx.WireValue{}, pageFailure(pageCtx)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return jsonx.WireValue{}, "upstream HTTP " + strconv.Itoa(resp.StatusCode)
	}
	// Every UTF-16 code unit takes at least one and at most three UTF-8 bytes, so this bound always
	// reads past the limit of a page that exceeds it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 3*MaxAnthropicPageUTF16Units+4))
	if err != nil {
		return jsonx.WireValue{}, pageFailure(pageCtx)
	}
	if utf16Units(body) > MaxAnthropicPageUTF16Units {
		return jsonx.WireValue{}, "catalog too large"
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

// utf16Units counts the UTF-16 code units of b decoded as UTF-8, with each invalid byte replaced by
// U+FFFD.
func utf16Units(b []byte) int {
	units := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		units++
		if r >= 0x10000 {
			units++
		}
		b = b[size:]
	}
	return units
}
