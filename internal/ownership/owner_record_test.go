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

package ownership_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// handleTHolderToken is the start token sampleOf gives the holder.
const handleTHolderToken procinfo.StartToken = "2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/880701"

// handleTSeed writes the supervisor record of the holder with the given generation and version, so that the record a
// classification compared can be told from any other one the state root held. mutate, when set, changes it further.
func handleTSeed(t testing.TB, root, generation, version string, mutate func(*procinfo.Record)) {
	t.Helper()
	r := recordFor(root, supervisorSample(holderPID), roleSupervisor, publicPort)
	r.Generation, r.Version = generation, version
	if mutate != nil {
		mutate(&r)
	}
	seed(t, root, recordName, r)
}

// handleTOurs is the supervisor record of the holder as handleTSeed writes it unchanged, without its state root,
// which is the row's own temporary directory.
func handleTOurs(generation, version string) procinfo.Record {
	return procinfo.Record{
		PID:        holderPID,
		Command:    gatewayExe + " serve-shim",
		StartedAt:  procinfo.LegacyStart(testBirth),
		Role:       roleSupervisor,
		Generation: generation,
		Port:       publicPort,
		Version:    version,
		Image:      procinfo.Image{Path: gatewayExe, Device: gatewayDevice, Inode: gatewayInode},
		StartToken: handleTHolderToken,
	}
}

// TestOwnerRecordSnapshot decides that Resolve carries the stored record that made the holder Ours, as it was read for
// that classification, and the zero record for every other class, including a retry that was Ours for another holder
// and a classification whose budget ended.
func TestOwnerRecordSnapshot(t *testing.T) {
	// elsewhere is a record of this root that names another process number: a classification reads it, and it must
	// never be carried for a holder it does not name.
	elsewhere := func(t *testing.T, s *resolveSetup) {
		seed(t, s.root, recordName, recordFor(s.root, supervisorSample(nextPID), roleSupervisor, publicPort))
	}
	tests := map[string]struct {
		prepare func(*testing.T, *resolveSetup)
		// budget, when positive, is the caller's deadline from the start of Resolve.
		budget time.Duration
		want   owner
		// reason, when set, is the exact refusal the owner must carry.
		reason string
		// record is the carried record without its state root; the zero record means none is carried.
		record procinfo.Record
	}{
		"success: ours carries exactly the stored record it compared": {
			prepare: func(t *testing.T, s *resolveSetup) { handleTSeed(t, s.root, "generation-7", "9.8.7", nil) },
			want:    owner{ownership.Ours, holderPID},
			record:  handleTOurs("generation-7", "9.8.7"),
		},
		"success: ours on the retry of the same holder carries the record the retry compared": {
			prepare: func(t *testing.T, s *resolveSetup) {
				// The first inspection reads a record that contradicts the holder; the record is replaced before the
				// retry samples the holder again, and only the retry's record made it Ours.
				handleTSeed(t, s.root, "generation-1", "1.0.0", func(r *procinfo.Record) { r.StartToken = "contradicting-token" })
				s.host.beforeIdentity = func(h *fakeHost, pid, n int) {
					if pid == holderPID && n == 2 {
						handleTSeed(t, s.root, "generation-2", "2.0.0", nil)
					}
				}
			},
			want:   owner{ownership.Ours, holderPID},
			record: handleTOurs("generation-2", "2.0.0"),
		},
		"error: a retry that is ours for another holder is discarded with its record": {
			prepare: func(t *testing.T, s *resolveSetup) {
				s.host.add(supervisorSample(nextPID))
				seed(t, s.root, recordName, recordFor(s.root, supervisorSample(nextPID), roleSupervisor, publicPort))
				// The first holder vanishes when it is sampled and the next one holds the port from then on.
				s.host.beforeIdentity = func(h *fakeHost, pid, n int) {
					if pid == holderPID {
						h.processes[holderPID].exited = true
						h.ports = map[uint16]int{publicPort: nextPID}
					}
				}
			},
			want: owner{ownership.Unknown, nextPID},
		},
		"error: a record that contradicts the holder is unknown and carries nothing": {
			prepare: func(t *testing.T, s *resolveSetup) {
				handleTSeed(t, s.root, "generation-7", "9.8.7", func(r *procinfo.Record) { r.StartToken = "contradicting-token" })
			},
			want: owner{ownership.Unknown, holderPID},
		},
		"error: a record of the holder for another port is unknown and carries nothing": {
			prepare: func(t *testing.T, s *resolveSetup) {
				handleTSeed(t, s.root, "generation-7", "9.8.7", func(r *procinfo.Record) { r.Port = proxyPort })
			},
			want: owner{ownership.Unknown, holderPID},
		},
		"error: a foreign holder carries no record": {
			prepare: func(t *testing.T, s *resolveSetup) {
				elsewhere(t, s)
				s.host.processes[holderPID].id.Argv = []string{"/usr/bin/node", otherInstall + "/bin/model-gateway.js", "serve-shim"}
				s.auth.Legacy = nodeGateway(thisInstall)
			},
			want: owner{ownership.Foreign, holderPID},
		},
		"error: the legacy gateway of this install carries no record": {
			prepare: func(t *testing.T, s *resolveSetup) {
				elsewhere(t, s)
				s.host.processes[holderPID].id.Argv = []string{"/usr/bin/node", thisInstall + "/bin/model-gateway.js", "serve-shim"}
				s.auth.Legacy = nodeGateway(thisInstall)
			},
			want: owner{ownership.LegacyNode, holderPID},
		},
		"error: an unowned port carries no record": {
			prepare: func(t *testing.T, s *resolveSetup) {
				handleTSeed(t, s.root, "generation-7", "9.8.7", nil)
				s.host.listener = []listenerAnswer{listenerState(procinfo.ListenerAbsent)}
			},
			want: owner{ownership.Unowned, 0},
		},
		"error: a port-owned holder carries no record": {
			prepare: func(t *testing.T, s *resolveSetup) {
				handleTSeed(t, s.root, "generation-7", "9.8.7", nil)
				s.host.listener = []listenerAnswer{listenerState(procinfo.ListenerUnresolved)}
			},
			want: owner{ownership.PortOwned, 0},
		},
		"error: a holder that hides its command line carries no record": {
			prepare: func(t *testing.T, s *resolveSetup) {
				handleTSeed(t, s.root, "generation-7", "9.8.7", nil)
				s.host.processes[holderPID].hiddenArgv = true
			},
			want: owner{ownership.Unknown, holderPID},
		},
		"error: a holder whose samples ran out of budget carries no record": {
			prepare: func(t *testing.T, s *resolveSetup) {
				handleTSeed(t, s.root, "generation-7", "9.8.7", nil)
				s.host.processes[holderPID].unreadable = errProbeTimeout
			},
			budget: budgetMS * time.Millisecond,
			want:   owner{ownership.Unknown, holderPID},
		},
		"error: a classification that ended after the budget is unknown for the observed holder and carries no record": {
			prepare: func(t *testing.T, s *resolveSetup) {
				elsewhere(t, s)
				// The Legacy reading is the classification's last step: it lets the budget run out and then reads the
				// holder as another install's gateway, which must be discarded for the late answer it is.
				s.auth.Legacy = func([]string) (string, string, bool) {
					time.Sleep(budgetMS*time.Millisecond + time.Millisecond)
					return otherInstall, thisInstall, false
				}
			},
			budget: budgetMS * time.Millisecond,
			want:   owner{ownership.Unknown, holderPID},
			reason: "could not confirm the owner of :18764 (last observed PID 701); the ownership probe exhausted its 20ms budget (set CODEX_GATEWAY_PROBE_TIMEOUT_MS to override); left the listener untouched",
		},
		"error: a recorded holder whose sample arrived after the budget ended carries no record": {
			prepare: func(t *testing.T, s *resolveSetup) {
				handleTSeed(t, s.root, "generation-7", "9.8.7", nil)
				s.host.identityDelay = budgetMS*time.Millisecond + time.Millisecond
			},
			budget: budgetMS * time.Millisecond,
			want:   owner{ownership.Unknown, holderPID},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := stateRoot(t)
				h := newHost(supervisorSample(holderPID))
				h.listener = []listenerAnswer{identified(holderPID)}
				s := &resolveSetup{root: root, host: h, auth: authorityOn(root, h), name: recordName, port: publicPort}
				tt.prepare(t, s)
				ctx := t.Context()
				if tt.budget > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tt.budget)
					defer cancel()
				}
				got, err := resolve(t, s.auth, ctx, s.name, s.port)
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if diff := gocmp.Diff(tt.want, owner{got.Class, got.PID}); diff != "" {
					t.Errorf("owner (-want +got):\n%s\nevents: %s", diff, describe(h.snapshot()))
				}
				if tt.reason != "" {
					if diff := gocmp.Diff(tt.reason, got.Reason); diff != "" {
						t.Errorf("refusal (-want +got):\n%s", diff)
					}
				}
				want := tt.record
				if want != (procinfo.Record{}) {
					want.StateRoot = root
				}
				if diff := gocmp.Diff(want, got.Record); diff != "" {
					t.Errorf("carried record (-want +got):\n%s", diff)
				}
			})
		})
	}
}
