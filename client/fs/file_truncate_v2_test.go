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
	return &File{
		super: &Super{
			mw: &meta.MetaWrapper{},
			ec: &stream.ExtentClient{},
		},
		info: &cfsproto.InodeInfo{
			Inode:  100,
			PoolId: 1,
		},
	}
}

func TestFileDoECTruncateV2_ENOENTTreatAsNewFile(t *testing.T) {
	f := newBlobFileForTruncateTest()
	w := &blobstore.Writer{}

	var gotObjExtents []cfsproto.ObjExtentKey
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(f), "ensureBlobStoreWriter",
		func(_ *File, _ uint64) (*blobstore.Writer, error) { return w, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Flush",
		func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f), "getECCurrentSizeAndExtents",
		func(_ *File, _ uint64) (uint64, []cfsproto.ObjExtentKey, error) { return 0, nil, syscall.ENOENT })
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, objExtents []cfsproto.ObjExtentKey) error {
			gotObjExtents = objExtents
			return nil
		})

	err := f.doECTruncateV2(100, 128, "/a")
	require.NoError(t, err)
	require.Nil(t, gotObjExtents)
}

func TestFileDoECTruncateV2_ExpandOnlyMeta(t *testing.T) {
	f := newBlobFileForTruncateTest()
	w := &blobstore.Writer{}
	current := []cfsproto.ObjExtentKey{{FileOffset: 0, Size: 64}}

	var gotObjExtents []cfsproto.ObjExtentKey
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(f), "ensureBlobStoreWriter",
		func(_ *File, _ uint64) (*blobstore.Writer, error) { return w, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Flush",
		func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f), "getECCurrentSizeAndExtents",
		func(_ *File, _ uint64) (uint64, []cfsproto.ObjExtentKey, error) { return 64, current, nil })
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, objExtents []cfsproto.ObjExtentKey) error {
			gotObjExtents = objExtents
			return nil
		})

	err := f.doECTruncateV2(100, 128, "/a")
	require.NoError(t, err)
	require.Equal(t, current, gotObjExtents)
}

func TestFileDoECTruncateV2_EqualSizeNoop(t *testing.T) {
	f := newBlobFileForTruncateTest()
	w := &blobstore.Writer{}
	var truncateCalled bool

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(f), "ensureBlobStoreWriter",
		func(_ *File, _ uint64) (*blobstore.Writer, error) { return w, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Flush",
		func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f), "getECCurrentSizeAndExtents",
		func(_ *File, _ uint64) (uint64, []cfsproto.ObjExtentKey, error) { return 64, nil, nil })
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, _ []cfsproto.ObjExtentKey) error {
			truncateCalled = true
			return nil
		})

	err := f.doECTruncateV2(100, 64, "/a")
	require.NoError(t, err)
	require.False(t, truncateCalled)
}

func TestFileDoECTruncateV2_ShrinkPath(t *testing.T) {
	f := newBlobFileForTruncateTest()
	w := &blobstore.Writer{}
	newObjExtents := []cfsproto.ObjExtentKey{{FileOffset: 0, Size: 32}}

	var gotObjExtents []cfsproto.ObjExtentKey
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(f), "ensureBlobStoreWriter",
		func(_ *File, _ uint64) (*blobstore.Writer, error) { return w, nil })
	patches.ApplyMethod(reflect.TypeOf(w), "Flush",
		func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f), "getECCurrentSizeAndExtents",
		func(_ *File, _ uint64) (uint64, []cfsproto.ObjExtentKey, error) {
			return 128, []cfsproto.ObjExtentKey{{FileOffset: 0, Size: 128}}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(f.super.ec), "OpenStream",
		func(_ *stream.ExtentClient, _ uint64, _ bool, _ bool, _ string) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(f.super.ec), "CloseStream",
		func(_ *stream.ExtentClient, _ uint64) {})
	patches.ApplyMethod(reflect.TypeOf(f.super.ec), "Flush",
		func(_ *stream.ExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(w), "TruncateV2",
		func(_ *blobstore.Writer, _ context.Context, _ uint64) ([]cfsproto.ObjExtentKey, error) {
			return newObjExtents, nil
		})
	patches.ApplyMethod(reflect.TypeOf(f.super.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, objExtents []cfsproto.ObjExtentKey) error {
			gotObjExtents = objExtents
			return nil
		})

	err := f.doECTruncateV2(100, 32, "/a")
	require.NoError(t, err)
	require.Equal(t, newObjExtents, gotObjExtents)
}
