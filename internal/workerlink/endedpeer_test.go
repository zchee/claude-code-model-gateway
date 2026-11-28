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

//go:build unix

package workerlink

import (
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The names each refusal must carry for the step that decided it, in the order the steps decide.
const (
	testStepCredentials = "LOCAL_PEERCRED"
	testStepPeerPID     = "LOCAL_PEERPID"
	testStepReadFilter  = "read filter"
	testStepPeek        = "receive peek"
)

// Distinct errors for the steps that fail for some other reason, so that a chain assertion can only hold when the
// refusal carries that very error.
var (
	errTestCredentialsOther = errors.New("test: the peer-credentials query failed for another reason")
	errTestPeerPIDOther     = errors.New("test: the peer-pid query failed for another reason")
	errTestReadFilterQuery  = errors.New("test: the read filter query failed")
	errTestPeekOther        = errors.New("test: the receive peek failed for another reason")
)

// testGetpeerEINVAL is the getpeername failure every row starts from unless it removes it.
var testGetpeerEINVAL = os.NewSyscallError("getpeername", unix.EINVAL)

// testEvidence returns what the system reports about a socket whose peer has ended and that is a usable drain
// channel: getpeername failed with EINVAL, the credentials query was made and answered, no peer is linked any more,
// the read filter reports the end of input and the peek was made and answered zero. change, when not nil, alters that
// report.
func testEvidence(change func(*peerEvidence)) peerEvidence {
	e := peerEvidence{getpeerErr: testGetpeerEINVAL, credAsked: true, peerPIDErr: unix.ENOTCONN, readEOF: true, peekAsked: true}
	if change != nil {
		change(&e)
	}
	return e
}

// TestPeerEvidenceDecide decides from reported facts alone, with no socket and no system call, whether a local stream
// socket whose getpeername failed with EINVAL is accepted as a drain channel whose peer has ended. It is accepted
// only when the credentials query was made and answered, the peer-pid query failed with ENOTCONN, the read filter
// reported the end of input and the receive peek was made and answered with a count of zero or more. Otherwise the
// steps decide in the order credentials, peer pid, read filter, peek, and the first step that refuses is the one the
// refusal names: its text carries that step's name and, where the step failed, its error is in the chain, while the
// errors and the names of the later steps are not. A step that was not made refuses at its own place in that order,
// so an omitted step can never pass for a measured answer. ENOTCONN counts wrapped as it does bare, and only the
// credentials step's ENOTCONN is the import's usual not-connected refusal. Every refusal keeps the getpeername
// failure in its chain when one was reported, and reads cleanly when none was.
func TestPeerEvidenceDecide(t *testing.T) {
	// laterSteps maps each step to the steps that decide after it.
	laterSteps := map[string][]string{
		testStepCredentials: {testStepPeerPID, testStepReadFilter, testStepPeek},
		testStepPeerPID:     {testStepReadFilter, testStepPeek},
		testStepReadFilter:  {testStepPeek},
		testStepPeek:        nil,
	}
	tests := map[string]struct {
		evidence peerEvidence
		// step is the step the refusal must name, empty when the socket is accepted.
		step string
		// is holds the errors the refusal's chain must contain, beyond the getpeername failure.
		is []error
		// isNot holds the errors the refusal's chain must not contain.
		isNot []error
		// text holds further substrings the refusal's text must contain.
		text []string
		// notText holds substrings the refusal's text must not contain.
		notText []string
	}{
		"success: an ended peer at the end of its input is accepted": {
			evidence: testEvidence(nil),
		},
		"success: an ended peer with bytes still queued is accepted": {
			evidence: testEvidence(func(e *peerEvidence) { e.peekN = 5 }),
		},
		"success: an ended peer is accepted when no getpeername failure was reported": {
			evidence: testEvidence(func(e *peerEvidence) { e.getpeerErr = nil }),
		},
		"success: a peer-pid query that failed with a wrapped ENOTCONN counts as no linked peer": {
			evidence: testEvidence(func(e *peerEvidence) { e.peerPIDErr = os.NewSyscallError("getsockopt", unix.ENOTCONN) }),
		},

		"error: a socket that was never connected gets the usual not-connected refusal": {
			evidence: testEvidence(func(e *peerEvidence) { e.credErr = unix.ENOTCONN }),
			step:     testStepCredentials, is: []error{errNotConnected},
		},
		"error: a credentials query that failed with a wrapped ENOTCONN gets the usual not-connected refusal": {
			evidence: testEvidence(func(e *peerEvidence) { e.credErr = os.NewSyscallError("getsockopt", unix.ENOTCONN) }),
			step:     testStepCredentials, is: []error{errNotConnected},
		},
		"error: a credentials query that failed for another reason is refused with that error": {
			evidence: testEvidence(func(e *peerEvidence) { e.credErr = errTestCredentialsOther }),
			step:     testStepCredentials, is: []error{errTestCredentialsOther}, isNot: []error{errNotConnected},
		},
		"error: a failed credentials query decides before every later refusal": {
			evidence: testEvidence(func(e *peerEvidence) {
				e.credErr, e.peerPIDErr, e.readErr, e.peekErr = errTestCredentialsOther, errTestPeerPIDOther, errTestReadFilterQuery, errTestPeekOther
			}),
			step: testStepCredentials, is: []error{errTestCredentialsOther},
			isNot: []error{errNotConnected, errTestPeerPIDOther, errTestReadFilterQuery, errTestPeekOther},
		},
		"error: a never-connected socket decides before a still linked peer": {
			evidence: testEvidence(func(e *peerEvidence) { e.credErr, e.peerPIDErr = unix.ENOTCONN, nil }),
			step:     testStepCredentials, is: []error{errNotConnected},
		},

		"error: a peer-pid query that answered means the socket is still linked to a peer": {
			evidence: testEvidence(func(e *peerEvidence) { e.peerPIDErr = nil }),
			step:     testStepPeerPID,
		},
		"error: a peer-pid query that failed for another reason is refused with that error": {
			evidence: testEvidence(func(e *peerEvidence) { e.peerPIDErr = errTestPeerPIDOther }),
			step:     testStepPeerPID, is: []error{errTestPeerPIDOther},
		},
		"error: a still linked peer decides before a socket that is not at the end of its input": {
			evidence: testEvidence(func(e *peerEvidence) {
				e.peerPIDErr, e.readEOF, e.readReport = nil, false, "test report: a listener with pending connections"
			}),
			step: testStepPeerPID, notText: []string{"test report: a listener with pending connections"},
		},
		"error: a failed peer-pid query decides before failed read filter and peek": {
			evidence: testEvidence(func(e *peerEvidence) {
				e.peerPIDErr, e.readErr, e.peekErr = errTestPeerPIDOther, errTestReadFilterQuery, errTestPeekOther
			}),
			step: testStepPeerPID, is: []error{errTestPeerPIDOther}, isNot: []error{errTestReadFilterQuery, errTestPeekOther},
		},

		"error: a failed read filter query is refused with that error": {
			evidence: testEvidence(func(e *peerEvidence) { e.readEOF, e.readErr = false, errTestReadFilterQuery }),
			step:     testStepReadFilter, is: []error{errTestReadFilterQuery},
		},
		"error: a failed read filter query is refused even beside a reported end of input": {
			evidence: testEvidence(func(e *peerEvidence) { e.readErr = errTestReadFilterQuery }),
			step:     testStepReadFilter, is: []error{errTestReadFilterQuery},
		},
		"error: a read filter that reports no end of input is refused with its report": {
			evidence: testEvidence(func(e *peerEvidence) {
				e.readEOF, e.readReport = false, "test report: a listener with pending connections"
			}),
			step: testStepReadFilter, text: []string{"test report: a listener with pending connections"},
		},
		"error: a failed read filter query decides before a failed peek": {
			evidence: testEvidence(func(e *peerEvidence) {
				e.readEOF, e.readErr, e.peekErr = false, errTestReadFilterQuery, errTestPeekOther
			}),
			step: testStepReadFilter, is: []error{errTestReadFilterQuery}, isNot: []error{errTestPeekOther},
		},
		"error: a read filter without the end of input decides before a failed peek": {
			evidence: testEvidence(func(e *peerEvidence) {
				e.readEOF, e.readReport, e.peekErr = false, "test report: 3 bytes readable", errTestPeekOther
			}),
			step: testStepReadFilter, text: []string{"test report: 3 bytes readable"}, isNot: []error{errTestPeekOther},
		},

		"error: a peek that fails as not connected, as a defunct socket does, is refused": {
			evidence: testEvidence(func(e *peerEvidence) { e.peekErr = unix.ENOTCONN }),
			step:     testStepPeek, is: []error{unix.ENOTCONN}, isNot: []error{errNotConnected},
		},
		"error: a peek that would block is refused": {
			evidence: testEvidence(func(e *peerEvidence) { e.peekErr = unix.EAGAIN }),
			step:     testStepPeek, is: []error{unix.EAGAIN},
		},
		"error: a peek that failed for another reason is refused with that error": {
			evidence: testEvidence(func(e *peerEvidence) { e.peekErr = errTestPeekOther }),
			step:     testStepPeek, is: []error{errTestPeekOther},
		},
		"error: a failed peek is refused even with a count of queued bytes": {
			evidence: testEvidence(func(e *peerEvidence) { e.peekN, e.peekErr = 5, errTestPeekOther }),
			step:     testStepPeek, is: []error{errTestPeekOther},
		},
		"error: a negative peek count is refused": {
			evidence: testEvidence(func(e *peerEvidence) { e.peekN = -1 }),
			step:     testStepPeek,
		},

		"error: a credentials refusal reads cleanly without a getpeername failure": {
			evidence: testEvidence(func(e *peerEvidence) { e.getpeerErr, e.credErr = nil, errTestCredentialsOther }),
			step:     testStepCredentials, is: []error{errTestCredentialsOther},
		},
		"error: a not-connected refusal reads cleanly without a getpeername failure": {
			evidence: testEvidence(func(e *peerEvidence) { e.getpeerErr, e.credErr = nil, unix.ENOTCONN }),
			step:     testStepCredentials, is: []error{errNotConnected},
		},
		"error: a peer-pid refusal reads cleanly without a getpeername failure": {
			evidence: testEvidence(func(e *peerEvidence) { e.getpeerErr, e.peerPIDErr = nil, nil }),
			step:     testStepPeerPID,
		},
		"error: a read filter refusal reads cleanly without a getpeername failure": {
			evidence: testEvidence(func(e *peerEvidence) {
				e.getpeerErr, e.readEOF, e.readReport = nil, false, "test report: 1 byte readable"
			}),
			step: testStepReadFilter, text: []string{"test report: 1 byte readable"},
		},
		"error: a peek refusal reads cleanly without a getpeername failure": {
			evidence: testEvidence(func(e *peerEvidence) { e.getpeerErr, e.peekN = nil, -1 }),
			step:     testStepPeek,
		},

		"error: a credentials query that was not made is refused": {
			evidence: testEvidence(func(e *peerEvidence) { e.credAsked = false }),
			step:     testStepCredentials, isNot: []error{errNotConnected},
		},
		"error: a credentials query that was not made is refused although its error field is set": {
			evidence: testEvidence(func(e *peerEvidence) { e.credAsked, e.credErr = false, errTestCredentialsOther }),
			step:     testStepCredentials,
		},
		"error: a credentials query that was not made decides before every later refusal": {
			evidence: testEvidence(func(e *peerEvidence) {
				e.credAsked, e.peerPIDErr, e.readErr, e.peekErr = false, nil, errTestReadFilterQuery, errTestPeekOther
				e.peekAsked = false
			}),
			step: testStepCredentials, isNot: []error{errNotConnected, errTestReadFilterQuery, errTestPeekOther},
		},
		"error: a receive peek that was not made is refused although every other step accepts": {
			evidence: testEvidence(func(e *peerEvidence) { e.peekAsked = false }),
			step:     testStepPeek,
		},
		"error: a receive peek that was not made is refused although a count of queued bytes is set": {
			evidence: testEvidence(func(e *peerEvidence) { e.peekAsked, e.peekN = false, 5 }),
			step:     testStepPeek,
		},
		"error: a receive peek that was not made is refused although its error field is set": {
			evidence: testEvidence(func(e *peerEvidence) { e.peekAsked, e.peekErr = false, errTestPeekOther }),
			step:     testStepPeek,
		},
		"error: neither the credentials query nor the receive peek was made": {
			evidence: testEvidence(func(e *peerEvidence) { e.credAsked, e.peekAsked = false, false }),
			step:     testStepCredentials, isNot: []error{errNotConnected},
		},
		"error: a failed credentials query decides before a receive peek that was not made": {
			evidence: testEvidence(func(e *peerEvidence) { e.credErr, e.peekAsked = errTestCredentialsOther, false }),
			step:     testStepCredentials, is: []error{errTestCredentialsOther},
		},
		"error: a still linked peer decides before a receive peek that was not made": {
			evidence: testEvidence(func(e *peerEvidence) { e.peerPIDErr, e.peekAsked = nil, false }),
			step:     testStepPeerPID,
		},
		"error: a read filter without the end of input decides before a receive peek that was not made": {
			evidence: testEvidence(func(e *peerEvidence) {
				e.readEOF, e.readReport, e.peekAsked = false, "test report: 2 bytes readable", false
			}),
			step: testStepReadFilter, text: []string{"test report: 2 bytes readable"},
		},
		"error: a credentials query that was not made reads cleanly without a getpeername failure": {
			evidence: testEvidence(func(e *peerEvidence) { e.getpeerErr, e.credAsked = nil, false }),
			step:     testStepCredentials,
		},
		"error: a receive peek that was not made reads cleanly without a getpeername failure": {
			evidence: testEvidence(func(e *peerEvidence) { e.getpeerErr, e.peekAsked = nil, false }),
			step:     testStepPeek,
		},

		"error: the zero value is refused at the credentials step": {
			evidence: peerEvidence{},
			step:     testStepCredentials,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := tt.evidence.decide()
			t.Logf("decide(%+v) = %v", tt.evidence, err)
			if tt.step == "" {
				if err != nil {
					t.Fatalf("decide refused a usable drain channel whose peer has ended: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("decide accepted the socket; want a refusal at the %s step", tt.step)
			}
			text := err.Error()
			if !strings.Contains(text, tt.step) {
				t.Errorf("refusal %q does not name the deciding step %q", text, tt.step)
			}
			for _, later := range laterSteps[tt.step] {
				if strings.Contains(text, later) {
					t.Errorf("refusal %q names the %q step, which decides only after %q", text, later, tt.step)
				}
			}
			for _, artefact := range []string{"%!", "<nil>"} {
				if strings.Contains(text, artefact) {
					t.Errorf("refusal %q contains the formatting artefact %q", text, artefact)
				}
			}
			if tt.step != testStepCredentials && errors.Is(err, errNotConnected) {
				t.Errorf("refusal %q at the %s step is the not-connected refusal, which only the credentials step gives", text, tt.step)
			}
			if tt.evidence.getpeerErr != nil && !errors.Is(err, unix.EINVAL) {
				t.Errorf("refusal %q does not keep the getpeername failure %v in its chain", text, tt.evidence.getpeerErr)
			}
			for _, want := range tt.is {
				if !errors.Is(err, want) {
					t.Errorf("refusal %q does not carry %q in its chain", text, want)
				}
			}
			for _, unwanted := range tt.isNot {
				if errors.Is(err, unwanted) {
					t.Errorf("refusal %q carries %q in its chain", text, unwanted)
				}
			}
			for _, want := range tt.text {
				if !strings.Contains(text, want) {
					t.Errorf("refusal %q does not contain %q", text, want)
				}
			}
			for _, unwanted := range tt.notText {
				if strings.Contains(text, unwanted) {
					t.Errorf("refusal %q contains %q", text, unwanted)
				}
			}
		})
	}
}
