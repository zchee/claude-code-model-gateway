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

package upstream_test

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// bodyGarbage is what the fake writes on the raw connection after part of a body: text, not a TLS
// record. Its first byte is no alert record type, and its second and third bytes, read as a record
// version, are not the 0x0303 every record of a TLS 1.2 or 1.3 connection carries, so the client's
// TLS layer rejects it as a record header and quotes that version in its error.
const bodyGarbage = "GARBAGE " + wireMarker + " not a TLS record\r\n"

// bodyHead returns a response head that declares a body of length bytes.
func bodyHead(length int) string {
	return "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: " + strconv.Itoa(length) + "\r\n\r\n"
}

// readUntilError reads body with a small buffer until a read returns an error, and returns the
// bytes read and that error exactly as Read returned it.
func readUntilError(body io.Reader) ([]byte, error) {
	var got []byte
	buf := make([]byte, 16)
	for {
		n, err := body.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			return got, err
		}
	}
}

func TestBodyReadHidesBrokenTLSRecords(t *testing.T) {
	const (
		partialBody  = "data: first\n\n"
		completeBody = "data: first\n\ndata: second\n\n"
	)

	tests := map[string]struct {
		// send writes the response after the fake has read the request, through the TLS connection
		// tc or, for bytes that are not TLS, on the raw connection under it.
		send func(raw net.Conn, tc *tls.Conn) error
		// wantBody is every byte the body yields before its read error.
		wantBody string
		// check asserts the error the body's Read returned.
		check func(t *testing.T, err error)
	}{
		"error: a record that is not TLS after part of the body is reported with the fixed text": {
			send: func(raw net.Conn, tc *tls.Conn) error {
				// One write, so the head and the start of the body arrive in one record and the
				// client parses the head without reading the next one.
				if _, err := io.WriteString(tc, bodyHead(len(completeBody))+partialBody); err != nil {
					return err
				}
				_, err := io.WriteString(raw, bodyGarbage)
				return err
			},
			wantBody: partialBody,
			check: func(t *testing.T, err error) {
				if got := err.Error(); got != tlsFailureText {
					t.Errorf("body read error text = %q, want exactly %q", got, tlsFailureText)
				}
				// The TLS layer quotes the record version the peer sent, in hexadecimal.
				peerVersion := fmt.Sprintf("%x", bodyGarbage[1:3])
				for _, peerText := range []string{bodyGarbage, wireMarker, peerVersion} {
					if strings.Contains(err.Error(), peerText) {
						t.Errorf("body read error text %q repeats the peer's %q", err, peerText)
					}
				}
				recordErr, ok := errors.AsType[tls.RecordHeaderError](err)
				if !ok {
					t.Fatalf("body read error = %v (%T), want tls.RecordHeaderError in the unwrap chain", err, err)
				}
				if diff := gocmp.Diff([5]byte([]byte(bodyGarbage[:5])), recordErr.RecordHeader); diff != "" {
					t.Errorf("tls.RecordHeaderError.RecordHeader is not the first bytes the peer sent (-want +got):\n%s", diff)
				}
				// Without the fixed text the peer's bytes would reach the error text; a TLS layer
				// that stopped quoting them would leave this test unable to see a leak.
				if !strings.Contains(recordErr.Msg, peerVersion) {
					t.Errorf("tls.RecordHeaderError message %q lacks the peer's record version %q; the test could not see a leak", recordErr.Msg, peerVersion)
				}
			},
		},
		"success: a complete body ends with io.EOF itself": {
			send: func(_ net.Conn, tc *tls.Conn) error {
				_, err := io.WriteString(tc, bodyHead(len(completeBody))+completeBody)
				return err
			},
			wantBody: completeBody,
			check: func(t *testing.T, err error) {
				if err != io.EOF { //nolint:errorlint // A reader ends with io.EOF itself, never a wrapped one; io.Copy and bufio compare it with ==.
					t.Errorf("body read error at the end of a complete body = %v (%T), want io.EOF itself", err, err)
				}
			},
		},
		"error: a connection closed in the middle of a body of declared length keeps io.ErrUnexpectedEOF": {
			send: func(_ net.Conn, tc *tls.Conn) error {
				if _, err := io.WriteString(tc, bodyHead(len(completeBody))+partialBody); err != nil {
					return err
				}
				// tc.Close sends the TLS close alert before closing the raw connection. The alert
				// lets the client finish reading the body, so the test can end and the fake's
				// cleanup, not the client, can close the server's raw connection before tc.Close
				// reaches its own close; that close then reports net.ErrClosed.
				if err := tc.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					return err
				}
				return nil
			},
			wantBody: partialBody,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("body read error of a body cut short = %v (%T), want io.ErrUnexpectedEOF in the unwrap chain", err, err)
				}
				if got, want := err.Error(), io.ErrUnexpectedEOF.Error(); got != want {
					t.Errorf("body read error text of a body cut short = %q, want io.ErrUnexpectedEOF's own text %q", got, want)
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cert, roots := newTrustedServerCert(t)
			fake := startLoopbackFake(t, afterHandshake(t, cert, tt.send))
			client := upstream.NewClient(upstream.Options{DialContext: fake.dialFake, TLSConfig: &tls.Config{RootCAs: roots}})
			defer func() { _ = client.Close() }()

			req := messagesRequest()
			req.BaseURL = "https://" + tlsDialName
			resp, err := doWithoutPanic(t, client, req)
			if err != nil {
				t.Fatalf("Do = %v, want the response whose head the fake served completely", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("Do status = %d, want %d", resp.StatusCode, http.StatusOK)
			}

			gotBody, err := readUntilError(resp.Body)
			t.Logf("body read error: %v (%T)", err, err)
			if diff := gocmp.Diff(tt.wantBody, string(gotBody)); diff != "" {
				t.Errorf("body bytes before the read error (-want +got):\n%s", diff)
			}
			tt.check(t, err)
		})
	}
}
