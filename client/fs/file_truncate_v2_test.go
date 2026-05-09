package fs

import (
	"context"
	"reflect"
	"syscall"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	cfsproto "github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/stretchr/testify/require"
)

// registerOecTestStreamer 向 oec 注入测试用 ECStreamer（ECExtentClient.SetStreamer；不经过 OpenStreamWithArgs/refCnt）。
func registerOecTestStreamer(s *Super, ino uint64, r *blobstore.Reader, w *blobstore.Writer) {
	var st *blobstore.ECStreamer
	switch {
	case r != nil && w != nil:
		st = newTestECStreamerWithReaderWriter(ino, r, w)
	case w != nil:
		st = newTestECStreamerWithWriter(ino, w)
	default:
		return
	}
	s.oec.SetStreamer(ino, st)
}

// newTestECStreamerWithWriter / newTestECStreamerWithReaderWriter 仅 client/fs 测试用，封装 blobstore.NewECStreamer。
func newTestECStreamerWithWriter(ino uint64, w *blobstore.Writer) *blobstore.ECStreamer {
	return blobstore.NewECStreamer(ino, nil, w)
}

func newTestECStreamerWithReaderWriter(ino uint64, r *blobstore.Reader, w *blobstore.Writer) *blobstore.ECStreamer {
	return blobstore.NewECStreamer(ino, r, w)
}

func newBlobFileForTruncateTest() (*File, *blobstore.Writer) {
	w := &blobstore.Writer{}
	oec := blobstore.NewObjExtentClient(nil)
	s := newTestECStreamerWithWriter(100, w)
	oec.SetStreamer(100, s)
	f := &File{
		super: &Super{
			mw:  &meta.MetaWrapper{},
			ec:  &stream.ExtentClient{},
			oec: oec,
		},
		ino: 100,
	}
	f.super.oec.BeforeEBSShrinkHook = func(ino uint64, fullPath string) (func(), error) {
		if err := f.super.ec.OpenStream(ino, true, true, fullPath); err != nil {
			return nil, err
		}
		if err := f.super.ec.Flush(ino); err != nil {
			_ = f.super.ec.CloseStream(ino)
			return nil, err
		}
		return func() { _ = f.super.ec.CloseStream(ino) }, nil
	}
	return f, w
}

func TestFileDoECTruncateV2_ENOENTTreatAsNewFile(t *testing.T) {
	f, w := newBlobFileForTruncateTest()
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var gotObjExtents []cfsproto.ObjExtentKey
	patches.ApplyMethod(reflect.TypeOf(w), "Flush",
		func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []cfsproto.ExtentKey, []cfsproto.ObjExtentKey, error) {
			return 0, 0, nil, nil, syscall.ENOENT
		})
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, objExtents []cfsproto.ObjExtentKey, _ []cfsproto.ObjExtentKey) error {
			gotObjExtents = objExtents
			return nil
		})

	err := f.doECTruncateV2(100, 128, "/a")
	require.NoError(t, err)
	require.Nil(t, gotObjExtents)
}

func TestFileDoECTruncateV2_ExpandOnlyMeta(t *testing.T) {
	f, w := newBlobFileForTruncateTest()
	current := []cfsproto.ObjExtentKey{{FileOffset: 0, Size: 64}}

	var gotObjExtents []cfsproto.ObjExtentKey
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush",
		func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []cfsproto.ExtentKey, []cfsproto.ObjExtentKey, error) {
			return 0, 64, nil, current, nil
		})
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, objExtents []cfsproto.ObjExtentKey, _ []cfsproto.ObjExtentKey) error {
			gotObjExtents = objExtents
			return nil
		})

	err := f.doECTruncateV2(100, 128, "/a")
	require.NoError(t, err)
	require.Equal(t, current, gotObjExtents)
}

func TestFileDoECTruncateV2_EqualSizeNoop(t *testing.T) {
	f, w := newBlobFileForTruncateTest()
	var truncateCalled bool

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush",
		func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []cfsproto.ExtentKey, []cfsproto.ObjExtentKey, error) {
			return 0, 64, nil, nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, _ []cfsproto.ObjExtentKey, _ []cfsproto.ObjExtentKey) error {
			truncateCalled = true
			return nil
		})

	err := f.doECTruncateV2(100, 64, "/a")
	require.NoError(t, err)
	require.False(t, truncateCalled)
}

func TestFileDoECTruncateV2_ShrinkPath(t *testing.T) {
	f, w := newBlobFileForTruncateTest()
	newObjExtents := []cfsproto.ObjExtentKey{{FileOffset: 0, Size: 32}}

	var gotObjExtents []cfsproto.ObjExtentKey
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w), "Flush",
		func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []cfsproto.ExtentKey, []cfsproto.ObjExtentKey, error) {
			return 0, 128, nil, []cfsproto.ObjExtentKey{{FileOffset: 0, Size: 128}}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(f.super.ec), "OpenStream",
		func(_ *stream.ExtentClient, _ uint64, _ bool, _ bool, _ string) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f.super.ec), "CloseStream",
		func(_ *stream.ExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f.super.ec), "Flush",
		func(_ *stream.ExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(w), "TruncateV2FromExtents",
		func(_ *blobstore.Writer, _ context.Context, _ uint64, _ uint64, _ []cfsproto.ObjExtentKey) ([]cfsproto.ObjExtentKey, []cfsproto.ObjExtentKey, error) {
			return newObjExtents, nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, objExtents []cfsproto.ObjExtentKey, _ []cfsproto.ObjExtentKey) error {
			gotObjExtents = objExtents
			return nil
		})

	err := f.doECTruncateV2(100, 32, "/a")
	require.NoError(t, err)
	require.Equal(t, newObjExtents, gotObjExtents)
}

func TestFileDoECTruncateV2_ErrorBranches(t *testing.T) {
	t.Run("writer flush error", func(t *testing.T) {
		f, w := newBlobFileForTruncateTest()
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(w), "Flush",
			func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return syscall.EIO })
		err := f.doECTruncateV2(100, 8, "/a")
		require.Error(t, err)
	})

	t.Run("GetObjExtents generic error", func(t *testing.T) {
		f, w := newBlobFileForTruncateTest()
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(w), "Flush",
			func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
		patches.ApplyMethod(reflect.TypeOf(f.super.mw), "GetObjExtents",
			func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []cfsproto.ExtentKey, []cfsproto.ObjExtentKey, error) {
				return 0, 0, nil, nil, syscall.EIO
			})
		err := f.doECTruncateV2(100, 8, "/a")
		require.Error(t, err)
	})

	t.Run("OpenStream error in shrink", func(t *testing.T) {
		f, w := newBlobFileForTruncateTest()
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(w), "Flush",
			func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
		patches.ApplyMethod(reflect.TypeOf(f.super.mw), "GetObjExtents",
			func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []cfsproto.ExtentKey, []cfsproto.ObjExtentKey, error) {
				return 0, 128, nil, []cfsproto.ObjExtentKey{{FileOffset: 0, Size: 128}}, nil
			})
		patches.ApplyMethod(reflect.TypeOf(f.super.ec), "OpenStream",
			func(_ *stream.ExtentClient, _ uint64, _ bool, _ bool, _ string) error { return syscall.EIO })
		err := f.doECTruncateV2(100, 64, "/a")
		require.Error(t, err)
	})

	t.Run("ec flush error in shrink", func(t *testing.T) {
		f, w := newBlobFileForTruncateTest()
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(w), "Flush",
			func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
		patches.ApplyMethod(reflect.TypeOf(f.super.mw), "GetObjExtents",
			func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []cfsproto.ExtentKey, []cfsproto.ObjExtentKey, error) {
				return 0, 128, nil, []cfsproto.ObjExtentKey{{FileOffset: 0, Size: 128}}, nil
			})
		patches.ApplyMethod(reflect.TypeOf(f.super.ec), "OpenStream",
			func(_ *stream.ExtentClient, _ uint64, _ bool, _ bool, _ string) error { return nil })
		patches.ApplyMethod(reflect.TypeOf(f.super.ec), "CloseStream",
			func(_ *stream.ExtentClient, _ uint64) error { return nil })
		patches.ApplyMethod(reflect.TypeOf(f.super.ec), "Flush",
			func(_ *stream.ExtentClient, _ uint64) error { return syscall.EIO })
		err := f.doECTruncateV2(100, 64, "/a")
		require.Error(t, err)
	})
}
