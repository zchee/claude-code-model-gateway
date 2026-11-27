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

//go:build unix && !linux

package ownership

// openProcess returns the numeric handle: macOS offers no process handle that an unrelated process can open and
// signal through. A process that exits after the second sample of a decision can have its number reused before the
// signal arrives, and the signal then reaches the new process; the two samples taken immediately before the signal
// only narrow that window.
func openProcess(pid int) (processHandle, error) { return numericProcess(pid), nil }
