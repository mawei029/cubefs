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

	err := c.EvictStream(22)
	require.ErrorIs(t, err, syscall.EAGAIN)
	require.NotNil(t, c.streamers[22])
}

func TestObjExtentClient_CloseStreamEvictWhenRefZero(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(33, nil, nil)
	atomic.StoreInt32(&s.refCnt, 1)
	c.streamers[33] = s

	got := c.CloseStream(33)
	require.NoError(t, got)
	require.Nil(t, c.streamers[33])
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
