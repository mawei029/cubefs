package blobstore

import (
	"bytes"
	"context"
	"errors"
	"hash"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/manager"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/log"
)

func TestNewObjExtentClient(t *testing.T) {
	t.Run("with_shared_limit_manager", func(t *testing.T) {
		lm := manager.NewLimitManager(nil)
		c := NewObjExtentClient(ObjExtentConfig{LimitManager: lm})
		require.NotNil(t, c)
		require.Equal(t, lm, c.LimitManager)
	})
	t.Run("default_limit_manager", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		require.NotNil(t, c.LimitManager)
	})
}

func TestECExtentClient_SetStreamerForTest_nil(t *testing.T) {
	t.Run("nil_client_noop", func(t *testing.T) {
		var c *ECExtentClient
		s := mustTestECStreamer(1, nil, nil)
		setStreamerForTest(c, 1, s)
	})
	t.Run("nil_streamer_noop", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		setStreamerForTest(c, 1, nil)
	})
	t.Run("nil_client_and_nil_streamer_safe", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		setStreamerForTest(c, 1, nil)
		var nilC *ECExtentClient
		setStreamerForTest(nilC, 1, mustTestECStreamer(1, nil, nil))
	})
}

func TestECExtentClient_OpenStream_ENOTSUP(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		require.ErrorIs(t, c.OpenStream(1, true, false, "/x"), syscall.ENOTSUP)
	})
	t.Run("debug_branch", func(t *testing.T) {
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyFunc(log.EnableDebug, func() bool { return true })
		c := NewObjExtentClient(ObjExtentConfig{})
		require.ErrorIs(t, c.OpenStream(91, false, false, "/x"), syscall.ENOTSUP)
	})
	t.Run("legacy_debug", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyFunc(log.EnableDebug, func() bool { return true })
		require.ErrorIs(t, c.OpenStream(1, true, false, "/p"), syscall.ENOTSUP)
	})
}

func TestECExtentClient_CloseStream_debug_ref_positive(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(log.EnableDebug, func() bool { return true })
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(92, nil, nil)
	atomic.StoreInt32(&s.refCnt, 2)
	setStreamerForTest(c, 92, s)
	require.NoError(t, c.CloseStream(92))
	require.Equal(t, int32(1), c.RefCnt(92))
}

func TestECExtentClient_EvictStream_debug_log(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(log.EnableDebug, func() bool { return true })
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(93, nil, nil)
	setStreamerForTest(c, 93, s)
	atomic.StoreInt32(&s.refCnt, 0)
	patches.ApplyPrivateMethod(reflect.TypeOf((*ECStreamer)(nil)), "closeReaderWriterLocked",
		func(_ *ECStreamer, _ uint64, _ context.Context) error { return nil })
	require.NoError(t, c.EvictStream(93))
	require.Nil(t, c.GetStreamer(93))
}

func TestECExtentClient_EBADF_without_stream(t *testing.T) {
	t.Run("read_write_flush_truncate", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		_, err := c.Read(9, make([]byte, 1), 0, 1)
		require.ErrorIs(t, err, syscall.EBADF)
		_, err = c.Write(9, 0, []byte("x"), 0)
		require.ErrorIs(t, err, syscall.EBADF)
		require.ErrorIs(t, c.Flush(9), syscall.EBADF)
		require.ErrorIs(t, c.Truncate(0, 9, 10, "/p"), syscall.EBADF)
	})
	t.Run("read_only", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		_, err := c.Read(99, []byte{0}, 0, 1)
		require.ErrorIs(t, err, syscall.EBADF)
	})
	t.Run("write_and_read", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		_, err := c.Write(9, 0, []byte("x"), 0)
		require.ErrorIs(t, err, syscall.EBADF)
		_, err = c.Read(9, make([]byte, 1), 0, 1)
		require.ErrorIs(t, err, syscall.EBADF)
	})
	t.Run("flush_and_truncate", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		require.ErrorIs(t, c.Flush(8), syscall.EBADF)
		require.ErrorIs(t, c.Truncate(0, 8, 10, "/p"), syscall.EBADF)
	})
	t.Run("truncate_no_writer", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		s := mustTestECStreamer(200, nil, nil)
		s.fWriter = nil
		setStreamerForTest(c, 200, s)
		require.ErrorIs(t, c.Truncate(0, 200, 10, "/p"), syscall.EBADF)
	})
}

func TestECExtentClient_Read_nil_ctx(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(2, nil, nil)
	setStreamerForTest(c, 2, s)
	r := s.Reader()
	r.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "Read",
		func(_ *ECStreamer, _ context.Context, buf []byte, _, _ int) (int, error) {
			if len(buf) > 0 {
				buf[0] = 'x'
			}
			return 1, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 1, nil, nil, nil
		})
	buf := make([]byte, 4)
	n, err := c.Read(2, buf, 0, 1)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestECExtentClient_needsReadViewSyncForTest(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.False(t, needsReadViewSyncForTest(c, 3))
	s := mustTestECStreamer(3, nil, nil)
	setStreamerForTest(c, 3, s)
	atomic.StoreUint32(&s.dirty, 1)
	require.True(t, needsReadViewSyncForTest(c, 3))
}

func TestECExtentClient_CloseStream_negative_ref_reset(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamerWithEbsc(4, nil, 16)
	atomic.StoreInt32(&s.refCnt, 0)
	setStreamerForTest(c, 4, s)
	r := s.fReader
	r.preReadLimiter = &blobPreReadLimiter{maxBytes: 256}
	r.readBuf = make([]byte, 16)
	r.prefetchReserved = 16

	var flushCalls int32

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "Flush",
		func(_ *ECStreamer, _ context.Context) error {
			atomic.AddInt32(&flushCalls, 1)
			return nil
		})
	s.mu.Lock()
	atomic.StoreInt32(&s.refCnt, -2)
	s.mu.Unlock()
	require.NoError(t, c.CloseStream(4))
	require.Equal(t, int32(0), atomic.LoadInt32(&s.refCnt))
	require.Equal(t, int32(1), atomic.LoadInt32(&flushCalls))
	require.Nil(t, r.readBuf)
	require.Equal(t, int64(0), r.prefetchReserved)
}

func TestECExtentClient_CloseStream_teardown_err_rollback(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(5, nil, nil)
	atomic.StoreInt32(&s.refCnt, 1)
	setStreamerForTest(c, 5, s)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "Flush",
		func(_ *ECStreamer, _ context.Context) error { return errors.New("teardown") })
	require.Error(t, c.CloseStream(5))
	require.Equal(t, int32(1), atomic.LoadInt32(&s.refCnt))
}

func TestECExtentClient_CloseStream_last_ref_resets_extents_once(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamerWithEbsc(72, nil, 16)
	atomic.StoreInt32(&s.refCnt, 1)
	setStreamerForTest(c, 72, s)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.fWriter), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 2, 4096, nil, nil, nil
		})

	require.NoError(t, c.CloseStream(72))

	var onceRuns int32
	s.mu.Lock()
	s.once.Do(func() {
		atomic.AddInt32(&onceRuns, 1)
		_ = s.updateMetaInfo(nil)
	})
	s.mu.Unlock()
	require.Equal(t, int32(1), onceRuns)
	require.Equal(t, uint64(4096), atomic.LoadUint64(&s.fileSize))
}

func TestECExtentClient_CloseStream_last_ref_releases_prefetch(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamerWithEbsc(71, nil, 16)
	atomic.StoreInt32(&s.refCnt, 1)
	setStreamerForTest(c, 71, s)
	r := s.fReader
	r.preReadLimiter = &blobPreReadLimiter{maxBytes: 256}
	r.readBuf = make([]byte, 32)
	r.prefetchReserved = 32

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 0, nil, nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.fWriter), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })

	require.NoError(t, c.CloseStream(71))
	require.NotNil(t, c.GetStreamer(71))
	require.Nil(t, r.readBuf)
	require.Equal(t, int64(0), r.prefetchReserved)
	require.NotNil(t, s.fReader)
}

func TestECExtentClient_EvictStream_teardown_err(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(6, nil, nil)
	setStreamerForTest(c, 6, s)
	atomic.StoreInt32(&s.refCnt, 0)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "CloseReaderWriter",
		func(_ *ECStreamer) error { return errors.New("ev") })
	require.Error(t, c.EvictStream(6))
	require.NotNil(t, c.GetStreamer(6))
}

func TestECExtentClient_OpenStreamWithArgs_ref_twice(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	args := ECStreamOpenArgs{
		Ino: 40, OpenFlags: syscall.O_RDONLY, PoolId: 1, FileSize: 10, InodeGeneration: 1,
		VolName: "v", VolType: 1, BlockSize: 4096, Mw: &meta.MetaWrapper{},
	}
	require.NoError(t, c.OpenStreamWithArgs(args))
	require.NoError(t, c.OpenStreamWithArgs(args))
	require.Equal(t, int32(2), c.RefCnt(40))
}

func TestECExtentClient_OpenStreamWithArgs_merge_snapshot(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	args1 := ECStreamOpenArgs{
		Ino: 300, OpenFlags: syscall.O_RDONLY, PoolId: 0, FileSize: 100, InodeGeneration: 2,
		VolName: "v", VolType: 1, BlockSize: 4096, Mw: &meta.MetaWrapper{},
	}
	require.NoError(t, c.OpenStreamWithArgs(args1))
	sz, gen, ok := c.FileSize(300)
	require.True(t, ok)
	require.Equal(t, 100, sz)
	require.GreaterOrEqual(t, gen, uint64(2))

	args2 := ECStreamOpenArgs{
		Ino: 300, OpenFlags: syscall.O_RDONLY, PoolId: 0, FileSize: 500, InodeGeneration: 9,
		VolName: "v", VolType: 1, BlockSize: 4096, Mw: &meta.MetaWrapper{},
	}
	require.NoError(t, c.OpenStreamWithArgs(args2))
	sz, gen, ok = c.FileSize(300)
	require.True(t, ok)
	// 再次 Open 仅增加 refCnt，不合并 FileSize（与当前 OpenStreamWithArgs 实现一致）。
	require.Equal(t, 100, sz)
	require.GreaterOrEqual(t, gen, uint64(2))
}

func TestECExtentClient_args_toClientConfig(t *testing.T) {
	s := mustTestECStreamer(50, nil, nil)
	args := ECStreamOpenArgs{
		Ino: 50, OpenFlags: 0, PoolId: 2, FileSize: 9, InodeGeneration: 3,
		VolName: "vn", VolType: 2, BlockSize: 8192, Ebsc: nil, Bc: nil, Mw: nil,
		EnableBcache: true, WConcurrency: 3, ReadConcurrency: 4,
		AheadReadEnable: true, MinReadAheadSize: 1, PrefetchTotalMem: 99, AheadWindowCnt: 4,
	}
	cfg := args.toClientConfig(s)
	require.Equal(t, "vn", cfg.VolName)
	require.Equal(t, 2, cfg.VolType)
	require.Equal(t, 8192, cfg.BlockSize)
	require.Equal(t, uint8(2), cfg.PoolId)
	require.True(t, cfg.AheadReadEnable)
	require.Equal(t, int64(99), cfg.PrefetchTotalMem)
	require.Equal(t, 4, cfg.AheadWindowCnt)

	t.Run("FreeCache_drops_reader_prefetch", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		s := mustTestECStreamerWithEbsc(401, nil, 16)
		r := s.fReader
		r.readBuf = make([]byte, 16)
		r.bufValidLen = 8
		setStreamerForTest(c, 401, s)
		c.FreeCache(401)
		require.Nil(t, r.readBuf)
		require.Equal(t, 0, r.bufValidLen)
	})
	t.Run("FreeCache_missing_stream_noop", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		c.FreeCache(404)
	})
}

func TestECExtentClient_CloseStream_ref_gt_zero(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamerWithEbsc(54, nil, 16)
	atomic.StoreInt32(&s.refCnt, 2)
	setStreamerForTest(c, 54, s)
	r := s.fReader
	r.readBuf = make([]byte, 32)
	r.prefetchReserved = 32

	var flushCalls int32
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "Flush",
		func(_ *ECStreamer, _ context.Context) error {
			atomic.AddInt32(&flushCalls, 1)
			return nil
		})

	var onceRuns int32
	s.mu.Lock()
	s.once.Do(func() { atomic.AddInt32(&onceRuns, 1) })
	s.once.Do(func() { atomic.AddInt32(&onceRuns, 1) })
	s.mu.Unlock()
	require.Equal(t, int32(1), onceRuns)

	require.NoError(t, c.CloseStream(54))
	require.Equal(t, int32(1), atomic.LoadInt32(&s.refCnt))
	require.Equal(t, int32(1), atomic.LoadInt32(&flushCalls))
	require.NotNil(t, c.GetStreamer(54))
	require.NotNil(t, r.readBuf)
	require.Equal(t, int64(32), r.prefetchReserved)

	onceRuns = 0
	s.mu.Lock()
	s.once.Do(func() { atomic.AddInt32(&onceRuns, 1) })
	s.mu.Unlock()
	require.Equal(t, int32(0), onceRuns)
}

func TestECExtentClient_CloseStream_ref_gt_zero_flush_err_rollback(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(56, nil, nil)
	atomic.StoreInt32(&s.refCnt, 2)
	setStreamerForTest(c, 56, s)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "Flush",
		func(_ *ECStreamer, _ context.Context) error { return errors.New("flush on partial close") })

	require.Error(t, c.CloseStream(56))
	require.Equal(t, int32(2), atomic.LoadInt32(&s.refCnt))
}

func TestECExtentClient_EvictStream_ok_delete(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(60, nil, nil)
	setStreamerForTest(c, 60, s)
	atomic.StoreInt32(&s.refCnt, 0)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "CloseReaderWriter",
		func(_ *ECStreamer) error { return nil })
	require.NoError(t, c.EvictStream(60))
	require.Nil(t, c.GetStreamer(60))
}

func TestECExtentClient_EvictStream_ref_positive_no_delete(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(100, nil, nil)
	setStreamerForTest(c, 100, s)
	atomic.StoreInt32(&s.refCnt, 1)
	require.ErrorIs(t, c.EvictStream(100), errStreamerBusy)
	require.NotNil(t, c.GetStreamer(100))
}

func TestECExtentClient_Close_evicts_all(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "CloseReaderWriter",
		func(_ *ECStreamer) error { return nil })
	s1 := mustTestECStreamer(101, nil, nil)
	s2 := mustTestECStreamer(102, nil, nil)
	setStreamerForTest(c, 101, s1)
	setStreamerForTest(c, 102, s2)
	atomic.StoreInt32(&s1.refCnt, 0)
	atomic.StoreInt32(&s2.refCnt, 0)
	require.NoError(t, c.Close())
	require.Nil(t, c.GetStreamer(101))
	require.Nil(t, c.GetStreamer(102))
}

func TestECExtentClient_RefreshExtentsCache(t *testing.T) {
	t.Run("missing_stream_noop", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		require.NoError(t, c.RefreshExtentsCache(404))
	})
	t.Run("delegates_when_present", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		s := mustTestECStreamer(405, nil, nil)
		setStreamerForTest(c, 405, s)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(s), "RefreshExtentsCache",
			func(_ *ECStreamer) error { return nil })
		require.NoError(t, c.RefreshExtentsCache(405))
	})
}

func TestECExtentClient_IsDirty(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.False(t, c.IsDirty(99))

	s := mustTestECStreamer(201, nil, nil)
	setStreamerForTest(c, 201, s)
	require.False(t, c.IsDirty(201))

	s.markDirty()
	require.True(t, c.IsDirty(201))
	require.False(t, c.IsDirty(202))
}

func TestECExtentClient_FileSize(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	sz, gen, ok := c.FileSize(1)
	require.False(t, ok)
	require.Equal(t, 0, sz)
	require.Equal(t, uint64(0), gen)

	s := mustTestECStreamer(201, nil, nil)
	atomic.StoreUint64(&s.fileSize, 500)
	atomic.StoreUint64(&s.inoVersion, 7)
	setStreamerForTest(c, 201, s)
	sz, gen, ok = c.FileSize(201)
	require.True(t, ok)
	require.Equal(t, 500, sz)
	require.Equal(t, uint64(7), gen)
}

func TestECExtentClient_FileSize_uses_max_of_meta_tail_and_writer_tail(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	w := &Writer{fileOffset: 800}
	s := mustTestECStreamer(203, nil, w)
	atomic.StoreUint64(&s.fileSize, 500)
	atomic.StoreUint64(&s.inoVersion, 9)
	setStreamerForTest(c, 203, s)

	sz, gen, ok := c.FileSize(203)
	require.True(t, ok)
	require.Equal(t, 800, sz)
	require.Equal(t, uint64(9), gen)
}

func TestECExtentClient_FileSize_writer_tail_below_atomic_fileSize(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	w := &Writer{fileOffset: 116736, blockPosition: 2048}
	s := mustTestECStreamer(205, nil, w)
	atomic.StoreUint64(&s.fileSize, 829440)
	setStreamerForTest(c, 205, s)

	sz, _, ok := c.FileSize(205)
	require.True(t, ok)
	require.Equal(t, 829440, sz)
}

func TestECExtentClient_missing_ino_noop(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	t.Run("close_stream", func(t *testing.T) {
		require.NoError(t, c.CloseStream(99999))
	})
	t.Run("evict_stream", func(t *testing.T) {
		require.NoError(t, c.EvictStream(99998))
	})
	t.Run("obj_extent_client_close_stream", func(t *testing.T) {
		require.NoError(t, c.CloseStream(404))
	})
}

func TestECExtentClient_OpenStreamWithArgs_new_streamer_error(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(NewECStreamer, func(_ ECStreamOpenArgs, _ *Reader, _ *Writer) (*ECStreamer, error) {
		return nil, errors.New("new streamer fail")
	})
	err := c.OpenStreamWithArgs(ECStreamOpenArgs{Ino: 70, VolName: "v", BlockSize: 4096, Mw: &meta.MetaWrapper{}})
	require.Error(t, err)
}

func TestECExtentClient_OpenStreamWithArgs_basic(t *testing.T) {
	t.Run("creates_streamer", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		args := ECStreamOpenArgs{Ino: 77, VolName: "v", BlockSize: 4096, Mw: &meta.MetaWrapper{}}
		require.NoError(t, c.OpenStreamWithArgs(args))
		require.Equal(t, int32(1), c.RefCnt(77))
		require.NotNil(t, c.GetStreamer(77))
	})
}

func TestECExtentClient_nil_streamer_in_map(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	t.Run("needs_read_view_sync", func(t *testing.T) {
		c.mu.Lock()
		c.streamers[87] = nil
		c.mu.Unlock()
		require.False(t, needsReadViewSyncForTest(c, 87))
	})
	t.Run("refresh_extents_cache", func(t *testing.T) {
		c.mu.Lock()
		c.streamers[88] = nil
		c.mu.Unlock()
		require.NoError(t, c.RefreshExtentsCache(88))
	})
}

func TestECExtentClient_CloseStream_flush_does_not_hold_client_mu(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(81, nil, nil)
	atomic.StoreInt32(&s.refCnt, 1)
	setStreamerForTest(c, 81, s)

	started := make(chan struct{})
	unblock := make(chan struct{})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "Flush",
		func(_ *ECStreamer, _ context.Context) error {
			close(started)
			<-unblock
			return nil
		})

	errCh := make(chan error, 1)
	go func() { errCh <- c.CloseStream(81) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseStream did not enter Flush")
	}
	done := make(chan struct{})
	go func() {
		require.NotNil(t, c.GetStreamer(81))
		require.Equal(t, int32(0), c.RefCnt(81))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GetStreamer blocked; CloseStream still holds c.mu during Flush")
	}
	close(unblock)
	require.NoError(t, <-errCh)
}

func TestECExtentClient_CloseStream_waitsInflightFlushBeforeDropCache(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(90, nil, nil)
	atomic.StoreInt32(&s.refCnt, 1)
	setStreamerForTest(c, 90, s)
	wireStreamerMetaForFlush(s)

	var flushCalls, freeCalls int32
	started := make(chan struct{})
	unblock := make(chan struct{})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.fWriter), "Flush",
		func(_ *Writer, _ uint64, _ context.Context) error {
			atomic.AddInt32(&flushCalls, 1)
			select {
			case <-started:
			default:
				close(started)
			}
			<-unblock
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.fWriter), "FreeCache", func(_ *Writer) {
		atomic.AddInt32(&freeCalls, 1)
	})

	firstErr := make(chan error, 1)
	go func() { firstErr <- s.Flush(context.Background()) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first Flush did not start")
	}

	closeErr := make(chan error, 1)
	go func() { closeErr <- c.CloseStream(90) }()
	select {
	case err := <-closeErr:
		t.Fatalf("CloseStream returned before inflight Flush finished: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	require.Equal(t, int32(0), atomic.LoadInt32(&freeCalls))

	close(unblock)
	require.NoError(t, <-firstErr)
	require.NoError(t, <-closeErr)
	require.Equal(t, int32(0), atomic.LoadInt32(&s.refCnt))
	require.GreaterOrEqual(t, atomic.LoadInt32(&flushCalls), int32(2))
	require.Equal(t, int32(1), atomic.LoadInt32(&freeCalls))
}

func TestECExtentClient_EvictStream_close_does_not_hold_client_mu(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(83, nil, nil)
	atomic.StoreInt32(&s.refCnt, 0)
	setStreamerForTest(c, 83, s)

	started := make(chan struct{})
	unblock := make(chan struct{})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "CloseReaderWriter",
		func(_ *ECStreamer) error {
			close(started)
			<-unblock
			return nil
		})

	errCh := make(chan error, 1)
	go func() { errCh <- c.EvictStream(83) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("EvictStream did not enter CloseReaderWriter")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		require.NotNil(t, c.GetStreamer(83))
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GetStreamer blocked; EvictStream still holds c.mu during CloseReaderWriter")
	}
	close(unblock)
	require.NoError(t, <-errCh)
	require.Nil(t, c.GetStreamer(83))
}

func TestECExtentClient_EvictStream_streamer_mismatch(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	old := mustTestECStreamer(85, nil, nil)
	newer := mustTestECStreamer(85, nil, nil)
	atomic.StoreInt32(&old.refCnt, 0)
	setStreamerForTest(c, 85, old)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "CloseReaderWriter",
		func(_ *ECStreamer) error {
			c.mu.Lock()
			c.streamers[85] = newer
			c.mu.Unlock()
			return nil
		})

	err := c.EvictStream(85)
	require.NoError(t, err)
	require.Equal(t, newer, c.GetStreamer(85))
	require.NotNil(t, newer.fReader)
}

func TestECExtentClient_EvictStream_reopened_during_close(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(86, nil, nil)
	atomic.StoreInt32(&s.refCnt, 0)
	setStreamerForTest(c, 86, s)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "CloseReaderWriter",
		func(_ *ECStreamer) error {
			atomic.StoreInt32(&s.refCnt, 1)
			return nil
		})

	require.ErrorIs(t, c.EvictStream(86), errStreamerBusy)
	require.Equal(t, s, c.GetStreamer(86))
}

func TestECExtentClient_NeedRefreshObjExtents(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.False(t, c.NeedRefreshObjExtents(404))

	s := mustTestECStreamer(405, nil, nil)
	setStreamerForTest(c, 405, s)
	require.True(t, c.NeedRefreshObjExtents(405))

	s.mu.Lock()
	s.oeks = &ReadOnlyOeks{items: []proto.ObjExtentKey{}}
	s.mu.Unlock()
	require.True(t, c.NeedRefreshObjExtents(405))

	s.mu.Lock()
	s.oeks = &ReadOnlyOeks{items: []proto.ObjExtentKey{{FileOffset: 0, Size: 32}}}
	s.mu.Unlock()
	require.False(t, c.NeedRefreshObjExtents(405))

	c.mu.Lock()
	c.streamers[406] = nil
	c.mu.Unlock()
	require.False(t, c.NeedRefreshObjExtents(406))
}

func TestECExtentClient_Truncate_delegates(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(89, nil, nil)
	setStreamerForTest(c, 89, s)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "Truncate",
		func(_ *ECStreamer, _ context.Context, size uint64, _ string) error {
			require.Equal(t, uint64(99), size)
			return nil
		})
	require.NoError(t, c.Truncate(0, 89, 99, "/p"))
}

func TestECExtentClient_EvictStream_ref_edges(t *testing.T) {
	t.Run("ref_positive_keeps_streamer", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		s := mustTestECStreamer(57, nil, nil)
		atomic.StoreInt32(&s.refCnt, 2)
		setStreamerForTest(c, 57, s)
		require.ErrorIs(t, c.EvictStream(57), errStreamerBusy)
		require.NotNil(t, c.GetStreamer(57))
	})
	t.Run("ref_zero_removes_streamer", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		s := mustTestECStreamer(58, nil, nil)
		setStreamerForTest(c, 58, s)
		require.NoError(t, c.EvictStream(58))
		require.Nil(t, c.GetStreamer(58))
	})
}

func TestECExtentClient_CloseStream_zero_ref(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(55, nil, nil)
	atomic.StoreInt32(&s.refCnt, 0)
	setStreamerForTest(c, 55, s)
	require.NoError(t, c.CloseStream(55))
}

func TestECExtentClient_Write_delegates_to_streamer(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(56, nil, nil)
	setStreamerForTest(c, 56, s)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "Write",
		func(_ *ECStreamer, _ context.Context, _ int, data []byte, _ int) (int, error) {
			return len(data), nil
		})
	n, err := c.Write(56, 0, []byte("ab"), 0)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

func TestECExtentClient_OpenStreamWithArgs_existing_streamer(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(59, nil, nil)
	setStreamerForTest(c, 59, s)
	args := ECStreamOpenArgs{Ino: 59, VolName: "v", BlockSize: 4096, Mw: &meta.MetaWrapper{}}
	require.NoError(t, c.OpenStreamWithArgs(args))
	require.Equal(t, int32(1), c.RefCnt(59))
}

func TestECExtentClient_Close_evicts_all_streamers(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	setStreamerForTest(c, 61, mustTestECStreamer(61, nil, nil))
	setStreamerForTest(c, 62, mustTestECStreamer(62, nil, nil))
	require.NoError(t, c.Close())
	require.Nil(t, c.GetStreamer(61))
	require.Nil(t, c.GetStreamer(62))
}

func TestObjExtentClient_FileSizeUsesStreamerSize(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(11, nil, nil)
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

	_, err := c.Read(1, make([]byte, 4), 0, 4)
	require.ErrorIs(t, err, syscall.EBADF)

	_, err = c.Write(1, 0, []byte("x"), 0)
	require.ErrorIs(t, err, syscall.EBADF)

	require.ErrorIs(t, c.Flush(1), syscall.EBADF)
}

func TestObjExtentClient_EvictStreamRefBusy(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(22, nil, nil)
	atomic.StoreInt32(&s.refCnt, 1)
	c.streamers[22] = s

	err := c.EvictStream(22)
	require.ErrorIs(t, err, errStreamerBusy)
	require.NotNil(t, c.streamers[22])
}

func TestObjExtentClient_CloseStreamEvictWhenRefZero(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(33, nil, nil)
	atomic.StoreInt32(&s.refCnt, 1)
	c.streamers[33] = s

	require.NoError(t, c.CloseStream(33))
	require.NotNil(t, c.streamers[33])
	require.Equal(t, int32(0), atomic.LoadInt32(&c.streamers[33].refCnt))

	require.NoError(t, c.EvictStream(33))
	require.Nil(t, c.streamers[33])
}

func TestObjExtentClient_CloseStreamWritableFlushNoDelete(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(44, nil, nil)
	atomic.StoreInt32(&s.refCnt, 1)
	c.streamers[44] = s

	require.NoError(t, c.CloseStream(44))
	require.NotNil(t, c.streamers[44])
	require.Equal(t, int32(0), atomic.LoadInt32(&c.streamers[44].refCnt))

	require.NoError(t, c.EvictStream(44))
	require.Nil(t, c.streamers[44])
}

func TestObjExtentClient_EvictStreamWritableRefBusy(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := mustTestECStreamer(55, nil, nil)
	atomic.StoreInt32(&s.refCnt, 1)
	c.streamers[55] = s

	require.ErrorIs(t, c.EvictStream(55), errStreamerBusy)
	require.NotNil(t, c.streamers[55])
}

func TestObjExtentClient_CloseEvictsAllStreams(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s1 := mustTestECStreamer(1, nil, nil)
	s2 := mustTestECStreamer(2, nil, nil)
	c.streamers[1] = s1
	c.streamers[2] = s2
	require.NoError(t, c.Close())
	require.Empty(t, c.streamers)
}

func TestECExtentClient_HasReader_HasWriter(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.False(t, c.HasReader(1))
	require.False(t, c.HasWriter(1))

	s := &ECStreamer{ino: 64}
	setStreamerForTest(c, 64, s)
	require.False(t, c.HasReader(64))
	require.False(t, c.HasWriter(64))

	s.fReader = &Reader{}
	require.True(t, c.HasReader(64))
	require.False(t, c.HasWriter(64))

	s.fWriter = &Writer{}
	require.True(t, c.HasWriter(64))
}

func TestECExtentClient_WriteFromReader(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	_, err := c.WriteFromReader(context.Background(), 8, nil, nil)
	require.ErrorIs(t, err, syscall.EBADF)

	s := mustTestECStreamer(65, nil, &Writer{})
	setStreamerForTest(c, 65, s)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "WriteFromReader",
		func(_ *ECStreamer, _ context.Context, _ io.Reader, _ hash.Hash) (uint64, error) {
			return 11, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 0, nil, nil, nil
		})
	n, err := c.WriteFromReader(context.Background(), 65, bytes.NewReader([]byte("x")), nil)
	require.NoError(t, err)
	require.Equal(t, uint64(11), n)
}

func TestECStreamer_BadfdOnMissingReaderWriter(t *testing.T) {
	s := mustTestECStreamer(44, nil, nil)
	s.fReader = nil
	n, got := s.Read(context.Background(), make([]byte, 1), 0, 1)
	require.Equal(t, 0, n)
	require.ErrorIs(t, got, syscall.EBADF)

	s.fWriter = nil
	n, got = s.Write(context.Background(), 0, []byte("x"), 0)
	require.Equal(t, 0, n)
	require.ErrorIs(t, got, syscall.EBADF)

	require.NoError(t, s.Flush(context.Background()))
}

func TestECExtentClient_removeDeletedStreamer(t *testing.T) {
	t.Run("drops_matching_pointer", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		s := mustTestECStreamer(401, nil, nil)
		setStreamerForTest(c, 401, s)
		c.removeDeletedStreamer(s)
		require.Nil(t, c.GetStreamer(401))
		require.NotNil(t, s.fReader)
		require.NotNil(t, s.fWriter)
	})
	t.Run("mismatch_keeps_newer", func(t *testing.T) {
		c := NewObjExtentClient(ObjExtentConfig{})
		old := mustTestECStreamer(402, nil, nil)
		newer := mustTestECStreamer(402, nil, nil)
		setStreamerForTest(c, 402, newer)
		c.removeDeletedStreamer(old)
		require.Equal(t, newer, c.GetStreamer(402))
		require.NotNil(t, newer.fReader)
	})
	t.Run("nil_args_safe", func(t *testing.T) {
		var c *ECExtentClient
		c.removeDeletedStreamer(mustTestECStreamer(403, nil, nil))
		NewObjExtentClient(ObjExtentConfig{}).removeDeletedStreamer(nil)
	})
}
