// Copyright 2022 The CubeFS Authors.
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

package fs

import (
	"testing"
)

// TestFileDoECTruncateFlow 校验 doECTruncateV2 存在；完整流程需 mock mw/ec/ebsc，见集成测试。
func TestFileDoECTruncateFlow(t *testing.T) {
	// 确保 *File 有 doECTruncateV2 方法（编译期检查）
	var _ = (*File).doECTruncateV2
	t.Log("doECTruncateV2 present; full flow requires MetaWrapper/ExtentClient/BlobStoreClient")
}

// TestFileSetattrECTruncateFromEntry 从 Setattr 入口跑 EC truncate。BlobStore 分支现走 doECTruncateV2，
// 依赖真实 mw/ec/ebsc，此处仅做占位；完整验证需集成环境。
func TestFileSetattrECTruncateFromEntry(t *testing.T) {
	t.Skip("Setattr BlobStore truncate uses doECTruncateV2; full test requires MetaWrapper/ExtentClient/BlobStoreClient")
}
