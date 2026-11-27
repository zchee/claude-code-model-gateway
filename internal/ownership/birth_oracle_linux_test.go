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

//go:build process && linux

package ownership_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// clockTicks reads the clock rate of /proc/<pid>/stat from this process's auxiliary vector (AT_CLKTCK), as the
// kernel hands it to every process, instead of assuming one.
func clockTicks(t *testing.T) int64 {
	t.Helper()
	const atClockTick = 17
	auxv, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		t.Fatal(err)
	}
	word := int(unsafe.Sizeof(uintptr(0)))
	for i := 0; i+2*word <= len(auxv); i += 2 * word {
		key, value := uint64(0), uint64(0)
		if word == 8 {
			key, value = binary.NativeEndian.Uint64(auxv[i:]), binary.NativeEndian.Uint64(auxv[i+word:])
		} else {
			key, value = uint64(binary.NativeEndian.Uint32(auxv[i:])), uint64(binary.NativeEndian.Uint32(auxv[i+word:]))
		}
		if key == atClockTick && value > 0 {
			return int64(value)
		}
	}
	t.Fatal("the auxiliary vector has no clock tick")
	return 0
}

// kernelBirth reads the start of pid from the kernel's records without the package under test: the boot time of
// /proc/stat plus the start ticks of /proc/<pid>/stat at the kernel's clock rate, truncated to the second as the
// legacy start text is.
func kernelBirth(t *testing.T, pid int) time.Time {
	t.Helper()
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		t.Fatal(err)
	}
	boot := int64(-1)
	for line := range strings.SplitSeq(string(stat), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if boot, err = strconv.ParseInt(strings.TrimSpace(v), 10, 64); err != nil {
				t.Fatalf("boot time %q: %v", v, err)
			}
		}
	}
	if boot < 0 {
		t.Fatal("/proc/stat has no boot time")
	}
	record, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(record, []byte(strconv.Itoa(pid)+" (")) {
		t.Fatalf("stat record of %d does not start with its number: %q", pid, record)
	}
	_, after, ok := bytes.CutLast(record, []byte(") "))
	if !ok {
		t.Fatalf("stat record of %d has no end of its command name: %q", pid, record)
	}
	// Field 22 of stat is the start time; the fields after the command name start at field 3.
	const startField = 22 - 3
	fields := strings.Fields(string(after))
	if len(fields) <= startField {
		t.Fatalf("stat record of %d has %d fields after its command name: %q", pid, len(fields), record)
	}
	ticks, err := strconv.ParseInt(fields[startField], 10, 64)
	if err != nil || ticks < 0 {
		t.Fatalf("start ticks %q of %d: %v", fields[startField], pid, err)
	}
	return time.Unix(boot+ticks/clockTicks(t), 0)
}
