package blobstore

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/sdk/data/manager"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/log"
	"github.com/stretchr/testify/require"
)

func TestNewObjExtentClient_WithSharedLimitManager(t *testing.T) {
	lm := manager.NewLimitManager(nil)
	c := NewObjExtentClient(ObjExtentConfig{LimitManager: lm})
	require.NotNil(t, c)
	require.Equal(t, lm, c.LimitManager)
}

func TestNewObjExtentClient_default_limit_manager(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.NotNil(t, c.LimitManager)
}

func TestECExtentClient_SetStreamer_nil_client_noop(t *testing.T) {
	var c *ECExtentClient
	s := NewECStreamer(1, nil, nil)
	c.SetStreamer(1, s)
}

func TestECExtentClient_OpenStream_ENOTSUP(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.ErrorIs(t, c.OpenStream(1, true, false, "/x"), syscall.ENOTSUP)
}

func TestECExtentClient_OpenStream_debug_branch(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(log.EnableDebug, func() bool { return true })
	c := NewObjExtentClient(ObjExtentConfig{})
	require.ErrorIs(t, c.OpenStream(91, false, false, "/x"), syscall.ENOTSUP)
}

func TestECExtentClient_CloseStream_debug_ref_positive(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(log.EnableDebug, func() bool { return true })
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(92, &Reader{valid: true}, &Writer{ino: 92, mw: &meta.MetaWrapper{}})
	atomic.StoreUint32(&s.rdonly, 0)
	atomic.StoreInt32(&s.refCnt, 2)
	c.SetStreamer(92, s)
	require.NoError(t, c.CloseStream(92))
	require.Equal(t, int32(1), c.RefCnt(92))
}

func TestECExtentClient_EvictStream_debug_log(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(log.EnableDebug, func() bool { return true })
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(93, &Reader{valid: true}, nil)
	c.SetStreamer(93, s)
	atomic.StoreInt32(&s.refCnt, 0)
	patches.ApplyPrivateMethod(reflect.TypeOf((*ECStreamer)(nil)), "closeReaderWriterLocked",
		func(_ *ECStreamer, _ uint64, _ context.Context) error { return nil })
	require.NoError(t, c.EvictStream(93))
	require.Nil(t, c.GetStreamer(93))
}

func TestECExtentClient_SetStreamer_nil_noop(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	c.SetStreamer(1, nil)
}

func TestECExtentClient_ReadWriteFlush_Truncate_nil_stream(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	_, err := c.Read(9, make([]byte, 1), 0, 1, 0, false)
	require.ErrorIs(t, err, syscall.EBADF)
	_, err = c.Write(9, 0, []byte("x"), 0, nil, 0, 0, false, false)
	require.ErrorIs(t, err, syscall.EBADF)
	require.ErrorIs(t, c.Flush(9), syscall.EBADF)
	require.ErrorIs(t, c.Truncate(&meta.MetaWrapper{}, 1, 9, 10, "/p"), syscall.EBADF)
}

func TestECExtentClient_Truncate_EINVAL_nil_mw(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	c.SetStreamer(1, NewECStreamer(1, &Reader{valid: true}, &Writer{ino: 1, mw: &meta.MetaWrapper{}}))
	require.ErrorIs(t, c.Truncate(nil, 1, 1, 10, "/p"), syscall.EINVAL)
}

func TestECExtentClient_Truncate_EBADF_no_writer(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	c.SetStreamer(200, NewECStreamer(200, &Reader{valid: true}, nil))
	require.ErrorIs(t, c.Truncate(&meta.MetaWrapper{}, 1, 200, 10, "/p"), syscall.EBADF)
}

func TestECExtentClient_ReadWithInodeView_nil_ctx(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(2, &Reader{valid: true}, nil)
	c.SetStreamer(2, s)
	r := s.Reader()
	r.ecStreamer = s
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "refreshExtents", func(_ *Reader) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(r), "Read",
		func(_ *Reader, _ context.Context, buf []byte, _, _ int) (int, error) {
			if len(buf) > 0 {
				buf[0] = 'x'
			}
			return 1, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*Reader)(nil)), "ensureAlignedForRead",
		func(_ *Reader, _ bool) error { return nil })
	buf := make([]byte, 4)
	n, err := c.ReadWithInodeView(nil, 2, buf, 0, 1, 0, false, 1, 100)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestECExtentClient_NeedsReadViewSync(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.False(t, c.NeedsReadViewSync(3))
	s := NewECStreamer(3, &Reader{valid: true}, nil)
	c.SetStreamer(3, s)
	s.fReader.ecStreamer = s
	atomic.StoreUint32(&s.dirty, 1)
	require.True(t, c.NeedsReadViewSync(3))
}

func TestECExtentClient_CloseStream_negative_ref_reset(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(4, nil, nil)
	atomic.StoreUint32(&s.rdonly, 0)
	atomic.StoreInt32(&s.refCnt, 0)
	c.SetStreamer(4, s)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*ECStreamer)(nil)), "closeReaderWriterLocked",
		func(_ *ECStreamer, _ uint64, _ context.Context) error { return nil })
	s.mu.Lock()
	atomic.StoreInt32(&s.refCnt, -2)
	s.mu.Unlock()
	require.NoError(t, c.CloseStream(4))
	require.Equal(t, int32(0), atomic.LoadInt32(&s.refCnt))
}

func TestECExtentClient_CloseStream_teardown_err_rollback(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(5, &Reader{valid: true}, &Writer{ino: 5, mw: &meta.MetaWrapper{}})
	atomic.StoreUint32(&s.rdonly, 0)
	atomic.StoreInt32(&s.refCnt, 1)
	c.SetStreamer(5, s)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*ECStreamer)(nil)), "closeReaderWriterLocked",
		func(_ *ECStreamer, _ uint64, _ context.Context) error { return errors.New("teardown") })
	require.Error(t, c.CloseStream(5))
	require.Equal(t, int32(1), atomic.LoadInt32(&s.refCnt))
}

func TestECExtentClient_EvictStream_teardown_err(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(6, &Reader{valid: true}, &Writer{ino: 6, mw: &meta.MetaWrapper{}})
	c.SetStreamer(6, s)
	atomic.StoreInt32(&s.refCnt, 0)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*ECStreamer)(nil)), "closeReaderWriterLocked",
		func(_ *ECStreamer, _ uint64, _ context.Context) error { return errors.New("ev") })
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
	require.Equal(t, 500, sz)
	require.GreaterOrEqual(t, gen, uint64(9))
}

func TestECExtentClient_GetStreamer_Reader_Writer_missing_logs(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.Nil(t, c.GetStreamer(999))
	require.Nil(t, c.Reader(999))
	require.Nil(t, c.Writer(999))
	require.Equal(t, int32(0), c.RefCnt(999))
}

func TestECExtentClient_args_toClientConfig(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(50, nil, nil)
	args := ECStreamOpenArgs{
		Ino: 50, OpenFlags: 0, PoolId: 2, FileSize: 9, InodeGeneration: 3,
		VolName: "vn", VolType: 2, BlockSize: 8192, Ebsc: nil, Bc: nil, Mw: nil,
		EnableBcache: true, WConcurrency: 3, ReadConcurrency: 4,
		AheadReadEnable: true, MinReadAheadSize: 1, PrefetchTotalMem: 99,
	}
	cfg := args.toClientConfig(c, s)
	require.Equal(t, "vn", cfg.VolName)
	require.Equal(t, 2, cfg.VolType)
	require.Equal(t, 8192, cfg.BlockSize)
	require.Equal(t, uint8(2), cfg.PoolId)
	require.True(t, cfg.AheadReadEnable)
	require.Equal(t, int64(99), cfg.PrefetchTotalMem)
}

func TestECExtentClient_CloseStream_ref_gt_zero(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(54, nil, nil)
	atomic.StoreUint32(&s.rdonly, 0)
	atomic.StoreInt32(&s.refCnt, 2)
	c.SetStreamer(54, s)
	require.NoError(t, c.CloseStream(54))
	require.Equal(t, int32(1), atomic.LoadInt32(&s.refCnt))
}

func TestECExtentClient_CloseStream_rdonly_only_dec_ref(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(400, &Reader{valid: true}, nil)
	atomic.StoreUint32(&s.rdonly, 1)
	atomic.StoreInt32(&s.refCnt, 1)
	c.SetStreamer(400, s)
	require.NoError(t, c.CloseStream(400))
	require.Equal(t, int32(0), atomic.LoadInt32(&s.refCnt))
	require.NotNil(t, c.GetStreamer(400))
	require.NotNil(t, s.Reader())
}

func TestECExtentClient_ReadWithInodeView_nil_stream(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	_, err := c.ReadWithInodeView(context.Background(), 99, []byte{0}, 0, 1, 0, false, 0, 0)
	require.ErrorIs(t, err, syscall.EBADF)
}

func TestECExtentClient_EvictStream_ok_delete(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(60, &Reader{valid: true}, nil)
	c.SetStreamer(60, s)
	atomic.StoreInt32(&s.refCnt, 0)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*ECStreamer)(nil)), "closeReaderWriterLocked",
		func(_ *ECStreamer, _ uint64, _ context.Context) error { return nil })
	require.NoError(t, c.EvictStream(60))
	require.Nil(t, c.GetStreamer(60))
}

func TestECExtentClient_EvictStream_ref_positive_no_delete(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	s := NewECStreamer(100, &Reader{valid: true}, nil)
	c.SetStreamer(100, s)
	atomic.StoreInt32(&s.refCnt, 1)
	require.NoError(t, c.EvictStream(100))
	require.NotNil(t, c.GetStreamer(100))
}

func TestECExtentClient_Close_evicts_all(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*ECStreamer)(nil)), "closeReaderWriterLocked",
		func(_ *ECStreamer, _ uint64, _ context.Context) error { return nil })
	s1 := NewECStreamer(101, &Reader{valid: true}, nil)
	s2 := NewECStreamer(102, &Reader{valid: true}, nil)
	c.SetStreamer(101, s1)
	c.SetStreamer(102, s2)
	atomic.StoreInt32(&s1.refCnt, 0)
	atomic.StoreInt32(&s2.refCnt, 0)
	require.NoError(t, c.Close())
	require.Nil(t, c.GetStreamer(101))
	require.Nil(t, c.GetStreamer(102))
}

func TestECExtentClient_SyncReaderViewAfterMetaChange(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.NoError(t, c.SyncReaderViewAfterMetaChange(nil, 404))

	mw := &meta.MetaWrapper{}
	r := &Reader{ino: 405, mw: mw, valid: true}
	s := NewECStreamer(405, r, nil)
	r.ecStreamer = s
	c.SetStreamer(405, s)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*ECStreamer)(nil)), "syncReaderViewAfterMetaChange",
		func(_ *ECStreamer, _ context.Context) error { return nil })
	require.NoError(t, c.SyncReaderViewAfterMetaChange(context.Background(), 405))
}

func TestECExtentClient_FstatSizeView(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	_, _, ok := c.FstatSizeView(1, 5, 100)
	require.False(t, ok)

	s := NewECStreamer(210, nil, nil)
	atomic.StoreUint64(&s.fileSize, 0xf3800)
	atomic.StoreUint64(&s.inoVersion, 12)
	c.SetStreamer(210, s)
	sz, gen, ok := c.FstatSizeView(210, 12, 0xf4000)
	require.True(t, ok)
	require.Equal(t, 0xf4000, sz)
	require.Equal(t, uint64(12), gen)
}

func TestECExtentClient_FileSize(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	sz, gen, ok := c.FileSize(1)
	require.False(t, ok)
	require.Equal(t, 0, sz)
	require.Equal(t, uint64(0), gen)

	s := NewECStreamer(201, nil, nil)
	atomic.StoreUint64(&s.fileSize, 500)
	atomic.StoreUint64(&s.inoVersion, 7)
	c.SetStreamer(201, s)
	sz, gen, ok = c.FileSize(201)
	require.True(t, ok)
	require.Equal(t, 500, sz)
	require.Equal(t, uint64(7), gen)
}

func TestECExtentClient_CloseStream_missing_ino(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.NoError(t, c.CloseStream(99999))
}

func TestECExtentClient_EvictStream_missing_ino(t *testing.T) {
	c := NewObjExtentClient(ObjExtentConfig{})
	require.NoError(t, c.EvictStream(99998))
}
