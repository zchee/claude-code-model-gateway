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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

const (
	publicPort  uint16 = 18764
	proxyPort   uint16 = 18765
	holderPID          = 701
	nextPID            = 702
	recordName         = "guardian"
	foreignPath        = "/srv/elsewhere/bin/model-gateway"
	// thisInstall and otherInstall are Node gateway install roots: this plugin's, and one of another marketplace.
	thisInstall  = "/home/user/.claude/plugins/cache/eigenwise-toolshed/model-gateway/0.50.0"
	otherInstall = "/home/user/.claude/plugins/cache/other-marketplace/model-gateway/0.49.0"
	// budgetMS is the explicit probe budget of the timeout cases; the refusal names it.
	budgetMS = 20
)

func observed(pid int) string {
	if pid == 0 {
		return "unknown"
	}
	return strconv.Itoa(pid)
}

// unidentifiedReason is the source's generic refusal (lib/process-supervision.js:613).
func unidentifiedReason(port uint16, pid int) string {
	return fmt.Sprintf("could not confirm the owner of :%d (last observed PID %s); the owning process could not be identified as this model-gateway install; left the listener untouched", port, observed(pid))
}

// hiddenCommandReason is the source's refusal for a holder that exposes no command line (lib/process-supervision.js:611).
func hiddenCommandReason(port uint16, pid int) string {
	return fmt.Sprintf("could not confirm the owner of :%d (last observed PID %s); the owning process did not expose its command line. It may be elevated or otherwise protected, so run stop or setup from a session with the same privileges; left the listener untouched", port, observed(pid))
}

// timeoutReason is the source's refusal for an exhausted probe budget (lib/process-supervision.js:608).
func timeoutReason(port uint16, pid, ms int) string {
	return fmt.Sprintf("could not confirm the owner of :%d (last observed PID %s); the ownership probe exhausted its %dms budget (set CODEX_GATEWAY_PROBE_TIMEOUT_MS to override); left the listener untouched", port, observed(pid), ms)
}

// foreignReason is the source's refusal for a holder of another install root (lib/process-supervision.js:604).
func foreignReason(port uint16, pid int, root, current string) string {
	return fmt.Sprintf("refusing to stop PID %d on :%d; it belongs to a different install root (%s), not %s", pid, port, root, current)
}

// nodeGateway reads a command line as a Node gateway's, the way the source does (lib/process-supervision.js:510-542):
// the install root is two directories above bin/model-gateway.js, and it belongs to this plugin when it is current.
func nodeGateway(current string) func([]string) (string, string, bool) {
	return func(argv []string) (string, string, bool) {
		for _, arg := range argv {
			if strings.HasSuffix(arg, "/bin/model-gateway.js") {
				root := filepath.Dir(filepath.Dir(arg))
				return root, current, root == current
			}
		}
		return "", current, false
	}
}

// legacyReason is the refusal for this install's Node gateway, as the lead ruled it.
func legacyReason(port uint16, pid int) string {
	return fmt.Sprintf("refusing to stop PID %d on :%d; it is the Node model-gateway of this install, not this gateway; stop it with its own command; left the listener untouched", pid, port)
}

func identified(pid int) listenerAnswer {
	return listenerAnswer{listener: procinfo.Listener{State: procinfo.ListenerIdentified, PID: pid}}
}

func listenerState(state procinfo.ListenerState) listenerAnswer {
	return listenerAnswer{listener: procinfo.Listener{State: state}}
}

// owner is the part of a resolved owner a case decides.
type owner struct {
	Class ownership.Class
	PID   int
}

type resolveSetup struct {
	root string
	host *fakeHost
	auth ownership.Authority
	name string
	port uint16
}

func TestResolveClasses(t *testing.T) {
	base := func(t *testing.T) *resolveSetup {
		root := stateRoot(t)
		h := newHost(supervisorSample(holderPID))
		h.listener = []listenerAnswer{identified(holderPID)}
		return &resolveSetup{root: root, host: h, auth: authorityOn(root, h), name: recordName, port: publicPort}
	}
	recorded := func(mutate func(*procinfo.Record)) func(*testing.T, *resolveSetup) {
		return func(t *testing.T, s *resolveSetup) {
			r := recordFor(s.root, s.host.processes[holderPID].id, roleSupervisor, publicPort)
			if mutate != nil {
				mutate(&r)
			}
			seed(t, s.root, recordName, r)
		}
	}
	live := func(mutate func(*procinfo.Identity)) func(*testing.T, *resolveSetup) {
		return func(t *testing.T, s *resolveSetup) { mutate(&s.host.processes[holderPID].id) }
	}
	both := func(steps ...func(*testing.T, *resolveSetup)) func(*testing.T, *resolveSetup) {
		return func(t *testing.T, s *resolveSetup) {
			for _, step := range steps {
				step(t, s)
			}
		}
	}
	directImage := func(image procinfo.Image) func(*testing.T, *resolveSetup) {
		return live(func(id *procinfo.Identity) {
			id.Image = procinfo.ImageEvidence{Image: image, DirectFromExecutable: true}
		})
	}
	node := func(script, current string) func(*testing.T, *resolveSetup) {
		return func(t *testing.T, s *resolveSetup) {
			s.host.processes[holderPID].id.Argv = []string{"/usr/bin/node", script, "serve-shim"}
			s.auth.Legacy = nodeGateway(current)
		}
	}
	contradicted := unidentifiedReason(publicPort, holderPID)
	tests := map[string]struct {
		prepare func(*testing.T, *resolveSetup)
		want    owner
		// reason is the exact refusal text, when the case decides it.
		reason string
		// image is the observed executable path, when the case decides it.
		image string
	}{
		"success: a recorded supervisor whose fresh sample matches every field is ours": {
			prepare: recorded(nil),
			want:    owner{ownership.Ours, holderPID},
			image:   gatewayExe,
		},
		"success: correlated image evidence that differs neither contradicts nor proves": {
			prepare: both(recorded(nil), live(func(id *procinfo.Identity) {
				id.Image = procinfo.ImageEvidence{Path: foreignPath, Device: 1, Inode: 2}
			})),
			want: owner{ownership.Ours, holderPID},
		},
		"success: another spelling of the recorded executable with its device and inode is the same image": {
			prepare: both(recorded(nil), directImage(procinfo.Image{Path: "/opt/model-gateway/bin/../bin/model-gateway", Device: gatewayDevice, Inode: gatewayInode})),
			want:    owner{ownership.Ours, holderPID},
		},
		"success: the proxy started through another spelling of its binary path is ours": {
			prepare: func(t *testing.T, s *resolveSetup) {
				alternate := "/home/user/.local/share/model-gateway/bin/../bin/claude-code-proxy"
				id := sampleOf(holderPID, procinfo.Image{Path: alternate, Device: gatewayDevice, Inode: proxyInode}, alternate, "serve", "--no-monitor")
				s.host.processes[holderPID].id = id
				r := recordFor(s.root, id, roleProxy, proxyPort)
				r.Image = proxyImage
				seed(t, s.root, "proxy", r)
				s.name, s.port = "proxy", proxyPort
			},
			want: owner{ownership.Ours, holderPID},
		},
		"error: direct image evidence of another inode at the recorded path is a contradiction": {
			prepare: both(recorded(nil), directImage(procinfo.Image{Path: gatewayExe, Device: gatewayDevice, Inode: gatewayInode + 1})),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
		},
		"error: direct image evidence of another device with the recorded inode is a contradiction": {
			prepare: both(recorded(nil), directImage(procinfo.Image{Path: gatewayExe, Device: gatewayDevice + 1, Inode: gatewayInode})),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
		},
		"error: direct image evidence of another file elsewhere is a contradiction": {
			prepare: both(recorded(nil), directImage(procinfo.Image{Path: foreignPath, Device: gatewayDevice, Inode: 9999})),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
			image:   foreignPath,
		},
		"error: correlated image evidence that matches does not rescue a contradicted birth": {
			prepare: both(recorded(func(r *procinfo.Record) { r.StartedAt = procinfo.LegacyStart(testBirth.Add(-time.Hour)) }), live(func(id *procinfo.Identity) {
				id.Image.DirectFromExecutable = false
			})),
			want:   owner{ownership.Unknown, holderPID},
			reason: contradicted,
		},
		"error: a recycled number, whose start token differs from the record, is not another install": {
			prepare: recorded(func(r *procinfo.Record) { r.StartToken = "2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/1" }),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
			image:   gatewayExe,
		},
		"error: a record whose birth differs is contradicted": {
			prepare: recorded(func(r *procinfo.Record) { r.StartedAt = procinfo.LegacyStart(testBirth.Add(time.Second)) }),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
		},
		"error: a record whose command differs is contradicted": {
			prepare: recorded(func(r *procinfo.Record) { r.Command = gatewayExe + " serve-worker" }),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
		},
		"error: a record whose role the command word contradicts is refused although its command matches": {
			prepare: recorded(func(r *procinfo.Record) { r.Role = roleWorker }),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
		},
		"error: a role word that is an operand after the command is not the role": {
			prepare: func(t *testing.T, s *resolveSetup) {
				s.host.processes[holderPID].id.Argv = []string{gatewayExe, "serve-shim", "serve-worker"}
				seed(t, s.root, recordName, recordFor(s.root, s.host.processes[holderPID].id, roleWorker, publicPort))
			},
			want:   owner{ownership.Unknown, holderPID},
			reason: contradicted,
		},
		"error: a role word after a first token that looks like a flag is not the role": {
			prepare: func(t *testing.T, s *resolveSetup) {
				s.host.processes[holderPID].id.Argv = []string{gatewayExe, "--verbose", "serve-shim"}
				seed(t, s.root, recordName, recordFor(s.root, s.host.processes[holderPID].id, roleSupervisor, publicPort))
			},
			want:   owner{ownership.Unknown, holderPID},
			reason: contradicted,
		},
		"error: a proxy record for a process that does not run the proxy binary is refused": {
			prepare: func(t *testing.T, s *resolveSetup) {
				r := recordFor(s.root, s.host.processes[holderPID].id, roleProxy, proxyPort)
				seed(t, s.root, "proxy", r)
				s.name, s.port = "proxy", proxyPort
			},
			want:   owner{ownership.Unknown, holderPID},
			reason: unidentifiedReason(proxyPort, holderPID),
		},
		"error: a record for another port is contradicted by the listener": {
			prepare: recorded(func(r *procinfo.Record) { r.Port = publicPort + 1 }),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
		},
		"error: the same executable without a record is not ours": {
			want:   owner{ownership.Unknown, holderPID},
			reason: contradicted,
			image:  gatewayExe,
		},
		"error: a record naming another process number does not name the holder": {
			prepare: recorded(func(r *procinfo.Record) { r.PID = 900 }),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
		},
		"error: a record without a start token grants nothing": {
			prepare: recorded(func(r *procinfo.Record) { r.StartToken = "" }),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
		},
		"error: a live sample without a start token is unknown": {
			prepare: both(recorded(nil), live(func(id *procinfo.Identity) { id.StartToken = "" })),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
		},
		"error: a record copied from another state root is not this root's record": {
			prepare: func(t *testing.T, s *resolveSetup) {
				other := stateRoot(t)
				seed(t, other, recordName, recordFor(other, s.host.processes[holderPID].id, roleSupervisor, publicPort))
				for _, suffix := range []string{".pid.json", ".pid"} {
					data, err := os.ReadFile(filepath.Join(other, recordName+suffix))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(s.root, recordName+suffix), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			},
			want:   owner{ownership.Unknown, holderPID},
			reason: contradicted,
		},
		"success: the Node gateway of this install is legacy": {
			prepare: node(thisInstall+"/bin/model-gateway.js", thisInstall),
			want:    owner{ownership.LegacyNode, holderPID},
			reason:  legacyReason(publicPort, holderPID),
		},
		"error: the Node gateway of another install root is foreign": {
			prepare: node(otherInstall+"/bin/model-gateway.js", thisInstall),
			want:    owner{ownership.Foreign, holderPID},
			reason:  foreignReason(publicPort, holderPID, otherInstall, thisInstall),
		},
		"success: no listener on the port is unowned": {
			prepare: func(t *testing.T, s *resolveSetup) {
				s.host.listener = []listenerAnswer{listenerState(procinfo.ListenerAbsent)}
			},
			want: owner{ownership.Unowned, 0},
		},
		"success: a bound port whose holder cannot be resolved is port-owned with the generic refusal": {
			prepare: func(t *testing.T, s *resolveSetup) {
				s.host.listener = []listenerAnswer{listenerState(procinfo.ListenerUnresolved)}
			},
			want:   owner{ownership.PortOwned, 0},
			reason: unidentifiedReason(publicPort, 0),
		},
		"error: several holders are unknown": {
			prepare: func(t *testing.T, s *resolveSetup) {
				s.host.listener = []listenerAnswer{listenerState(procinfo.ListenerAmbiguous)}
			},
			want:   owner{ownership.Unknown, 0},
			reason: unidentifiedReason(publicPort, 0),
		},
		"error: denied listener evidence is unknown, never absence": {
			prepare: func(t *testing.T, s *resolveSetup) {
				s.host.listener = []listenerAnswer{{listener: procinfo.Listener{State: procinfo.ListenerDenied}, err: os.ErrPermission}}
			},
			want:   owner{ownership.Unknown, 0},
			reason: unidentifiedReason(publicPort, 0),
		},
		"error: an inconclusive listener query is unknown, never absence": {
			prepare: func(t *testing.T, s *resolveSetup) {
				s.host.listener = []listenerAnswer{{listener: procinfo.Listener{State: procinfo.ListenerUnknown}, err: procinfo.ErrUnknown}}
			},
			want:   owner{ownership.Unknown, 0},
			reason: unidentifiedReason(publicPort, 0),
		},
		"error: an inconclusive identity sample is unknown, never absence": {
			prepare: both(recorded(nil), func(t *testing.T, s *resolveSetup) { s.host.processes[holderPID].unreadable = errInconclusive }),
			want:    owner{ownership.Unknown, holderPID},
			reason:  contradicted,
		},
		"error: a holder that exposes no command line gets the protected-process refusal": {
			prepare: func(t *testing.T, s *resolveSetup) { s.host.processes[holderPID].hiddenArgv = true },
			want:    owner{ownership.Unknown, holderPID},
			reason:  hiddenCommandReason(publicPort, holderPID),
		},
		"success: a holder that vanished from a released port is unowned": {
			prepare: func(t *testing.T, s *resolveSetup) {
				s.host.processes[holderPID].exited = true
				s.host.listener = []listenerAnswer{identified(holderPID), listenerState(procinfo.ListenerAbsent)}
			},
			want: owner{ownership.Unowned, holderPID},
		},
		"success: a holder that vanished from a port still bound, then released, is unowned": {
			prepare: func(t *testing.T, s *resolveSetup) {
				s.host.processes[holderPID].exited = true
				s.host.listener = []listenerAnswer{identified(holderPID), listenerState(procinfo.ListenerUnresolved), listenerState(procinfo.ListenerAbsent)}
			},
			want: owner{ownership.Unowned, 0},
		},
		"error: a recorded holder that replaced a vanished one on the retry stays unknown": {
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
			want:   owner{ownership.Unknown, nextPID},
			reason: unidentifiedReason(publicPort, nextPID),
		},
		"success: an inconclusive first sample is retried once": {
			prepare: both(recorded(nil), func(t *testing.T, s *resolveSetup) {
				s.host.beforeIdentity = func(h *fakeHost, pid, n int) {
					if n == 1 {
						h.processes[pid].unreadable = errInconclusive
					} else {
						h.processes[pid].unreadable = nil
					}
				}
			}),
			want: owner{ownership.Ours, holderPID},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := base(t)
			if tt.prepare != nil {
				tt.prepare(t, s)
			}
			before := readRaw(t, s.root, s.name)
			got, err := resolve(t, s.auth, t.Context(), s.name, s.port)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if diff := gocmp.Diff(tt.want, owner{got.Class, got.PID}); diff != "" {
				t.Errorf("owner (-want +got):\n%s\nevents: %s", diff, describe(s.host.snapshot()))
			}
			switch {
			case tt.reason != "":
				if diff := gocmp.Diff(tt.reason, got.Reason); diff != "" {
					t.Errorf("refusal text (-want +got):\n%s", diff)
				}
			case got.Class == ownership.Foreign || got.Class == ownership.LegacyNode:
				t.Errorf("class %q came with no decided refusal", got.Class)
			}
			if tt.image != "" {
				if diff := gocmp.Diff(tt.image, got.Image); diff != "" {
					t.Errorf("observed image (-want +got):\n%s", diff)
				}
			}
			if n := s.host.count("signal"); n != 0 {
				t.Errorf("Resolve sent %d signals; it never signals", n)
			}
			if diff := gocmp.Diff(before, readRaw(t, s.root, s.name)); diff != "" {
				t.Errorf("Resolve changed the record files (-before +after):\n%s", diff)
			}
		})
	}
}

// TestResolveBudget decides the explicit probe budget: the refusal names it, evidence that arrives after it ended
// proves nothing, and no query follows its end.
func TestResolveBudget(t *testing.T) {
	const budget = budgetMS * time.Millisecond
	late := budget + time.Millisecond
	tests := map[string]struct {
		prepare func(*fakeHost)
		wantPID int
		// queries and identities are the listener queries and identity samples, when the case decides them.
		queries, identities int
	}{
		"error: a listener query that timed out": {
			prepare: func(h *fakeHost) {
				h.listener = []listenerAnswer{{listener: procinfo.Listener{State: procinfo.ListenerTimedOut}, err: context.DeadlineExceeded}}
			},
		},
		"error: an identity sample that ran out of budget": {
			prepare: func(h *fakeHost) {
				h.listener = []listenerAnswer{identified(holderPID)}
				h.processes[holderPID].unreadable = errProbeTimeout
			},
			wantPID: holderPID,
		},
		"error: an absent listener reported after the budget ended": {
			prepare: func(h *fakeHost) {
				h.listener = []listenerAnswer{listenerState(procinfo.ListenerAbsent)}
				h.listenerDelay = late
			},
			queries: 1, identities: 0,
		},
		"error: a vanished holder reported after the budget ended": {
			prepare: func(h *fakeHost) {
				h.listener = []listenerAnswer{identified(holderPID), listenerState(procinfo.ListenerAbsent)}
				h.processes[holderPID].exited = true
				h.identityDelay = late
			},
			wantPID: holderPID, queries: 1, identities: 1,
		},
		"error: the confirmation of a vanished holder's absence reported after the budget ended": {
			prepare: func(h *fakeHost) {
				h.listener = []listenerAnswer{identified(holderPID), listenerState(procinfo.ListenerAbsent)}
				h.processes[holderPID].exited = true
				h.confirmDelay = late
			},
			wantPID: holderPID, queries: 2, identities: 1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := stateRoot(t)
				h := newHost(supervisorSample(holderPID))
				tt.prepare(h)
				ctx, cancel := context.WithTimeout(t.Context(), budget)
				defer cancel()
				got, err := resolve(t, authorityOn(root, h), ctx, recordName, publicPort)
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if diff := gocmp.Diff(owner{ownership.Unknown, tt.wantPID}, owner{got.Class, got.PID}); diff != "" {
					t.Errorf("owner (-want +got):\n%s\nevents: %s", diff, describe(h.snapshot()))
				}
				if diff := gocmp.Diff(timeoutReason(publicPort, tt.wantPID, budgetMS), got.Reason); diff != "" {
					t.Errorf("refusal text (-want +got):\n%s", diff)
				}
				if tt.queries > 0 {
					if diff := gocmp.Diff([]int{tt.queries, tt.identities}, []int{h.count("listener"), h.count("identity")}); diff != "" {
						t.Errorf("listener queries and identity samples (-want +got):\n%s", diff)
					}
				}
			})
		})
	}
}

// TestResolveRetriesOnce decides that an inconclusive answer with budget left is retried exactly once, and that an
// inconclusive identity sample is never followed by an absence check.
func TestResolveRetriesOnce(t *testing.T) {
	tests := map[string]struct {
		unreadable error
		reason     string
	}{
		"error: an identity sample that keeps running out of budget": {unreadable: errProbeTimeout, reason: timeoutReason(publicPort, holderPID, budgetMS)},
		"error: an identity sample that stays inconclusive":          {unreadable: errInconclusive, reason: unidentifiedReason(publicPort, holderPID)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := stateRoot(t)
				seed(t, root, recordName, recordFor(root, supervisorSample(holderPID), roleSupervisor, publicPort))
				h := newHost(supervisorSample(holderPID))
				h.processes[holderPID].unreadable = tt.unreadable
				// After two queries the port looks free; an inconclusive identity must not be read as its absence.
				h.listener = []listenerAnswer{identified(holderPID), identified(holderPID), listenerState(procinfo.ListenerAbsent)}
				ctx, cancel := context.WithTimeout(t.Context(), budgetMS*time.Millisecond)
				defer cancel()
				got, err := resolve(t, authorityOn(root, h), ctx, recordName, publicPort)
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if diff := gocmp.Diff(owner{ownership.Unknown, holderPID}, owner{got.Class, got.PID}); diff != "" {
					t.Errorf("owner (-want +got):\n%s\nevents: %s", diff, describe(h.snapshot()))
				}
				if diff := gocmp.Diff(tt.reason, got.Reason); diff != "" {
					t.Errorf("refusal text (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]int{2, 2}, []int{h.count("listener"), h.count("identity")}); diff != "" {
					t.Errorf("listener queries and identity samples (-want +got): two attempts with budget left, no absence check:\n%s", diff)
				}
			})
		})
	}
}

// TestResolveRefusalTexts decides that a refusal for an exhausted budget names the budget and the override variable
// and differs from the refusal for an unrecognized holder, which names neither.
func TestResolveRefusalTexts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := stateRoot(t)
		exhausted := newHost(supervisorSample(holderPID))
		exhausted.listener = []listenerAnswer{identified(holderPID)}
		exhausted.processes[holderPID].unreadable = errProbeTimeout
		unrecognized := newHost(sampleOf(holderPID, procinfo.Image{Path: "/usr/bin/unrecognized", Device: 1, Inode: 3}, "unrecognized", "command"))
		unrecognized.listener = []listenerAnswer{identified(holderPID)}
		ctx, cancel := context.WithTimeout(t.Context(), budgetMS*time.Millisecond)
		defer cancel()
		timedOut, err := resolve(t, authorityOn(root, exhausted), ctx, recordName, publicPort)
		if err != nil {
			t.Fatalf("Resolve with an exhausted budget: %v", err)
		}
		unidentified, err := resolve(t, authorityOn(root, unrecognized), ctx, recordName, publicPort)
		if err != nil {
			t.Fatalf("Resolve of an unrecognized holder: %v", err)
		}
		if diff := gocmp.Diff([]owner{{ownership.Unknown, holderPID}, {ownership.Unknown, holderPID}}, []owner{{timedOut.Class, timedOut.PID}, {unidentified.Class, unidentified.PID}}); diff != "" {
			t.Errorf("owners of the exhausted and the unrecognized holder (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(timeoutReason(publicPort, holderPID, budgetMS), timedOut.Reason); diff != "" {
			t.Errorf("exhausted budget refusal (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(unidentifiedReason(publicPort, holderPID), unidentified.Reason); diff != "" {
			t.Errorf("unidentified refusal (-want +got):\n%s", diff)
		}
		if timedOut.Reason == unidentified.Reason {
			t.Errorf("both refusals are %q; an exhausted budget must be told apart from an unrecognized owner", timedOut.Reason)
		}
		if strings.Contains(unidentified.Reason, "CODEX_GATEWAY_PROBE_TIMEOUT_MS") || strings.Contains(unidentified.Reason, "budget") {
			t.Errorf("unidentified refusal %q names the probe budget", unidentified.Reason)
		}
	})
}

// TestResolveInvalidRequest decides that a request the authority cannot answer is an error and never an owner
// that permits acting.
func TestResolveInvalidRequest(t *testing.T) {
	tests := map[string]struct {
		root string
		name string
		port uint16
	}{
		"error: a relative state root":     {root: "relative/state", name: recordName, port: publicPort},
		"error: an unclean state root":     {root: "/tmp/../tmp/state", name: recordName, port: publicPort},
		"error: a record name with a path": {name: "../guardian", port: publicPort},
		"error: an empty record name":      {name: "", port: publicPort},
		"error: port zero":                 {name: recordName, port: 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := tt.root
			if root == "" {
				root = stateRoot(t)
			}
			h := newHost(supervisorSample(holderPID))
			h.listener = []listenerAnswer{identified(holderPID)}
			got, err := resolve(t, authorityOn(root, h), t.Context(), tt.name, tt.port)
			if err == nil {
				t.Errorf("Resolve(%q, %q, %d) = %+v, nil; want an error", root, tt.name, tt.port, got)
			}
			if got.Class == ownership.Ours {
				t.Errorf("Resolve(%q, %q, %d) returned class %q with its error", root, tt.name, tt.port, got.Class)
			}
			if n := h.count("signal"); n != 0 {
				t.Errorf("Resolve sent %d signals", n)
			}
		})
	}
}
