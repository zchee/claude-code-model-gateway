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

package workerlink_test

import (
	"errors"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// The registry is the lease source the relay consumes.
var _ workerlink.Source = (*workerlink.Registry)(nil)

// TestDeclaredNames fixes the descriptor numbers, the environment variable names, the drain wire constants and
// the sentinel texts that the supervisor, the worker command and the fixtures share.
func TestDeclaredNames(t *testing.T) {
	tests := map[string]struct {
		got, want any
	}{
		"success: report descriptor":           {workerlink.ReportFD, 3},
		"success: liveness descriptor":         {workerlink.LivenessFD, 4},
		"success: listener descriptor":         {workerlink.ListenerFD, 5},
		"success: drain descriptor":            {workerlink.DrainFD, 6},
		"success: listener variable":           {workerlink.ListenerFDEnv, "MODEL_GATEWAY_WORKER_LISTENER_FD"},
		"success: drain variable":              {workerlink.DrainFDEnv, "MODEL_GATEWAY_WORKER_DRAIN_FD"},
		"success: drain line limit":            {workerlink.DrainLineLimit, 256},
		"success: draining type":               {workerlink.Draining, "draining"},
		"success: retired type":                {workerlink.Retired, "retired"},
		"success: unavailable text":            {workerlink.ErrUnavailable.Error(), "worker unavailable"},
		"success: retired text":                {workerlink.ErrRetired.Error(), "worker generation retired"},
		"success: drain message text":          {workerlink.ErrDrainMessage.Error(), "invalid worker drain message"},
		"success: drain line too long text":    {workerlink.ErrDrainLineTooLong.Error(), "worker drain line too long"},
		"success: generation is a string kind": {string(workerlink.Generation("g")), "g"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

// TestSentinelsAreDistinct keeps every declared failure distinguishable from the others.
func TestSentinelsAreDistinct(t *testing.T) {
	sentinels := map[string]error{
		"ErrUnavailable":      workerlink.ErrUnavailable,
		"ErrRetired":          workerlink.ErrRetired,
		"ErrDrainMessage":     workerlink.ErrDrainMessage,
		"ErrDrainLineTooLong": workerlink.ErrDrainLineTooLong,
	}
	for name, err := range sentinels {
		for other, target := range sentinels {
			if name != other && errors.Is(err, target) {
				t.Errorf("%s matches %s", name, other)
			}
		}
	}
}
