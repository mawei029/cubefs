// Copyright 2022 The CubeFS Authors.
//
// 覆盖 io_ec.go 中 ExtentClientAPI / ECStreamerAPI 与实现方的编译期契约（接口文件无可执行语句时仍便于回归与重构校验）。

package stream_test

import (
	"testing"

	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
)

func TestIOECInterfacesImplemented(t *testing.T) {
	var _ stream.ExtentClientAPI = (*stream.ExtentClient)(nil)
	var _ stream.ExtentClientAPI = (*blobstore.ECExtentClient)(nil)
	var _ stream.ECStreamerAPI = (*blobstore.ECStreamer)(nil)
}
