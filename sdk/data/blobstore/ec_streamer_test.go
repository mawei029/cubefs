package blobstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/util/buf"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/meta"
)

func TestECStreamer_nil_receiver_edges(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		var s *ECStreamer
		require.Equal(t, "ECStreamer{nil}", s.String())
	})
	t.Run("file_size_view", func(t *testing.T) {
		var s *ECStreamer
		sz, gen := s.FileSizeView()
		require.Equal(t, 0, sz)
		require.Equal(t, uint64(0), gen)
	})
	t.Run("raise_file_size", func(t *testing.T) {
		var s *ECStreamer
		s.raiseFileSize(100)
	})
}

func TestECStreamer_Accessors_and_NewReaderWriter(t *testing.T) {
	s := mustTestECStreamer(7, nil, nil)
	require.Equal(t, uint64(7), s.Inode())
	require.Equal(t, "v", s.Volume())
	require.Equal(t, 8<<20, s.BlockSize())
	require.Equal(t, int32(0), s.RefCnt())
	require.NotNil(t, s.Reader())
	require.NotNil(t, s.Writer())

	cfg := ClientConfig{VolName: "v", VolType: 1, BlockSize: 4096, Ino: 7, Mw: &meta.MetaWrapper{}, ECStreamer: s}
	ensureReaderForTest(s, cfg)
	ensureWriterForTest(s, cfg)
	require.NotNil(t, s.Reader())
	require.NotNil(t, s.Writer())
}

func TestECStreamer_isDirty_and_FileSizeView(t *testing.T) {
	s := mustTestECStreamer(10, nil, nil)
	require.False(t, s.isDirty())
	SeedLogicalViewForTest(s, 120, 3)
	sz, gen := s.FileSizeView()
	require.Equal(t, 120, sz)
	require.Equal(t, uint64(3), gen)
	seedDirtyForTest(s)
	require.True(t, s.isDirty())
}

func TestECStreamer_Flush_short(t *testing.T) {
	t.Run("not_dirty_noop", func(t *testing.T) {
		s := mustTestECStreamer(11, nil, nil)
		require.NoError(t, s.Flush(context.Background()))
	})
	t.Run("dirty_empty_buffer_cleans", func(t *testing.T) {
		s := mustTestECStreamer(12, nil, nil)
		seedDirtyForTest(s)
		require.NoError(t, s.Flush(context.Background()))
		require.False(t, s.isDirty())
	})
	t.Run("flush_and_free_cache", func(t *testing.T) {
		s := mustTestECStreamer(26, nil, nil)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(s.fWriter), "FreeCache", func(_ *Writer) {})
		require.NoError(t, flushAndFreeCacheForTest(s, context.Background()))
	})
}

func TestECStreamer_Read_short(t *testing.T) {
	t.Run("reader_nil_EBADF", func(t *testing.T) {
		s := mustTestECStreamer(13, nil, nil)
		s.fReader = nil
		_, err := s.Read(context.Background(), make([]byte, 2), 0, 1)
		require.ErrorIs(t, err, syscall.EBADF)
	})
	t.Run("zero_size", func(t *testing.T) {
		s := mustTestECStreamer(28, nil, nil)
		n, err := s.Read(context.Background(), make([]byte, 4), 0, 0)
		require.NoError(t, err)
		require.Equal(t, 0, n)
	})
}

func TestECStreamer_Read_get_extents_error(t *testing.T) {
	s := mustTestECStreamer(31, &Reader{}, nil)
	s.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 0, 0, nil, nil, errors.New("meta down")
		})
	_, err := s.Read(context.Background(), make([]byte, 2), 0, 1)
	require.Error(t, err)
}

func TestECStreamer_NewReader_idempotent(t *testing.T) {
	s := mustTestECStreamer(30, nil, nil)
	cfg := ClientConfig{VolName: "v", Ino: 30, Mw: &meta.MetaWrapper{}, ECStreamer: s}
	ensureReaderForTest(s, cfg)
	first := s.Reader()
	ensureReaderForTest(s, cfg)
	require.Same(t, first, s.Reader())
}

func TestECStreamer_Read_dirty_flushes_before_read(t *testing.T) {
	s := mustTestECStreamer(25, &Reader{}, nil)
	seedDirtyForTest(s)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.fWriter), "Flush",
		func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 8, nil, nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.fReader), "Read",
		func(_ *Reader, _ context.Context, dst []byte, _, _ int) (int, error) {
			return len(dst), nil
		})

	n, err := s.Read(context.Background(), make([]byte, 4), 0, 4)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.False(t, s.isDirty())
}

func TestECStreamer_Flush_writer_flush_error(t *testing.T) {
	s := mustTestECStreamer(27, nil, nil)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.fWriter), "Flush",
		func(_ *Writer, _ uint64, _ context.Context) error { return errors.New("w flush") })
	require.Error(t, s.Flush(context.Background()))
}

func TestECStreamer_Write_delegates_to_writer(t *testing.T) {
	s := mustTestECStreamer(14, nil, nil)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.fWriter), "Write",
		func(_ *Writer, _ context.Context, _ int, data []byte, _ int) (int, error) {
			return len(data), nil
		})
	n, err := s.Write(context.Background(), 0, []byte("xy"), 0)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

func TestECStreamer_Write_short(t *testing.T) {
	t.Run("writer_nil_EBADF", func(t *testing.T) {
		s := mustTestECStreamer(14, nil, nil)
		s.fWriter = nil
		_, err := s.Write(context.Background(), 0, []byte("x"), 0)
		require.ErrorIs(t, err, syscall.EBADF)
	})
	t.Run("empty_data", func(t *testing.T) {
		s := mustTestECStreamer(34, nil, nil)
		n, err := s.Write(context.Background(), 0, nil, 0)
		require.NoError(t, err)
		require.Equal(t, 0, n)
	})
}

func TestECStreamer_updateMetaInfo_dirty_with_buffer_keeps_dirty(t *testing.T) {
	w := &Writer{fileOffset: 100, blockPosition: 10}
	s := mustTestECStreamer(15, nil, w)
	seedDirtyForTest(s)

	mw := &meta.MetaWrapper{}
	s.mw = mw
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 50, nil, nil, nil
		})

	require.NoError(t, s.updateMetaInfo(nil))
	require.True(t, s.isDirty())
	require.Equal(t, uint64(100), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_updateMetaInfo_with_commitSize(t *testing.T) {
	w := &Writer{fileOffset: 500, blockPosition: 20}
	s := mustTestECStreamer(50, nil, w)
	seedDirtyForTest(s)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 3, 200, nil, nil, nil
		})

	commit := uint64(60)
	require.NoError(t, s.updateMetaInfo(&commit))
	require.False(t, s.isDirty())
	require.Equal(t, uint64(60), atomic.LoadUint64(&s.fileSize))
	require.Equal(t, uint64(3), atomic.LoadUint64(&s.inoVersion))
	require.Equal(t, 60, w.fileOffset)
}

func TestECStreamer_updateMetaInfo_dirty_no_buffer_cleans(t *testing.T) {
	s := mustTestECStreamer(16, nil, nil)
	seedDirtyForTest(s)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 2, 80, nil, nil, nil
		})

	require.NoError(t, s.updateMetaInfo(nil))
	require.False(t, s.isDirty())
	require.Equal(t, uint64(80), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_commitFileSize_discards_buffer_and_cleans_dirty(t *testing.T) {
	w := &Writer{fileOffset: 200, blockPosition: 30}
	s := mustTestECStreamer(17, nil, w)
	seedDirtyForTest(s)
	commitLogicalSizeForTest(s, 150)
	require.False(t, s.isDirty())
	require.Equal(t, 0, w.bufferDirtyLen())
	require.Equal(t, uint64(150), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_commitFileSize_releasesPooledBuf(t *testing.T) {
	const blockSize = 64
	buf.InitCachePool(blockSize, 1)
	s := mustTestECStreamerWithEbsc(171, &BlobStoreClient{}, blockSize)
	w := s.fWriter
	w.allocateCache()
	require.True(t, w.bufPooled)

	acquired := make(chan []byte, 1)
	go func() {
		acquired <- buf.CachePool.Get()
	}()
	select {
	case <-acquired:
		t.Fatal("second Get should block while pooled buf is held")
	default:
	}

	commitLogicalSizeForTest(s, 32)
	require.Nil(t, w.buf)
	require.False(t, w.bufPooled)

	select {
	case b := <-acquired:
		buf.CachePool.Put(b)
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Get did not wake after commitFileSize released pool block")
	}
}

func TestECStreamer_commitFileSize_discardsHeapBuf(t *testing.T) {
	const blockSize = 64
	buf.InitCachePool(blockSize, 4)
	s := mustTestECStreamerWithEbsc(172, &BlobStoreClient{}, blockSize)
	w := s.fWriter
	w.buf = append([]byte(nil), []byte("heap")...)
	w.bufPooled = false

	commitLogicalSizeForTest(s, 32)
	require.Nil(t, w.buf)
	require.False(t, w.bufPooled)
}

func TestECStreamer_truncateV2_same_size_dirty_only_meta(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(18, nil, w)
	s.mw = &meta.MetaWrapper{}
	seedDirtyForTest(s)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 64, nil, nil, nil
		})

	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), 18, 64, "/p")
	s.mu.Unlock()
	require.NoError(t, err)
	require.False(t, s.isDirty())
}

func TestECStreamer_truncateV2_ENOENT_new_file(t *testing.T) {
	w := &Writer{}
	args := ECStreamOpenArgs{Ino: 19, VolName: "v", BlockSize: 8 << 20, Mw: &meta.MetaWrapper{}}
	s, err := NewECStreamer(args, nil, w)
	require.NoError(t, err)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	var getCalls int
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			getCalls++
			if getCalls == 1 {
				return 0, 0, nil, nil, syscall.ENOENT
			}
			return 1, 128, nil, nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, _ proto.ObjExtentKey, _ proto.ObjExtentKey) error {
			return nil
		})

	s.mu.Lock()
	err = s.truncateV2Locked(context.Background(), 19, 128, "/new")
	s.mu.Unlock()
	require.NoError(t, err)
}

func TestECStreamer_RefreshExtentsCache(t *testing.T) {
	s := mustTestECStreamer(20, nil, nil)
	s.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 3, 90, nil, nil, nil
		})
	require.NoError(t, s.RefreshExtentsCache())
}

func TestECStreamer_resetExtentsOnceLocked(t *testing.T) {
	s := mustTestECStreamer(88, nil, nil)
	var n int32
	s.mu.Lock()
	s.once.Do(func() { atomic.AddInt32(&n, 1) })
	s.once.Do(func() { atomic.AddInt32(&n, 1) })
	require.Equal(t, int32(1), n)
	s.resetExtentsOnceLocked()
	s.once.Do(func() { atomic.AddInt32(&n, 1) })
	s.mu.Unlock()
	require.Equal(t, int32(2), n)
}

func TestECStreamer_closeReaderWriterLocked_releases_prefetch_keeps_endpoints(t *testing.T) {
	w := &Writer{buf: make([]byte, 8)}
	s := mustTestECStreamerWithEbsc(77, nil, 16)
	s.fWriter = w
	r := s.fReader
	l := &blobPreReadLimiter{maxBytes: 64}
	require.True(t, l.tryAcquire(32))
	r.preReadLimiter = l
	r.readBuf = make([]byte, 32)
	r.prefetchReserved = 32

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })

	s.mu.Lock()
	require.NoError(t, s.closeReaderWriterLocked(77, context.Background()))
	s.mu.Unlock()
	// 端点置空由 EvictStream 负责；closeReaderWriterLocked 仅 dropIOCachesLocked。
	require.NotNil(t, s.fReader)
	require.NotNil(t, s.fWriter)
	require.Nil(t, r.readBuf)
	require.Equal(t, int64(0), r.prefetchReserved)
	require.Equal(t, int64(0), atomic.LoadInt64(&l.usedBytes))
}

func TestECStreamer_closeReaderWriterLocked_flush_err(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(21, nil, w)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error {
		return errors.New("flush fail")
	})
	s.mu.Lock()
	err := s.closeReaderWriterLocked(21, context.Background())
	s.mu.Unlock()
	require.Error(t, err)
}

func TestECStreamer_mergeInodeGen_and_Truncate(t *testing.T) {
	t.Run("merge_inode_gen_zero_noop", func(t *testing.T) {
		s := mustTestECStreamer(36, nil, nil)
		s.mergeInodeGen(0)
		require.Equal(t, uint64(0), atomic.LoadUint64(&s.inoVersion))
	})
	t.Run("truncate_public_api_no_writer", func(t *testing.T) {
		s := mustTestECStreamer(29, nil, nil)
		s.fWriter = nil
		require.ErrorIs(t, s.Truncate(context.Background(), 10, "/p"), syscall.EBADF)
	})
}

func TestECStreamer_flushExt_empty_dirty_only_meta(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(22, nil, w)
	s.mw = &meta.MetaWrapper{}
	seedDirtyForTest(s)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 10, nil, nil, nil
		})

	require.NoError(t, w.Flush(22, context.Background()))
	require.False(t, s.isDirty())
}

func TestECStreamer_OeksLocked_copy(t *testing.T) {
	s := mustTestECStreamer(23, nil, nil)
	s.oeks = []proto.ObjExtentKey{{FileOffset: 1, Size: 2}}
	cp := s.OeksLocked()
	require.Len(t, cp, 1)
	cp[0].FileOffset = 99
	require.Equal(t, uint64(1), s.oeks[0].FileOffset)
}

func TestECStreamer_HasObjExtents(t *testing.T) {
	s := mustTestECStreamer(24, nil, nil)
	require.False(t, s.HasObjExtents())

	s.mu.Lock()
	s.oeks = []proto.ObjExtentKey{}
	s.mu.Unlock()
	require.False(t, s.HasObjExtents())

	s.mu.Lock()
	s.oeks = []proto.ObjExtentKey{{FileOffset: 0, Size: 8}}
	s.mu.Unlock()
	require.True(t, s.HasObjExtents())
}

func TestECStreamer_updateMetaInfo_dirty_with_buffer(t *testing.T) {
	w := &Writer{fileOffset: 60, blockPosition: 5}
	s := mustTestECStreamer(35, nil, w)
	seedDirtyForTest(s)
	s.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 10, nil, nil, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "bufferDirtyLen", func(_ *Writer) int { return 8 })

	require.NoError(t, s.updateMetaInfo(nil))
	require.True(t, s.isDirty())
	require.Equal(t, uint64(60), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_updateMetaInfo_dirty_with_buffer_stale_meta_tail_capped(t *testing.T) {
	w := &Writer{fileOffset: 952320, blockPosition: 2048}
	s := mustTestECStreamer(38, nil, w)
	atomic.StoreUint64(&s.fileSize, 952320)
	seedDirtyForTest(s)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 9, 1038336, nil, []proto.ObjExtentKey{{FileOffset: 950272, Size: 2048}}, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "bufferDirtyLen", func(_ *Writer) int { return 2048 })

	require.NoError(t, s.updateMetaInfo(nil))
	require.True(t, s.isDirty())
	require.Equal(t, uint64(1038336), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_updateMetaInfo_dirty_with_buffer_middle_overwrite_keeps_tail(t *testing.T) {
	w := &Writer{fileOffset: 116736, blockPosition: 2048}
	s := mustTestECStreamer(39, nil, w)
	atomic.StoreUint64(&s.fileSize, 829440)
	seedDirtyForTest(s)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 9, 829440, nil, []proto.ObjExtentKey{{FileOffset: 0, Size: 829440}}, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "bufferDirtyLen", func(_ *Writer) int { return 2048 })

	require.NoError(t, s.updateMetaInfo(nil))
	require.True(t, s.isDirty())
	require.Equal(t, uint64(829440), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_raiseFileSize_edges(t *testing.T) {
	t.Run("zero_lb_skipped", func(t *testing.T) {
		s := mustTestECStreamer(32, nil, nil)
		s.raiseFileSize(0)
		require.Equal(t, uint64(0), atomic.LoadUint64(&s.fileSize))
	})
}

func TestECStreamer_updateMetaInfo_under_mu(t *testing.T) {
	s := mustTestECStreamer(33, nil, nil)
	s.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 2, 40, nil, []proto.ObjExtentKey{{FileOffset: 0, Size: 40}}, nil
		})
	s.mu.Lock()
	err := s.updateMetaInfo(nil)
	s.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, uint64(40), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_updateMetaInfo_dirty_without_buffer(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(37, nil, w)
	seedDirtyForTest(s)
	s.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 3, 80, nil, []proto.ObjExtentKey{{FileOffset: 0, Size: 80}}, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "bufferDirtyLen", func(_ *Writer) int { return 0 })

	require.NoError(t, s.updateMetaInfo(nil))
	require.False(t, s.isDirty())
	require.Equal(t, uint64(80), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_raiseFileSize_and_mergeInodeGen(t *testing.T) {
	s := mustTestECStreamer(24, nil, nil)
	s.raiseFileSize(100)
	s.raiseFileSize(80)
	require.Equal(t, uint64(100), atomic.LoadUint64(&s.fileSize))
	s.mergeInodeGen(5)
	s.mergeInodeGen(9)
	require.Equal(t, uint64(9), atomic.LoadUint64(&s.inoVersion))
}

func TestECStreamer_truncateV2_grow_truncateV2_error(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(267, nil, w)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 50, nil, nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, _, _ proto.ObjExtentKey) error {
			return errors.New("grow truncate failed")
		})

	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), 267, 100, "/grow-err")
	s.mu.Unlock()
	require.Error(t, err)
	require.Contains(t, err.Error(), "grow truncate failed")
}

func TestECStreamer_truncateV2_grow_meta_only(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(25, nil, w)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 100, nil, []proto.ObjExtentKey{{FileOffset: 0, Size: 100}}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, size uint64, _ string, _ proto.ObjExtentKey, _ proto.ObjExtentKey) error {
			require.Equal(t, uint64(200), size)
			return nil
		})

	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), 25, 200, "/grow")
	s.mu.Unlock()
	require.NoError(t, err)
}

func TestECStreamer_truncateV2_shrink_with_meta_deltas(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(261, nil, w)
	s.mw = &meta.MetaWrapper{}
	newDelta := proto.ObjExtentKey{FileOffset: 100, Size: 50}
	delAnchor := proto.ObjExtentKey{FileOffset: 200, Size: 30}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 250, nil, []proto.ObjExtentKey{
				{FileOffset: 0, Size: 100},
				{FileOffset: 100, Size: 100},
				{FileOffset: 200, Size: 50},
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(w), "TruncateV2FromExtents",
		func(_ *Writer, _ context.Context, target, current uint64, _ []proto.ObjExtentKey) (proto.ObjExtentKey, proto.ObjExtentKey, error) {
			require.Equal(t, uint64(150), target)
			require.Equal(t, uint64(250), current)
			return newDelta, delAnchor, nil
		})
	var gotNew, gotDel proto.ObjExtentKey
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, size uint64, _ string, ne, td proto.ObjExtentKey) error {
			require.Equal(t, uint64(150), size)
			gotNew, gotDel = ne, td
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(s), "updateMetaInfo",
		func(_ *ECStreamer, _ *uint64) error { return nil })

	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), 261, 150, "/shrink-delta")
	s.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, newDelta, gotNew)
	require.Equal(t, delAnchor, gotDel)
}

func TestECStreamer_truncateV2_shrink_calls_writer_with_deltas(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(26, nil, w)
	s.mw = &meta.MetaWrapper{}
	delAnchor := proto.ObjExtentKey{FileOffset: 0, Size: 200}
	newDelta := proto.ObjExtentKey{FileOffset: 0, Size: 100, Cid: 9}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 200, nil, []proto.ObjExtentKey{delAnchor}, nil
		})
	var shrinkCalled bool
	patches.ApplyMethod(reflect.TypeOf(w), "TruncateV2FromExtents",
		func(_ *Writer, _ context.Context, target, current uint64, _ []proto.ObjExtentKey) (proto.ObjExtentKey, proto.ObjExtentKey, error) {
			shrinkCalled = true
			require.Equal(t, uint64(100), target)
			require.Equal(t, uint64(200), current)
			return newDelta, delAnchor, nil
		})
	var gotNew, gotDel proto.ObjExtentKey
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, size uint64, _ string, ne, td proto.ObjExtentKey) error {
			require.Equal(t, uint64(100), size)
			gotNew, gotDel = ne, td
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(s), "updateMetaInfo",
		func(_ *ECStreamer, _ *uint64) error { return nil })

	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), 26, 100, "/shrink")
	s.mu.Unlock()
	require.NoError(t, err)
	require.True(t, shrinkCalled)
	require.Equal(t, newDelta, gotNew)
	require.Equal(t, delAnchor, gotDel)
}

func TestECStreamer_truncateV2_shrink_no_oeks(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(262, nil, w)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	var writerShrink bool
	patches.ApplyMethod(reflect.TypeOf(w), "TruncateV2FromExtents",
		func(_ *Writer, _ context.Context, _, _ uint64, _ []proto.ObjExtentKey) (proto.ObjExtentKey, proto.ObjExtentKey, error) {
			writerShrink = true
			return proto.ObjExtentKey{}, proto.ObjExtentKey{}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 80, nil, nil, nil
		})
	var truncSize uint64
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, size uint64, _ string, ne, td proto.ObjExtentKey) error {
			truncSize = size
			require.True(t, ne.IsEmpty())
			require.True(t, td.IsEmpty())
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(s), "updateMetaInfo",
		func(_ *ECStreamer, _ *uint64) error { return nil })

	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), 262, 50, "/shrink-empty-oeks")
	s.mu.Unlock()
	require.NoError(t, err)
	require.False(t, writerShrink)
	require.Equal(t, uint64(50), truncSize)
}

func TestECStreamer_truncateV2_shrink_size_only_past_last_extent_end(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(263, nil, w)
	s.mw = &meta.MetaWrapper{}
	lastEk := proto.ObjExtentKey{FileOffset: 0, Size: 100}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 200, nil, []proto.ObjExtentKey{lastEk}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(w), "TruncateV2FromExtents",
		func(_ *Writer, _ context.Context, target, current uint64, _ []proto.ObjExtentKey) (proto.ObjExtentKey, proto.ObjExtentKey, error) {
			require.Equal(t, uint64(150), target)
			require.Equal(t, uint64(200), current)
			return proto.ObjExtentKey{}, proto.ObjExtentKey{}, nil
		})
	var truncSize uint64
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, size uint64, _ string, ne, td proto.ObjExtentKey) error {
			truncSize = size
			require.True(t, ne.IsEmpty())
			require.True(t, td.IsEmpty())
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(s), "updateMetaInfo",
		func(_ *ECStreamer, _ *uint64) error { return nil })

	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), 263, 150, "/shrink-size-only")
	s.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, uint64(150), truncSize)
}

func TestECStreamer_truncateV2_same_size_not_dirty(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(265, nil, w)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	var truncCalls int
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 64, nil, nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, _, _ proto.ObjExtentKey) error {
			truncCalls++
			return nil
		})

	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), 265, 64, "/same")
	s.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, 0, truncCalls)
}

func TestECStreamer_truncateV2_shrink_truncateV2_error(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(266, nil, w)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 80, nil, nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, _, _ proto.ObjExtentKey) error {
			return errors.New("truncate failed")
		})

	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), 266, 50, "/shrink-err")
	s.mu.Unlock()
	require.Error(t, err)
	require.Contains(t, err.Error(), "truncate failed")
}

func TestECStreamer_truncateV2_shrink_invalid_empty_delta(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(264, nil, w)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 200, nil, []proto.ObjExtentKey{{FileOffset: 0, Size: 200}}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(w), "TruncateV2FromExtents",
		func(_ *Writer, _ context.Context, _, _ uint64, _ []proto.ObjExtentKey) (proto.ObjExtentKey, proto.ObjExtentKey, error) {
			return proto.ObjExtentKey{}, proto.ObjExtentKey{}, nil
		})

	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), 264, 100, "/shrink-bad-delta")
	s.mu.Unlock()
	require.Error(t, err)
	require.Contains(t, err.Error(), "newObjExtent is not empty or targetSize is wrong")
}

func TestECStreamer_updateMetaInfo_getExtents_err(t *testing.T) {
	s := mustTestECStreamer(27, nil, nil)
	s.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 0, 0, nil, nil, errors.New("meta down")
		})
	err := s.updateMetaInfo(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "meta down")
}

func TestECStreamer_truncateV2_ENOENT_error_string_match(t *testing.T) {
	w := &Writer{}
	args := ECStreamOpenArgs{Ino: 28, VolName: "v", BlockSize: 8 << 20, Mw: &meta.MetaWrapper{}}
	s, err := NewECStreamer(args, nil, w)
	require.NoError(t, err)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	var getCalls int
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			getCalls++
			if getCalls == 1 {
				return 0, 0, nil, nil, errors.New(syscall.ENOENT.Error())
			}
			return 1, 1, nil, nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, _ proto.ObjExtentKey, _ proto.ObjExtentKey) error {
			return nil
		})
	s.mu.Lock()
	err = s.truncateV2Locked(context.Background(), 28, 1, "/p")
	s.mu.Unlock()
	require.NoError(t, err)
}

func TestECStreamer_invalidateReaderPrefetchBuf_and_nilReceiver(t *testing.T) {
	var nilS *ECStreamer
	nilS.invalidateReaderPrefetchBuf()

	r := &Reader{readBuf: make([]byte, 16)}
	s := mustTestECStreamer(62, r, nil)
	r.ecStreamer = s
	r.bufBaseOff = 0
	r.bufValidLen = 8
	s.invalidateReaderPrefetchBuf()
	require.Equal(t, 0, r.bufValidLen)

	s2 := mustTestECStreamer(63, nil, nil)
	s2.invalidateReaderPrefetchBuf()
}

func TestECStreamer_WriteFromReader_WriteWithoutPool_FlushWithoutPool(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(66, nil, w)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 0, nil, nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(w), "WriteFromReader",
		func(_ *Writer, _ context.Context, _ io.Reader, _ interface{}) (uint64, error) {
			return 7, nil
		})
	patches.ApplyMethod(reflect.TypeOf(w), "WriteWithoutPool",
		func(_ *Writer, _ context.Context, _ int, data []byte) (int, error) {
			return len(data), nil
		})
	patches.ApplyMethod(reflect.TypeOf(w), "FlushWithoutPool",
		func(_ *Writer, _ uint64, _ context.Context) error { return nil })

	n, err := s.WriteFromReader(context.Background(), bytes.NewReader([]byte("ab")), nil)
	require.NoError(t, err)
	require.Equal(t, uint64(7), n)

	wn, err := s.WriteWithoutPool(context.Background(), 0, []byte("cd"))
	require.NoError(t, err)
	require.Equal(t, 2, wn)

	require.NoError(t, s.FlushWithoutPool(66, context.Background()))
}
