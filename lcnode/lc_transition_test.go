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
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util"
	"github.com/stretchr/testify/require"
)

type md5ClassifyMW struct {
	*MockMetaWrapper
	inodeGet func(uint64, bool) (*proto.InodeInfo, error)
}

func (m *md5ClassifyMW) InodeGet_ll(inode uint64, isAsync bool) (*proto.InodeInfo, error) {
	if m.inodeGet != nil {
		return m.inodeGet(inode, isAsync)
	}
	return m.MockMetaWrapper.InodeGet_ll(inode, isAsync)
}

func TestClassifyMd5Mismatch(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	e := &proto.ScanDentry{
		Inode: 9001,
		InodeInfo: &proto.InodeInfo{
			Inode:      9001,
			ModifyTime: baseTime,
		},
	}
	mgr := &TransitionMgr{meta: &md5ClassifyMW{MockMetaWrapper: NewMockMetaWrapper()}}

	t.Run("inode get error", func(t *testing.T) {
		t.Parallel()
		m := mgr
		m.meta = &md5ClassifyMW{
			MockMetaWrapper: NewMockMetaWrapper(),
			inodeGet: func(uint64, bool) (*proto.InodeInfo, error) {
				return nil, errors.New("inode gone")
			},
		}
		err := m.classifyMd5Mismatch(e, "dst", "aaa", "bbb")
		require.Error(t, err)
		require.Contains(t, err.Error(), "get inode failed after check md5")
	})

	t.Run("modify time advanced", func(t *testing.T) {
		t.Parallel()
		m := &TransitionMgr{meta: &md5ClassifyMW{
			MockMetaWrapper: NewMockMetaWrapper(),
			inodeGet: func(uint64, bool) (*proto.InodeInfo, error) {
				return &proto.InodeInfo{
					Inode:      e.Inode,
					ModifyTime: baseTime.Add(time.Second),
				}, nil
			},
		}}
		err := m.classifyMd5Mismatch(e, "src", "deadbeef", "expected")
		require.Error(t, err)
		require.Contains(t, err.Error(), "file modified when migrating")
	})

	t.Run("pure md5 mismatch", func(t *testing.T) {
		t.Parallel()
		stale := baseTime.Add(-time.Second)
		m := &TransitionMgr{meta: &md5ClassifyMW{
			MockMetaWrapper: NewMockMetaWrapper(),
			inodeGet: func(uint64, bool) (*proto.InodeInfo, error) {
				return &proto.InodeInfo{Inode: e.Inode, ModifyTime: stale}, nil
			},
		}}
		err := m.classifyMd5Mismatch(e, "dst", "got", "want")
		require.Error(t, err)
		require.Contains(t, err.Error(), "check dst md5 inconsistent")
	})

	t.Run("nil inode info falls through to md5 mismatch", func(t *testing.T) {
		t.Parallel()
		m := &TransitionMgr{meta: &md5ClassifyMW{
			MockMetaWrapper: NewMockMetaWrapper(),
			inodeGet: func(uint64, bool) (*proto.InodeInfo, error) {
				return nil, nil
			},
		}}
		err := m.classifyMd5Mismatch(e, "src", "a", "b")
		require.Error(t, err)
		require.Contains(t, err.Error(), "check src md5 inconsistent")
	})
}

// zeroReadExtent always returns zero bytes (src path).
type zeroReadExtent struct{ *MockExtentClient }

func (z *zeroReadExtent) Read(_ uint64, data []byte, _ int, size int, _ uint8, _ bool) (int, error) {
	if size <= 0 {
		return 0, nil
	}
	n := size
	if n > len(data) {
		n = len(data)
	}
	return n, nil
}

// shortReadExtentClient returns at most maxChunk bytes per Read while honoring offset/size.
type shortReadExtentClient struct {
	src      []byte
	dst      []byte
	maxChunk int
}

func newShortReadExtentClient(fileSize, maxChunk int) *shortReadExtentClient {
	src := make([]byte, fileSize)
	for i := range src {
		src[i] = 'a'
	}
	return &shortReadExtentClient{src: src, maxChunk: maxChunk}
}

func (m *shortReadExtentClient) OpenStream(inode uint64, openForWrite bool, isCache bool, fullPath string) error {
	return nil
}

func (m *shortReadExtentClient) CloseStream(inode uint64) error {
	return nil
}

func (m *shortReadExtentClient) Read(inode uint64, data []byte, offset int, size int, poolId uint8, isMigration bool) (int, error) {
	content := m.src
	if isMigration {
		content = m.dst
	}
	if offset >= len(content) {
		return 0, io.EOF
	}
	remain := len(content) - offset
	n := size
	if n > remain {
		n = remain
	}
	if m.maxChunk > 0 && n > m.maxChunk {
		n = m.maxChunk
	}
	copy(data, content[offset:offset+n])
	if offset+n >= len(content) {
		return n, io.EOF
	}
	return n, nil
}

func (m *shortReadExtentClient) Write(inode uint64, offset int, data []byte, flags int, checkFunc func() error, poolId uint8, storageClass uint32, isMigration, waitForFlush bool) (int, error) {
	need := offset + len(data)
	if len(m.dst) < need {
		buf := make([]byte, need)
		copy(buf, m.dst)
		m.dst = buf
	}
	copy(m.dst[offset:], data)
	return len(data), nil
}

func (m *shortReadExtentClient) Flush(inode uint64) error {
	return nil
}

func (m *shortReadExtentClient) Close() error {
	return nil
}

// ffReadExtent returns 0xff bytes (dst migration extent path).
type ffReadExtent struct{ *MockExtentClient }

func (f *ffReadExtent) Read(_ uint64, data []byte, _ int, size int, _ uint8, _ bool) (int, error) {
	if size <= 0 {
		return 0, nil
	}
	n := size
	if n > len(data) {
		n = len(data)
	}
	for i := 0; i < n; i++ {
		data[i] = 0xff
	}
	return n, nil
}

func TestMigrate_dstMd5MismatchUsesClassify(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	e := &proto.ScanDentry{
		Inode: 9100,
		Size:  util.BlockSize,
		InodeInfo: &proto.InodeInfo{
			Inode:      9100,
			ModifyTime: baseTime,
		},
	}
	mgr := &TransitionMgr{
		ec:     &zeroReadExtent{NewMockExtentClient()},
		ecForW: &ffReadExtent{NewMockExtentClient()},
		meta: &md5ClassifyMW{
			MockMetaWrapper: NewMockMetaWrapper(),
			inodeGet: func(uint64, bool) (*proto.InodeInfo, error) {
				return &proto.InodeInfo{Inode: e.Inode, ModifyTime: baseTime}, nil
			},
		},
	}
	err := mgr.migrate(e)
	require.Error(t, err)
	require.Contains(t, err.Error(), "check dst md5 inconsistent")
}

func TestLcNodeIoLimiterSnapshotAndUpdate(t *testing.T) {
	limiter := NewLcNodeIoLimiter(10, 5)
	snapshot := limiter.Snapshot()
	require.Equal(t, int64(10), snapshot.ReadMBps)
	require.Equal(t, int64(5), snapshot.WriteMBps)
	require.Equal(t, int64(10*bytesPerMB), snapshot.ReadBytesPerSec)
	require.Equal(t, int64(5*bytesPerMB), snapshot.WriteBytesPerSec)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, limiter.WaitRead(ctx, 1024))
	require.NoError(t, limiter.WaitWrite(ctx, 1024))

	limiter.UpdateByMBps(0, 0)
	snapshot = limiter.Snapshot()
	require.Equal(t, int64(0), snapshot.ReadMBps)
	require.Equal(t, int64(0), snapshot.WriteMBps)
	require.NoError(t, limiter.WaitRead(context.Background(), defaultLcIoLimitBurst+1))
	require.NoError(t, limiter.WaitWrite(context.Background(), defaultLcIoLimitBurst+1))
}

func TestReadFromExtentClientLimiterUsesActualReadN(t *testing.T) {
	const (
		fileSize = 32
		maxChunk = 8
	)
	src := newShortReadExtentClient(fileSize, maxChunk)
	var readWaits []int
	limiter := NewLcNodeIoLimiter(0, 0)
	limiter.readWaitHook = func(n int) {
		readWaits = append(readWaits, n)
	}
	transitionMgr := &TransitionMgr{
		ec:      src,
		limiter: limiter,
	}
	dentry := &proto.ScanDentry{
		Inode: 1,
		Size:  fileSize,
	}

	require.NoError(t, transitionMgr.readFromExtentClient(dentry, ioutil.Discard, false, 0, 0, true))
	require.Equal(t, []int{8, 8, 8, 8}, readWaits)
	require.Equal(t, fileSize, sumInts(readWaits))
}

func TestReadFromExtentClientNilLimiterDoesNotFail(t *testing.T) {
	src := NewMockExtentClient()
	transitionMgr := &TransitionMgr{
		ec: src,
		// Intentionally keep limiter nil to cover constructor paths like /getFile.
	}
	dentry := &proto.ScanDentry{
		Inode: 1,
		Size:  16,
	}

	require.NoError(t, transitionMgr.readFromExtentClient(dentry, ioutil.Discard, false, 0, 0, true))
	require.Greater(t, src.readBytes, 0)
}

func TestMigrateCopyChargesReadAndWriteLimiters(t *testing.T) {
	const (
		fileSize = 32
		maxChunk = 8
	)
	src := newShortReadExtentClient(fileSize, maxChunk)
	dst := newShortReadExtentClient(0, maxChunk)
	var readWaits, writeWaits []int
	limiter := NewLcNodeIoLimiter(0, 0)
	limiter.readWaitHook = func(n int) {
		readWaits = append(readWaits, n)
	}
	limiter.writeWaitHook = func(n int) {
		writeWaits = append(writeWaits, n)
	}
	transitionMgr := &TransitionMgr{
		volume:  "test_vol",
		ec:      src,
		ecForW:  dst,
		meta:    NewMockMetaWrapper(),
		limiter: limiter,
	}
	dentry := &proto.ScanDentry{
		Inode:        1,
		Size:         fileSize,
		SrcPoolId:    proto.DefaultSSDPoolId,
		DstPoolId:    proto.DefaultHDDPoolId,
		StorageClass: proto.OpTypeToStorageType(proto.OpTypeStorageClassHDD),
		InodeInfo:    &proto.InodeInfo{},
	}

	require.NoError(t, transitionMgr.migrate(dentry))
	// Copy loop bills both read and write quotas; src/dst MD5 checks bill read quota.
	require.Equal(t, fileSize, sumInts(writeWaits))
	require.Equal(t, fileSize*3, sumInts(readWaits))
	for _, n := range append(readWaits, writeWaits...) {
		require.LessOrEqual(t, n, maxChunk)
	}
}

func sumInts(vals []int) int {
	total := 0
	for _, v := range vals {
		total += v
	}
	return total
}

func TestTransitionMgrMigrateWithLimiter(t *testing.T) {
	src := NewMockExtentClient()
	dst := NewMockExtentClient()
	transitionMgr := &TransitionMgr{
		volume:  "test_vol",
		ec:      src,
		ecForW:  dst,
		meta:    NewMockMetaWrapper(),
		limiter: NewLcNodeIoLimiter(0, 0),
	}
	dentry := &proto.ScanDentry{
		Inode:        1,
		Size:         16,
		SrcPoolId:    proto.DefaultSSDPoolId,
		DstPoolId:    proto.DefaultHDDPoolId,
		StorageClass: proto.OpTypeToStorageType(proto.OpTypeStorageClassHDD),
		InodeInfo:    &proto.InodeInfo{},
	}

	require.NoError(t, transitionMgr.migrate(dentry))
	require.Greater(t, src.readBytes, 0)
	require.Greater(t, dst.writeBytes, 0)
}

func TestTransitionMgrMigrateToEbsWithLimiter(t *testing.T) {
	src := NewMockExtentClient()
	ebs := NewMockEbsClient()
	var readWaits, writeWaits []int
	limiter := NewLcNodeIoLimiter(0, 0)
	limiter.readWaitHook = func(n int) {
		readWaits = append(readWaits, n)
	}
	limiter.writeWaitHook = func(n int) {
		writeWaits = append(writeWaits, n)
	}
	transitionMgr := &TransitionMgr{
		volume:    "test_vol",
		ec:        src,
		ebsClient: ebs,
		limiter:   limiter,
	}
	dentry := &proto.ScanDentry{
		Inode: 1,
		Size:  16,
	}

	_, err := transitionMgr.migrateToEbs(dentry)
	require.NoError(t, err)
	require.Len(t, ebs.data, int(dentry.Size))
	require.Greater(t, src.readBytes, 0)
	require.Equal(t, int(dentry.Size)*2, sumInts(readWaits))
	require.Equal(t, int(dentry.Size), sumInts(writeWaits))
}

type stubLcWriteLimiter struct {
	waitFn func(ctx context.Context, n int) error
}

func (s *stubLcWriteLimiter) WaitWrite(ctx context.Context, n int) error {
	if s.waitFn != nil {
		return s.waitFn(ctx, n)
	}
	return nil
}

type countReadCloser struct {
	r     io.Reader
	reads int
}

func (c *countReadCloser) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

func TestLcLimitedWriteReaderWaitWriteFailDoesNotExposeBytes(t *testing.T) {
	payload := []byte("hello")
	src := &countReadCloser{r: bytes.NewReader(payload)}
	var waitCalls int
	limiter := &stubLcWriteLimiter{
		waitFn: func(ctx context.Context, n int) error {
			waitCalls++
			if waitCalls == 1 {
				require.Equal(t, len(payload), n)
				return context.Canceled
			}
			return nil
		},
	}
	limited := &lcLimitedWriteReader{reader: src, limiter: limiter}

	buf := make([]byte, len(payload))
	expectBuf := make([]byte, len(payload)) // all zeros
	n, err := limited.Read(buf)
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, src.reads)
	require.Equal(t, expectBuf, buf)

	n, err = limited.Read(buf)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	require.Equal(t, payload, buf[:n])
	require.Equal(t, 1, src.reads)
	require.Equal(t, 2, waitCalls)

	n, err = limited.Read(buf)
	require.Equal(t, 0, n)
	require.Equal(t, io.EOF, err)
}

func TestHttpServiceSetAndGetLcIoLimit(t *testing.T) {
	l := &LcNode{ioLimiter: NewLcNodeIoLimiter(0, 0)}

	req := httptest.NewRequest(http.MethodGet, "/setLcIoLimit?readMBps=10&writeMBps=5", nil)
	resp := httptest.NewRecorder()
	l.httpServiceSetLcIoLimit(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)

	snapshot := l.ioLimiter.Snapshot()
	require.Equal(t, int64(10), snapshot.ReadMBps)
	require.Equal(t, int64(5), snapshot.WriteMBps)

	req = httptest.NewRequest(http.MethodGet, "/getLcIoLimit", nil)
	resp = httptest.NewRecorder()
	l.httpServiceGetLcIoLimit(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"readBytesPerSec":10485760`)
	require.Contains(t, resp.Body.String(), `"writeBytesPerSec":5242880`)
	require.NotContains(t, resp.Body.String(), `"readMBps"`)
	require.NotContains(t, resp.Body.String(), `"writeMBps"`)
}

func TestHttpServiceSetLcIoLimitRejectsNegativeValue(t *testing.T) {
	l := &LcNode{ioLimiter: NewLcNodeIoLimiter(0, 0)}

	req := httptest.NewRequest(http.MethodGet, "/setLcIoLimit?readMBps=-1", nil)
	resp := httptest.NewRecorder()
	l.httpServiceSetLcIoLimit(resp, req)
	require.Equal(t, http.StatusBadRequest, resp.Code)
}
