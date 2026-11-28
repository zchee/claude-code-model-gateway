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

package workerproc_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/workerproc"
)

// restartTLaunch says how a row makes one launch of its rule. The zero value makes the zero Launch.
type restartTLaunch struct {
	// stable makes the launch by accepting exe as the stable binary with owner; otherwise NewConfiguredLaunch makes it.
	stable  bool
	exe     workerproc.Executable
	version string
	owner   int
}

// restartTConfiguredSpec is the configured executable's launch with version.
func restartTConfiguredSpec(e workerproc.Executable, version string) restartTLaunch {
	return restartTLaunch{exe: e, version: version}
}

// restartTStableSpec is a launch accepted for e as the stable binary of launchTOwner with version.
func restartTStableSpec(e workerproc.Executable, version string) restartTLaunch {
	return restartTLaunch{stable: true, exe: e, version: version, owner: launchTOwner}
}

// restartTRule says how a row makes its RestartRule.
type restartTRule struct {
	configured restartTLaunch
	// currentIsConfigured makes Current the same value as Configured, as it is before any restart accepted a candidate.
	currentIsConfigured bool
	current             restartTLaunch
	stable              workerproc.Executable
	uid                 int
}

// restartTFresh is the rule before any restart: the configured executable at version is the current choice and the
// stable binary is installed.
func restartTFresh(version string) restartTRule {
	return restartTRule{
		configured:          restartTConfiguredSpec(launchTConfiguredExe, version),
		currentIsConfigured: true,
		stable:              launchTStableExe,
		uid:                 launchTOwner,
	}
}

// restartTAdopted is the rule after a restart adopted the stable binary at version current while the configured
// executable reports version configured; the stable binary at its path is unchanged since.
func restartTAdopted(configured, current string) restartTRule {
	return restartTRule{
		configured: restartTConfiguredSpec(launchTConfiguredExe, configured),
		current:    restartTStableSpec(launchTStableExe, current),
		stable:     launchTStableExe,
		uid:        launchTOwner,
	}
}

// restartTMake makes the launch spec describes.
func restartTMake(t *testing.T, spec restartTLaunch) workerproc.Launch {
	t.Helper()
	switch {
	case spec == restartTLaunch{}:
		return workerproc.Launch{}
	case spec.stable:
		return launchTStable(t, spec.exe, spec.owner, spec.version)
	default:
		return launchTConfigured(t, spec.exe, spec.version)
	}
}

// restartTBuild makes the RestartRule spec describes.
func restartTBuild(t *testing.T, spec restartTRule) workerproc.RestartRule {
	t.Helper()
	r := workerproc.RestartRule{Configured: restartTMake(t, spec.configured), Stable: spec.stable, UID: spec.uid}
	if spec.currentIsConfigured {
		r.Current = r.Configured
	} else {
		r.Current = restartTMake(t, spec.current)
	}
	return r
}

// restartTOutcome is what Judge decides for a candidate.
type restartTOutcome int

const (
	// restartTRefused is the frozen refusal: the zero Launch, false and an error matching ErrCandidate.
	restartTRefused restartTOutcome = iota
	// restartTCurrent returns the rule's current choice itself.
	restartTCurrent
	// restartTConfigured returns the rule's configured launch itself.
	restartTConfigured
	// restartTNeedVersion returns the zero Launch, true and no error.
	restartTNeedVersion
)

// restartTCheckJudge runs Judge and compares its result with the outcome, and for an outcome that returns a launch
// with the image and version that launch must show.
func restartTCheckJudge(t *testing.T, r workerproc.RestartRule, c workerproc.Executable, want restartTOutcome, wantImage procinfo.Image, wantVersion string) {
	t.Helper()
	l, needVersion, err := r.Judge(c)
	switch want {
	case restartTRefused, restartTNeedVersion:
		wantNeed := want == restartTNeedVersion
		if l != (workerproc.Launch{}) {
			t.Errorf("Judge(%+v) launch = %+v, want the zero Launch", c, l)
		}
		if needVersion != wantNeed {
			t.Errorf("Judge(%+v) needVersion = %t, want %t", c, needVersion, wantNeed)
		}
		if wantNeed && err != nil {
			t.Errorf("Judge(%+v) error = %v, want nil", c, err)
		}
		if !wantNeed && !errors.Is(err, workerproc.ErrCandidate) {
			t.Errorf("Judge(%+v) error = %v, want an error matching ErrCandidate", c, err)
		}
	case restartTCurrent, restartTConfigured:
		if err != nil || needVersion {
			t.Fatalf("Judge(%+v) = (%+v, %t, %v), want an accepted launch, false and nil", c, l, needVersion, err)
		}
		wantLaunch, what := r.Current, "the current choice"
		if want == restartTConfigured {
			wantLaunch, what = r.Configured, "the configured launch"
		}
		if l != wantLaunch {
			t.Errorf("Judge(%+v) launch = %+v, want %s %+v", c, l, what, wantLaunch)
		}
		if diff := gocmp.Diff(wantImage, l.Image()); diff != "" {
			t.Errorf("Judge(%+v) launch Image() mismatch (-want +got):\n%s", c, diff)
		}
		if diff := gocmp.Diff(wantVersion, l.Version()); diff != "" {
			t.Errorf("Judge(%+v) launch Version() mismatch (-want +got):\n%s", c, diff)
		}
	default:
		t.Fatalf("unknown outcome %d", want)
	}
}

// restartTCheckRefusedLaunch fails the row unless (l, err) is the frozen refusal of AcceptVersion.
func restartTCheckRefusedLaunch(t *testing.T, what string, l workerproc.Launch, err error) {
	t.Helper()
	if l != (workerproc.Launch{}) {
		t.Errorf("%s launch = %+v, want the zero Launch", what, l)
	}
	if !errors.Is(err, workerproc.ErrCandidate) {
		t.Errorf("%s error = %v, want an error matching ErrCandidate", what, err)
	}
}

// restartTCheckStable fails the row unless (l, err) is an accepted stable binary with the image and version given,
// authorised for the owner of e: it matches its own inspection, and no longer matches one of any other owner (the
// root owner, launchTOwner, launchTOtherOwner and e.UID+1, whichever are not e.UID) or with group write permission.
func restartTCheckStable(t *testing.T, what string, l workerproc.Launch, err error, e workerproc.Executable, wantImage procinfo.Image, wantVersion string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s error = %v, want nil", what, err)
	}
	if diff := gocmp.Diff(wantImage, l.Image()); diff != "" {
		t.Errorf("%s launch Image() mismatch (-want +got):\n%s", what, diff)
	}
	if diff := gocmp.Diff(wantVersion, l.Version()); diff != "" {
		t.Errorf("%s launch Version() mismatch (-want +got):\n%s", what, diff)
	}
	if !l.Matches(e) {
		t.Errorf("%s launch does not match the inspection it was accepted from (owner %d)", what, e.UID)
	}
	for _, other := range []int{0, launchTOwner, launchTOtherOwner, e.UID + 1} {
		if other != e.UID && l.Matches(launchTUID(e, other)) {
			t.Errorf("%s launch accepted for owner %d matches an inspection of owner %d; a stable binary keeps its owner", what, e.UID, other)
		}
	}
	if l.Matches(launchTMode(e, 0o775)) {
		t.Errorf("%s launch matches an inspection with group write permission; a stable binary refuses it", what)
	}
}

func TestRestartCandidate(t *testing.T) {
	t.Run("ParseRestart", func(t *testing.T) {
		tests := map[string]struct {
			body string
			// nilBody passes a nil slice instead of the bytes of body.
			nilBody bool
			want    string
			wantErr bool
		}{
			"success: an empty object names no candidate": {
				body: `{}`,
				want: "",
			},
			"success: an object with only other members names no candidate": {
				body: `{"x":1,"reason":"update"}`,
				want: "",
			},
			"success: member names are case-sensitive": {
				body: `{"Script":"bin/x","BINARY":"bin/y"}`,
				want: "",
			},
			"success: JSON white space around the object": {
				body: " \t\r\n{}\n\t \r",
				want: "",
			},
			"success: JSON white space inside the object": {
				body: "{ \"script\" :\n\t\"/opt/bin/model-gateway\" \r\n}",
				want: "/opt/bin/model-gateway",
			},
			"success: script names the candidate": {
				body: `{"script":"/home/user/.claude/model-gateway/bin/model-gateway"}`,
				want: "/home/user/.claude/model-gateway/bin/model-gateway",
			},
			"success: binary names the candidate": {
				body: `{"binary":"/home/user/.claude/model-gateway/bin/model-gateway"}`,
				want: "/home/user/.claude/model-gateway/bin/model-gateway",
			},
			"success: script and binary with equal strings": {
				body: `{"script":"/opt/bin/model-gateway","binary":"/opt/bin/model-gateway"}`,
				want: "/opt/bin/model-gateway",
			},
			"success: binary before script with equal strings": {
				body: `{"binary":"/opt/bin/model-gateway","script":"/opt/bin/model-gateway"}`,
				want: "/opt/bin/model-gateway",
			},
			"success: script and binary equal after decoding an escaped slash": {
				body: `{"script":"/a","binary":"\/a"}`,
				want: "/a",
			},
			"success: script and binary equal after decoding unicode escapes": {
				body: "{\"script\":\"\\u002fopt\\u002fbin\",\"binary\":\"/opt/bin\"}",
				want: "/opt/bin",
			},
			"success: the candidate is decoded": {
				body: "{\"script\":\"\\/opt\\/model\\u002dgateway\"}",
				want: "/opt/model-gateway",
			},
			"success: binary is decoded": {
				body: "{\"binary\":\"\\/opt\\/model\\u002dgateway\"}",
				want: "/opt/model-gateway",
			},
			"success: a member name is decoded before it is read": {
				body: "{\"scr\\u0069pt\":\"/opt/bin/model-gateway\"}",
				want: "/opt/bin/model-gateway",
			},
			"success: an escaped binary name is decoded before it is read": {
				body: "{\"\\u0062inary\":\"/opt/bin/model-gateway\"}",
				want: "/opt/bin/model-gateway",
			},
			"success: an escaped name and a plain name of the two aliases with equal strings": {
				body: "{\"\\u0073cript\":\"/opt/bin/model-gateway\",\"binary\":\"\\/opt\\/bin\\/model-gateway\"}",
				want: "/opt/bin/model-gateway",
			},
			"success: an absolute path that is not clean is returned verbatim": {
				body: `{"script":"/opt//bin/../lib/./model-gateway/"}`,
				want: "/opt//bin/../lib/./model-gateway/",
			},
			"success: binary with a path that is not clean is returned verbatim": {
				body: `{"binary":"/opt//bin/../lib/./model-gateway/"}`,
				want: "/opt//bin/../lib/./model-gateway/",
			},
			"success: the root directory is an absolute path": {
				body: `{"script":"/"}`,
				want: "/",
			},
			"success: a path with UTF-8 names": {
				body: `{"script":"/opt/ゲートウェイ/model-gateway"}`,
				want: "/opt/ゲートウェイ/model-gateway",
			},
			"success: a valid escaped surrogate pair in the candidate": {
				body: "{\"script\":\"/opt/\\ud83d\\ude00/model-gateway\"}",
				want: "/opt/\U0001F600/model-gateway",
			},
			"success: a valid escaped surrogate pair in another member": {
				body: "{\"note\":\"\\ud83d\\ude00\",\"script\":\"/opt/bin/model-gateway\"}",
				want: "/opt/bin/model-gateway",
			},
			"success: a valid escaped surrogate pair in a member name": {
				body: "{\"\\ud83d\\ude00\":1}",
				want: "",
			},
			"success: a character outside the basic plane written directly in UTF-8": {
				body: "{\"script\":\"/opt/\xf0\x9f\x98\x80/model-gateway\"}",
				want: "/opt/\U0001F600/model-gateway",
			},
			"success: script with an escaped surrogate pair equals binary with the character in UTF-8": {
				body: "{\"script\":\"/opt/\\ud83d\\ude00/model-gateway\",\"binary\":\"/opt/\xf0\x9f\x98\x80/model-gateway\"}",
				want: "/opt/\U0001F600/model-gateway",
			},
			"success: binary with an escaped surrogate pair equals script with the character in UTF-8": {
				body: "{\"script\":\"/opt/\xf0\x9f\x98\x80/model-gateway\",\"binary\":\"/opt/\\ud83d\\ude00/model-gateway\"}",
				want: "/opt/\U0001F600/model-gateway",
			},
			"success: a replacement character written directly in UTF-8 in the candidate": {
				body: "{\"script\":\"/opt/\xef\xbf\xbd/model-gateway\"}",
				want: "/opt/\uFFFD/model-gateway",
			},
			"success: an escaped replacement character in the candidate": {
				body: `{"script":"/opt/\ufffd/model-gateway"}`,
				want: "/opt/\uFFFD/model-gateway",
			},
			"success: an escaped backslash before the text of a surrogate escape in another member": {
				body: `{"note":"\\ud800","script":"/ok"}`,
				want: "/ok",
			},
			"success: an escaped backslash before the text of a surrogate escape in the candidate": {
				body: `{"script":"/opt/\\ud800"}`,
				want: `/opt/\ud800`,
			},
			"success: names that differ only in letter case are not duplicates": {
				body: `{"a":1,"A":2}`,
				want: "",
			},
			"success: a member named like script in another letter case is another member": {
				body: `{"script":"/opt/bin/model-gateway","Script":"relative","BINARY":null}`,
				want: "/opt/bin/model-gateway",
			},
			"success: other members are ignored whatever their value": {
				body: `{"a":null,"b":true,"c":false,"d":-1.5e-3,"e":"x","f":[1,[2,{"g":{}}]],"h":{"i":{"j":null}},"script":"/opt/bin/model-gateway"}`,
				want: "/opt/bin/model-gateway",
			},
			"success: another member holding a number too large for a float": {
				body: `{"n":1e400,"m":-1e400,"script":"/opt/bin/model-gateway"}`,
				want: "/opt/bin/model-gateway",
			},
			"success: another member holding a long integer": {
				body: `{"n":123456789012345678901234567890,"script":"/opt/bin/model-gateway"}`,
				want: "/opt/bin/model-gateway",
			},
			"success: equal names in different objects are not duplicates": {
				body: `{"x":{"a":1},"y":{"a":2},"z":[{"a":3},{"a":4}],"a":5}`,
				want: "",
			},
			"success: script inside a nested object is not the candidate": {
				body: `{"x":{"script":"relative","binary":null}}`,
				want: "",
			},
			"success: an empty member name": {
				body: `{"":1}`,
				want: "",
			},
			"success: a body longer than any request limit": {
				body: `{"script":"/opt/bin/model-gateway","pad":"` + strings.Repeat("x", 70000) + `"}`,
				want: "/opt/bin/model-gateway",
			},
			"success: a candidate longer than any request limit": {
				body: `{"script":"/` + strings.Repeat("a", 5000) + `"}`,
				want: "/" + strings.Repeat("a", 5000),
			},
			"error: an empty body": {
				body:    ``,
				wantErr: true,
			},
			"error: a nil body": {
				nilBody: true,
				wantErr: true,
			},
			"error: white space only": {
				body:    " \t\r\n",
				wantErr: true,
			},
			"error: an unterminated object": {
				body:    `{"script":"/opt/bin/model-gateway"`,
				wantErr: true,
			},
			"error: an unterminated string": {
				body:    `{"script":"/opt/bin/model-gateway}`,
				wantErr: true,
			},
			"error: a trailing comma": {
				body:    `{"script":"/opt/bin/model-gateway",}`,
				wantErr: true,
			},
			"error: a single-quoted string": {
				body:    `{'script':'/opt/bin/model-gateway'}`,
				wantErr: true,
			},
			"error: an unquoted name": {
				body:    `{script:"/opt/bin/model-gateway"}`,
				wantErr: true,
			},
			"error: a comment": {
				body:    `{/* restart */}`,
				wantErr: true,
			},
			"error: a number with a leading zero": {
				body:    `{"n":01}`,
				wantErr: true,
			},
			"error: NaN": {
				body:    `{"n":NaN}`,
				wantErr: true,
			},
			"error: a raw control character in a string": {
				body:    "{\"script\":\"/opt/bin\tmodel-gateway\"}",
				wantErr: true,
			},
			"error: an invalid escape": {
				body:    `{"script":"\/opt\x41"}`,
				wantErr: true,
			},
			"error: a second object after the first": {
				body:    `{} {}`,
				wantErr: true,
			},
			"error: a closing brace after the object": {
				body:    `{"script":"/opt/bin/model-gateway"}}`,
				wantErr: true,
			},
			"error: a number after the object": {
				body:    `{} 1`,
				wantErr: true,
			},
			"error: a letter after the object": {
				body:    `{}x`,
				wantErr: true,
			},
			"error: a NUL byte after the object": {
				body:    "{}\x00",
				wantErr: true,
			},
			"error: a form feed is not JSON white space": {
				body:    "\f{}",
				wantErr: true,
			},
			"error: a vertical tab is not JSON white space": {
				body:    "{}\v",
				wantErr: true,
			},
			"error: a no-break space is not JSON white space": {
				body:    "\xc2\xa0{}",
				wantErr: true,
			},
			"error: a byte order mark before the object": {
				body:    "\xef\xbb\xbf{}",
				wantErr: true,
			},
			"error: null": {
				body:    `null`,
				wantErr: true,
			},
			"error: an array": {
				body:    `[]`,
				wantErr: true,
			},
			"error: an array holding a candidate object": {
				body:    `[{"script":"/opt/bin/model-gateway"}]`,
				wantErr: true,
			},
			"error: a string": {
				body:    `"/opt/bin/model-gateway"`,
				wantErr: true,
			},
			"error: a number": {
				body:    `1`,
				wantErr: true,
			},
			"error: true": {
				body:    `true`,
				wantErr: true,
			},
			"error: false": {
				body:    `false`,
				wantErr: true,
			},
			"error: two members with one name": {
				body:    `{"a":1,"a":2}`,
				wantErr: true,
			},
			"error: two members with one name and equal values": {
				body:    `{"a":1,"a":1}`,
				wantErr: true,
			},
			"error: script twice with equal values": {
				body:    `{"script":"/opt/bin/model-gateway","script":"/opt/bin/model-gateway"}`,
				wantErr: true,
			},
			"error: binary twice with equal values": {
				body:    `{"binary":"/opt/bin/model-gateway","binary":"/opt/bin/model-gateway"}`,
				wantErr: true,
			},
			"error: script twice when one name is escaped": {
				body:    "{\"script\":\"/opt/bin/model-gateway\",\"scr\\u0069pt\":\"/opt/bin/model-gateway\"}",
				wantErr: true,
			},
			"error: two names equal only after decoding in another member": {
				body:    "{\"\\u0061\":1,\"a\":2}",
				wantErr: true,
			},
			"error: an escaped surrogate pair and the same character in UTF-8 as two names": {
				body:    "{\"\\ud83d\\ude00\":1,\"\xf0\x9f\x98\x80\":2}",
				wantErr: true,
			},
			"error: two empty member names": {
				body:    `{"":1,"":2}`,
				wantErr: true,
			},
			"error: a duplicate name in a nested object": {
				body:    `{"script":"/opt/bin/model-gateway","x":{"y":{"a":1,"a":2}}}`,
				wantErr: true,
			},
			"error: a duplicate name in an object inside an array": {
				body:    `{"x":[1,{"a":1,"a":2}]}`,
				wantErr: true,
			},
			"error: invalid UTF-8 in the candidate": {
				body:    "{\"script\":\"/opt/\xffbin\"}",
				wantErr: true,
			},
			"error: invalid UTF-8 in another member": {
				body:    "{\"note\":\"\xc3\",\"script\":\"/opt/bin/model-gateway\"}",
				wantErr: true,
			},
			"error: invalid UTF-8 in a member name": {
				body:    "{\"\xff\":1}",
				wantErr: true,
			},
			"error: an overlong UTF-8 encoding": {
				body:    "{\"script\":\"/opt\xc0\xafbin\"}",
				wantErr: true,
			},
			"error: a surrogate encoded directly in UTF-8": {
				body:    "{\"script\":\"/opt/\xed\xa0\x80\"}",
				wantErr: true,
			},
			"error: an escaped lone high surrogate in the candidate": {
				body:    "{\"script\":\"/opt/\\ud800\"}",
				wantErr: true,
			},
			"error: an escaped lone low surrogate in another member": {
				body:    "{\"note\":\"\\udc00\",\"script\":\"/opt/bin/model-gateway\"}",
				wantErr: true,
			},
			"error: an escaped high surrogate followed by an escaped letter": {
				body:    "{\"script\":\"/opt/\\ud800\\u0041\"}",
				wantErr: true,
			},
			"error: an escaped high surrogate followed by a raw letter": {
				body:    "{\"script\":\"/opt/\\ud800A\"}",
				wantErr: true,
			},
			"error: an escaped high surrogate at the end of a string": {
				body:    "{\"script\":\"/opt/\\ud83d\"}",
				wantErr: true,
			},
			"error: two escaped high surrogates": {
				body:    "{\"script\":\"/opt/\\ud83d\\ud83d\"}",
				wantErr: true,
			},
			"error: an escaped low surrogate before a high one": {
				body:    "{\"script\":\"/opt/\\ude00\\ud83d\"}",
				wantErr: true,
			},
			"error: an escaped lone surrogate in a member name": {
				body:    "{\"\\ud800\":1}",
				wantErr: true,
			},
			"error: an escaped lone surrogate in a nested string": {
				body:    "{\"x\":[\"\\udfff\"]}",
				wantErr: true,
			},
			"error: script is null": {
				body:    `{"script":null}`,
				wantErr: true,
			},
			"error: script is a number": {
				body:    `{"script":1}`,
				wantErr: true,
			},
			"error: script is true": {
				body:    `{"script":true}`,
				wantErr: true,
			},
			"error: script is false": {
				body:    `{"script":false}`,
				wantErr: true,
			},
			"error: script is an object": {
				body:    `{"script":{"path":"/opt/bin/model-gateway"}}`,
				wantErr: true,
			},
			"error: script is an array": {
				body:    `{"script":["/opt/bin/model-gateway"]}`,
				wantErr: true,
			},
			"error: binary is null": {
				body:    `{"binary":null}`,
				wantErr: true,
			},
			"error: binary is a number": {
				body:    `{"binary":1}`,
				wantErr: true,
			},
			"error: binary is false": {
				body:    `{"binary":false}`,
				wantErr: true,
			},
			"error: binary is an object": {
				body:    `{"binary":{}}`,
				wantErr: true,
			},
			"error: binary is an array": {
				body:    `{"binary":["/opt/bin/model-gateway"]}`,
				wantErr: true,
			},
			"error: script is a string and binary is null": {
				body:    `{"script":"/opt/bin/model-gateway","binary":null}`,
				wantErr: true,
			},
			"error: binary is a string and script is null": {
				body:    `{"script":null,"binary":"/opt/bin/model-gateway"}`,
				wantErr: true,
			},
			"error: script and binary differ by a trailing slash": {
				body:    `{"script":"/a","binary":"/a/"}`,
				wantErr: true,
			},
			"error: script and binary name different files": {
				body:    `{"script":"/opt/bin/model-gateway","binary":"/home/user/.claude/model-gateway/bin/model-gateway"}`,
				wantErr: true,
			},
			"error: script and binary name one file only after cleaning": {
				body:    `{"script":"/opt/bin/model-gateway","binary":"/opt//bin/model-gateway"}`,
				wantErr: true,
			},
			"error: script and binary differ in letter case": {
				body:    `{"script":"/opt/bin/model-gateway","binary":"/opt/bin/Model-Gateway"}`,
				wantErr: true,
			},
			"error: script is a path and binary is empty": {
				body:    `{"script":"/opt/bin/model-gateway","binary":""}`,
				wantErr: true,
			},
			"error: script and binary are both empty": {
				body:    `{"script":"","binary":""}`,
				wantErr: true,
			},
			"error: script and binary are both the same relative path": {
				body:    `{"script":"bin/model-gateway","binary":"bin/model-gateway"}`,
				wantErr: true,
			},
			"error: script is empty": {
				body:    `{"script":""}`,
				wantErr: true,
			},
			"error: binary is empty": {
				body:    `{"binary":""}`,
				wantErr: true,
			},
			"error: script is a relative path into a foreign tree": {
				body:    `{"script":"../foreign.js"}`,
				wantErr: true,
			},
			"error: binary is a relative path into a foreign tree": {
				body:    `{"binary":"../foreign.js"}`,
				wantErr: true,
			},
			"error: script is a relative path below the working directory": {
				body:    `{"script":"bin/model-gateway"}`,
				wantErr: true,
			},
			"error: script starts with a dot element": {
				body:    `{"script":"./model-gateway"}`,
				wantErr: true,
			},
			"error: script starts with a tilde": {
				body:    `{"script":"~/.claude/model-gateway/bin/model-gateway"}`,
				wantErr: true,
			},
			"error: script holds an escaped NUL": {
				body:    `{"script":"/opt/bin/model-gateway\u0000"}`,
				wantErr: true,
			},
			"error: binary holds an escaped NUL": {
				body:    `{"binary":"/opt/\u0000/model-gateway"}`,
				wantErr: true,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				launchTGuard(t, func() {
					body := []byte(tt.body)
					if tt.nilBody {
						body = nil
					}
					got, err := workerproc.ParseRestart(body)
					if tt.wantErr {
						if !errors.Is(err, workerproc.ErrCandidate) {
							t.Errorf("ParseRestart(%q) error = %v, want an error matching ErrCandidate", tt.body, err)
						}
						if got != "" {
							t.Errorf("ParseRestart(%q) candidate = %q, want \"\" with a refusal", tt.body, got)
						}
						return
					}
					if err != nil {
						t.Fatalf("ParseRestart(%q) error = %v, want nil", tt.body, err)
					}
					if diff := gocmp.Diff(tt.want, got); diff != "" {
						t.Errorf("ParseRestart(%q) candidate mismatch (-want +got):\n%s", tt.body, diff)
					}
				})
			})
		}
	})

	t.Run("Judge", func(t *testing.T) {
		configuredImage := procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001}
		stableImage := procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2002}
		tests := map[string]struct {
			rule        restartTRule
			c           workerproc.Executable
			want        restartTOutcome
			wantImage   procinfo.Image
			wantVersion string
		}{
			"error: without a current choice the configured executable is refused": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					stable:     launchTStableExe,
					uid:        launchTOwner,
				},
				c:    launchTConfiguredExe,
				want: restartTRefused,
			},
			"error: without a current choice the stable binary is refused": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					stable:     launchTStableExe,
					uid:        launchTOwner,
				},
				c:    launchTStableExe,
				want: restartTRefused,
			},
			"error: with an invalid current version the current file is refused": {
				rule: restartTFresh("1.2"),
				c:    launchTConfiguredExe,
				want: restartTRefused,
			},
			"error: with an invalid current version the stable binary is refused": {
				rule: restartTFresh("1.2"),
				c:    launchTStableExe,
				want: restartTRefused,
			},
			"error: with an empty current version the current file is refused": {
				rule: restartTFresh(""),
				c:    launchTConfiguredExe,
				want: restartTRefused,
			},
			"error: with an empty current version the stable binary is refused": {
				rule: restartTFresh(""),
				c:    launchTStableExe,
				want: restartTRefused,
			},
			"error: a symbolic link at the current file's device and inode is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTConfiguredExe, fs.ModeSymlink|0o777),
				want: restartTRefused,
			},
			"error: a directory at the current file's device and inode is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTConfiguredExe, fs.ModeDir|0o755),
				want: restartTRefused,
			},
			"error: a named pipe at the current file's device and inode is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTConfiguredExe, fs.ModeNamedPipe|0o755),
				want: restartTRefused,
			},
			"error: a socket at the current file's device and inode is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTConfiguredExe, fs.ModeSocket|0o755),
				want: restartTRefused,
			},
			"error: a device at the current file's device and inode is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTConfiguredExe, fs.ModeDevice|fs.ModeCharDevice|0o755),
				want: restartTRefused,
			},
			"error: an irregular file at the current file's device and inode is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTConfiguredExe, fs.ModeIrregular|0o755),
				want: restartTRefused,
			},
			"error: a path to the current file that is not clean is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTPath(launchTConfiguredExe, "/opt/model-gateway/lib/../bin/claude-code-model-gateway"),
				want: restartTRefused,
			},
			"error: a relative path to the current file is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTPath(launchTConfiguredExe, "bin/claude-code-model-gateway"),
				want: restartTRefused,
			},
			"error: an empty path with the current file's device and inode is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTPath(launchTConfiguredExe, ""),
				want: restartTRefused,
			},
			"error: a path with a NUL byte to the current file is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTPath(launchTConfiguredExe, "/opt/model-gateway/bin/claude-code-model-gateway\x00"),
				want: restartTRefused,
			},
			"error: inode zero is refused even when the stable inspection shows it too": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTInode(launchTStableExe, 0),
					uid:                 launchTOwner,
				},
				c:    launchTInode(launchTStableExe, 0),
				want: restartTRefused,
			},
			"error: a directory at the stable path is refused even when the stable inspection shows it too": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTMode(launchTStableExe, fs.ModeDir|0o755),
					uid:                 launchTOwner,
				},
				c:    launchTMode(launchTStableExe, fs.ModeDir|0o755),
				want: restartTRefused,
			},
			"error: a path with a NUL byte is refused even when the stable inspection shows it too": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTPath(launchTStableExe, "/home/user/.claude/model-gateway/bin/model\x00gateway"),
					uid:                 launchTOwner,
				},
				c:    launchTPath(launchTStableExe, "/home/user/.claude/model-gateway/bin/model\x00gateway"),
				want: restartTRefused,
			},
			"success: the current file returns the current choice": {
				rule:        restartTFresh("1.2.3"),
				c:           launchTConfiguredExe,
				want:        restartTCurrent,
				wantImage:   configuredImage,
				wantVersion: "1.2.3",
			},
			"success: another clean path to the current file returns the recorded path": {
				rule:        restartTFresh("1.2.3"),
				c:           launchTPath(launchTConfiguredExe, "/usr/local/bin/claude-code-model-gateway"),
				want:        restartTCurrent,
				wantImage:   configuredImage,
				wantVersion: "1.2.3",
			},
			"success: another path to the current file holding a byte that is not UTF-8 returns the recorded path": {
				rule:        restartTFresh("1.2.3"),
				c:           launchTPath(launchTConfiguredExe, "/opt/model-gateway/bin/\xffclaude-code-model-gateway"),
				want:        restartTCurrent,
				wantImage:   configuredImage,
				wantVersion: "1.2.3",
			},
			"success: the configured current file is accepted whatever its owner and mode": {
				rule:        restartTFresh("1.2.3"),
				c:           launchTUID(launchTMode(launchTConfiguredExe, 0o777), 0),
				want:        restartTCurrent,
				wantImage:   configuredImage,
				wantVersion: "1.2.3",
			},
			"success: the configured current file is accepted when no stable binary is installed": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					uid:                 launchTOwner,
				},
				c:           launchTConfiguredExe,
				want:        restartTCurrent,
				wantImage:   configuredImage,
				wantVersion: "1.2.3",
			},
			"success: a current version with a v and a suffix is valid and returned verbatim": {
				rule:        restartTFresh("v1.2.3-rc.1"),
				c:           launchTConfiguredExe,
				want:        restartTCurrent,
				wantImage:   configuredImage,
				wantVersion: "v1.2.3-rc.1",
			},
			"success: the adopted stable file returns the adopted choice without a version run": {
				rule:        restartTAdopted("1.2.3", "1.4.0"),
				c:           launchTStableExe,
				want:        restartTCurrent,
				wantImage:   stableImage,
				wantVersion: "1.4.0",
			},
			"success: another clean path to the adopted stable file returns its recorded path": {
				rule:        restartTAdopted("1.2.3", "1.4.0"),
				c:           launchTPath(launchTStableExe, "/home/user/.local/bin/model-gateway"),
				want:        restartTCurrent,
				wantImage:   stableImage,
				wantVersion: "1.4.0",
			},
			"success: the adopted stable file with owner-only permission": {
				rule:        restartTAdopted("1.2.3", "1.4.0"),
				c:           launchTMode(launchTStableExe, 0o500),
				want:        restartTCurrent,
				wantImage:   stableImage,
				wantVersion: "1.4.0",
			},
			"success: the adopted stable file when the stable binary is no longer installed": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					uid:        launchTOwner,
				},
				c:           launchTStableExe,
				want:        restartTCurrent,
				wantImage:   stableImage,
				wantVersion: "1.4.0",
			},
			"success: the adopted stable file is judged by the owner it was accepted with, not by the rule's owner": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:     launchTStableExe,
					uid:        launchTOtherOwner,
				},
				c:           launchTStableExe,
				want:        restartTCurrent,
				wantImage:   stableImage,
				wantVersion: "1.4.0",
			},
			"success: an invalid configured version does not matter for the current file": {
				rule:        restartTAdopted("development build", "1.4.0"),
				c:           launchTStableExe,
				want:        restartTCurrent,
				wantImage:   stableImage,
				wantVersion: "1.4.0",
			},
			"success: a newer configured version does not matter for the current file": {
				rule:        restartTAdopted("9.0.0", "1.4.0"),
				c:           launchTStableExe,
				want:        restartTCurrent,
				wantImage:   stableImage,
				wantVersion: "1.4.0",
			},
			"error: the adopted stable file with another owner is refused": {
				rule: restartTAdopted("1.2.3", "1.4.0"),
				c:    launchTUID(launchTStableExe, launchTOtherOwner),
				want: restartTRefused,
			},
			"error: the adopted stable file owned by the rule's owner but not by its accepted owner is refused": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:     launchTUID(launchTStableExe, launchTOtherOwner),
					uid:        launchTOtherOwner,
				},
				c:    launchTUID(launchTStableExe, launchTOtherOwner),
				want: restartTRefused,
			},
			"error: the adopted stable file with group write permission is refused": {
				rule: restartTAdopted("1.2.3", "1.4.0"),
				c:    launchTMode(launchTStableExe, 0o775),
				want: restartTRefused,
			},
			"error: the adopted stable file with world write permission is refused": {
				rule: restartTAdopted("1.2.3", "1.4.0"),
				c:    launchTMode(launchTStableExe, 0o757),
				want: restartTRefused,
			},
			"success: the configured file after a stable adoption of the same version": {
				rule:        restartTAdopted("1.4.0", "1.4.0"),
				c:           launchTConfiguredExe,
				want:        restartTConfigured,
				wantImage:   configuredImage,
				wantVersion: "1.4.0",
			},
			"success: a newer configured file after a stable adoption": {
				rule:        restartTAdopted("1.5.0", "1.4.0"),
				c:           launchTConfiguredExe,
				want:        restartTConfigured,
				wantImage:   configuredImage,
				wantVersion: "1.5.0",
			},
			"success: a configured file newer by its minor number compared as a number": {
				rule:        restartTAdopted("1.10.0", "1.9.0"),
				c:           launchTConfiguredExe,
				want:        restartTConfigured,
				wantImage:   configuredImage,
				wantVersion: "1.10.0",
			},
			"success: a configured version equal apart from its v and suffix": {
				rule:        restartTAdopted("v1.4.0-rc.1", "1.4.0"),
				c:           launchTConfiguredExe,
				want:        restartTConfigured,
				wantImage:   configuredImage,
				wantVersion: "v1.4.0-rc.1",
			},
			"success: another clean path to the configured file returns its recorded path": {
				rule:        restartTAdopted("1.4.0", "1.4.0"),
				c:           launchTPath(launchTConfiguredExe, "/usr/local/bin/claude-code-model-gateway"),
				want:        restartTConfigured,
				wantImage:   configuredImage,
				wantVersion: "1.4.0",
			},
			"success: the configured file is accepted whatever its owner and mode": {
				rule:        restartTAdopted("1.4.0", "1.4.0"),
				c:           launchTUID(launchTMode(launchTConfiguredExe, 0o777), launchTOtherOwner),
				want:        restartTConfigured,
				wantImage:   configuredImage,
				wantVersion: "1.4.0",
			},
			"error: an older configured file is refused": {
				rule: restartTAdopted("1.3.9", "1.4.0"),
				c:    launchTConfiguredExe,
				want: restartTRefused,
			},
			"error: a configured file older by its minor number compared as a number is refused": {
				rule: restartTAdopted("1.9.0", "1.10.0"),
				c:    launchTConfiguredExe,
				want: restartTRefused,
			},
			"error: a configured development build is older than a release": {
				rule: restartTAdopted("0.0.0-dev+0123abcd", "1.4.0"),
				c:    launchTConfiguredExe,
				want: restartTRefused,
			},
			"error: a configured file with an invalid version is refused": {
				rule: restartTAdopted("1.4", "1.4.0"),
				c:    launchTConfiguredExe,
				want: restartTRefused,
			},
			"error: a configured file with an empty version is refused": {
				rule: restartTAdopted("", "1.4.0"),
				c:    launchTConfiguredExe,
				want: restartTRefused,
			},
			"error: a symbolic link at the configured file's device and inode is refused after a stable adoption": {
				rule: restartTAdopted("1.5.0", "1.4.0"),
				c:    launchTMode(launchTConfiguredExe, fs.ModeSymlink|0o777),
				want: restartTRefused,
			},
			"error: a relative path to the configured file is refused after a stable adoption": {
				rule: restartTAdopted("1.5.0", "1.4.0"),
				c:    launchTPath(launchTConfiguredExe, "bin/claude-code-model-gateway"),
				want: restartTRefused,
			},
			"error: without a configured launch the configured executable is another file": {
				rule: restartTRule{
					current: restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:  launchTStableExe,
					uid:     launchTOwner,
				},
				c:    launchTConfiguredExe,
				want: restartTRefused,
			},
			"error: a stable launch placed in Configured is refused even as a well-owned stable binary": {
				rule: restartTRule{
					configured: restartTStableSpec(launchTStableExe, "1.5.0"),
					current:    restartTConfiguredSpec(launchTConfiguredExe, "1.4.0"),
					stable:     launchTStableExe,
					uid:        launchTOwner,
				},
				c:    launchTStableExe,
				want: restartTRefused,
			},
			"success: a configured file that is also the stable binary returns the configured launch without a version run": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTStableExe, "1.5.0"),
					current:    restartTStableSpec(launchTOtherExe, "1.4.0"),
					stable:     launchTStableExe,
					uid:        launchTOwner,
				},
				c:           launchTStableExe,
				want:        restartTConfigured,
				wantImage:   stableImage,
				wantVersion: "1.5.0",
			},
			"error: an older configured file that is also the stable binary is refused without a version run": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTStableExe, "1.3.0"),
					current:    restartTStableSpec(launchTOtherExe, "1.4.0"),
					stable:     launchTStableExe,
					uid:        launchTOwner,
				},
				c:    launchTStableExe,
				want: restartTRefused,
			},
			"success: a current file that is also the configured image and the stable binary returns the current choice": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTStableExe, "1.4.0"),
					currentIsConfigured: true,
					stable:              launchTStableExe,
					uid:                 launchTOwner,
				},
				c:           launchTMode(launchTStableExe, 0o777),
				want:        restartTCurrent,
				wantImage:   stableImage,
				wantVersion: "1.4.0",
			},
			"success: a current stable file that is also the configured image under another version returns the current choice": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTStableExe, "1.5.0"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:     launchTStableExe,
					uid:        launchTOwner,
				},
				c:           launchTStableExe,
				want:        restartTCurrent,
				wantImage:   stableImage,
				wantVersion: "1.4.0",
			},
			"error: a current stable file that is also the configured image is refused with group write permission": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTStableExe, "1.5.0"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:     launchTStableExe,
					uid:        launchTOwner,
				},
				c:    launchTMode(launchTStableExe, 0o775),
				want: restartTRefused,
			},
			"error: a current stable file that is also the configured image is refused with another owner": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTStableExe, "1.5.0"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:     launchTStableExe,
					uid:        launchTOwner,
				},
				c:    launchTUID(launchTStableExe, launchTOtherOwner),
				want: restartTRefused,
			},
			"success: the stable binary needs its version": {
				rule: restartTFresh("1.2.3"),
				c:    launchTStableExe,
				want: restartTNeedVersion,
			},
			"success: the stable binary with owner-only permission needs its version": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTStableExe, 0o700),
				want: restartTNeedVersion,
			},
			"success: the stable binary without write permission needs its version": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTStableExe, 0o555),
				want: restartTNeedVersion,
			},
			"success: the stable binary readable by everyone needs its version": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTStableExe, 0o744),
				want: restartTNeedVersion,
			},
			"success: the stable binary with the set-user-ID bit needs its version": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTStableExe, fs.ModeSetuid|0o755),
				want: restartTNeedVersion,
			},
			"success: the candidate's own owner and mode decide, not those of the stable inspection": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTUID(launchTMode(launchTStableExe, 0o777), launchTOtherOwner),
					uid:                 launchTOwner,
				},
				c:    launchTStableExe,
				want: restartTNeedVersion,
			},
			"success: the stable binary on device zero needs its version": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTDevice(launchTStableExe, 0),
					uid:                 launchTOwner,
				},
				c:    launchTDevice(launchTStableExe, 0),
				want: restartTNeedVersion,
			},
			"success: the stable binary owned by root needs its version when root is the rule's owner": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTUID(launchTStableExe, 0),
					uid:                 0,
				},
				c:    launchTUID(launchTStableExe, 0),
				want: restartTNeedVersion,
			},
			"success: the stable binary without any execute permission needs its version": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTStableExe, 0o644),
				want: restartTNeedVersion,
			},
			"success: the replaced stable binary needs its version whatever the configured version": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "9.0.0"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:     launchTReplacedStableExe,
					uid:        launchTOwner,
				},
				c:    launchTReplacedStableExe,
				want: restartTNeedVersion,
			},
			"success: the stable binary replaced after an adoption needs its version": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:     launchTReplacedStableExe,
					uid:        launchTOwner,
				},
				c:    launchTReplacedStableExe,
				want: restartTNeedVersion,
			},
			"success: the stable binary of the rule's owner needs its version": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTUID(launchTStableExe, launchTOtherOwner),
					uid:                 launchTOtherOwner,
				},
				c:    launchTUID(launchTStableExe, launchTOtherOwner),
				want: restartTNeedVersion,
			},
			"error: the stable binary with group write permission is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTStableExe, 0o775),
				want: restartTRefused,
			},
			"error: the stable binary with world write permission is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTStableExe, 0o757),
				want: restartTRefused,
			},
			"error: the stable binary with group write permission alone is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTStableExe, 0o020),
				want: restartTRefused,
			},
			"error: the stable binary with world write permission alone is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTMode(launchTStableExe, 0o002),
				want: restartTRefused,
			},
			"error: the stable binary of another owner is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTUID(launchTStableExe, launchTOtherOwner),
				want: restartTRefused,
			},
			"error: the stable binary of the root owner is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTUID(launchTStableExe, 0),
				want: restartTRefused,
			},
			"error: another clean path to the stable binary is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTPath(launchTStableExe, "/home/user/.local/bin/model-gateway"),
				want: restartTRefused,
			},
			"error: the stable path showing another inode than the stable inspection is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTReplacedStableExe,
				want: restartTRefused,
			},
			"error: the stable path showing another device than the stable inspection is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTDevice(launchTStableExe, 64771),
				want: restartTRefused,
			},
			"error: the stable binary is refused when none is installed": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					uid:                 launchTOwner,
				},
				c:    launchTStableExe,
				want: restartTRefused,
			},
			"error: a stable inspection with an empty path names no stable binary": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTPath(launchTStableExe, ""),
					uid:                 launchTOwner,
				},
				c:    launchTStableExe,
				want: restartTRefused,
			},
			"error: another file of the same owner is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTOtherExe,
				want: restartTRefused,
			},
			"error: a file of a foreign installation is refused": {
				rule: restartTFresh("1.2.3"),
				c: workerproc.Executable{
					Image: procinfo.Image{Path: "/home/other/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 4004},
					Mode:  0o755,
					UID:   launchTOtherOwner,
				},
				want: restartTRefused,
			},
			"error: the current path showing another inode is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTInode(launchTConfiguredExe, 1002),
				want: restartTRefused,
			},
			"error: the current file's inode on another device is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTDevice(launchTConfiguredExe, 64771),
				want: restartTRefused,
			},
			"error: the stable binary's inode on another device at another path is refused": {
				rule: restartTFresh("1.2.3"),
				c:    launchTDevice(launchTPath(launchTStableExe, "/mnt/backup/model-gateway"), 64771),
				want: restartTRefused,
			},
			"error: an earlier stable choice's file is refused once another choice is current": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					current:    restartTStableSpec(launchTReplacedStableExe, "1.5.0"),
					stable:     launchTReplacedStableExe,
					uid:        launchTOwner,
				},
				c:    launchTPath(launchTStableExe, "/home/user/.claude/model-gateway/bin/model-gateway.old-1.4.0-1"),
				want: restartTRefused,
			},
			"error: the zero inspection is refused": {
				rule: restartTFresh("1.2.3"),
				c:    workerproc.Executable{},
				want: restartTRefused,
			},
			"error: a regular file with the zero image is refused when no launch has a zero image": {
				rule: restartTFresh("1.2.3"),
				c:    workerproc.Executable{Image: procinfo.Image{Path: "/"}, Mode: 0o755, UID: launchTOwner},
				want: restartTRefused,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				launchTGuard(t, func() {
					r := restartTBuild(t, tt.rule)
					restartTCheckJudge(t, r, tt.c, tt.want, tt.wantImage, tt.wantVersion)
				})
			})
		}
	})

	t.Run("AcceptVersion", func(t *testing.T) {
		stableImage := procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2002}
		replacedImage := procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2003}
		tests := map[string]struct {
			rule   restartTRule
			before workerproc.Executable
			output []byte
			after  workerproc.Executable
			// reuse overwrites every byte of output after AcceptVersion returned, as a caller reusing its buffer does.
			reuse       bool
			wantErr     bool
			wantImage   procinfo.Image
			wantVersion string
		}{
			"success: a newer stable binary with one version line": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("1.3.0\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "1.3.0",
			},
			"success: a stable binary of the same version": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("1.2.3\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "1.2.3",
			},
			"success: a version with a v is equal and kept verbatim": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("v1.2.3\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "v1.2.3",
			},
			"success: a pre-release suffix is ignored, not older": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("1.2.3-rc.1\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "1.2.3-rc.1",
			},
			"success: a build suffix is ignored": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("1.2.3+abc\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "1.2.3+abc",
			},
			"success: leading zeros do not change the number": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("01.002.0003\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "01.002.0003",
			},
			"success: a patch number with two digits is newer than one with one digit": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("1.2.10\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "1.2.10",
			},
			"success: a newer major number": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("2.0.0\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "2.0.0",
			},
			"success: the largest numbers": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("999999999.999999999.999999999\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "999999999.999999999.999999999",
			},
			"success: a release equal to a pre-release baseline": {
				rule:        restartTFresh("v2.0.0-rc.1"),
				before:      launchTStableExe,
				output:      []byte("2.0.0\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "2.0.0",
			},
			"success: a development build baseline counts as 0.0.0": {
				rule:        restartTFresh("0.0.0-dev+0123abcd"),
				before:      launchTStableExe,
				output:      []byte("0.0.0\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "0.0.0",
			},
			"success: a version line of exactly 4096 bytes": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("1.2.4-" + strings.Repeat("a", 4089) + "\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "1.2.4-" + strings.Repeat("a", 4089),
			},
			"success: the replaced stable binary against the adopted choice of the same version": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:     launchTReplacedStableExe,
					uid:        launchTOwner,
				},
				before:      launchTReplacedStableExe,
				output:      []byte("1.4.0\n"),
				after:       launchTReplacedStableExe,
				wantImage:   replacedImage,
				wantVersion: "1.4.0",
			},
			"success: a stable binary of the rule's owner keeps that owner": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTUID(launchTStableExe, launchTOtherOwner),
					uid:                 launchTOtherOwner,
				},
				before:      launchTUID(launchTStableExe, launchTOtherOwner),
				output:      []byte("1.3.0\n"),
				after:       launchTUID(launchTStableExe, launchTOtherOwner),
				wantImage:   stableImage,
				wantVersion: "1.3.0",
			},
			"success: the accepted owner is the rule's owner, not the owner the stable inspection showed": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTUID(launchTStableExe, launchTOtherOwner),
					uid:                 launchTOwner,
				},
				before:      launchTStableExe,
				output:      []byte("1.3.0\n"),
				after:       launchTStableExe,
				wantImage:   stableImage,
				wantVersion: "1.3.0",
			},
			"success: a stable binary owned by root keeps the root owner when root is the rule's owner": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					stable:              launchTUID(launchTStableExe, 0),
					uid:                 0,
				},
				before:      launchTUID(launchTStableExe, 0),
				output:      []byte("1.3.0\n"),
				after:       launchTUID(launchTStableExe, 0),
				wantImage:   stableImage,
				wantVersion: "1.3.0",
			},
			"success: only the current choice's version is the baseline, not a newer configured version": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "9.0.0"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:     launchTReplacedStableExe,
					uid:        launchTOwner,
				},
				before:      launchTReplacedStableExe,
				output:      []byte("1.5.0\n"),
				after:       launchTReplacedStableExe,
				wantImage:   replacedImage,
				wantVersion: "1.5.0",
			},
			"success: the accepted version does not change when the caller reuses the output bytes": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTStableExe,
				output:      []byte("1.3.0\n"),
				after:       launchTStableExe,
				reuse:       true,
				wantImage:   stableImage,
				wantVersion: "1.3.0",
			},
			"success: a stable binary without write permission": {
				rule:        restartTFresh("1.2.3"),
				before:      launchTMode(launchTStableExe, 0o555),
				output:      []byte("1.3.0\n"),
				after:       launchTMode(launchTStableExe, 0o555),
				wantImage:   stableImage,
				wantVersion: "1.3.0",
			},
			"error: an older patch number": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.2.2\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: an older minor number with a newer patch number": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.1.9\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: an older major number with newer other numbers": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("0.9.9\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: an older version with a v and a suffix": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("v1.2.2+zzz\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a minor number compared as a number, not as text": {
				rule:    restartTFresh("1.10.0"),
				before:  launchTStableExe,
				output:  []byte("1.9.0\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a development build is older than a release": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("0.0.0-dev+0123abcd\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a release older than a pre-release baseline": {
				rule:    restartTFresh("v2.0.0-rc.1"),
				before:  launchTStableExe,
				output:  []byte("1.99.99\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: older than the adopted choice although newer than the configured executable": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "1.0.0"),
					current:    restartTStableSpec(launchTStableExe, "1.4.0"),
					stable:     launchTReplacedStableExe,
					uid:        launchTOwner,
				},
				before:  launchTReplacedStableExe,
				output:  []byte("1.2.0\n"),
				after:   launchTReplacedStableExe,
				wantErr: true,
			},
			"error: a version line without its line end": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a version line with two line ends": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a version line ended by a carriage return and a line feed": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\r\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a version line ended by a carriage return": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\r"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a second line after the version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\ncommit 0123abcd\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a second line without a line end after the version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\nmore"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a line before the version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("\n1.3.0\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: the program name before the version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("model-gateway 1.3.0\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a space before the version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte(" 1.3.0\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a space after the version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0 \n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a NUL byte after the version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\x00\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: only a line end": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: empty output": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte{},
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: no output": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  nil,
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: an invalid version line": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: a version line of 4097 bytes": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.2.4-" + strings.Repeat("a", 4090) + "\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: the current file does not take a version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTConfiguredExe,
				output:  []byte("1.3.0\n"),
				after:   launchTConfiguredExe,
				wantErr: true,
			},
			"error: the adopted stable file does not take a version": {
				rule:    restartTAdopted("1.2.3", "1.4.0"),
				before:  launchTStableExe,
				output:  []byte("1.5.0\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: the configured file does not take a version": {
				rule:    restartTAdopted("1.4.0", "1.4.0"),
				before:  launchTConfiguredExe,
				output:  []byte("1.5.0\n"),
				after:   launchTConfiguredExe,
				wantErr: true,
			},
			"error: a configured file that is also the stable binary does not take a version": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTStableExe, "1.3.0"),
					current:    restartTStableSpec(launchTOtherExe, "1.4.0"),
					stable:     launchTStableExe,
					uid:        launchTOwner,
				},
				before:  launchTStableExe,
				output:  []byte("1.5.0\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: another file does not take a version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTOtherExe,
				output:  []byte("1.3.0\n"),
				after:   launchTOtherExe,
				wantErr: true,
			},
			"error: a stable binary with group write permission does not take a version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTMode(launchTStableExe, 0o775),
				output:  []byte("1.3.0\n"),
				after:   launchTMode(launchTStableExe, 0o775),
				wantErr: true,
			},
			"error: a stable binary of another owner does not take a version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTUID(launchTStableExe, launchTOtherOwner),
				output:  []byte("1.3.0\n"),
				after:   launchTUID(launchTStableExe, launchTOtherOwner),
				wantErr: true,
			},
			"error: a symbolic link at the stable path does not take a version": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTMode(launchTStableExe, fs.ModeSymlink|0o755),
				output:  []byte("1.3.0\n"),
				after:   launchTMode(launchTStableExe, fs.ModeSymlink|0o755),
				wantErr: true,
			},
			"error: the stable binary does not take a version when none is installed": {
				rule: restartTRule{
					configured:          restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					currentIsConfigured: true,
					uid:                 launchTOwner,
				},
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: the stable binary does not take a version without a current choice": {
				rule: restartTRule{
					configured: restartTConfiguredSpec(launchTConfiguredExe, "1.2.3"),
					stable:     launchTStableExe,
					uid:        launchTOwner,
				},
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: the stable binary does not take a version while the current version is invalid": {
				rule:    restartTFresh("1.2"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: the second inspection shows another path": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTPath(launchTStableExe, "/home/user/.local/bin/model-gateway"),
				wantErr: true,
			},
			"error: the second inspection shows another device": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTDevice(launchTStableExe, 64771),
				wantErr: true,
			},
			"error: the second inspection shows another inode": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTReplacedStableExe,
				wantErr: true,
			},
			"error: the second inspection shows other permission bits": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTMode(launchTStableExe, 0o750),
				wantErr: true,
			},
			"error: the second inspection adds the set-user-ID bit": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTMode(launchTStableExe, fs.ModeSetuid|0o755),
				wantErr: true,
			},
			"error: the second inspection adds the set-group-ID bit": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTMode(launchTStableExe, fs.ModeSetgid|0o755),
				wantErr: true,
			},
			"error: the second inspection adds the sticky bit": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTMode(launchTStableExe, fs.ModeSticky|0o755),
				wantErr: true,
			},
			"error: the second inspection no longer shows the set-user-ID bit": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTMode(launchTStableExe, fs.ModeSetuid|0o755),
				output:  []byte("1.3.0\n"),
				after:   launchTStableExe,
				wantErr: true,
			},
			"error: the second inspection shows a symbolic link": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTMode(launchTStableExe, fs.ModeSymlink|0o755),
				wantErr: true,
			},
			"error: the second inspection shows another owner": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   launchTUID(launchTStableExe, launchTOtherOwner),
				wantErr: true,
			},
			"error: the second inspection is the zero inspection": {
				rule:    restartTFresh("1.2.3"),
				before:  launchTStableExe,
				output:  []byte("1.3.0\n"),
				after:   workerproc.Executable{},
				wantErr: true,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				launchTGuard(t, func() {
					r := restartTBuild(t, tt.rule)
					l, err := r.AcceptVersion(tt.before, tt.output, tt.after)
					copied := l
					if tt.reuse {
						for i := range tt.output {
							tt.output[i] = '9'
						}
					}
					what := "AcceptVersion(" + tt.before.Image.Path + ")"
					if tt.wantErr {
						restartTCheckRefusedLaunch(t, what, l, err)
						return
					}
					restartTCheckStable(t, what, l, err, tt.before, tt.wantImage, tt.wantVersion)
					if diff := gocmp.Diff(tt.wantVersion, copied.Version()); diff != "" {
						t.Errorf("%s: a copy of the launch taken before the output was reused: Version() mismatch (-want +got):\n%s", what, diff)
					}
				})
			})
		}
	})

	t.Run("version grammar", restartTGrammar)
	t.Run("version text longer than the output bound", restartTLongText)
	t.Run("version order", restartTOrder)
	t.Run("field spellings", restartTSpellings)
}

// restartTGrammar decides which version texts are valid at each place a version is read: the current choice's
// version, the configured launch's version and the stable binary's version output.
func restartTGrammar(t *testing.T) {
	configuredImage := procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001}
	stableImage := procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2002}
	tests := map[string]struct {
		text  string
		valid bool
	}{
		"success: three numbers":                                     {text: "1.2.3", valid: true},
		"success: a lowercase v before the numbers":                  {text: "v1.2.3", valid: true},
		"success: a leading zero":                                    {text: "01.2.3", valid: true},
		"success: leading zeros before the digits 8 and 9":           {text: "1.08.009", valid: true},
		"success: a leading zero before 8 in the major number":       {text: "08.0.0", valid: true},
		"success: a pre-release suffix":                              {text: "1.2.3-rc.1", valid: true},
		"success: a build suffix":                                    {text: "1.2.3+abc", valid: true},
		"success: a development build":                               {text: "0.0.0-dev+0123abcd", valid: true},
		"success: all zeros":                                         {text: "0.0.0", valid: true},
		"success: nine digits in every number":                       {text: "999999999.999999999.999999999", valid: true},
		"success: nine zeros in every number":                        {text: "000000000.000000000.000000000", valid: true},
		"success: a one-letter pre-release suffix":                   {text: "1.2.3-a", valid: true},
		"success: a one-letter build suffix":                         {text: "1.2.3+a", valid: true},
		"success: uppercase letters in the suffix":                   {text: "1.2.3-RC.1", valid: true},
		"success: a v with a pre-release and a build part":           {text: "v0.0.1-alpha.1+build.2026-11-28", valid: true},
		"success: a hyphen as the only suffix character":             {text: "1.2.3--", valid: true},
		"success: a plus sign after the hyphen delimiter":            {text: "1.2.3-+", valid: true},
		"success: a hyphen after the plus delimiter":                 {text: "1.2.3+-", valid: true},
		"success: a dot as the only suffix character":                {text: "1.2.3-.", valid: true},
		"success: dots and plus signs in a build suffix":             {text: "1.2.3+.+.", valid: true},
		"error: the empty text":                                      {text: "", valid: false},
		"error: an uppercase V":                                      {text: "V1.2.3", valid: false},
		"error: two v letters":                                       {text: "vv1.2.3", valid: false},
		"error: a v alone":                                           {text: "v", valid: false},
		"error: two numbers":                                         {text: "1.2", valid: false},
		"error: one number":                                          {text: "1", valid: false},
		"error: four numbers":                                        {text: "1.2.3.4", valid: false},
		"error: a hyphen delimiter without a suffix":                 {text: "1.2.3-", valid: false},
		"error: a plus delimiter without a suffix":                   {text: "1.2.3+", valid: false},
		"error: a trailing space":                                    {text: "1.2.3 ", valid: false},
		"error: a leading space":                                     {text: " 1.2.3", valid: false},
		"error: a leading tab":                                       {text: "\t1.2.3", valid: false},
		"error: a trailing line feed":                                {text: "1.2.3\n", valid: false},
		"error: a trailing carriage return":                          {text: "1.2.3\r", valid: false},
		"error: a trailing NUL byte":                                 {text: "1.2.3\x00", valid: false},
		"error: ten digits in the major number":                      {text: "1234567890.0.0", valid: false},
		"error: ten digits in the minor number":                      {text: "0.1234567890.0", valid: false},
		"error: ten digits in the patch number":                      {text: "0.0.1234567890", valid: false},
		"error: ten digits with leading zeros":                       {text: "0000000001.2.3", valid: false},
		"error: an underscore delimiter":                             {text: "1.2.3_x", valid: false},
		"error: an underscore in the suffix":                         {text: "1.2.3-rc_1", valid: false},
		"error: a space in the suffix":                               {text: "1.2.3-rc 1", valid: false},
		"error: a non-ASCII letter in the suffix":                    {text: "1.2.3-é", valid: false},
		"error: a slash after the numbers":                           {text: "1.2.3/x", valid: false},
		"error: a tilde after the numbers":                           {text: "1.2.3~x", valid: false},
		"error: a letter directly after the numbers":                 {text: "1.2.3a", valid: false},
		"error: a v after the numbers":                               {text: "1.2.3v", valid: false},
		"error: a letter before the numbers":                         {text: "a1.2.3", valid: false},
		"error: an empty minor number":                               {text: "1..3", valid: false},
		"error: a leading dot":                                       {text: ".1.2.3", valid: false},
		"error: a trailing dot":                                      {text: "1.2.3.", valid: false},
		"error: a negative patch number":                             {text: "1.2.-3", valid: false},
		"error: a leading hyphen":                                    {text: "-1.2.3", valid: false},
		"error: a leading plus sign":                                 {text: "+1.2.3", valid: false},
		"error: a plus sign before the patch number":                 {text: "1.2.+3", valid: false},
		"error: a hexadecimal major number":                          {text: "0x1.2.3", valid: false},
		"error: commas between the numbers":                          {text: "1,2,3", valid: false},
		"error: a space after the v":                                 {text: "v 1.2.3", valid: false},
		"error: a full-width digit":                                  {text: "１.2.3", valid: false},
		"error: an Arabic-Indic digit":                               {text: "١.2.3", valid: false},
		"error: the program name before the version":                 {text: "model-gateway 1.2.3", valid: false},
		"error: a version followed by a second line":                 {text: "1.2.3\n1.2.4", valid: false},
		"error: a pre-release suffix followed by a trailing newline": {text: "1.2.3-rc.1\n", valid: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Run("as the current choice's version", func(t *testing.T) {
				launchTGuard(t, func() {
					want := restartTRefused
					if tt.valid {
						want = restartTCurrent
					}
					r := restartTBuild(t, restartTFresh(tt.text))
					restartTCheckJudge(t, r, launchTConfiguredExe, want, configuredImage, tt.text)
				})
			})
			t.Run("as the configured launch's version", func(t *testing.T) {
				launchTGuard(t, func() {
					want := restartTRefused
					if tt.valid {
						want = restartTConfigured
					}
					r := restartTBuild(t, restartTAdopted(tt.text, "0.0.0"))
					restartTCheckJudge(t, r, launchTConfiguredExe, want, configuredImage, tt.text)
				})
			})
			t.Run("as the stable binary's version output", func(t *testing.T) {
				launchTGuard(t, func() {
					r := restartTBuild(t, restartTFresh("0.0.0"))
					l, err := r.AcceptVersion(launchTStableExe, []byte(tt.text+"\n"), launchTStableExe)
					if !tt.valid {
						restartTCheckRefusedLaunch(t, "AcceptVersion", l, err)
						return
					}
					restartTCheckStable(t, "AcceptVersion", l, err, launchTStableExe, stableImage, tt.text)
				})
			})
		})
	}
}

// restartTLongText decides that the version grammar has no length limit of its own: a valid text of 4097 bytes is a
// valid version of the current choice and of the configured launch, while the same text as the stable binary's version
// output, with its line end, exceeds the 4096-byte bound of that output.
func restartTLongText(t *testing.T) {
	configuredImage := procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001}
	// text is "1.2.4-" and 4091 letters: 4097 bytes.
	text := "1.2.4-" + strings.Repeat("a", 4091)
	tests := map[string]struct {
		rule restartTRule
		// output is the stable binary's version output; when it is nil the row judges the configured executable.
		output      []byte
		want        restartTOutcome
		wantVersion string
	}{
		"success: as the current choice's version": {
			rule:        restartTFresh(text),
			want:        restartTCurrent,
			wantVersion: text,
		},
		"success: as the configured launch's version": {
			rule:        restartTAdopted(text, "0.0.0"),
			want:        restartTConfigured,
			wantVersion: text,
		},
		"error: as the stable binary's version output with its line end": {
			rule:   restartTFresh("0.0.0"),
			output: []byte(text + "\n"),
			want:   restartTRefused,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			launchTGuard(t, func() {
				if len(text) != 4097 {
					t.Fatalf("setup: the version text has %d bytes, want 4097", len(text))
				}
				r := restartTBuild(t, tt.rule)
				if tt.output == nil {
					restartTCheckJudge(t, r, launchTConfiguredExe, tt.want, configuredImage, tt.wantVersion)
					return
				}
				l, err := r.AcceptVersion(launchTStableExe, tt.output, launchTStableExe)
				restartTCheckRefusedLaunch(t, "AcceptVersion", l, err)
			})
		})
	}
}

// restartTOrder decides how two version texts compare, where a candidate's version is compared with the current
// choice's: the stable binary's version output and the configured launch's version.
func restartTOrder(t *testing.T) {
	configuredImage := procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001}
	stableImage := procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2002}
	tests := map[string]struct {
		current   string
		candidate string
		// accepted is true when the candidate's version is not older than the current choice's.
		accepted bool
	}{
		"success: an equal version":                                      {current: "1.2.3", candidate: "1.2.3", accepted: true},
		"success: a newer patch number":                                  {current: "1.2.3", candidate: "1.2.4", accepted: true},
		"success: a newer minor number with an older patch number":       {current: "1.2.3", candidate: "1.3.0", accepted: true},
		"success: a newer major number with older other numbers":         {current: "1.2.3", candidate: "2.0.0", accepted: true},
		"success: a two-digit patch number after a one-digit one":        {current: "1.2.9", candidate: "1.2.10", accepted: true},
		"success: a two-digit minor number after a one-digit one":        {current: "1.9.0", candidate: "1.10.0", accepted: true},
		"success: a two-digit major number after a one-digit one":        {current: "9.0.0", candidate: "10.0.0", accepted: true},
		"success: a v on the candidate only":                             {current: "1.2.3", candidate: "v1.2.3", accepted: true},
		"success: a v on the current version only":                       {current: "v1.2.3", candidate: "1.2.3", accepted: true},
		"success: a pre-release candidate of the current release":        {current: "1.2.3", candidate: "1.2.3-rc.1", accepted: true},
		"success: a release of the current pre-release":                  {current: "1.2.3-rc.1", candidate: "1.2.3", accepted: true},
		"success: an earlier pre-release of the same numbers":            {current: "1.2.3-rc.2", candidate: "1.2.3-rc.1", accepted: true},
		"success: different build suffixes of the same numbers":          {current: "1.2.3+abc", candidate: "1.2.3-zzz", accepted: true},
		"success: leading zeros on the candidate":                        {current: "1.2.3", candidate: "01.02.03", accepted: true},
		"success: a minor number with a leading zero read as a decimal":  {current: "1.9.0", candidate: "1.010.0", accepted: true},
		"success: a development build is equal to 0.0.0":                 {current: "0.0.0-dev+0123abcd", candidate: "0.0.0", accepted: true},
		"success: 0.0.0 is equal to a development build":                 {current: "0.0.0", candidate: "0.0.0-dev+0123abcd", accepted: true},
		"success: the largest numbers after one less":                    {current: "999999999.999999999.999999998", candidate: "999999999.999999999.999999999", accepted: true},
		"error: an older patch number":                                   {current: "1.2.3", candidate: "1.2.2", accepted: false},
		"error: an older minor number with a newer patch number":         {current: "1.2.3", candidate: "1.1.9", accepted: false},
		"error: an older major number with newer other numbers":          {current: "1.2.3", candidate: "0.9.9", accepted: false},
		"error: a one-digit patch number after a two-digit one":          {current: "1.2.10", candidate: "1.2.9", accepted: false},
		"error: a one-digit minor number after a two-digit one":          {current: "1.10.0", candidate: "1.9.0", accepted: false},
		"error: a one-digit major number after a two-digit one":          {current: "10.0.0", candidate: "9.0.0", accepted: false},
		"error: an older major number written with leading zeros":        {current: "10.0.0", candidate: "000000002.0.0", accepted: false},
		"error: a minor number read as a decimal despite a leading zero": {current: "1.010.0", candidate: "1.9.0", accepted: false},
		"error: a development build after a release":                     {current: "0.0.1", candidate: "0.0.0-dev+0123abcd", accepted: false},
		"error: an older release than a pre-release":                     {current: "1.2.4-alpha", candidate: "1.2.3", accepted: false},
		"error: one less than the largest numbers":                       {current: "999999999.999999999.999999999", candidate: "999999999.999999999.999999998", accepted: false},
		"error: a candidate with four numbers is never newer":            {current: "1.2.3", candidate: "1.2.3.4", accepted: false},
		"error: a candidate with an empty suffix is never equal":         {current: "1.2.3", candidate: "1.2.3-", accepted: false},
		"error: a candidate with an uppercase V is never equal":          {current: "1.2.3", candidate: "V1.2.3", accepted: false},
		"error: a candidate with two numbers is never newer":             {current: "1.2.3", candidate: "1.3", accepted: false},
		"error: a candidate with a ten-digit number is never newer":      {current: "1.2.3", candidate: "1234567890.0.0", accepted: false},
		"error: no candidate is newer than an invalid current version":   {current: "1.2", candidate: "1.2.3", accepted: false},
		"error: no candidate is newer than an empty current version":     {current: "", candidate: "1.0.0", accepted: false},
		"error: no candidate is equal to a current version with spaces":  {current: "1.2.3 ", candidate: "9.9.9", accepted: false},
		"error: no candidate is newer than a current uppercase V":        {current: "V1.0.0", candidate: "2.0.0", accepted: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Run("stable binary's version output", func(t *testing.T) {
				launchTGuard(t, func() {
					r := restartTBuild(t, restartTFresh(tt.current))
					l, err := r.AcceptVersion(launchTStableExe, []byte(tt.candidate+"\n"), launchTStableExe)
					if !tt.accepted {
						restartTCheckRefusedLaunch(t, "AcceptVersion", l, err)
						return
					}
					restartTCheckStable(t, "AcceptVersion", l, err, launchTStableExe, stableImage, tt.candidate)
				})
			})
			t.Run("configured launch's version", func(t *testing.T) {
				launchTGuard(t, func() {
					want := restartTRefused
					if tt.accepted {
						want = restartTConfigured
					}
					r := restartTBuild(t, restartTRule{
						configured: restartTConfiguredSpec(launchTConfiguredExe, tt.candidate),
						current:    restartTConfiguredSpec(launchTOtherExe, tt.current),
						stable:     launchTStableExe,
						uid:        launchTOwner,
					})
					restartTCheckJudge(t, r, launchTConfiguredExe, want, configuredImage, tt.candidate)
				})
			})
		})
	}
}

// restartTStep names the unit whose result ends one restart decision.
type restartTStep int

const (
	// restartTByParse: ParseRestart refused the body.
	restartTByParse restartTStep = iota + 1
	// restartTByJudge: Judge accepted or refused the candidate without asking for its version.
	restartTByJudge
	// restartTByAccept: Judge asked for the version and AcceptVersion accepted or refused the candidate.
	restartTByAccept
)

// String names the unit, for failure messages.
func (s restartTStep) String() string {
	switch s {
	case restartTByParse:
		return "ParseRestart"
	case restartTByJudge:
		return "Judge"
	case restartTByAccept:
		return "AcceptVersion"
	default:
		return "no unit"
	}
}

// restartTSpellings runs whole restart decisions from the request body to the accepted launch, through each spelling
// of the candidate field. The inspections are written by hand: resolving a symbolic link is the route's work.
func restartTSpellings(t *testing.T) {
	foreignExe := workerproc.Executable{
		Image: procinfo.Image{Path: "/home/other/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 4004},
		Mode:  0o755,
		UID:   launchTOtherOwner,
	}
	inspections := map[string]workerproc.Executable{
		launchTConfiguredExe.Image.Path: launchTConfiguredExe,
		launchTStableExe.Image.Path:     launchTStableExe,
		launchTOtherExe.Image.Path:      launchTOtherExe,
		foreignExe.Image.Path:           foreignExe,
	}
	tests := map[string]struct {
		rule restartTRule
		body string
		// output is what the stable binary's version command prints, when the decision asks for it.
		output string
		// step is the unit whose result ends the decision.
		step        restartTStep
		wantErr     bool
		wantImage   procinfo.Image
		wantVersion string
	}{
		"success: script names the recorded image": {
			rule:        restartTFresh("1.2.3"),
			body:        `{"script":"/opt/model-gateway/bin/claude-code-model-gateway"}`,
			step:        restartTByJudge,
			wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
			wantVersion: "1.2.3",
		},
		"success: binary names the recorded image": {
			rule:        restartTFresh("1.2.3"),
			body:        `{"binary":"/opt/model-gateway/bin/claude-code-model-gateway"}`,
			step:        restartTByJudge,
			wantImage:   procinfo.Image{Path: "/opt/model-gateway/bin/claude-code-model-gateway", Device: 64769, Inode: 1001},
			wantVersion: "1.2.3",
		},
		"success: script names a validated newer stable image": {
			rule:        restartTFresh("1.2.3"),
			body:        `{"script":"/home/user/.claude/model-gateway/bin/model-gateway"}`,
			step:        restartTByAccept,
			output:      "1.3.0\n",
			wantImage:   procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2002},
			wantVersion: "1.3.0",
		},
		"success: binary names a validated newer stable image": {
			rule:        restartTFresh("1.2.3"),
			body:        `{"binary":"/home/user/.claude/model-gateway/bin/model-gateway"}`,
			step:        restartTByAccept,
			output:      "1.3.0\n",
			wantImage:   procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2002},
			wantVersion: "1.3.0",
		},
		"success: script and binary name the same validated stable image": {
			rule:        restartTFresh("1.2.3"),
			body:        `{"script":"/home/user/.claude/model-gateway/bin/model-gateway","binary":"/home/user/.claude/model-gateway/bin/model-gateway"}`,
			step:        restartTByAccept,
			output:      "1.3.0\n",
			wantImage:   procinfo.Image{Path: "/home/user/.claude/model-gateway/bin/model-gateway", Device: 64770, Inode: 2002},
			wantVersion: "1.3.0",
		},
		"error: script names an older stable image": {
			rule:    restartTFresh("1.2.3"),
			body:    `{"script":"/home/user/.claude/model-gateway/bin/model-gateway"}`,
			step:    restartTByAccept,
			output:  "1.2.0\n",
			wantErr: true,
		},
		"error: binary names an older stable image": {
			rule:    restartTFresh("1.2.3"),
			body:    `{"binary":"/home/user/.claude/model-gateway/bin/model-gateway"}`,
			step:    restartTByAccept,
			output:  "1.2.0\n",
			wantErr: true,
		},
		"error: script names the configured image older than the adopted choice": {
			rule:    restartTAdopted("1.2.3", "1.4.0"),
			body:    `{"script":"/opt/model-gateway/bin/claude-code-model-gateway"}`,
			step:    restartTByJudge,
			wantErr: true,
		},
		"error: binary names the configured image older than the adopted choice": {
			rule:    restartTAdopted("1.2.3", "1.4.0"),
			body:    `{"binary":"/opt/model-gateway/bin/claude-code-model-gateway"}`,
			step:    restartTByJudge,
			wantErr: true,
		},
		"error: script names a file of a foreign installation": {
			rule:    restartTFresh("1.2.3"),
			body:    `{"script":"/home/other/.claude/model-gateway/bin/model-gateway"}`,
			step:    restartTByJudge,
			output:  "9.0.0\n",
			wantErr: true,
		},
		"error: binary names a file of a foreign installation": {
			rule:    restartTFresh("1.2.3"),
			body:    `{"binary":"/home/other/.claude/model-gateway/bin/model-gateway"}`,
			step:    restartTByJudge,
			output:  "9.0.0\n",
			wantErr: true,
		},
		"error: script names another file of the same owner": {
			rule:    restartTFresh("1.2.3"),
			body:    `{"script":"/home/user/projects/model-gateway/bin/model-gateway"}`,
			step:    restartTByJudge,
			output:  "9.0.0\n",
			wantErr: true,
		},
		"error: binary names another file of the same owner": {
			rule:    restartTFresh("1.2.3"),
			body:    `{"binary":"/home/user/projects/model-gateway/bin/model-gateway"}`,
			step:    restartTByJudge,
			output:  "9.0.0\n",
			wantErr: true,
		},
		"error: script names a relative foreign path": {
			rule:    restartTFresh("1.2.3"),
			body:    `{"script":"../foreign.js"}`,
			step:    restartTByParse,
			wantErr: true,
		},
		"error: binary names a relative foreign path": {
			rule:    restartTFresh("1.2.3"),
			body:    `{"binary":"../foreign.js"}`,
			step:    restartTByParse,
			wantErr: true,
		},
		"error: script and binary name the recorded image and the stable image": {
			rule:    restartTFresh("1.2.3"),
			body:    `{"script":"/opt/model-gateway/bin/claude-code-model-gateway","binary":"/home/user/.claude/model-gateway/bin/model-gateway"}`,
			step:    restartTByParse,
			output:  "1.3.0\n",
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			launchTGuard(t, func() {
				r := restartTBuild(t, tt.rule)
				l, step, err := restartTDecide(t, r, inspections, tt.body, tt.output)
				if step != tt.step {
					t.Errorf("restart of %s: the decision ended in %v, want %v", tt.body, step, tt.step)
				}
				if tt.wantErr {
					restartTCheckRefusedLaunch(t, "restart of "+tt.body, l, err)
					return
				}
				if err != nil {
					t.Fatalf("restart of %s: error = %v, want nil", tt.body, err)
				}
				if diff := gocmp.Diff(tt.wantImage, l.Image()); diff != "" {
					t.Errorf("restart of %s: Image() mismatch (-want +got):\n%s", tt.body, diff)
				}
				if diff := gocmp.Diff(tt.wantVersion, l.Version()); diff != "" {
					t.Errorf("restart of %s: Version() mismatch (-want +got):\n%s", tt.body, diff)
				}
			})
		})
	}
}

// restartTDecide runs one restart decision as the integration does: it reads the candidate from body, takes its
// inspection, judges it and, only when the judgement asks for it, completes the decision with output and a second,
// unchanged inspection. A refusal is returned as the unit returned it, with the unit whose result ended the decision.
func restartTDecide(t *testing.T, r workerproc.RestartRule, inspections map[string]workerproc.Executable, body, output string) (workerproc.Launch, restartTStep, error) {
	t.Helper()
	path, err := workerproc.ParseRestart([]byte(body))
	if err != nil {
		if path != "" {
			t.Errorf("ParseRestart(%q) refused with the candidate %q, want \"\"", body, path)
		}
		return workerproc.Launch{}, restartTByParse, err
	}
	if path == "" {
		t.Fatalf("ParseRestart(%q) named no candidate; every row of this table names one", body)
	}
	c, ok := inspections[path]
	if !ok {
		t.Fatalf("ParseRestart(%q) = %q, which no inspection of this table describes", body, path)
	}
	l, needVersion, err := r.Judge(c)
	if err != nil || !needVersion {
		if err != nil && (l != (workerproc.Launch{}) || needVersion) {
			t.Errorf("Judge(%+v) refused with (%+v, %t), want the zero Launch and false", c, l, needVersion)
		}
		return l, restartTByJudge, err
	}
	if l != (workerproc.Launch{}) {
		t.Errorf("Judge(%+v) asked for the version with the launch %+v, want the zero Launch", c, l)
	}
	if output == "" {
		t.Fatalf("Judge(%+v) asked for the version, but the row gives no version output", c)
	}
	l, err = r.AcceptVersion(c, []byte(output), c)
	return l, restartTByAccept, err
}
