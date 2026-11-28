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

package serveshim

import "time"

// drainTimer converts the drain timeout in milliseconds to the restart's deadline as the source's timer converts it.
func drainTimer(ms float64) time.Duration {
	panic("not implemented")
}

// drainPostBudget converts the drain timeout to the bound of the drain request, and reports false when no request is sent.
func drainPostBudget(ms float64) (time.Duration, bool) {
	panic("not implemented")
}

// drainSeconds is the drain timeout in whole seconds as the source prints it in the timeout diagnostic.
func drainSeconds(ms float64) string {
	panic("not implemented")
}

// recoveryInterval converts the proxy recovery interval in milliseconds, and reports whether a too-large value was replaced by the default.
func recoveryInterval(ms float64) (d time.Duration, replaced bool) {
	panic("not implemented")
}
