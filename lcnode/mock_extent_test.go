// Copyright 2026 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the License);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package lcnode

import (
	"bytes"
	"context"
	"crypto/md5"
	"io"
	"io/ioutil"

	"github.com/cubefs/cubefs/proto"
)

type MockExtentClient struct {
	data       []byte
	readBytes  int
	writeBytes int
}

func NewMockExtentClient() *MockExtentClient {
	return &MockExtentClient{}
}

func (*MockExtentClient) OpenStream(uint64, bool, bool, string) error { return nil }
func (*MockExtentClient) CloseStream(uint64) error                    { return nil }

// Read returns requested bytes so migrate/readFromExtentClient loops make progress.
func (m *MockExtentClient) Read(_ uint64, data []byte, _ int, size int, _ uint8, isMigration bool) (int, error) {
	if size <= 0 {
		return 0, nil
	}
	m.readBytes += size
	n := size
	if n > len(data) {
		n = len(data)
	}
	if isMigration {
		copy(data[:n], m.data)
		return n, io.EOF
	}
	for i := 0; i < n; i++ {
		data[i] = 'a'
	}
	return n, io.EOF
}

func (m *MockExtentClient) Write(_ uint64, _ int, data []byte, _ int, _ func() error, _ uint8, _ uint32, _ bool, _ bool) (int, error) {
	m.writeBytes += len(data)
	m.data = append(m.data[:0], data...)
	return len(data), nil
}

func (*MockExtentClient) Flush(uint64) error { return nil }
func (*MockExtentClient) Close() error       { return nil }

type MockEbsClient struct {
	data []byte
}

func NewMockEbsClient() *MockEbsClient {
	return &MockEbsClient{}
}

func (m *MockEbsClient) Put(_ context.Context, _ string, r io.Reader, _ uint64) ([]proto.ObjExtentKey, [][]byte, error) {
	h := md5.New()
	var err error
	m.data, err = ioutil.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}
	h.Write(m.data)
	sum := h.Sum(nil)
	return []proto.ObjExtentKey{{}}, [][]byte{sum}, nil
}

func (*MockEbsClient) Get(context.Context, string, uint64, uint64, proto.ObjExtentKey) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}
