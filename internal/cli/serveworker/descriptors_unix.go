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

package serveworker

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

// controlDescriptors are the supervised worker's report and liveness pipes.
type controlDescriptors struct {
	generation string
	reportFile *os.File
	live       *os.File
	closeOnce  sync.Once
}

// supervisedDescriptors validates MODEL_GATEWAY_CONTROL_FDS. It returns nil when the worker is not supervised.
// A descriptor that is missing, a standard stream, not a pipe or socket, or open in the wrong direction is an error:
// the number may since have been reused by an unrelated descriptor, so it is never used on trust.
func supervisedDescriptors(lookup config.LookupEnv) (*controlDescriptors, error) {
	raw, ok := lookup(controlFDsKey)
	if !ok {
		return nil, nil
	}
	first, second, found := strings.Cut(raw, ",")
	if !found {
		return nil, fmt.Errorf("model-gateway: %s must name the report and liveness descriptors as two numbers, such as 3,4", controlFDsKey)
	}
	report, err := descriptor(first, unix.O_WRONLY, "report")
	if err != nil {
		return nil, err
	}
	live, err := descriptor(second, unix.O_RDONLY, "liveness")
	if err != nil {
		return nil, err
	}
	if report == live {
		return nil, fmt.Errorf("model-gateway: %s names descriptor %d for both the report and liveness pipes", controlFDsKey, report)
	}
	generation, _ := lookup(generationKey)
	if generation == "" {
		return nil, fmt.Errorf("model-gateway: a supervised worker needs %s; refusing to start without its generation", generationKey)
	}
	reportFile, err := openDescriptor(report, "port report")
	if err != nil {
		return nil, err
	}
	liveFile, err := openDescriptor(live, "parent liveness")
	if err != nil {
		_ = reportFile.Close()
		return nil, err
	}
	return &controlDescriptors{generation: generation, reportFile: reportFile, live: liveFile}, nil
}

// descriptor parses and checks one inherited descriptor. access is the direction the worker uses it in.
func descriptor(spelling string, access int, role string) (int, error) {
	fd, err := strconv.Atoi(spelling)
	if err != nil || fd < 0 || strconv.Itoa(fd) != spelling {
		return 0, fmt.Errorf("model-gateway: %s has a malformed %s descriptor %q", controlFDsKey, role, spelling)
	}
	if fd <= 2 {
		return 0, fmt.Errorf("model-gateway: %s names standard stream %d as the %s descriptor; the worker never uses stdin, stdout or stderr for it", controlFDsKey, fd, role)
	}
	if err := checkDescriptor(fd, access); err != nil {
		return 0, fmt.Errorf("model-gateway: %s %s descriptor %d is unusable: %w", controlFDsKey, role, fd, err)
	}
	return fd, nil
}

func checkDescriptor(fd, access int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if kind := st.Mode & unix.S_IFMT; kind != unix.S_IFIFO && kind != unix.S_IFSOCK {
		return errors.New("not a pipe or socket")
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if mode := flags & unix.O_ACCMODE; mode != access && mode != unix.O_RDWR {
		return errors.New("open in the wrong direction")
	}
	return nil
}

// openDescriptor takes ownership of a checked descriptor. It is made close-on-exec and non-blocking, so that closing
// the returned file ends a read that is waiting on it.
func openDescriptor(fd int, name string) (*os.File, error) {
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, fmt.Errorf("model-gateway: could not prepare the %s descriptor %d: %w", name, fd, err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// report writes the single listening line and closes the report pipe.
func (c *controlDescriptors) report(port int) error {
	line, err := reportLine(port, c.generation)
	if err == nil {
		_, err = c.reportFile.Write(line)
	}
	if closeErr := c.reportFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("model-gateway: could not report the worker port to the supervisor: %w", err)
	}
	return nil
}

func (c *controlDescriptors) close() {
	c.closeOnce.Do(func() {
		_ = c.reportFile.Close()
		_ = c.live.Close()
	})
}

// fixtureChannel returns the test harness's channel descriptor when this worker must stop at its end. Outside the
// test guard the descriptor is closed unread and nil is returned.
func fixtureChannel(lookup config.LookupEnv) (*os.File, error) {
	raw, ok := lookup(nodeChannelKey)
	if !ok {
		return nil, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd <= 2 || strconv.Itoa(fd) != raw {
		if guarded(lookup) {
			return nil, fmt.Errorf("model-gateway: %s names an unusable descriptor %q", nodeChannelKey, raw)
		}
		return nil, nil
	}
	if err := checkDescriptor(fd, unix.O_RDONLY); err != nil {
		if guarded(lookup) {
			return nil, fmt.Errorf("model-gateway: %s descriptor %d is unusable: %w", nodeChannelKey, fd, err)
		}
		return nil, nil
	}
	file, err := openDescriptor(fd, "fixture channel")
	if err != nil {
		return nil, err
	}
	if !guarded(lookup) {
		_ = file.Close()
		return nil, nil
	}
	return file, nil
}

// errorCode returns the system error name of a failure, as the source prints error.code.
func errorCode(err error) string {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		if name := unix.ErrnoName(errno); name != "" {
			return name
		}
	}
	return err.Error()
}
