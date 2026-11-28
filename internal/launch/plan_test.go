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

package launch_test

import (
	"math"
	"slices"
	"testing"
	"time"
	"unicode/utf16"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/launch"
	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/shimstate"
)

// planTCall runs one call of the unit under test and fails only the calling row when it panics, so that every row
// reports on its own and the package's other tests still run.
func planTCall[T any](t *testing.T, call func() T) (got T, ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("the call panicked: %v", r)
		}
	}()
	return call(), true
}

// planTCopy returns a copy of the health document h, or nil, so that a row can prove the call left h unchanged.
func planTCopy(h *shimstate.Health) *shimstate.Health {
	if h == nil {
		return nil
	}
	c := *h
	return &c
}

// planTServing returns a new health document of a supervisor that is ok, recovers its proxy and serves version.
func planTServing(version string) *shimstate.Health {
	return &shimstate.Health{OK: true, HasServingVersion: true, ServingVersion: version, ProxyRecovery: true}
}

// planTPopulated returns a new health document like planTServing's whose parsed document is populated with the
// members it was read from, so that a row can prove a call left the nested document unchanged.
func planTPopulated(version string) *shimstate.Health {
	h := planTServing(version)
	h.Document = jsonx.WireValue{Kind: '{', Members: []jsonx.Member{
		{Name: utf16.Encode([]rune("ok")), Value: jsonx.WireValue{Kind: 't'}},
		{Name: utf16.Encode([]rune("supervisorVersion")), Value: jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune(version))}},
		{Name: utf16.Encode([]rune("proxyRecovery")), Value: jsonx.WireValue{Kind: 't'}},
	}}
	return h
}

// planTOurs is the reading of a healthy supervisor of this state root with process number 5100 and health h.
func planTOurs(h *shimstate.Health) shimstate.Shim {
	return shimstate.Shim{
		State:      shimstate.RunningOurs,
		PID:        5100,
		HasVersion: h.HasServingVersion,
		Version:    h.ServingVersion,
		Owner:      ownership.Owner{Class: ownership.Ours, PID: 5100},
		Health:     h,
	}
}

// planTForeign is a refused reading of a holder of class c with process number pid (zero when none was observed),
// install root image, executable exe (empty when none was observed), refusal text reason and health h (nil when no
// document answered).
func planTForeign(c ownership.Class, pid int, image, exe, reason string, h *shimstate.Health) shimstate.Shim {
	return shimstate.Shim{
		State:  shimstate.RunningForeign,
		PID:    pid,
		Image:  image,
		Owner:  ownership.Owner{Class: c, PID: pid, Reason: reason, Image: exe},
		Health: h,
	}
}

// planTStarting is a new reading of a supervisor of this state root that holds the port and does not answer as ok.
func planTStarting() shimstate.Shim {
	return shimstate.Shim{
		State:  shimstate.Starting,
		PID:    5100,
		Since:  "2026-11-28T05:00:00.000Z",
		Owner:  ownership.Owner{Class: ownership.Ours, PID: 5100},
		Health: &shimstate.Health{HasServingVersion: true, ServingVersion: "0.4.2", ProxyRecovery: true},
	}
}

// planTRecorded is a new reading of a recorded supervisor that does not hold the port yet.
func planTRecorded() shimstate.Shim {
	return shimstate.Shim{State: shimstate.Starting, PID: 5200, Owner: ownership.Owner{Class: ownership.Unowned}}
}

// planTStopped is a new reading of a port with no supervisor of this state root.
func planTStopped() shimstate.Shim {
	return shimstate.Shim{State: shimstate.Stopped, Owner: ownership.Owner{Class: ownership.Unowned}}
}

func TestAdmit(t *testing.T) {
	tests := map[string]struct {
		req            launch.StartRequest
		stopped        bool
		proxyInstalled bool
		want           string
	}{
		"success: a start with no stop marker and an installed proxy may read the shim": {
			req:            launch.StartRequest{Operation: "start"},
			proxyInstalled: true,
			want:           "",
		},
		"success: a normal resuming start with no stop marker may read the shim": {
			req:            launch.StartRequest{Operation: "start", ResumeIntent: true},
			proxyInstalled: true,
			want:           "",
		},
		"success: a resuming start proceeds over the stop marker": {
			req:            launch.StartRequest{Operation: "start", ResumeIntent: true},
			stopped:        true,
			proxyInstalled: true,
			want:           "",
		},
		"error: the stop marker refuses a start without resume intent": {
			req:            launch.StartRequest{Operation: "recover"},
			stopped:        true,
			proxyInstalled: true,
			want:           "gateway intentionally stopped; run start to resume",
		},
		"error: the stop marker refuses before the missing proxy is named": {
			req:     launch.StartRequest{Operation: "recover"},
			stopped: true,
			want:    "gateway intentionally stopped; run start to resume",
		},
		"error: neither quiet nor preserving a running supervisor lifts the stop marker": {
			req:            launch.StartRequest{Operation: "ensure", Quiet: true, PreserveRunning: true},
			stopped:        true,
			proxyInstalled: true,
			want:           "gateway intentionally stopped; run start to resume",
		},
		"error: a missing proxy refuses a start with no stop marker": {
			req:  launch.StartRequest{Operation: "start"},
			want: "proxy binary missing (run setup)",
		},
		"error: a missing proxy refuses a normal resuming start with no stop marker": {
			req:  launch.StartRequest{Operation: "start", ResumeIntent: true},
			want: "proxy binary missing (run setup)",
		},
		"error: a missing proxy refuses a resuming start that passed the stop marker": {
			req:     launch.StartRequest{Operation: "start", ResumeIntent: true},
			stopped: true,
			want:    "proxy binary missing (run setup)",
		},
		"error: a missing proxy refuses an unnamed quiet start": {
			req:  launch.StartRequest{Quiet: true},
			want: "proxy binary missing (run setup)",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := planTCall(t, func() string { return launch.Admit(tt.req, tt.stopped, tt.proxyInstalled) })
			if !ok {
				return
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Admit(%+v, stopped %t, proxy installed %t) mismatch (-want +got):\n%s", tt.req, tt.stopped, tt.proxyInstalled, diff)
			}
		})
	}
}

func TestChoose(t *testing.T) {
	const (
		legacy18080    = "refusing to stop PID 4200 on :18080; it is the Node model-gateway of this install, not this gateway; stop it with its own command; left the listener untouched"
		legacy28080    = "refusing to stop PID 4210 on :28080; it is the Node model-gateway of this install, not this gateway; stop it with its own command; left the listener untouched"
		legacy38080    = "refusing to stop PID 4220 on :38080; it is the Node model-gateway of this install, not this gateway; stop it with its own command; left the listener untouched"
		unknown18080   = "could not confirm the owner of :18080 (last observed PID 4300); the owning process could not be identified as this model-gateway install; left the listener untouched"
		unknown28080   = "could not confirm the owner of :28080 (last observed PID 4310); the ownership probe exhausted its 2000ms budget (set CODEX_GATEWAY_PROBE_TIMEOUT_MS to override); left the listener untouched"
		unknownNoPID   = "could not confirm the owner of :18080 (last observed PID unknown); the ownership probe exhausted its 2000ms budget (set CODEX_GATEWAY_PROBE_TIMEOUT_MS to override); left the listener untouched"
		hidden18080    = "could not confirm the owner of :18080 (last observed PID 4300); the owning process did not expose its command line. It may be elevated or otherwise protected, so run stop or setup from a session with the same privileges; left the listener untouched"
		portOwned18080 = "could not confirm the owner of :18080 (last observed PID unknown); the owning process could not be identified as this model-gateway install; left the listener untouched"
		portOwned28080 = "could not confirm the owner of :28080 (last observed PID unknown); the owning process could not be identified as this model-gateway install; left the listener untouched"
		portOwned38080 = "could not confirm the owner of :38080 (last observed PID unknown); the owning process could not be identified as this model-gateway install; left the listener untouched"
	)

	tests := map[string]struct {
		req       launch.StartRequest
		s         shimstate.Shim
		installed string
		port      int
		want      launch.Plan
		// unchanged, when set, is the reading's health document built again from the same literal; otherwise the row
		// compares a copy taken before the call.
		unchanged *shimstate.Health
	}{
		// A supervisor of this root serving a current version is kept, whatever the operation's name.
		"success: a supervisor of this root serving the installed version is kept and its orphans swept": {
			req:       launch.StartRequest{Operation: "start"},
			s:         planTOurs(planTPopulated("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true},
			unchanged: planTPopulated("0.4.2"),
		},
		"success: a resumed start keeps a current supervisor": {
			req:       launch.StartRequest{Operation: "start", ResumeIntent: true},
			s:         planTOurs(planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true},
		},
		"success: a kept supervisor is waited for when the start preserves running supervisors": {
			req:       launch.StartRequest{Operation: "ensure", PreserveRunning: true},
			s:         planTOurs(planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true, AwaitReadiness: true},
		},
		"success: an ensure that does not preserve running supervisors keeps without waiting": {
			req:       launch.StartRequest{Operation: "ensure"},
			s:         planTOurs(planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true},
		},
		"success: an unnamed start keeps a current supervisor and sweeps its orphans too": {
			req:       launch.StartRequest{},
			s:         planTOurs(planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true},
		},
		"success: an unnamed preserving start keeps a current supervisor and waits for it": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTOurs(planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true, AwaitReadiness: true},
		},
		"success: a supervisor serving a newer version than this CLI is kept": {
			req:       launch.StartRequest{Operation: "recover", Quiet: true},
			s:         planTOurs(planTServing("0.5.0")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true},
		},
		"success: a preserving setup keeps a newer supervisor and waits for it": {
			req:       launch.StartRequest{Operation: "setup", PreserveRunning: true},
			s:         planTOurs(planTServing("0.5.0")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true, AwaitReadiness: true},
		},
		"success: an unnamed start keeps a newer supervisor": {
			req:       launch.StartRequest{},
			s:         planTOurs(planTServing("1.0.0")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true},
		},
		"success: an unnamed preserving start keeps a newer supervisor and waits for it": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTOurs(planTServing("1.0.0")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true, AwaitReadiness: true},
		},
		"success: versions compare by number, not by text": {
			req:       launch.StartRequest{Operation: "start"},
			s:         planTOurs(planTServing("0.4.10")),
			installed: "0.4.9",
			port:      18080,
			want:      launch.Plan{Action: launch.KeepRunning, ReapOrphans: true},
		},

		// A supervisor of this root that must be replaced is stopped first, whatever the operation's name.
		"success: a resumed start stops a supervisor serving an older version and replaces it": {
			req:       launch.StartRequest{Operation: "start", ResumeIntent: true},
			s:         planTOurs(planTServing("0.4.1")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: preserving running supervisors does not keep an older one": {
			req:       launch.StartRequest{Operation: "setup", PreserveRunning: true},
			s:         planTOurs(planTServing("0.3.9")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed start stops an older supervisor before it replaces it": {
			req:       launch.StartRequest{},
			s:         planTOurs(planTServing("0.4.1")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed preserving start stops an older supervisor before it replaces it": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTOurs(planTServing("0.3.0")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: a supervisor that does not recover its proxy is stopped and replaced": {
			req:       launch.StartRequest{Operation: "start"},
			s:         planTOurs(&shimstate.Health{OK: true, HasServingVersion: true, ServingVersion: "0.4.2"}),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: a preserving ensure replaces a supervisor that does not recover its proxy": {
			req:       launch.StartRequest{Operation: "ensure", PreserveRunning: true},
			s:         planTOurs(&shimstate.Health{OK: true, HasServingVersion: true, ServingVersion: "0.4.2"}),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed start stops a supervisor that does not recover its proxy": {
			req:       launch.StartRequest{},
			s:         planTOurs(&shimstate.Health{OK: true, HasServingVersion: true, ServingVersion: "0.5.0"}),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed preserving start stops a supervisor that does not recover its proxy": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTOurs(&shimstate.Health{OK: true, HasServingVersion: true, ServingVersion: "0.4.2"}),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: a supervisor that names no serving version is stopped and replaced": {
			req:       launch.StartRequest{Operation: "start"},
			s:         planTOurs(&shimstate.Health{OK: true, ProxyRecovery: true}),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed start stops a supervisor that names no serving version": {
			req:       launch.StartRequest{},
			s:         planTOurs(&shimstate.Health{OK: true, ProxyRecovery: true}),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed preserving start stops a supervisor that names no serving version": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTOurs(&shimstate.Health{OK: true, ProxyRecovery: true}),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: a serving version that does not parse is stopped and replaced": {
			req:       launch.StartRequest{Operation: "recover"},
			s:         planTOurs(planTServing("latest")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed preserving start replaces a serving version that does not parse": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTOurs(planTServing("latest")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an installed version that does not parse replaces the supervisor": {
			req:       launch.StartRequest{Operation: "start"},
			s:         planTOurs(planTServing("0.4.2")),
			installed: "dev",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed start with an installed version that does not parse replaces the supervisor": {
			req:       launch.StartRequest{},
			s:         planTOurs(planTServing("0.4.2")),
			installed: "dev",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},

		"success: a preserving ensure stops a supervisor that names no serving version": {
			req:       launch.StartRequest{Operation: "ensure", PreserveRunning: true},
			s:         planTOurs(&shimstate.Health{OK: true, ProxyRecovery: true}),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: a preserving setup replaces a serving version that does not parse": {
			req:       launch.StartRequest{Operation: "setup", PreserveRunning: true},
			s:         planTOurs(planTServing("latest")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed start replaces a serving version that does not parse": {
			req:       launch.StartRequest{},
			s:         planTOurs(planTServing("nightly")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: a preserving ensure with an installed version that does not parse replaces the supervisor": {
			req:       launch.StartRequest{Operation: "ensure", PreserveRunning: true},
			s:         planTOurs(planTServing("0.4.2")),
			installed: "dev",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed preserving start with an installed version that does not parse replaces the supervisor": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTOurs(planTServing("0.4.2")),
			installed: "local-build",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},

		// A starting supervisor is waited for only when the start preserves running supervisors.
		"success: a starting supervisor is stopped and replaced when the start does not preserve it": {
			req:       launch.StartRequest{Operation: "recover"},
			s:         planTStarting(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an ensure that does not preserve running supervisors replaces a starting one": {
			req:       launch.StartRequest{Operation: "ensure"},
			s:         planTStarting(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: a starting supervisor is waited for when the start preserves running supervisors": {
			req:       launch.StartRequest{Operation: "ensure", PreserveRunning: true},
			s:         planTStarting(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.WaitForRunning},
		},
		"success: an unnamed start stops a starting supervisor before it replaces it": {
			req:       launch.StartRequest{},
			s:         planTStarting(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed preserving start waits for a starting supervisor": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTStarting(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.WaitForRunning},
		},
		"success: a recorded supervisor that does not hold the port yet is stopped and replaced": {
			req:       launch.StartRequest{Operation: "start"},
			s:         planTRecorded(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: a recorded supervisor that does not hold the port yet is waited for by a preserving start": {
			req:       launch.StartRequest{Operation: "setup", PreserveRunning: true},
			s:         planTRecorded(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.WaitForRunning},
		},
		"success: an unnamed start stops a recorded supervisor before it replaces it": {
			req:       launch.StartRequest{},
			s:         planTRecorded(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace, StopFirst: true},
		},
		"success: an unnamed preserving start waits for a recorded supervisor": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTRecorded(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.WaitForRunning},
		},

		// A stopped port is launched on without a stop.
		"success: a stopped port is launched on without a stop": {
			req:       launch.StartRequest{},
			s:         planTStopped(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace},
		},
		"success: a stopped port is launched on without a stop also by a preserving start": {
			req:       launch.StartRequest{Operation: "ensure", PreserveRunning: true},
			s:         planTStopped(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace},
		},
		"success: a stopped port is launched on without a stop by an unnamed preserving start": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTStopped(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace},
		},
		"success: a resumed start launches on a stopped port without a stop": {
			req:       launch.StartRequest{Operation: "start", ResumeIntent: true},
			s:         planTStopped(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace},
		},
		"success: the plan does not refuse on the stop marker, which the admission decided": {
			req:       launch.StartRequest{Operation: "recover", Quiet: true},
			s:         planTStopped(),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Replace},
		},

		// A holder from another install root is refused with its root, without an event, whatever its health.
		"error: a holder from another install root is refused with that root and no event": {
			req:       launch.StartRequest{Operation: "start"},
			s:         planTForeign(ownership.Foreign, 4100, "/opt/other/plugins/model-gateway", "/usr/local/bin/node", "refusing to stop PID 4100 on :18080; it belongs to a different install root (/opt/other/plugins/model-gateway), not /home/u/.claude/plugins/model-gateway", planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: "PID 4100 owns :18080 from a different install root (/opt/other/plugins/model-gateway)"},
		},
		"error: the refusal of another install root names the port argument": {
			req:       launch.StartRequest{Operation: "recover"},
			s:         planTForeign(ownership.Foreign, 4101, "/srv/root two", "/usr/local/bin/node", "refusing to stop PID 4101 on :18080; it belongs to a different install root (/srv/root two), not /home/u/.claude/plugins/model-gateway", planTServing("0.4.2")),
			installed: "0.4.2",
			port:      28080,
			want:      launch.Plan{Action: launch.Refuse, Reason: "PID 4101 owns :28080 from a different install root (/srv/root two)"},
		},
		"error: a holder from another install root without a health document is refused by a preserving setup": {
			req:       launch.StartRequest{Operation: "setup", PreserveRunning: true},
			s:         planTForeign(ownership.Foreign, 4102, "/opt/third", "/usr/local/bin/node", "refusing to stop PID 4102 on :18080; it belongs to a different install root (/opt/third), not /home/u/.claude/plugins/model-gateway", nil),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: "PID 4102 owns :18080 from a different install root (/opt/third)"},
		},
		"error: a holder from another install root without a health document is refused by an unnamed start": {
			req:       launch.StartRequest{},
			s:         planTForeign(ownership.Foreign, 4103, "/home/v/.claude/plugins/model-gateway", "/usr/local/bin/node", "refusing to stop PID 4103 on :38080; it belongs to a different install root (/home/v/.claude/plugins/model-gateway), not /home/u/.claude/plugins/model-gateway", nil),
			installed: "0.4.2",
			port:      38080,
			want:      launch.Plan{Action: launch.Refuse, Reason: "PID 4103 owns :38080 from a different install root (/home/v/.claude/plugins/model-gateway)"},
		},
		"error: a healthy holder from another install root is refused by an unnamed preserving start": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTForeign(ownership.Foreign, 4104, "/x/y", "/usr/local/bin/node", "refusing to stop PID 4104 on :18080; it belongs to a different install root (/x/y), not /home/u/.claude/plugins/model-gateway", planTServing("0.5.0")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: "PID 4104 owns :18080 from a different install root (/x/y)"},
		},
		"error: a resumed start refuses a holder from another install root": {
			req:       launch.StartRequest{Operation: "start", ResumeIntent: true},
			s:         planTForeign(ownership.Foreign, 4105, "/opt/resumed", "/usr/local/bin/node", "refusing to stop PID 4105 on :18080; it belongs to a different install root (/opt/resumed), not /home/u/.claude/plugins/model-gateway", nil),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: "PID 4105 owns :18080 from a different install root (/opt/resumed)"},
		},

		// This install's Node gateway is refused with ownership's text, without an event, whatever its health.
		"error: this install's Node gateway is refused with ownership's text and no event": {
			req:       launch.StartRequest{Operation: "recover"},
			s:         planTForeign(ownership.LegacyNode, 4200, "/home/u/.claude/plugins/model-gateway", "/usr/local/bin/node", legacy18080, planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: legacy18080},
		},
		"error: this install's Node gateway without a health document is refused by a preserving ensure": {
			req:       launch.StartRequest{Operation: "ensure", PreserveRunning: true},
			s:         planTForeign(ownership.LegacyNode, 4210, "/home/u/.claude/plugins/model-gateway", "/usr/local/bin/node", legacy28080, nil),
			installed: "0.4.2",
			port:      28080,
			want:      launch.Plan{Action: launch.Refuse, Reason: legacy28080},
		},
		"error: this install's Node gateway without a health document is refused by an unnamed start": {
			req:       launch.StartRequest{},
			s:         planTForeign(ownership.LegacyNode, 4220, "/home/u/.claude/plugins/model-gateway", "/usr/local/bin/node", legacy38080, nil),
			installed: "0.4.2",
			port:      38080,
			want:      launch.Plan{Action: launch.Refuse, Reason: legacy38080},
		},
		"error: a healthy Node gateway of this install is refused by an unnamed preserving start": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTForeign(ownership.LegacyNode, 4210, "/home/u/.claude/plugins/model-gateway", "/usr/local/bin/node", legacy28080, planTServing("0.4.2")),
			installed: "0.4.2",
			port:      28080,
			want:      launch.Plan{Action: launch.Refuse, Reason: legacy28080},
		},
		"error: a resumed start refuses this install's Node gateway": {
			req:       launch.StartRequest{Operation: "start", ResumeIntent: true},
			s:         planTForeign(ownership.LegacyNode, 4200, "/home/u/.claude/plugins/model-gateway", "/usr/local/bin/node", legacy18080, nil),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: legacy18080},
		},

		// An unidentified holder is refused with its reason and an event named after the operation, or start.
		"error: an unidentified holder is refused and the event is named after the operation": {
			req:       launch.StartRequest{Operation: "recover"},
			s:         planTForeign(ownership.Unknown, 4300, "", "/usr/local/bin/node", unknown18080, planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: unknown18080, OwnerUnknownEvent: "recover-owner-unknown"},
		},
		"error: an unnamed start refusing an unidentified holder names its event start": {
			req:       launch.StartRequest{},
			s:         planTForeign(ownership.Unknown, 4300, "", "/usr/local/bin/node", hidden18080, planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: hidden18080, OwnerUnknownEvent: "start-owner-unknown"},
		},
		"error: a preserving setup refuses an unidentified holder without a health document": {
			req:       launch.StartRequest{Operation: "setup", PreserveRunning: true},
			s:         planTForeign(ownership.Unknown, 4310, "", "/usr/local/bin/node", unknown28080, nil),
			installed: "0.4.2",
			port:      28080,
			want:      launch.Plan{Action: launch.Refuse, Reason: unknown28080, OwnerUnknownEvent: "setup-owner-unknown"},
		},
		"error: an unnamed preserving start refusing an unidentified holder names its event start": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTForeign(ownership.Unknown, 4300, "", "/usr/local/bin/node", unknown18080, nil),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: unknown18080, OwnerUnknownEvent: "start-owner-unknown"},
		},
		"error: a resumed start refuses an unidentified holder": {
			req:       launch.StartRequest{Operation: "start", ResumeIntent: true},
			s:         planTForeign(ownership.Unknown, 4310, "", "/usr/local/bin/node", unknown28080, nil),
			installed: "0.4.2",
			port:      28080,
			want:      launch.Plan{Action: launch.Refuse, Reason: unknown28080, OwnerUnknownEvent: "start-owner-unknown"},
		},

		"error: a recover refuses a holder whose process number and image were never observed": {
			req:       launch.StartRequest{Operation: "recover"},
			s:         planTForeign(ownership.Unknown, 0, "", "", unknownNoPID, nil),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: unknownNoPID, OwnerUnknownEvent: "recover-owner-unknown"},
		},

		// A port whose holder could not be resolved is refused like an unidentified holder.
		"error: a port whose holder could not be resolved is refused like an unidentified holder": {
			req:       launch.StartRequest{Operation: "ensure", PreserveRunning: true},
			s:         planTForeign(ownership.PortOwned, 0, "", "", portOwned18080, planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: portOwned18080, OwnerUnknownEvent: "ensure-owner-unknown"},
		},
		"error: an unnamed start refusing an unresolved holder names its event start": {
			req:       launch.StartRequest{Quiet: true},
			s:         planTForeign(ownership.PortOwned, 0, "", "", portOwned18080, planTServing("0.4.2")),
			installed: "0.4.2",
			port:      18080,
			want:      launch.Plan{Action: launch.Refuse, Reason: portOwned18080, OwnerUnknownEvent: "start-owner-unknown"},
		},
		"error: a recover refuses an unresolved holder without a health document": {
			req:       launch.StartRequest{Operation: "recover"},
			s:         planTForeign(ownership.PortOwned, 0, "", "", portOwned28080, nil),
			installed: "0.4.2",
			port:      28080,
			want:      launch.Plan{Action: launch.Refuse, Reason: portOwned28080, OwnerUnknownEvent: "recover-owner-unknown"},
		},
		"error: an unnamed preserving start refusing an unresolved holder names its event start": {
			req:       launch.StartRequest{PreserveRunning: true},
			s:         planTForeign(ownership.PortOwned, 0, "", "", portOwned38080, nil),
			installed: "0.4.2",
			port:      38080,
			want:      launch.Plan{Action: launch.Refuse, Reason: portOwned38080, OwnerUnknownEvent: "start-owner-unknown"},
		},
		"error: a resumed start refuses an unresolved holder": {
			req:       launch.StartRequest{Operation: "start", ResumeIntent: true},
			s:         planTForeign(ownership.PortOwned, 0, "", "", portOwned28080, nil),
			installed: "0.4.2",
			port:      28080,
			want:      launch.Plan{Action: launch.Refuse, Reason: portOwned28080, OwnerUnknownEvent: "start-owner-unknown"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := tt.unchanged
			if before == nil {
				before = planTCopy(tt.s.Health)
			}
			got, ok := planTCall(t, func() launch.Plan { return launch.Choose(tt.req, tt.s, tt.installed, tt.port) })
			if !ok {
				return
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Choose(%+v, %s reading, %q, %d) mismatch (-want +got):\n%s", tt.req, tt.s.State, tt.installed, tt.port, diff)
			}
			if diff := gocmp.Diff(before, tt.s.Health); diff != "" {
				t.Errorf("Choose changed the reading's health document (-before +after):\n%s", diff)
			}
		})
	}
}

func TestStartupLimit(t *testing.T) {
	tests := map[string]struct {
		quiet          bool
		drainTimeoutMS float64
		want           time.Duration
	}{
		"success: a quiet start waits 12 s whatever the drain timeout": {
			quiet:          true,
			drainTimeoutMS: 30000,
			want:           12 * time.Second,
		},
		"success: a quiet start waits 12 s also for a drain timeout that would saturate": {
			quiet:          true,
			drainTimeoutMS: 1e300,
			want:           12 * time.Second,
		},
		"success: a quiet start waits 12 s for a drain timeout that is not a number": {
			quiet:          true,
			drainTimeoutMS: math.NaN(),
			want:           12 * time.Second,
		},
		"success: the default drain timeout of 30000 ms gives 42 s": {
			drainTimeoutMS: 30000,
			want:           42 * time.Second,
		},
		"success: a drain timeout of 1 ms adds to the 12 s": {
			drainTimeoutMS: 1,
			want:           12001 * time.Millisecond,
		},
		"success: a zero drain timeout gives 12 s": {
			drainTimeoutMS: 0,
			want:           12 * time.Second,
		},
		"success: a positive drain timeout below a nanosecond gives exactly 12 s": {
			drainTimeoutMS: 1e-7,
			want:           12 * time.Second,
		},
		"success: a negative drain timeout never shortens the wait below 12 s": {
			drainTimeoutMS: -5000,
			want:           12 * time.Second,
		},
		"success: a drain timeout of minus infinity gives 12 s": {
			drainTimeoutMS: math.Inf(-1),
			want:           12 * time.Second,
		},
		"success: a drain timeout that is not a number gives 12 s": {
			drainTimeoutMS: math.NaN(),
			want:           12 * time.Second,
		},
		"success: a fraction of a nanosecond is truncated toward zero": {
			drainTimeoutMS: 1.0000015,
			want:           12001000001 * time.Nanosecond,
		},
		"success: a large drain timeout below the largest duration is not saturated": {
			drainTimeoutMS: 9e12,
			want:           9000000012000000000 * time.Nanosecond,
		},
		"success: a drain timeout that fits only with the allowance added is not saturated": {
			drainTimeoutMS: 9223372024848,
			want:           9223372036848000000 * time.Nanosecond,
		},
		"success: a drain timeout that fits alone but overflows with the allowance saturates": {
			drainTimeoutMS: 9223372024864,
			want:           time.Duration(math.MaxInt64),
		},
		"success: the largest drain timeout below saturation is truncated without rounding the product first": {
			drainTimeoutMS: 0x1.0c6f7a0582d8dp+43,
			want:           9223372036854775390 * time.Nanosecond,
		},
		"success: the next larger drain timeout saturates": {
			drainTimeoutMS: 0x1.0c6f7a0582d8ep+43,
			want:           time.Duration(math.MaxInt64),
		},
		"success: a drain timeout beyond the largest duration saturates": {
			drainTimeoutMS: 1e300,
			want:           time.Duration(math.MaxInt64),
		},
		"success: a drain timeout just beyond the largest duration saturates instead of wrapping": {
			drainTimeoutMS: 9.3e12,
			want:           time.Duration(math.MaxInt64),
		},
		"success: an infinite drain timeout saturates": {
			drainTimeoutMS: math.Inf(1),
			want:           time.Duration(math.MaxInt64),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := planTCall(t, func() time.Duration { return launch.StartupLimit(tt.quiet, tt.drainTimeoutMS) })
			if !ok {
				return
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("StartupLimit(quiet %t, %v ms) mismatch (-want +got):\n%s", tt.quiet, tt.drainTimeoutMS, diff)
			}
		})
	}
}

func TestStaleSessionNotice(t *testing.T) {
	tests := map[string]struct {
		installed string
		h         *shimstate.Health
		want      string
		// unchanged, when set, is h built again from the same literal; otherwise the row compares a copy taken before
		// the call.
		unchanged *shimstate.Health
	}{
		"success: a newer serving supervisor gets the reload notice": {
			installed: "0.4.2",
			h:         planTPopulated("0.5.0"),
			unchanged: planTPopulated("0.5.0"),
			want:      "model-gateway: this session loaded 0.4.2, but the serving shim is newer (0.5.0). Reload plugins with /reload-plugins or restart Claude Code; the newer shim was left running.",
		},
		"success: the versions are written as they were given, not as they were parsed": {
			installed: "v0.4.2-dev+abc",
			h:         planTServing("model-gateway 0.10.0 (build 7)"),
			want:      "model-gateway: this session loaded v0.4.2-dev+abc, but the serving shim is newer (model-gateway 0.10.0 (build 7)). Reload plugins with /reload-plugins or restart Claude Code; the newer shim was left running.",
		},
		"success: a replacement character in the serving version is written as it is": {
			installed: "0.4.2",
			h:         planTServing("0.4.3-�"),
			want:      "model-gateway: this session loaded 0.4.2, but the serving shim is newer (0.4.3-�). Reload plugins with /reload-plugins or restart Claude Code; the newer shim was left running.",
		},
		"success: the notice depends on the versions only, not on the document's other members": {
			installed: "0.4.2",
			h:         &shimstate.Health{HasServingVersion: true, ServingVersion: "1.0.0"},
			want:      "model-gateway: this session loaded 0.4.2, but the serving shim is newer (1.0.0). Reload plugins with /reload-plugins or restart Claude Code; the newer shim was left running.",
		},
		"success: equal versions get no notice": {
			installed: "0.4.2",
			h:         planTServing("0.4.2"),
			want:      "",
		},
		"success: versions equal once parsed get no notice although they are spelled differently": {
			installed: "v0.4.2-dev",
			h:         planTServing("0.4.2+build"),
			want:      "",
		},
		"success: an older serving supervisor gets no notice": {
			installed: "0.4.10",
			h:         planTServing("0.4.9"),
			want:      "",
		},
		"success: a serving version that does not parse gets no notice": {
			installed: "0.4.2",
			h:         planTServing("latest"),
			want:      "",
		},
		"success: a document that selects no serving version gets no notice": {
			installed: "0.4.2",
			h:         &shimstate.Health{OK: true, ProxyRecovery: true},
			want:      "",
		},
		"success: an installed version that does not parse gets no notice": {
			installed: "dev",
			h:         planTServing("0.5.0"),
			want:      "",
		},
		"success: no health document gets no notice": {
			installed: "0.4.2",
			h:         nil,
			want:      "",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := tt.unchanged
			if before == nil {
				before = planTCopy(tt.h)
			}
			got, ok := planTCall(t, func() string { return launch.StaleSessionNotice(tt.installed, tt.h) })
			if !ok {
				return
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("StaleSessionNotice(%q, %+v) mismatch (-want +got):\n%s", tt.installed, tt.h, diff)
			}
			if diff := gocmp.Diff(before, tt.h); diff != "" {
				t.Errorf("StaleSessionNotice changed the health document (-before +after):\n%s", diff)
			}
		})
	}
}

func TestFinish(t *testing.T) {
	const (
		homeLogs  = "/home/u/.local/state/model gateway/logs"
		otherLogs = "/var/tmp/other state/logs"
	)

	t.Run("finish", func(t *testing.T) {
		tests := map[string]struct {
			r        launch.ReadyResult
			started  []string
			pid      int
			limit    time.Duration
			logs     string
			quiet    bool
			want     launch.Outcome
			recovery string
		}{
			"success: a ready launch reports what it launched": {
				r:        launch.ReadyResult{Ready: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    42 * time.Second,
				logs:     homeLogs,
				want:     launch.Outcome{OK: true, Started: []string{"shim"}},
				recovery: "ready",
			},
			"success: a quiet ready launch still reports what it launched": {
				r:        launch.ReadyResult{Ready: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    12 * time.Second,
				logs:     homeLogs,
				quiet:    true,
				want:     launch.Outcome{OK: true, Started: []string{"shim"}},
				recovery: "ready",
			},
			"success: a ready kept supervisor reports nothing launched, also when quiet": {
				r:        launch.ReadyResult{Ready: true},
				pid:      5100,
				limit:    12 * time.Second,
				logs:     homeLogs,
				quiet:    true,
				want:     launch.Outcome{OK: true},
				recovery: "ready",
			},
			"success: a ready start passes an allocated empty launch list through as it is": {
				r:        launch.ReadyResult{Ready: true},
				started:  []string{},
				pid:      5100,
				limit:    42 * time.Second,
				logs:     homeLogs,
				want:     launch.Outcome{OK: true, Started: []string{}},
				recovery: "ready",
			},
			"error: a failure record's message is the reason, and nothing is reported launched": {
				r:        launch.ReadyResult{Reason: "listen tcp 127.0.0.1:18080: bind: address already in use"},
				started:  []string{"shim"},
				pid:      4242,
				limit:    42 * time.Second,
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "listen tcp 127.0.0.1:18080: bind: address already in use"},
				recovery: "failed",
			},
			"error: a failure message that already carries the product prefix is copied as it is": {
				r:        launch.ReadyResult{Reason: "model-gateway: shim supervisor cannot bind 127.0.0.1:18080: EADDRINUSE"},
				started:  []string{"shim"},
				pid:      4242,
				limit:    12 * time.Second,
				logs:     homeLogs,
				quiet:    true,
				want:     launch.Outcome{Reason: "model-gateway: shim supervisor cannot bind 127.0.0.1:18080: EADDRINUSE"},
				recovery: "failed",
			},
			"error: a timed-out wait for a live supervisor reports it still starting": {
				r:        launch.ReadyResult{TimedOut: true, StillStarting: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    42 * time.Second,
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "still starting after 42s; supervisor pid 4242 is running", Started: []string{"shim"}, StillStarting: true, SupervisorPID: 4242},
				recovery: "starting",
			},
			"error: a quiet timed-out wait for a live supervisor was cut short": {
				r:        launch.ReadyResult{TimedOut: true, StillStarting: true},
				started:  []string{"shim"},
				pid:      4243,
				limit:    12 * time.Second,
				logs:     homeLogs,
				quiet:    true,
				want:     launch.Outcome{Reason: "still starting after 12s; supervisor pid 4243 is running", Started: []string{"shim"}, StillStarting: true, SupervisorPID: 4243, WaitCutShort: true},
				recovery: "starting",
			},
			"error: a 12 s wait for a live supervisor that is not quiet was not cut short": {
				r:        launch.ReadyResult{TimedOut: true, StillStarting: true},
				started:  []string{"shim"},
				pid:      4244,
				limit:    12 * time.Second,
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "still starting after 12s; supervisor pid 4244 is running", Started: []string{"shim"}, StillStarting: true, SupervisorPID: 4244},
				recovery: "starting",
			},
			"error: a timed-out wait for a kept supervisor that is still starting names its pid": {
				r:        launch.ReadyResult{TimedOut: true, StillStarting: true},
				pid:      5100,
				limit:    42 * time.Second,
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "still starting after 42s; supervisor pid 5100 is running", StillStarting: true, SupervisorPID: 5100},
				recovery: "starting",
			},
			"error: a timed-out wait passes an allocated empty launch list through as it is": {
				r:        launch.ReadyResult{TimedOut: true, StillStarting: true},
				started:  []string{},
				pid:      5100,
				limit:    42 * time.Second,
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "still starting after 42s; supervisor pid 5100 is running", Started: []string{}, StillStarting: true, SupervisorPID: 5100},
				recovery: "starting",
			},
			"error: a timed-out wait for a supervisor that is gone points at the logs": {
				r:        launch.ReadyResult{TimedOut: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    42 * time.Second,
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "not healthy after 42s (check logs in /home/u/.local/state/model gateway/logs)", Started: []string{"shim"}},
				recovery: "failed",
			},
			"error: an unhealthy kept supervisor reports nothing launched and names the logs it was given": {
				r:        launch.ReadyResult{TimedOut: true},
				pid:      5100,
				limit:    42 * time.Second,
				logs:     otherLogs,
				want:     launch.Outcome{Reason: "not healthy after 42s (check logs in /var/tmp/other state/logs)"},
				recovery: "failed",
			},
			"error: a quiet timed-out wait for a supervisor that is gone was cut short": {
				r:        launch.ReadyResult{TimedOut: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    12 * time.Second,
				logs:     homeLogs,
				quiet:    true,
				want:     launch.Outcome{Reason: "not healthy after 12s (check logs in /home/u/.local/state/model gateway/logs)", Started: []string{"shim"}, WaitCutShort: true},
				recovery: "failed",
			},
			"error: a 12 s wait for a supervisor that is gone and not quiet was not cut short": {
				r:        launch.ReadyResult{TimedOut: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    12 * time.Second,
				logs:     otherLogs,
				want:     launch.Outcome{Reason: "not healthy after 12s (check logs in /var/tmp/other state/logs)", Started: []string{"shim"}},
				recovery: "failed",
			},
			"error: the waited seconds are the ceiling of the limit, not its rounding": {
				r:        launch.ReadyResult{TimedOut: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    12001 * time.Millisecond,
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "not healthy after 13s (check logs in /home/u/.local/state/model gateway/logs)", Started: []string{"shim"}},
				recovery: "failed",
			},
			"error: a limit of whole seconds is not raised by its ceiling": {
				r:        launch.ReadyResult{TimedOut: true, StillStarting: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    30 * time.Second,
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "still starting after 30s; supervisor pid 4242 is running", Started: []string{"shim"}, StillStarting: true, SupervisorPID: 4242},
				recovery: "starting",
			},
			"error: a limit a nanosecond over a second waits two seconds": {
				r:        launch.ReadyResult{TimedOut: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    time.Second + time.Nanosecond,
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "not healthy after 2s (check logs in /home/u/.local/state/model gateway/logs)", Started: []string{"shim"}},
				recovery: "failed",
			},
			"error: the ceiling of a large limit is exact where seconds as a float would round it down": {
				r:        launch.ReadyResult{TimedOut: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    9000000003000000512 * time.Nanosecond,
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "not healthy after 9000000004s (check logs in /home/u/.local/state/model gateway/logs)", Started: []string{"shim"}},
				recovery: "failed",
			},
			"error: a saturated limit is written as a decimal number of seconds": {
				r:        launch.ReadyResult{TimedOut: true, StillStarting: true},
				started:  []string{"shim"},
				pid:      4242,
				limit:    time.Duration(math.MaxInt64),
				logs:     homeLogs,
				want:     launch.Outcome{Reason: "still starting after 9223372037s; supervisor pid 4242 is running", Started: []string{"shim"}, StillStarting: true, SupervisorPID: 4242},
				recovery: "starting",
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				before := slices.Clone(tt.started)
				got, ok := planTCall(t, func() launch.Outcome {
					return launch.Finish(tt.r, tt.started, tt.pid, tt.limit, tt.logs, tt.quiet)
				})
				if !ok {
					return
				}
				if diff := gocmp.Diff(tt.want, got); diff != "" {
					t.Errorf("Finish(%+v, %q, %d, %v, %q, quiet %t) mismatch (-want +got):\n%s", tt.r, tt.started, tt.pid, tt.limit, tt.logs, tt.quiet, diff)
				}
				if diff := gocmp.Diff(before, tt.started); diff != "" {
					t.Errorf("Finish changed the launch list it was given (-before +after):\n%s", diff)
				}
				recovery, ok := planTCall(t, got.RecoveryOutcome)
				if !ok {
					return
				}
				if diff := gocmp.Diff(tt.recovery, recovery); diff != "" {
					t.Errorf("RecoveryOutcome of %+v mismatch (-want +got):\n%s", got, diff)
				}
			})
		}
	})

	t.Run("recovery outcome", func(t *testing.T) {
		tests := map[string]struct {
			o    launch.Outcome
			want string
		}{
			"success: an ok outcome is ready": {
				o:    launch.Outcome{OK: true},
				want: "ready",
			},
			"success: an ok outcome is ready before anything else it reports": {
				o:    launch.Outcome{OK: true, StillStarting: true, SupervisorPID: 4242, RecoveryAttempted: true},
				want: "ready",
			},
			"success: a supervisor still starting at the limit is starting": {
				o:    launch.Outcome{Reason: "still starting after 42s; supervisor pid 4242 is running", StillStarting: true, SupervisorPID: 4242},
				want: "starting",
			},
			"success: a still-starting outcome is starting also without a supervisor pid": {
				o:    launch.Outcome{StillStarting: true},
				want: "starting",
			},
			"error: a supervisor pid alone does not make an outcome starting": {
				o:    launch.Outcome{SupervisorPID: 4242},
				want: "failed",
			},
			"error: a failed outcome is failed": {
				o:    launch.Outcome{Reason: "proxy binary missing (run setup)"},
				want: "failed",
			},
			"error: a failed outcome of an attempted recovery is failed": {
				o:    launch.Outcome{Reason: "could not stop the shim supervisor", RecoveryAttempted: true},
				want: "failed",
			},
			"error: a quiet wait cut short at its limit is failed when the supervisor was not alive": {
				o:    launch.Outcome{Reason: "not healthy after 12s (check logs in /tmp/logs)", Started: []string{"shim"}, WaitCutShort: true},
				want: "failed",
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				before := slices.Clone(tt.o.Started)
				got, ok := planTCall(t, tt.o.RecoveryOutcome)
				if !ok {
					return
				}
				if diff := gocmp.Diff(tt.want, got); diff != "" {
					t.Errorf("RecoveryOutcome of %+v mismatch (-want +got):\n%s", tt.o, diff)
				}
				if diff := gocmp.Diff(before, tt.o.Started); diff != "" {
					t.Errorf("RecoveryOutcome changed the outcome's launch list (-before +after):\n%s", diff)
				}
			})
		}
	})
}
