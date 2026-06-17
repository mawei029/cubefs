package fs

import (
	"context"
	"errors"
	"reflect"
	"syscall"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/depends/bazil.org/fuse"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
)

// registerOecTestStreamer 向 oec 注入测试用 ECStreamer（SetStreamerForTest；不经过 OpenStreamWithArgs/refCnt）。
func registerOecTestStreamer(s *Super, ino uint64, r *blobstore.Reader, w *blobstore.Writer) {
	var st *blobstore.ECStreamer
	args := blobstore.ECStreamOpenArgs{Ino: ino}
	switch {
	case r != nil && w != nil:
		st, _ = blobstore.NewECStreamer(args, r, w)
	case w != nil:
		st, _ = blobstore.NewECStreamer(args, nil, w)
	default:
		return
	}
	injectOECStreamer(s.oec, ino, st)
}

func newBlobFileForTruncateTest() (*File, *blobstore.Writer) {
	w := &blobstore.Writer{}
	oec := blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{})
	s, _ := blobstore.NewECStreamer(blobstore.ECStreamOpenArgs{Ino: 100, Mw: &meta.MetaWrapper{}}, nil, w)
	injectOECStreamer(oec, 100, s)
	f := &File{
		super: &Super{
			mw:  &meta.MetaWrapper{},
			ec:  &stream.ExtentClient{},
			oec: oec,
		},
		ino:       100,
		parentIno: 1,
	}
	return f, w
}

func TestFileDoECTruncateV2_delegates_to_oec(t *testing.T) {
	f, _ := newBlobFileForTruncateTest()
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(f.super.oec), "Truncate",
		func(_ *blobstore.ECExtentClient, parentIno, ino, size uint64, path string) error {
			require.Equal(t, uint64(1), parentIno)
			require.Equal(t, uint64(100), ino)
			require.Equal(t, uint64(128), size)
			require.Equal(t, "/a", path)
			return nil
		})
	require.NoError(t, f.doECTruncateV2(100, 128, "/a"))
}

func TestFileDoECTruncateV2_oec_error_propagates(t *testing.T) {
	f, _ := newBlobFileForTruncateTest()
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(f.super.oec), "Truncate",
		func(_ *blobstore.ECExtentClient, _, _ uint64, _ uint64, _ string) error {
			return syscall.EIO
		})
	require.Error(t, f.doECTruncateV2(100, 64, "/a"))
}

func TestFileDoECTruncateV2_EBADF_no_streamer(t *testing.T) {
	f, _ := newBlobFileForTruncateTest()
	require.ErrorIs(t, f.doECTruncateV2(999, 8, "/a"), syscall.EBADF)
}

func blobInode(ino uint64) *proto.InodeInfo {
	return &proto.InodeInfo{
		Inode:        ino,
		PoolId:       1,
		StorageClass: proto.StorageClass_BlobStore,
		Size:         64,
		Generation:   3,
	}
}

func TestFile_Open_ColdBlobStore_success(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	s.ebsc = map[uint8]*blobstore.BlobStoreClient{1: {}}
	f := &File{super: s, ino: 40, parentIno: 1, name: "ecopen.dat"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet",
		func(_ *Super, _ uint64) (*proto.InodeInfo, error) { return blobInode(40), nil })
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "RefreshExtentsCache",
		func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "OpenStreamWithArgs",
		func(_ *blobstore.ECExtentClient, _ blobstore.ECStreamOpenArgs) error { return nil })

	req := &fuse.OpenRequest{Flags: syscall.O_RDWR}
	resp := &fuse.OpenResponse{}
	h, err := f.Open(context.Background(), req, resp)
	require.NoError(t, err)
	require.Same(t, f, h)
}

func TestFile_Open_ColdBlob_flushBeforeOpenFails(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	s.ebsc = map[uint8]*blobstore.BlobStoreClient{1: {}}
	f := &File{super: s, ino: 41, parentIno: 1, name: "flushfail.dat"}
	registerOecTestStreamerWithLogicalView(s, 41, nil, &blobstore.Writer{}, 0, 1)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet",
		func(_ *Super, _ uint64) (*proto.InodeInfo, error) { return blobInode(41), nil })
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "RefreshExtentsCache",
		func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECStreamer)(nil)), "Flush",
		func(_ *blobstore.ECStreamer, _ context.Context) error { return errors.New("flush failed") })

	req := &fuse.OpenRequest{Flags: syscall.O_RDONLY}
	_, err := f.Open(context.Background(), req, &fuse.OpenResponse{})
	require.Error(t, err)
}

func TestFile_Open_HotReplica_metaCacheRefreshWithExtents(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeHot
	s.metaCacheAcceleration = true
	f := &File{super: s, ino: 42, parentIno: 1, name: "hot.dat"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	info := &proto.InodeInfo{
		Inode: 42, StorageClass: proto.StorageClass_Replica_HDD, Size: 8,
		Extents: &proto.GetExtentsResponse{Extents: []proto.ExtentKey{{}}},
	}
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet",
		func(_ *Super, _ uint64) (*proto.InodeInfo, error) { return info, nil })
	var refreshWithCache bool
	patches.ApplyMethod(reflect.TypeOf(s.ec), "OpenStream",
		func(_ *stream.ExtentClient, _ uint64, _ bool, _ bool, _ string) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.ec), "RefreshExtentsWithCache",
		func(_ *stream.ExtentClient, _ *proto.InodeInfo) error {
			refreshWithCache = true
			return nil
		})

	_, err := f.Open(context.Background(), &fuse.OpenRequest{Flags: syscall.O_RDONLY}, &fuse.OpenResponse{})
	require.NoError(t, err)
	require.True(t, refreshWithCache)
}

func TestFile_Open_ReplicaRefreshExtentsCache(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeHot
	f := &File{super: s, ino: 43, parentIno: 1, name: "rep.dat"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet",
		func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: 43, StorageClass: proto.StorageClass_Replica_HDD}, nil
		})
	var refreshed bool
	patches.ApplyMethod(reflect.TypeOf(s.ec), "OpenStream",
		func(_ *stream.ExtentClient, _ uint64, _ bool, _ bool, _ string) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.ec), "RefreshExtentsCache",
		func(_ *stream.ExtentClient, ino uint64) error {
			require.Equal(t, uint64(43), ino)
			refreshed = true
			return nil
		})

	_, err := f.Open(context.Background(), &fuse.OpenRequest{Flags: syscall.O_RDONLY}, &fuse.OpenResponse{})
	require.NoError(t, err)
	require.True(t, refreshed)
}

func TestFile_Open_ReplicaInodeGetFail_fallsBackRefreshCache(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeHot
	s.metaCacheAcceleration = true
	f := &File{super: s, ino: 44, parentIno: 1, name: "rep2.dat"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet",
		func(_ *Super, ino uint64) (*proto.InodeInfo, error) {
			if ino == 44 {
				return &proto.InodeInfo{Inode: 44, StorageClass: proto.StorageClass_Replica_HDD}, nil
			}
			return nil, errors.New("cache miss")
		})
	var refreshed bool
	patches.ApplyMethod(reflect.TypeOf(s.ec), "OpenStream",
		func(_ *stream.ExtentClient, _ uint64, _ bool, _ bool, _ string) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.ec), "RefreshExtentsCache",
		func(_ *stream.ExtentClient, ino uint64) error {
			require.Equal(t, uint64(44), ino)
			refreshed = true
			return nil
		})

	_, err := f.Open(context.Background(), &fuse.OpenRequest{Flags: syscall.O_RDONLY}, &fuse.OpenResponse{})
	require.NoError(t, err)
	require.True(t, refreshed)
}

func TestFile_Write_BlobErrorMetrics(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 50, parentIno: 1, name: "w.dat"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet",
		func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: 50, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
		})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Write",
		func(_ *blobstore.ECExtentClient, _ uint64, _ int, _ []byte, _ int) (int, error) {
			return 0, syscall.EOPNOTSUPP
		})

	req := &fuse.WriteRequest{Offset: 0, Data: []byte("x")}
	err := f.Write(context.Background(), req, &fuse.WriteResponse{})
	require.Equal(t, fuse.Errno(syscall.ENOTSUP), err)
}

func TestFile_Write_BlobOsyncFlush(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 51, parentIno: 1, name: "osync.dat"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet",
		func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: 51, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
		})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Write",
		func(_ *blobstore.ECExtentClient, _ uint64, _ int, data []byte, _ int) (int, error) {
			return len(data), nil
		})
	flushed := false
	patches.ApplyMethod(reflect.TypeOf(s.oec), "Flush",
		func(_ *blobstore.ECExtentClient, ino uint64) error {
			require.Equal(t, uint64(51), ino)
			flushed = true
			return nil
		})

	req := &fuse.WriteRequest{Offset: 0, Data: []byte("ab"), FileFlags: fuse.OpenSync}
	resp := &fuse.WriteResponse{}
	require.NoError(t, f.Write(context.Background(), req, resp))
	require.True(t, flushed)
	require.Equal(t, 2, resp.Size)
}
