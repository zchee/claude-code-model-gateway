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

package lifecycle_test

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const stopLimitWitness = "stop write reached the file size limit\n"

func stopWithFileSizeLimit(t testing.TB, args []string) {
	t.Helper()
	if len(args) != 0 {
		t.Fatalf("unexpected helper arguments %q", args)
	}
	state, err := os.Getwd()
	noError(t, err)
	f := filesFor(state)
	f.Now = func() time.Time { return time.Unix(1, 0) }
	// Confine the file size limit to this process and verify the write was interrupted.
	var limit unix.Rlimit
	noError(t, unix.Getrlimit(unix.RLIMIT_FSIZE, &limit))
	limit.Cur = 8
	signal.Ignore(unix.SIGXFSZ)
	noError(t, unix.Setrlimit(unix.RLIMIT_FSIZE, &limit))
	if err := f.Stop(); !errors.Is(err, unix.EFBIG) && !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Stop() under an 8-byte file size limit: %v, want file size limit error", err)
	}
	_, err = io.WriteString(os.Stdout, stopLimitWitness)
	noError(t, err)
}

func TestAtomicStopReplacement(t *testing.T) {
	const oldMarker = `{"stoppedAt":"1970-01-01T00:00:00.123Z"}`
	const newMarker = `{"stoppedAt":"1970-01-01T00:00:01.000Z"}`
	tests := map[string]struct {
		interrupt bool
		want      string
	}{
		"success: publishes the complete replacement":   {want: newMarker},
		"error: interrupted write keeps the old marker": {interrupt: true, want: oldMarker},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			f := filesFor(filepath.Join(home, "state"))
			noError(t, f.Stop())
			path := filepath.Join(f.StateDir, "intentionally-stopped.json")
			prior, err := os.Open(path)
			noError(t, err)
			t.Cleanup(func() { noError(t, prior.Close()) })

			if tt.interrupt {
				env := gatewaytest.NewEnv(home, nil, nil, nil)
				cmd, err := env.HelperCommand(t, "stop-with-file-size-limit")
				noError(t, err)
				cmd.Dir = f.StateDir
				var output strings.Builder
				cmd.Stdout, cmd.Stderr = &output, &output
				if err := env.RunFixtureWithin(cmd, 25*time.Second); err != nil {
					t.Fatalf("stop helper: %v: %s", err, output.String())
				}
				if !strings.Contains(output.String(), stopLimitWitness) {
					t.Fatalf("stop helper did not confirm the interruption: %s", output.String())
				}
			} else {
				f.Now = func() time.Time { return time.Unix(1, 0) }
				noError(t, f.Stop())
			}

			equal(t, tt.want, bytesAt(t, path))
			data, err := io.ReadAll(prior)
			noError(t, err)
			equal(t, oldMarker, string(data))
		})
	}
}
