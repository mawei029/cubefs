package blobstore

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObjExtentClient_FileSizeUsesStreamerSize(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(11, &Reader{
		valid:            true,
		metaReportedSize: 1024,
	}, nil)
	atomic.StoreUint64(&s.fileSize, 128)
	atomic.StoreUint64(&s.inoVersion, 9)
	c.streamers[11] = s

	size, gen, ok := c.FileSize(11)
	require.True(t, ok)
	require.Equal(t, 128, size)
	require.Equal(t, uint64(9), gen)
}

func TestObjExtentClient_ReadWriteBadfdWhenStreamMissing(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})

	_, err := c.Read(1, make([]byte, 4), 0, 4, 0, false)
	require.ErrorIs(t, err, syscall.EBADF)

	_, err = c.Write(1, 0, []byte("x"), 0, nil, 0, 0, false, false)
	require.ErrorIs(t, err, syscall.EBADF)

	require.ErrorIs(t, c.Flush(1), syscall.EBADF)
}

func TestObjExtentClient_EvictStreamRefBusy(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(22, nil, nil)
	atomic.StoreInt32(&s.refCnt, 1)
	c.streamers[22] = s

	// rdonly 默认 1：与副本 EvictStream(rdonly) 一致，ref>0 时 return nil 且不删表
	err := c.EvictStream(22)
	require.NoError(t, err)
	require.NotNil(t, c.streamers[22])
}

func TestObjExtentClient_CloseStreamEvictWhenRefZero(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(33, nil, nil)
	atomic.StoreInt32(&s.refCnt, 1)
	c.streamers[33] = s

	require.NoError(t, c.CloseStream(33))
	// CloseStream 不删表项；rdonly 路径也不 teardown
	require.NotNil(t, c.streamers[33])
	require.Equal(t, int32(0), atomic.LoadInt32(&c.streamers[33].refCnt))

	require.NoError(t, c.EvictStream(33))
	require.Nil(t, c.streamers[33])
}

func TestObjExtentClient_CloseStreamWritableTeardownNoDelete(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(44, nil, nil)
	atomic.StoreUint32(&s.rdonly, 0)
	atomic.StoreInt32(&s.refCnt, 1)
	c.streamers[44] = s

	require.NoError(t, c.CloseStream(44))
	require.NotNil(t, c.streamers[44])
	require.Equal(t, int32(0), atomic.LoadInt32(&c.streamers[44].refCnt))

	require.NoError(t, c.EvictStream(44))
	require.Nil(t, c.streamers[44])
}

func TestObjExtentClient_EvictStreamWritableRefBusyReturnsNil(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(55, nil, nil)
	atomic.StoreUint32(&s.rdonly, 0)
	atomic.StoreInt32(&s.refCnt, 1)
	c.streamers[55] = s

	// 与副本 EvictStream(rdonly) refcnt>0 一致：Warn 语义 + return nil，非 EAGAIN
	require.NoError(t, c.EvictStream(55))
	require.NotNil(t, c.streamers[55])
}

func TestObjExtentClient_CloseStreamMissingIsNoop(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.NoError(t, c.CloseStream(999))
}

func TestObjExtentClient_CloseEvictsAllStreams(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	c.streamers[1] = NewECStreamer(1, nil, nil)
	c.streamers[2] = NewECStreamer(2, nil, nil)
	require.NoError(t, c.Close())
	require.Empty(t, c.streamers)
}

func TestECStreamer_BadfdOnMissingReaderWriter(t *testing.T) {
	s := NewECStreamer(44, nil, nil)

	n, got := s.Read(context.Background(), make([]byte, 1), 0, 1, 0, false)
	require.Equal(t, 0, n)
	require.ErrorIs(t, got, syscall.EBADF)

	n, got = s.WriteWithOpts(context.Background(), 0, []byte("x"), 0, nil, 0, 0, false, false)
	require.Equal(t, 0, n)
	require.ErrorIs(t, got, syscall.EBADF)

	require.NoError(t, s.Flush(context.Background()))
}
