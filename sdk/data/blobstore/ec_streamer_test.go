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
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/stretchr/testify/require"
)

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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Write",
		func(_ *Writer, _ context.Context, _ int, data []byte, _ int) (int, error) { return len(data), nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Flush", func(_ *Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(w), "HasDirtyBuffer", func(_ *Writer) bool { return false })
	patches.ApplyMethod(reflect.TypeOf(r), "LogicalReadBound", func(_ *Reader) (uint64, bool) { return 0, false })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(r), "Read",
		func(_ *Reader, _ context.Context, _ []byte, _ int, _ int) (int, error) { return 3, nil })
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "ensureAlignedForRead",
		func(_ *Reader, _, _ uint64, _ bool) error { return nil })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 0, errors.New("re") })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 2, nil })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
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

func TestECStreamer_ensureReadViewCurrentExternal(t *testing.T) {
	s := NewECStreamer(23, &Reader{valid: true}, nil)
	require.NoError(t, s.ensureReadViewCurrentExternal(context.Background()))
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

func TestECStreamer_syncEffectiveSize(t *testing.T) {
	s := NewECStreamer(32, nil, nil)
	s.syncEffectiveSize(123)
	require.Equal(t, uint64(123), atomic.LoadUint64(&s.fileSize))
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "ensureAlignedForRead",
		func(_ *Reader, _, _ uint64, _ bool) error { return errors.New("align") })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 0, errors.New("r") })
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Write",
		func(_ *Writer, _ context.Context, _ int, _ []byte, _ int) (int, error) { return 0, errors.New("w") })
	_, err := s.WriteWithOpts(context.Background(), 0, []byte("z"), 0, func() error { return nil }, 0, 0, false, false)
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
	patches.ApplyMethod(reflect.TypeOf(r), "RefreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyFunc(time.Since, func(_ time.Time) time.Duration { return ecSlowEnsureReadViewLogThreshold + time.Second })
	s.mu.Lock()
	_ = s.ensureReadViewCurrentLocked(context.Background())
	s.mu.Unlock()
}
