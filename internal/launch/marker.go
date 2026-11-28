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

package launch

import "context"

// publishUnderFence runs publish while the fence that acquire takes is held. An acquisition error is returned with
// nothing published. After a successful acquisition the release is called exactly once, also when publish fails;
// when both fail, publish's error is returned first and the release error is joined after it.
func publishUnderFence(ctx context.Context, acquire func(context.Context) (release func() error, err error), publish func() error) error {
	panic("not implemented")
}
