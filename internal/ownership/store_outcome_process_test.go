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

//go:build process && (linux || darwin)

package ownership_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

const (
	// storeOutTReconcileHelperName is the fixture helper that reconciles one record in a child process.
	storeOutTReconcileHelperName = "store-outcome-reconcile"
	// storeOutTOutcomePrefix starts the line on which that child reports its outcome.
	storeOutTOutcomePrefix = "store-outcome:"
	// storeOutTChildLimit bounds the child: the runner kills it at this limit and reaps it.
	storeOutTChildLimit = 10 * time.Second
)

// storeOutTChildRecord is the valid record of root that the child reconciles.
func storeOutTChildRecord(root string) procinfo.Record {
	return procinfo.Record{
		PID:        43101,
		Command:    "/opt/model-gateway/bin/model-gateway serve-worker",
		StartedAt:  procinfo.LegacyStart(time.Date(2026, time.November, 28, 9, 30, 0, 0, time.Local)),
		Role:       "worker",
		StateRoot:  root,
		Generation: "generation-7",
		Port:       43117,
		Version:    "1.2.3",
		Image:      procinfo.Image{Path: "/opt/model-gateway/bin/model-gateway", Device: 64769, Inode: 4242},
		StartToken: "6f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b/43101",
	}
}

// storeOutTChildOutcome is what the child reports of its call.
type storeOutTChildOutcome struct {
	Wrote       bool
	RecordEntry bool
	Repair      bool
}

// storeOutTReconcileHelper is the child of TestStoreReconcileNamedPipe: it reconciles the record named by its second
// argument in the state root of its first and reports the outcome on its standard output.
func storeOutTReconcileHelper(t testing.TB, args []string) {
	if len(args) != 2 {
		t.Fatalf("%s wants a state root and a record name, got %q", storeOutTReconcileHelperName, args)
	}
	root, name := args[0], args[1]
	wrote, err := ownership.Store{Root: root}.Reconcile(t.Context(), name, storeOutTChildRecord(root))
	if _, werr := fmt.Fprintf(os.Stdout, "%s wrote=%t recordEntry=%t repair=%t error=%q\n", storeOutTOutcomePrefix, wrote, errors.Is(err, procinfo.ErrRecordEntry), errors.Is(err, ownership.ErrRepair), fmt.Sprint(err)); werr != nil {
		t.Fatal(werr)
	}
}

// storeOutTParseOutcome finds the child's outcome line in its output.
func storeOutTParseOutcome(out string) (storeOutTChildOutcome, string, bool) {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), storeOutTOutcomePrefix+" ")
		if !ok {
			continue
		}
		var o storeOutTChildOutcome
		fields := strings.SplitN(rest, " ", 4)
		if len(fields) != 4 {
			return o, rest, false
		}
		o.Wrote = fields[0] == "wrote=true"
		o.RecordEntry = fields[1] == "recordEntry=true"
		o.Repair = fields[2] == "repair=true"
		return o, rest, true
	}
	return storeOutTChildOutcome{}, "", false
}

// TestStoreReconcileNamedPipe decides that Reconcile refuses a named pipe at the record's name without blocking and
// without changing anything, under the record lock. The call runs in a child process, so that a call that blocks is
// ended by the runner at its limit and no other test waits for it. Whether the store opens the pipe at all is not
// decided here: that is the regular-file check before the open in the directory's reader.
func TestStoreReconcileNamedPipe(t *testing.T) {
	tests := map[string]struct {
		want storeOutTChildOutcome
	}{
		"error: a named pipe at the record's name is refused without blocking and without changing anything, and is no required repair": {
			want: storeOutTChildOutcome{RecordEntry: true},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(parent, "state")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			pipe := filepath.Join(root, "worker.pid.json")
			if err := unix.Mkfifo(pipe, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(pipe)
			if err != nil {
				t.Fatal(err)
			}
			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			cmd, err := env.HelperCommand(t, storeOutTReconcileHelperName, root, "worker")
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			started := time.Now()
			runErr := env.RunFixtureWithin(cmd, storeOutTChildLimit)
			t.Logf("the child ran for %v and ended with %v", time.Since(started).Round(time.Millisecond), runErr)
			switch {
			case errors.Is(runErr, context.DeadlineExceeded):
				t.Errorf("harness timeout: the runner ended the child at its limit of %v (%v); this says only that the call did not finish in time", storeOutTChildLimit, runErr)
			case runErr != nil:
				t.Errorf("the child did not exit cleanly: %v", runErr)
			}
			got, line, ok := storeOutTParseOutcome(out.String())
			if !ok {
				t.Errorf("the child reported no outcome (%q; run: %v); its output:\n%s", line, runErr, out.String())
			} else if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("the child's outcome %q (-want +got):\n%s", line, diff)
			}
			after, err := os.Lstat(pipe)
			if err != nil {
				t.Fatalf("the named pipe is gone: %v", err)
			}
			if after.Mode().Type() != os.ModeNamedPipe || !os.SameFile(before, after) {
				t.Errorf("the named pipe at the record's name was replaced: mode %v", after.Mode())
			}
			// The refusal comes after the record lock was taken: the retained lock file exists and is a regular file.
			lock, err := os.Lstat(filepath.Join(root, "process-records.lock"))
			if err != nil || !lock.Mode().IsRegular() {
				t.Errorf("the retained record lock file after the call: %v (lstat: %v); want a regular file", lock, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != "worker.pid.json" && e.Name() != "process-records.lock" {
					t.Errorf("the state root holds %s after the call; want only the pipe and the record lock", e.Name())
				}
			}
		})
	}
}
