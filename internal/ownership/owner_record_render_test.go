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

package ownership

import (
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

const (
	// handleTRenderPID is the holder the rendering rows classify.
	handleTRenderPID = 701
	// handleTRenderPort is the port its record names.
	handleTRenderPort uint16 = 18764
	// handleTRenderName is the record the rendering rows read.
	handleTRenderName = "guardian"
	// handleTRenderToken is the holder's start token.
	handleTRenderToken procinfo.StartToken = "2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/880701"
)

// handleTRenderBirth is a whole-second time, so that its legacy start text names it exactly.
var handleTRenderBirth = time.Date(2026, time.September, 9, 23, 24, 12, 0, time.Local)

// handleTRenderSample is the supervisor the rendering rows classify.
func handleTRenderSample() procinfo.Identity {
	return procinfo.Identity{
		PID:        handleTRenderPID,
		ParentPID:  1,
		Argv:       []string{"/opt/model-gateway/bin/model-gateway", "serve-shim"},
		Birth:      handleTRenderBirth,
		Image:      procinfo.ImageEvidence{Image: procinfo.Image{Path: "/opt/model-gateway/bin/model-gateway", Device: 64769, Inode: 4242}, DirectFromExecutable: true},
		StartToken: handleTRenderToken,
	}
}

// handleTRenderRecord is the supervisor record of the sample in root with the given generation and start token.
func handleTRenderRecord(root, generation string, token procinfo.StartToken) procinfo.Record {
	return procinfo.Record{
		PID:        handleTRenderPID,
		Command:    "/opt/model-gateway/bin/model-gateway serve-shim",
		StartedAt:  procinfo.LegacyStart(handleTRenderBirth),
		Role:       "supervisor",
		StateRoot:  root,
		Generation: generation,
		Port:       handleTRenderPort,
		Version:    "1.2.3",
		Image:      procinfo.Image{Path: "/opt/model-gateway/bin/model-gateway", Device: 64769, Inode: 4242},
		StartToken: token,
	}
}

// TestOwnerRecordRendered decides which record a rendered classification carries, one classification at a time and
// without a retry: the record the classification read and compared, even when the store holds another one by the
// time the owner is rendered; and for a classification that a contradicting record made Unknown, the zero record.
func TestOwnerRecordRendered(t *testing.T) {
	tests := map[string]struct {
		// classifiedGeneration and classifiedToken make the record the classification reads.
		classifiedGeneration string
		classifiedToken      procinfo.StartToken
		// replaced, when set, is the generation of the record published after the classification and before the
		// owner is rendered.
		replaced string
		class    Class
		// recordGeneration and recordToken make the carried record; an empty generation means the zero record.
		recordGeneration string
		recordToken      procinfo.StartToken
	}{
		"success: ours carries the record it compared, not the one stored when it is rendered": {
			classifiedGeneration: "generation-A",
			classifiedToken:      handleTRenderToken,
			replaced:             "generation-B",
			class:                Ours,
			recordGeneration:     "generation-A",
			recordToken:          handleTRenderToken,
		},
		"error: a contradicting record of the same holder renders unknown with the zero record": {
			classifiedGeneration: "generation-A",
			classifiedToken:      "contradicting-token",
			class:                Unknown,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			store := Store{Root: root}
			if err := store.Publish(handleTRenderName, handleTRenderRecord(root, tt.classifiedGeneration, tt.classifiedToken)); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Hour)
			r := &resolver{a: Authority{Records: store}, ctx: t.Context(), name: handleTRenderName, port: handleTRenderPort, deadline: deadline, budget: time.Until(deadline)}
			f := r.classify(handleTRenderSample())
			if tt.replaced != "" {
				if err := store.Publish(handleTRenderName, handleTRenderRecord(root, tt.replaced, handleTRenderToken)); err != nil {
					t.Fatal(err)
				}
			}
			o, err := r.owner(f)
			if err != nil {
				t.Fatalf("owner: %v", err)
			}
			if diff := gocmp.Diff(tt.class, o.Class); diff != "" {
				t.Errorf("class (-want +got):\n%s", diff)
			}
			var want procinfo.Record
			if tt.recordGeneration != "" {
				want = handleTRenderRecord(root, tt.recordGeneration, tt.recordToken)
			}
			if diff := gocmp.Diff(want, o.Record); diff != "" {
				t.Errorf("carried record (-want +got):\n%s", diff)
			}
		})
	}
}
