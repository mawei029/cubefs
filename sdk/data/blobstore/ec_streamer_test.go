package blobstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/meta"
)

// SeedLogicalViewForTest 仅供跨包单测注入 ECStreamer 上 logical 尾与 inoVersion（如 client/fs LTP 模拟）。
func SeedLogicalViewForTest(s *ECStreamer, fileSize, inoGen uint64) {
	if s == nil {
		return
	}
	atomic.StoreUint64(&s.fileSize, fileSize)
	atomic.StoreUint64(&s.inoVersion, inoGen)
}

func TestECStreamer_String_nil_receiver(t *testing.T) {
	var s *ECStreamer
	require.Equal(t, "ECStreamer{nil}", s.String())
}

func TestECStreamer_Cleanup_NewReader_NewWriter_Accessors(t *testing.T) {
	s := NewECStreamer(7, nil, nil)
	require.Equal(t, uint64(7), s.Inode())
	require.NoError(t, s.Open(true))
	require.NoError(t, s.Release())
	require.NoError(t, s.Evict())
	require.Equal(t, int32(0), s.RefCnt())

	cfg := ClientConfig{VolName: "v", VolType: 1, BlockSize: 4096, Ino: 7, Mw: &meta.MetaWrapper{}}
	s.NewReader(cfg)
	require.NotNil(t, s.Reader())
	s.NewWriter(cfg)
	require.NotNil(t, s.Writer())

	s.Cleanup()
	require.Nil(t, s.Reader())
	require.Nil(t, s.Writer())
}

func TestECStreamer_Flush_when_clean(t *testing.T) {
	s := NewECStreamer(8, &Reader{valid: true}, nil)
	atomic.StoreUint32(&s.dirty, 0)
	require.NoError(t, s.Flush(context.Background()))
}

func TestECStreamer_FlushAndFreeCache_with_writer(t *testing.T) {
	w := &Writer{ino: 9, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(9, &Reader{valid: true}, w)
	w.ecStreamer = s
	require.NoError(t, s.FlushAndFreeCache(context.Background()))
}

func TestECStreamer_Truncate_EBADF(t *testing.T) {
	s := NewECStreamer(10, nil, &Writer{})
	require.ErrorIs(t, s.Truncate(context.Background(), 100, "/p"), syscall.EBADF)
}

func TestECStreamer_WriteWithOpts_loadExtents_reader_nil(t *testing.T) {
	w := &Writer{ino: 11, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(11, nil, w)
	w.ecStreamer = s
	_, err := s.WriteWithOpts(context.Background(), 0, []byte("a"), 0, func() error { return nil }, 0, 0, false, false)
	require.ErrorIs(t, err, syscall.EBADF)
}

func TestECStreamer_WriteWithOpts_writer_write_err(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 12, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(12, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Write",
		func(_ *Writer, _ context.Context, _ int, _ []byte, _ int) (int, error) {
			return 0, errors.New("boom")
		})
	_, err := s.WriteWithOpts(context.Background(), 0, []byte("x"), 0, func() error { return nil }, 0, 0, false, false)
	require.Error(t, err)
}

func TestECStreamer_WriteWithOpts_waitForFlush_err(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 13, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(13, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	atomic.StoreUint32(&s.dirty, 1)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Write",
		func(_ *Writer, _ context.Context, _ int, data []byte, _ int) (int, error) {
			return len(data), nil
		})
	patches.ApplyMethod(reflect.TypeOf(w), "Flush",
		func(_ *Writer, _ uint64, _ context.Context) error { return errors.New("flush err") })
	_, err := s.WriteWithOpts(context.Background(), 0, []byte("ab"), 0, func() error { return nil }, 0, 0, false, true)
	require.Error(t, err)
}

func TestECStreamer_Write_delegates_WriteWithOpts(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 80, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(80, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Write",
		func(_ *Writer, _ context.Context, _ int, data []byte, _ int) (int, error) { return len(data), nil })
	n, err := s.Write(context.Background(), 0, []byte("hi"), 0, nil, 0, false)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

func TestECStreamer_WriteWithOpts_writer_nil_EBADF(t *testing.T) {
	r := &Reader{valid: true}
	s := NewECStreamer(81, r, nil)
	r.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	_, err := s.WriteWithOpts(context.Background(), 0, []byte("x"), 0, func() error { return nil }, 0, 0, false, false)
	require.ErrorIs(t, err, syscall.EBADF)
}

func TestECStreamer_WriteWithOpts_waitForFlush_ok(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 82, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(82, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	atomic.StoreUint32(&s.dirty, 1)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Write",
		func(_ *Writer, _ context.Context, _ int, data []byte, _ int) (int, error) { return len(data), nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(w), "HasDirtyBuffer", func(_ *Writer) bool { return false })
	n, err := s.WriteWithOpts(context.Background(), 0, []byte("ab"), 0, func() error { return nil }, 0, 0, false, true)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

func TestECStreamer_Truncate_public_equal_size(t *testing.T) {
	mw := &meta.MetaWrapper{}
	w := &Writer{ino: 83, mw: mw}
	s := NewECStreamer(83, &Reader{valid: true}, w)
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 50, nil, nil, nil
		})
	require.NoError(t, s.Truncate(context.Background(), 50, "/p"))
}

func TestECStreamer_truncateV2Locked_mw_nil_EBADF(t *testing.T) {
	w := &Writer{ino: 84, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(84, &Reader{valid: true}, w)
	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), nil, 84, 10, "/p", w)
	s.mu.Unlock()
	require.ErrorIs(t, err, syscall.EBADF)
}

func TestECStreamer_truncateV2Locked_writer_nil_EBADF(t *testing.T) {
	mw := &meta.MetaWrapper{}
	s := NewECStreamer(85, &Reader{valid: true}, nil)
	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), mw, 85, 10, "/p", nil)
	s.mu.Unlock()
	require.ErrorIs(t, err, syscall.EBADF)
}

func TestECStreamer_truncateV2Locked_ENOENT_truncateV2_err(t *testing.T) {
	mw := &meta.MetaWrapper{}
	w := &Writer{ino: 86, mw: mw}
	s := NewECStreamer(86, &Reader{valid: true}, w)
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 0, 0, nil, nil, syscall.ENOENT
		})
	patches.ApplyMethod(reflect.TypeOf(mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, _ []proto.ObjExtentKey, _ []proto.ObjExtentKey) error {
			return errors.New("truncate failed")
		})
	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), mw, 86, 10, "/p", w)
	s.mu.Unlock()
	require.Error(t, err)
}

func TestECStreamer_ensureReadViewCurrentLocked_reader_nil_EBADF(t *testing.T) {
	w := &Writer{ino: 87, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(87, nil, w)
	w.ecStreamer = s
	atomic.StoreUint32(&s.dirty, 1)
	s.mu.Lock()
	err := s.ensureReadViewCurrentLocked(context.Background())
	s.mu.Unlock()
	require.ErrorIs(t, err, syscall.EBADF)
}

func TestECStreamer_mergeInodeGen_mergeOpenSnapshot_noteOpenAccessMode(t *testing.T) {
	s := NewECStreamer(14, nil, nil)
	s.mergeInodeGen(3)
	s.mergeInodeGen(1)
	require.Equal(t, uint64(3), atomic.LoadUint64(&s.inoVersion))
	s.mergeOpenSnapshot(500, 9)
	require.GreaterOrEqual(t, atomic.LoadUint64(&s.fileSize), uint64(500))
	s.noteOpenAccessMode(uint32(syscall.O_RDWR))
	require.Equal(t, uint32(0), atomic.LoadUint32(&s.rdonly))
	// O_RDONLY 分支：若当前已是 rdonly=0，则不会强行置 1（与 noteOpenAccessMode 实现一致）
	s.noteOpenAccessMode(uint32(syscall.O_RDONLY))
	require.Equal(t, uint32(0), atomic.LoadUint32(&s.rdonly))
}

func TestECStreamer_noteWriteFinished_noteWriterFlushCommitted(t *testing.T) {
	s := NewECStreamer(15, nil, nil)
	s.noteWriteFinished(777)
	require.Equal(t, uint64(777), atomic.LoadUint64(&s.fileSize))
	require.NotEqual(t, uint32(0), atomic.LoadUint32(&s.dirty))
	s.noteWriterFlushCommitted(888)
	require.Equal(t, uint64(888), atomic.LoadUint64(&s.fileSize))
	require.Equal(t, uint32(0), atomic.LoadUint32(&s.dirty))

	// 覆盖写抬尾：不得因 logicalEnd 小于原尾而缩小 fileSize（tryOverWrite / flushExt 路径）
	atomic.StoreUint64(&s.fileSize, 1<<20)
	s.noteWriterFlushCommitted(100)
	require.Equal(t, uint64(1<<20), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_readAfterFlush_zero_size(t *testing.T) {
	r := &Reader{valid: true}
	s := NewECStreamer(16, r, nil)
	r.ecStreamer = s
	n, err := s.readAfterFlush(context.Background(), []byte{1}, 0, 0, 0, false, false, 0, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestECStreamer_readAfterFlush_align_inode_path(t *testing.T) {
	r := &Reader{valid: true}
	s := NewECStreamer(17, r, nil)
	r.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(r), "Read",
		func(_ *Reader, _ context.Context, _ []byte, _ int, _ int) (int, error) { return 3, nil })
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "ensureAlignedForReadUnderStreamerLock",
		func(_ *Reader, _ *ECStreamer, _ bool) error { return nil })
	n, err := s.readAfterFlush(context.Background(), make([]byte, 8), 0, 4, 0, false, true, 1, 100)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}

func TestECStreamer_readAfterFlush_objExtents_err(t *testing.T) {
	r := &Reader{valid: true}
	s := NewECStreamer(18, r, nil)
	r.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 0, errors.New("re") })
	_, err := s.readAfterFlush(context.Background(), make([]byte, 4), 0, 2, 0, false, false, 0, 0)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "get extents err"))
}

func TestECStreamer_readAfterFlush_reader_nil(t *testing.T) {
	s := NewECStreamer(19, nil, nil)
	atomic.StoreUint32(&s.rdonly, 1)
	_, err := s.readAfterFlush(context.Background(), make([]byte, 2), 0, 1, 0, false, false, 0, 0)
	require.ErrorIs(t, err, syscall.EBADF)
}

func TestECStreamer_ensureReadViewCurrentLocked_dirty_flush_path(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 20, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(20, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	atomic.StoreUint32(&s.dirty, 1)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(w), "HasDirtyBuffer", func(_ *Writer) bool { return false })
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 2, nil })
	s.mu.Lock()
	err := s.ensureReadViewCurrentLocked(context.Background())
	s.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, uint32(0), atomic.LoadUint32(&s.dirty))
}

func TestECStreamer_ensureReadViewCurrentLocked_ctx_cancel_dirty_loop(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 21, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(21, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	atomic.StoreUint32(&s.dirty, 1)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(w), "HasDirtyBuffer", func(_ *Writer) bool { return true })
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.mu.Lock()
	err := s.ensureReadViewCurrentLocked(ctx)
	s.mu.Unlock()
	require.ErrorIs(t, err, context.Canceled)
}

func TestECStreamer_ensureReadViewCurrentLocked_reader_nil(t *testing.T) {
	w := &Writer{ino: 22, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(22, nil, w)
	w.ecStreamer = s
	atomic.StoreUint32(&s.dirty, 1)
	s.mu.Lock()
	err := s.ensureReadViewCurrentLocked(context.Background())
	s.mu.Unlock()
	require.ErrorIs(t, err, syscall.EBADF)
}

func TestECStreamer_syncReadView(t *testing.T) {
	s := NewECStreamer(23, &Reader{valid: true}, nil)
	require.NoError(t, s.syncReadView(context.Background()))
}

func TestECStreamer_hasReaderForViewSync(t *testing.T) {
	s := NewECStreamer(24, nil, nil)
	require.False(t, s.hasReaderForViewSync())
	s = NewECStreamer(24, &Reader{valid: true}, nil)
	require.True(t, s.hasReaderForViewSync())
}

func TestECStreamer_lazyInitReaderWriter(t *testing.T) {
	s := NewECStreamer(25, nil, nil)
	cfg := ClientConfig{VolName: "v", VolType: 1, BlockSize: 4096, Ino: 25, Mw: &meta.MetaWrapper{}}
	s.mu.Lock()
	s.lazyInitReaderWriter(cfg)
	s.mu.Unlock()
	require.NotNil(t, s.Reader())
	require.NotNil(t, s.Writer())
}

func TestECStreamer_truncateV2Locked_ENOENT_path(t *testing.T) {
	mw := &meta.MetaWrapper{}
	w := &Writer{ino: 26, mw: mw}
	s := NewECStreamer(26, &Reader{valid: true}, w)
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 0, 0, nil, nil, syscall.ENOENT
		})
	var truncTo uint64
	patches.ApplyMethod(reflect.TypeOf(mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, size uint64, _ string, _ []proto.ObjExtentKey, _ []proto.ObjExtentKey) error {
			truncTo = size
			return nil
		})
	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), mw, 26, 64, "/p", w)
	s.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, uint64(64), truncTo)
}

func TestECStreamer_truncateV2Locked_equal_size_noop(t *testing.T) {
	mw := &meta.MetaWrapper{}
	w := &Writer{ino: 27, mw: mw}
	s := NewECStreamer(27, &Reader{valid: true}, w)
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 100, nil, nil, nil
		})
	var truncCalls int
	patches.ApplyMethod(reflect.TypeOf(mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, _ []proto.ObjExtentKey, _ []proto.ObjExtentKey) error {
			truncCalls++
			return nil
		})
	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), mw, 27, 100, "/p", w)
	s.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, 0, truncCalls)
}

func TestECStreamer_truncateV2Locked_expand(t *testing.T) {
	mw := &meta.MetaWrapper{}
	w := &Writer{ino: 28, mw: mw}
	s := NewECStreamer(28, &Reader{valid: true}, w)
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 50, nil, []proto.ObjExtentKey{{FileOffset: 0, Size: 50}}, nil
		})
	var gotSize uint64
	patches.ApplyMethod(reflect.TypeOf(mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, size uint64, _ string, _ []proto.ObjExtentKey, _ []proto.ObjExtentKey) error {
			gotSize = size
			return nil
		})
	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), mw, 28, 200, "/p", w)
	s.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, uint64(200), gotSize)
}

func TestECStreamer_truncateV2Locked_shrink(t *testing.T) {
	mw := &meta.MetaWrapper{}
	w := &Writer{ino: 29, mw: mw}
	s := NewECStreamer(29, &Reader{valid: true}, w)
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	eks := []proto.ObjExtentKey{{FileOffset: 0, Size: 200}}
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 200, nil, eks, nil
		})
	patches.ApplyMethod(reflect.TypeOf(w), "TruncateV2FromExtents",
		func(_ *Writer, _ context.Context, _ uint64, _ uint64, _ []proto.ObjExtentKey) ([]proto.ObjExtentKey, []proto.ObjExtentKey, error) {
			return []proto.ObjExtentKey{{FileOffset: 0, Size: 80}}, nil, nil
		})
	var gotSize uint64
	patches.ApplyMethod(reflect.TypeOf(mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, size uint64, _ string, _ []proto.ObjExtentKey, _ []proto.ObjExtentKey) error {
			gotSize = size
			return nil
		})
	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), mw, 29, 80, "/p", w)
	s.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, uint64(80), gotSize)
}

func TestECStreamer_truncateV2Locked_GetObjExtents_generic_err(t *testing.T) {
	mw := &meta.MetaWrapper{}
	w := &Writer{ino: 30, mw: mw}
	s := NewECStreamer(30, &Reader{valid: true}, w)
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 0, 0, nil, nil, errors.New("meta down")
		})
	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), mw, 30, 10, "/p", w)
	s.mu.Unlock()
	require.Error(t, err)
}

func TestECStreamer_closeReaderWriterLocked(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 31, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(31, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Close", func(_ *Writer, _ context.Context) {})
	patches.ApplyMethod(reflect.TypeOf(r), "Close", func(_ *Reader, _ context.Context) {})
	s.mu.Lock()
	err := s.closeReaderWriterLocked(31, context.Background())
	s.mu.Unlock()
	require.NoError(t, err)
	require.Nil(t, s.Reader())
	require.Nil(t, s.Writer())
}

func TestECStreamer_commitLogicalSize(t *testing.T) {
	s := NewECStreamer(32, nil, nil)
	s.markDirty()
	s.commitLogicalSize(123)
	require.Equal(t, uint64(123), atomic.LoadUint64(&s.fileSize))
	require.Equal(t, uint32(0), atomic.LoadUint32(&s.dirty))
}

// TestECStreamer_EffectiveLogicalSize_truncateOscillation 模拟缩→扩→缩→扩与未下刷写，任意时刻有效尾应可查询。
func TestECStreamer_EffectiveLogicalSize_truncateOscillation(t *testing.T) {
	w := &Writer{}
	s := NewECStreamer(99, nil, w)
	w.ecStreamer = s
	atomic.StoreUint64(&w.fileSize, 16<<20)

	s.commitLogicalSize(4 << 20)
	require.Equal(t, uint64(4<<20), s.EffectiveLogicalSize())

	atomic.StoreUint64(&w.fileSize, 12<<20)
	s.noteWriteFinished(uint64(w.CacheFileSize()))
	require.Equal(t, uint64(12<<20), s.EffectiveLogicalSize())

	s.commitLogicalSize(6 << 20)
	atomic.StoreUint64(&w.fileSize, 6<<20)
	require.Equal(t, uint64(6<<20), s.EffectiveLogicalSize())

	atomic.StoreUint64(&w.fileSize, 10<<20)
	s.raiseLogicalSize(uint64(w.CacheFileSize()))
	require.Equal(t, uint64(10<<20), s.EffectiveLogicalSize())
}

func TestECStreamer_readAfterFlush_ensureReadView_err(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 41, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(41, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	atomic.StoreUint32(&s.rdonly, 1)
	atomic.StoreUint32(&s.dirty, 1)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return errors.New("e") })
	_, err := s.readAfterFlush(context.Background(), make([]byte, 2), 0, 1, 0, false, false, 0, 0)
	require.Error(t, err)
}

func TestECStreamer_readAfterFlush_ensureAligned_err(t *testing.T) {
	r := &Reader{valid: true}
	s := NewECStreamer(42, r, nil)
	r.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "ensureAlignedForReadUnderStreamerLock",
		func(_ *Reader, _ *ECStreamer, _ bool) error { return errors.New("align") })
	_, err := s.readAfterFlush(context.Background(), make([]byte, 2), 0, 1, 0, false, true, 1, 10)
	require.Error(t, err)
}

func TestECStreamer_ensureReadViewCurrentLocked_max_loops(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 43, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(43, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	atomic.StoreUint32(&s.dirty, 1)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(w), "HasDirtyBuffer", func(_ *Writer) bool { return true })
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	ctx := context.Background()
	s.mu.Lock()
	err := s.ensureReadViewCurrentLocked(ctx)
	s.mu.Unlock()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "exceeded"))
}

func TestECStreamer_FlushAndFreeCache_flush_err(t *testing.T) {
	s := NewECStreamer(51, &Reader{valid: true}, &Writer{ino: 51, mw: &meta.MetaWrapper{}})
	s.Writer().ecStreamer = s
	atomic.StoreUint32(&s.dirty, 1)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "Flush", func(_ *ECStreamer, _ context.Context) error { return errors.New("f") })
	require.Error(t, s.FlushAndFreeCache(context.Background()))
}

func TestECStreamer_closeReaderWriterLocked_flush_err(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 52, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(52, r, w)
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return errors.New("fl") })
	s.mu.Lock()
	err := s.closeReaderWriterLocked(52, context.Background())
	s.mu.Unlock()
	require.Error(t, err)
}

func TestECStreamer_loadObjExtents_refresh_err(t *testing.T) {
	r := &Reader{valid: true}
	s := NewECStreamer(53, r, nil)
	r.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 0, errors.New("r") })
	s.mu.Lock()
	s.loadObjExtentsFromMetaLocked()
	err := s.objExtentsOnceErr
	s.mu.Unlock()
	require.Error(t, err)
}

func TestECStreamer_writeOnce_err_after_once(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 55, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(55, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Write",
		func(_ *Writer, _ context.Context, _ int, _ []byte, _ int) (int, error) { return 0, errors.New("w") })
	_, err := s.WriteWithOpts(context.Background(), 0, []byte("z"), 0, func() error { return nil }, 0, 0, false, false)
	require.Error(t, err)
}

func TestECStreamer_invalidateReaderPrefetchBuf_and_nilReceiver(t *testing.T) {
	var nilS *ECStreamer
	nilS.invalidateReaderPrefetchBuf()

	r := &Reader{readBuf: make([]byte, 16)}
	s := NewECStreamer(62, r, nil)
	r.ecStreamer = s
	r.bufBaseOff = 0
	r.bufValidLen = 8
	s.invalidateReaderPrefetchBuf()
	require.Equal(t, 0, r.bufValidLen)

	s2 := NewECStreamer(63, nil, nil)
	s2.invalidateReaderPrefetchBuf()
}

func TestECStreamer_syncLogicalSizeFromInode_shrinkAndGrow(t *testing.T) {
	s := NewECStreamer(64, nil, nil)
	atomic.StoreUint64(&s.fileSize, 1000)
	s.syncLogicalSizeFromInode(500)
	require.Equal(t, uint64(500), atomic.LoadUint64(&s.fileSize))
	s.syncLogicalSizeFromInode(800)
	require.Equal(t, uint64(800), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_mergeMaxFileSize_nilReceiver(t *testing.T) {
	var s *ECStreamer
	s.mergeMaxFileSize(100)
}

func TestMergeFstatLogicalSize_replicaAlignedCases(t *testing.T) {
	const inodeGen, streamGen = uint64(10), uint64(12)
	require.Equal(t, uint64(500), MergeFstatLogicalSize(500, streamGen, inodeGen, 100))
	require.Equal(t, uint64(100), MergeFstatLogicalSize(500, 8, inodeGen, 100))
	require.Equal(t, uint64(500), MergeFstatLogicalSize(400, inodeGen, inodeGen, 500))
	require.Equal(t, uint64(0xf4000), MergeFstatLogicalSize(0xf3800, inodeGen, inodeGen, 0xf4000))
}

func TestECStreamer_FstatLogicalSize_truncCommitSize_metaLagOneChunk(t *testing.T) {
	// LTP ftest01：expand-truncate 后 meta inode.Size 可能滞后 1×csize，truncCommitSize 仍应抬高 fstat。
	s := NewECStreamer(711, nil, nil)
	s.commitLogicalSize(0x100000)
	require.Equal(t, uint64(0x100000), s.FstatLogicalSize(10, 0xFF800))
}

func TestECStreamer_FstatLogicalSize_and_syncSizeFromExtentMeta_expandLag(t *testing.T) {
	s := NewECStreamer(710, nil, nil)
	s.commitLogicalSize(0xf4000)
	require.Equal(t, uint64(0xf4000), s.FstatLogicalSize(12, 0xf4000))
	s.syncSizeFromExtentMeta(12, 0xf3800, nil, false)
	require.Equal(t, uint64(0xf4000), atomic.LoadUint64(&s.fileSize))
	require.Equal(t, uint64(0xf4000), s.FstatLogicalSize(12, 0xf4000))
	// shrink：截断 commit 后允许 meta/extent 快照压低
	s.commitLogicalSize(0xf2000)
	require.Equal(t, uint64(0xf2000), atomic.LoadUint64(&s.fileSize))
	s.syncSizeFromExtentMeta(13, 0xf2000, nil, false)
	require.Equal(t, uint64(0xf2000), atomic.LoadUint64(&s.fileSize))
}

func TestECStreamer_syncReaderViewAfterMetaChange_mergeInodeOnlyNoRefreshExtents(t *testing.T) {
	mw := &meta.MetaWrapper{}
	r := &Reader{ino: 702, mw: mw, valid: true}
	s := NewECStreamer(702, r, nil)
	r.ecStreamer = s
	s.commitLogicalSize(0xf4000)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	var refreshCalls int32
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents",
		func(_ *Reader) (uint64, error) {
			atomic.AddInt32(&refreshCalls, 1)
			return 0, errors.New("must not refresh after truncate commit")
		})
	patches.ApplyMethod(reflect.TypeOf(mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Generation: 11, Size: 0xf4000}, nil
		})
	require.NoError(t, s.syncReaderViewAfterMetaChange(context.Background()))
	require.Equal(t, int32(0), atomic.LoadInt32(&refreshCalls))
	require.Equal(t, uint64(0xf4000), s.EffectiveLogicalSize())
}

func TestECStreamer_syncSizeFromExtentMeta_staleGenMergeMaxOnly(t *testing.T) {
	s := NewECStreamer(65, nil, nil)
	atomic.StoreUint64(&s.inoVersion, 10)
	atomic.StoreUint64(&s.fileSize, 50)
	s.syncSizeFromExtentMeta(5, 200, nil, false)
	require.Equal(t, uint64(200), atomic.LoadUint64(&s.fileSize))
	require.Equal(t, uint64(10), atomic.LoadUint64(&s.inoVersion))
}

func TestECStreamer_committedLogicalSize_and_FileSizeView_nil(t *testing.T) {
	var s *ECStreamer
	require.Equal(t, uint64(0), s.committedLogicalSize())
	sz, gen := s.FileSizeView()
	require.Equal(t, 0, sz)
	require.Equal(t, uint64(0), gen)
}

func TestECStreamer_syncReaderViewAfterMetaChange(t *testing.T) {
	mw := &meta.MetaWrapper{}
	r := &Reader{ino: 66, mw: mw, valid: true}
	s := NewECStreamer(66, r, nil)
	r.ecStreamer = s
	atomic.StoreUint64(&s.fileSize, 100)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Generation: 3, Size: 4096}, nil
		})
	require.NoError(t, s.syncReaderViewAfterMetaChange(nil))
	require.Equal(t, uint64(3), atomic.LoadUint64(&s.inoVersion))
	require.Equal(t, uint64(4096), atomic.LoadUint64(&s.fileSize))

	s2 := NewECStreamer(67, nil, nil)
	require.NoError(t, s2.syncReaderViewAfterMetaChange(context.Background()))

	r2 := &Reader{ino: 68, mw: nil, valid: true}
	s3 := NewECStreamer(68, r2, nil)
	r2.ecStreamer = s3
	require.NoError(t, s3.syncReaderViewAfterMetaChange(context.Background()))
}

func TestECStreamer_truncateV2Locked_equalSize_refreshExtents_err(t *testing.T) {
	mw := &meta.MetaWrapper{}
	w := &Writer{ino: 69, mw: mw}
	r := &Reader{valid: true, ino: 69, mw: mw}
	s := NewECStreamer(69, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 40, nil, nil, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents",
		func(_ *Reader) (uint64, error) { return 0, errors.New("refresh fail") })
	s.mu.Lock()
	err := s.truncateV2Locked(context.Background(), mw, 69, 40, "/p", w)
	s.mu.Unlock()
	require.Error(t, err)
}

func TestECStreamer_slow_ensureReadView_log_threshold(t *testing.T) {
	r := &Reader{valid: true}
	w := &Writer{ino: 61, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(61, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	atomic.StoreUint32(&s.dirty, 1)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(w), "HasDirtyBuffer", func(_ *Writer) bool { return false })
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyFunc(time.Since, func(_ time.Time) time.Duration { return ecSlowEnsureReadViewLogThreshold + time.Second })
	s.mu.Lock()
	_ = s.ensureReadViewCurrentLocked(context.Background())
	s.mu.Unlock()
}

// patchWriterAccessorCounter 将 ECStreamer.Writer 替换为计数器；持 s.mu 时若误调 Writer() 会与同 goroutine 自死锁。
// 回归：commitLogicalSize / readAfterFlush→fileSize / ensureAlignedForReadUnderStreamerLock 不得重入 Writer()。
func patchWriterAccessorCounter(patches *gomonkey.Patches, calls *int32) {
	patches.ApplyMethod(reflect.TypeOf((*ECStreamer)(nil)), "Writer",
		func(es *ECStreamer) *Writer {
			atomic.AddInt32(calls, 1)
			es.mu.Lock()
			defer es.mu.Unlock()
			return es.fWriter
		})
}

func setupDeadlockRegressionStreamer(t *testing.T, ino uint64, logicalSize, writerCache uint64) (
	*ECStreamer, *Reader, *Writer, *meta.MetaWrapper, *gomonkey.Patches,
) {
	t.Helper()
	mw := &meta.MetaWrapper{}
	r := &Reader{valid: true, ino: ino, mw: mw}
	w := &Writer{ino: ino, mw: mw}
	s := NewECStreamer(ino, r, w)
	r.ecStreamer = s
	w.ecStreamer = s
	atomic.StoreUint64(&s.fileSize, logicalSize)
	if writerCache > 0 {
		atomic.StoreUint64(&w.fileSize, writerCache)
	}
	atomic.StoreUint32(&s.rdonly, 0)
	atomic.StoreUint32(&s.dirty, 0)

	patches := gomonkey.NewPatches()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents",
		func(reader *Reader) (uint64, error) {
			reader.Lock()
			reader.valid = true
			reader.metaReportedSize = logicalSize
			reader.extentEpochSeen = atomic.LoadUint32(&reader.ecStreamer.extentMetaEpoch)
			reader.Unlock()
			return 1, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "readEbsRange",
		func(_ *Reader, _ context.Context, _ int, size uint32) ([]byte, error) {
			return make([]byte, size), nil
		})
	return s, r, w, mw, patches
}

func runWithTimeout(t *testing.T, timeout time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("operation did not finish within timeout (possible s.mu reentrant deadlock)")
	}
}

// TestECStreamer_commitLogicalSize_underMu_noWriterReentry 场景1：truncateV2Locked 持锁 commitLogicalSize 不得调 Writer()。
func TestECStreamer_commitLogicalSize_underMu_noWriterReentry(t *testing.T) {
	w := &Writer{ino: 901, mw: &meta.MetaWrapper{}}
	s := NewECStreamer(901, &Reader{valid: true}, w)
	w.ecStreamer = s
	atomic.StoreUint64(&w.fileSize, 200)

	var calls int32
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchWriterAccessorCounter(patches, &calls)

	s.mu.Lock()
	s.commitLogicalSize(150)
	s.mu.Unlock()

	require.Equal(t, uint64(150), atomic.LoadUint64(&s.fileSize))
	require.Equal(t, uint64(150), atomic.LoadUint64(&w.fileSize))
	require.Equal(t, int32(0), atomic.LoadInt32(&calls), "commitLogicalSize must use s.fWriter, not Writer()")
}

// TestECStreamer_truncateV2Locked_equalSize_underMu_noWriterReentry 场景1：等长截断在已持 s.mu 下完成且不抢 Writer()。
func TestECStreamer_truncateV2Locked_equalSize_underMu_noWriterReentry(t *testing.T) {
	mw := &meta.MetaWrapper{}
	s, _, w, _, patches := setupDeadlockRegressionStreamer(t, 902, 50, 0)
	defer patches.Reset()

	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 50, nil, nil, nil
		})

	var calls int32
	patchWriterAccessorCounter(patches, &calls)

	runWithTimeout(t, 2*time.Second, func() {
		s.mu.Lock()
		err := s.truncateV2Locked(context.Background(), mw, 902, 50, "/p", w)
		s.mu.Unlock()
		require.NoError(t, err)
	})
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))
}

// TestECStreamer_Truncate_equalSize_noWriterReentry 场景1：公开 Truncate 等长路径（内部持 s.mu）同样不得重入 Writer()。
func TestECStreamer_Truncate_equalSize_noWriterReentry(t *testing.T) {
	mw := &meta.MetaWrapper{}
	s, _, w, _, patches := setupDeadlockRegressionStreamer(t, 903, 80, 0)
	defer patches.Reset()

	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 80, nil, nil, nil
		})

	var calls int32
	patchWriterAccessorCounter(patches, &calls)

	runWithTimeout(t, 2*time.Second, func() {
		require.NoError(t, s.Truncate(context.Background(), 80, "/p"))
	})
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))
}

// TestECStreamer_readAfterFlush_realRead_noWriterReentry 场景2：readAfterFlush→Reader.Read→fileSize 持锁路径不得经 Writer()。
func TestECStreamer_readAfterFlush_realRead_noWriterReentry(t *testing.T) {
	s, _, _, _, patches := setupDeadlockRegressionStreamer(t, 904, 64, 128)
	defer patches.Reset()

	var calls int32
	patchWriterAccessorCounter(patches, &calls)

	buf := make([]byte, 4)
	runWithTimeout(t, 2*time.Second, func() {
		n, err := s.readAfterFlush(context.Background(), buf, 0, 4, 0, false, false, 0, 0)
		require.NoError(t, err)
		require.Equal(t, len(buf), n)
	})
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))
	require.Equal(t, uint64(128), s.effectiveLogicalSizePeek())
}

// TestECStreamer_readerFileSize_underMu_noWriterReentry 场景2：持 s.mu 调用 Reader.fileSize 与 readAfterFlush 内路径一致。
func TestECStreamer_readerFileSize_underMu_noWriterReentry(t *testing.T) {
	s, r, _, _, patches := setupDeadlockRegressionStreamer(t, 905, 40, 100)
	defer patches.Reset()

	var calls int32
	patchWriterAccessorCounter(patches, &calls)

	s.mu.Lock()
	sz, ok := r.fileSize()
	s.mu.Unlock()

	require.True(t, ok)
	require.Equal(t, uint64(100), sz)
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))
}

// TestECStreamer_EffectiveLogicalSize_noWriterAccessor 场景2：有效尾查询应走 effectiveLogicalSizePeek，不抢 s.mu。
func TestECStreamer_EffectiveLogicalSize_noWriterAccessor(t *testing.T) {
	s, _, w, _, patches := setupDeadlockRegressionStreamer(t, 906, 10, 99)
	defer patches.Reset()

	var calls int32
	patchWriterAccessorCounter(patches, &calls)

	require.Equal(t, uint64(99), s.EffectiveLogicalSize())
	size, gen := s.FileSizeView()
	require.Equal(t, 99, size)
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))
	_ = gen
	_ = w
}

// TestECStreamer_ensureAlignedUnderStreamerLock_noWriterReentry 场景3：readAfterFlush 对齐路径不得调 Writer()/syncReadView。
func TestECStreamer_ensureAlignedUnderStreamerLock_noWriterReentry(t *testing.T) {
	s, r, _, _, patches := setupDeadlockRegressionStreamer(t, 907, 200, 0)
	defer patches.Reset()

	atomic.AddUint32(&s.extentMetaEpoch, 1)
	r.extentEpochSeen = 0

	var calls int32
	patchWriterAccessorCounter(patches, &calls)

	runWithTimeout(t, 2*time.Second, func() {
		s.mu.Lock()
		err := r.ensureAlignedForReadUnderStreamerLock(s, true)
		s.mu.Unlock()
		require.NoError(t, err)
	})
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))
}

// TestECStreamer_readAfterFlush_alignInode_noWriterReentry 场景3：readAfterFlush(alignInode=true) 端到端不卡死、不重入 Writer()。
func TestECStreamer_readAfterFlush_alignInode_noWriterReentry(t *testing.T) {
	s, _, _, _, patches := setupDeadlockRegressionStreamer(t, 908, 32, 0)
	defer patches.Reset()

	atomic.AddUint32(&s.extentMetaEpoch, 1)
	s.fReader.extentEpochSeen = 0

	var calls int32
	patchWriterAccessorCounter(patches, &calls)

	buf := make([]byte, 4)
	runWithTimeout(t, 2*time.Second, func() {
		n, err := s.readAfterFlush(context.Background(), buf, 0, 4, 0, false, true, 0, 0)
		require.NoError(t, err)
		require.Equal(t, 4, n)
	})
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))
}
