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

package venue

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// query runs a system database reader without inheriting HOME, loader options,
// or name-resolution settings. A warning can indicate an incomplete result.
func query(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	if stderr.Len() != 0 {
		return "", errors.New("system query reported a warning")
	}
	return string(out), nil
}

// homeDir queries the password database by effective UID. os/user may fall
// back to environment variables when built without cgo and a record is absent.
func homeDir() (string, error) {
	uid := strconv.Itoa(os.Geteuid())
	switch runtime.GOOS {
	case "linux":
		out, err := query("/usr/bin/getent", "passwd", uid)
		if err != nil {
			return "", err
		}
		fields := strings.Split(strings.TrimSuffix(out, "\n"), ":")
		if len(fields) == 7 && fields[2] == uid && !strings.Contains(fields[5], "\n") {
			return fields[5], nil
		}
	case "darwin":
		out, err := query("/usr/bin/dscacheutil", "-q", "user", "-a", "uid", uid)
		if err != nil {
			return "", err
		}
		var home, id string
		for line := range strings.SplitSeq(out, "\n") {
			if value, ok := strings.CutPrefix(line, "dir: "); ok && home == "" {
				home = value
			} else if value, ok := strings.CutPrefix(line, "uid: "); ok && id == "" {
				id = value
			} else if strings.HasPrefix(line, "dir:") || strings.HasPrefix(line, "uid:") {
				return "", errors.New("ambiguous password database record")
			}
		}
		if id == uid {
			return home, nil
		}
	}
	return "", errors.New("no unique password database record")
}

// listening reads the kernel TCP listener table for both address families:
// ss filters by local port on Linux; netstat lists numeric TCP endpoints on
// Darwin. Neither command binds, dials, accepts, or sends gateway traffic, so
// concurrently starting test binaries cannot interfere with each other's probe.
func listening(port string) (bool, error) {
	switch runtime.GOOS {
	case "linux":
		out, err := query("/usr/bin/ss", "-ltn", "sport = :"+port)
		if err != nil {
			return false, err
		}
		header, rows, _ := strings.Cut(out, "\n")
		if !strings.HasPrefix(header, "State ") || !strings.Contains(header, "Local Address:Port") {
			return false, errors.New("missing TCP table header")
		}
		return strings.TrimSpace(rows) != "", nil
	case "darwin":
		out, err := query("/usr/sbin/netstat", "-an", "-p", "tcp")
		if err != nil {
			return false, err
		}
		return darwinListener(out, port)
	default:
		return false, errors.New("no listener probe on this platform")
	}
}

func darwinListener(out, port string) (bool, error) {
	var header bool
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0, strings.HasPrefix(line, "Active Internet connections"):
			continue
		case strings.Join(fields, " ") == "Proto Recv-Q Send-Q Local Address Foreign Address (state)":
			header = true
		case header && len(fields) == 6 && strings.HasPrefix(fields[0], "tcp"):
			_, localPort, ok := strings.CutLast(fields[3], ".")
			if !ok {
				return false, errors.New("invalid local TCP endpoint")
			}
			if fields[5] == "LISTEN" {
				if _, err := strconv.ParseUint(localPort, 10, 16); err != nil {
					return false, errors.New("invalid local TCP port")
				}
				if localPort == port {
					return true, nil
				}
			}
		default:
			return false, errors.New("unrecognized TCP table")
		}
	}
	if !header {
		return false, errors.New("missing TCP table header")
	}
	return false, nil
}
