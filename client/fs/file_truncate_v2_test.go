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

func newBlobFileForTruncateTest() *File {
	f := &File{
		super: &Super{
			mw: &meta.MetaWrapper{},
			ec: &stream.ExtentClient{},
		},
		ino: 100,
	}
	f.setReaderWriter(nil, &blobstore.Writer{})
	return f
}

func TestFileDoECTruncateV2_ENOENTTreatAsNewFile(t *testing.T) {
	f := newBlobFileForTruncateTest()
	w := f.getWriter()

	var gotObjExtents []cfsproto.ObjExtentKey
	patches := gomonkey.NewPatches()
	defer patches.Reset()
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
	f := newBlobFileForTruncateTest()
	w := f.getWriter()
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
	f := newBlobFileForTruncateTest()
	w := f.getWriter()
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
	f := newBlobFileForTruncateTest()
	w := f.getWriter()
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
	patches.ApplyMethod(reflect.TypeOf(w), "TruncateV2",
		func(_ *blobstore.Writer, _ context.Context, _ uint64) ([]cfsproto.ObjExtentKey, []cfsproto.ObjExtentKey, error) {
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
		f := newBlobFileForTruncateTest()
		w := f.getWriter()
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(w), "Flush",
			func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return syscall.EIO })
		err := f.doECTruncateV2(100, 8, "/a")
		require.Error(t, err)
	})

	t.Run("GetObjExtents generic error", func(t *testing.T) {
		f := newBlobFileForTruncateTest()
		w := f.getWriter()
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
		f := newBlobFileForTruncateTest()
		w := f.getWriter()
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
		f := newBlobFileForTruncateTest()
		w := f.getWriter()
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
