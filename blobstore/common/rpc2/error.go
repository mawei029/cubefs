// Copyright 2024 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package rpc2

import "github.com/cubefs/cubefs/blobstore/common/rpc"

var _ rpc.HTTPError = (*Error)(nil)

func (m *Error) StatusCode() int   { return int(m.GetStatus()) }
func (m *Error) ErrorCode() string { return m.GetReason() }
func (m *Error) Error() string     { return m.GetError_() }