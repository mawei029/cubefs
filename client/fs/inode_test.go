package fs

import (
	"context"
	"errors"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/depends/bazil.org/fuse"
	bazilfs "github.com/cubefs/cubefs/depends/bazil.org/fuse/fs"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
)

func newTestSuperForInode() *Super {
	return &Super{
		ic:                NewInodeCache(time.Hour, 64, true),
		mw:                &meta.MetaWrapper{},
		ec:                &stream.ExtentClient{},
		oec:               blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{}),
		nodeCache:         make(map[uint64]bazilfs.Node),
		dirExtendInfoMap:  make(map[uint64]*DirExtendInfo),
		fileExtendInfoMap: make(map[uint64]*FileExtendInfo),
		ebsc:              make(map[uint8]*blobstore.BlobStoreClient),
	}
}

func TestInodeGet_FromCache(t *testing.T) {
	s := newTestSuperForInode()
	expect := &proto.InodeInfo{Inode: 1, Size: 10}
	s.ic.Put(expect)

	got, err := s.InodeGet(1)
	require.NoError(t, err)
	require.Equal(t, expect.Inode, got.Inode)
}

func TestInodeGet_BlobStoreHasReaderWriterEarlyReturn(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(99)
	registerOecTestStreamerWithLogicalView(s, ino, &blobstore.Reader{}, &blobstore.Writer{}, 0, 1)
	f := &File{super: s, ino: ino}
	s.nodeCache[ino] = f

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: ino, StorageClass: proto.StorageClass_BlobStore, PoolId: 1}, nil
		})
	openCalled := false
	patches.ApplyPrivateMethod(reflect.TypeOf((*File)(nil)), "openOECStream",
		func(_ *File, _ *proto.InodeInfo, _ uint32, _ uint64) error {
			openCalled = true
			return nil
		})

	got, err := s.InodeGet(ino)
	require.NoError(t, err)
	require.False(t, openCalled)
	require.True(t, proto.IsStorageClassBlobStore(got.StorageClass))
}

func TestInodeGet_BlobStoreFileRefreshReaderWriter(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(100)
	poolID := uint8(1)

	f := &File{super: s, ino: ino}
	f.setFlag(syscall.O_RDWR)
	s.nodeCache[ino] = f
	s.ebsc[poolID] = &blobstore.BlobStoreClient{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, gotIno uint64, _ bool) (*proto.InodeInfo, error) {
			require.Equal(t, ino, gotIno)
			return &proto.InodeInfo{
				Inode:        ino,
				StorageClass: proto.StorageClass_BlobStore,
				PoolId:       poolID,
				Size:         64,
				Generation:   7,
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "OpenStreamWithArgs",
		func(_ *blobstore.ECExtentClient, _ blobstore.ECStreamOpenArgs) error {
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*File)(nil)), "openOECStream",
		func(_ *File, _ *proto.InodeInfo, _ uint32, _ uint64) error {
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.oec), "Flush", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })

	got, err := s.InodeGet(ino)
	require.NoError(t, err)
	require.True(t, proto.IsStorageClassBlobStore(got.StorageClass))
}

func TestInodeGet_BlobFlushBeforeRefreshFails(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(101)
	poolID := uint8(1)

	f := &File{super: s, ino: ino}
	f.setFlag(syscall.O_RDONLY)
	w := &blobstore.Writer{}
	ei := f.getOrCreateExtendInfo()
	ei.coldBlobWriter = w
	s.nodeCache[ino] = f
	s.ebsc[poolID] = &blobstore.BlobStoreClient{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, gotIno uint64, _ bool) (*proto.InodeInfo, error) {
			require.Equal(t, ino, gotIno)
			return &proto.InodeInfo{
				Inode:        ino,
				StorageClass: proto.StorageClass_BlobStore,
				PoolId:       poolID,
				Size:         0,
				Generation:   1,
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.Writer)(nil)), "Flush",
		func(_ *blobstore.Writer, gotIno uint64, _ context.Context) error {
			require.Equal(t, ino, gotIno)
			return errors.New("flush failed")
		})

	_, err := s.InodeGet(ino)
	require.Error(t, err)
}

func TestInodeGet_NoExtentsRefreshCache(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(200)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{
				Inode:        ino,
				StorageClass: proto.StorageClass_Replica_HDD,
				Size:         1,
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.ec), "RefreshExtentsCache",
		func(_ *stream.ExtentClient, gotIno uint64) error {
			require.Equal(t, ino, gotIno)
			return nil
		})

	_, err := s.InodeGet(ino)
	require.NoError(t, err)
}

func TestInodeGet_RefreshExtentsError(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(201)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{
				Inode:        ino,
				StorageClass: proto.StorageClass_Replica_HDD,
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.ec), "RefreshExtentsCache",
		func(_ *stream.ExtentClient, _ uint64) error { return errors.New("refresh failed") })

	_, err := s.InodeGet(ino)
	require.Error(t, err)
}

func TestSetattrFillAttrAndExpirationHelpers(t *testing.T) {
	info := &proto.InodeInfo{
		Inode:      1,
		Mode:       uint32(0o644),
		Size:       10,
		Nlink:      2,
		Uid:        10,
		Gid:        11,
		AccessTime: time.Unix(1, 0),
		CreateTime: time.Unix(2, 0),
		ModifyTime: time.Unix(3, 0),
	}

	req := &fuse.SetattrRequest{
		Valid: fuse.SetattrMode | fuse.SetattrUid | fuse.SetattrGid | fuse.SetattrAtime | fuse.SetattrMtime,
		Mode:  0o755,
		Uid:   100,
		Gid:   101,
		Atime: time.Unix(4, 0),
		Mtime: time.Unix(5, 0),
	}
	valid := setattr(info, req)
	require.NotZero(t, valid&proto.AttrMode)
	require.NotZero(t, valid&proto.AttrUid)
	require.NotZero(t, valid&proto.AttrGid)
	require.NotZero(t, valid&proto.AttrAccessTime)
	require.NotZero(t, valid&proto.AttrModifyTime)

	var attr fuse.Attr
	fillAttr(info, &attr)
	require.Equal(t, info.Inode, attr.Inode)
	require.Equal(t, info.Size>>9, attr.Blocks)
	require.Equal(t, info.Uid, attr.Uid)
	require.Equal(t, info.Gid, attr.Gid)

	info.SetExpiration(time.Now().Add(-time.Second).UnixNano())
	require.True(t, inodeExpired(info))
	inodeSetExpiration(info, time.Second)
	require.False(t, inodeExpired(info))
}

func TestInodeGet_BlobEvictZeroRef_and_largerFileSize(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(150)
	poolID := uint8(1)
	f := &File{super: s, ino: ino}
	f.setFlag(syscall.O_RDWR)
	s.nodeCache[ino] = f
	s.ebsc[poolID] = &blobstore.BlobStoreClient{}
	registerOecTestStreamerWithLogicalView(s, ino, nil, &blobstore.Writer{}, 0, 1)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.oec), "RefCnt",
		func(_ *blobstore.ECExtentClient, _ uint64) int32 { return 0 })
	patches.ApplyMethod(reflect.TypeOf(s.oec), "HasReader",
		func(_ *blobstore.ECExtentClient, _ uint64) bool { return false })
	patches.ApplyMethod(reflect.TypeOf(s.oec), "HasWriter",
		func(_ *blobstore.ECExtentClient, _ uint64) bool { return false })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{
				Inode:        ino,
				StorageClass: proto.StorageClass_BlobStore,
				PoolId:       poolID,
				Size:         50,
				Generation:   2,
			}, nil
		})
	evicted := false
	patches.ApplyMethod(reflect.TypeOf(s.oec), "EvictStream",
		func(_ *blobstore.ECExtentClient, got uint64) error {
			require.Equal(t, ino, got)
			evicted = true
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.oec), "Flush",
		func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyPrivateMethod(reflect.TypeOf((*File)(nil)), "fileSizeVersion2",
		func(_ *File, _ uint64) (int, uint64) { return 200, 2 })
	patches.ApplyMethod(reflect.TypeOf(s.oec), "OpenStreamWithArgs",
		func(_ *blobstore.ECExtentClient, args blobstore.ECStreamOpenArgs) error {
			require.Equal(t, uint64(200), args.FileSize)
			return nil
		})

	got, err := s.InodeGet(ino)
	require.NoError(t, err)
	require.True(t, evicted)
	require.True(t, proto.IsStorageClassBlobStore(got.StorageClass))
}

func TestInodeGet_BlobOpenOECStreamError(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(151)
	f := &File{super: s, ino: ino}
	s.nodeCache[ino] = f
	s.ebsc[1] = &blobstore.BlobStoreClient{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: ino, StorageClass: proto.StorageClass_BlobStore, PoolId: 1}, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*File)(nil)), "openOECStream",
		func(_ *File, _ *proto.InodeInfo, _ uint32, _ uint64) error {
			return errors.New("open oec failed")
		})

	_, err := s.InodeGet(ino)
	require.Error(t, err)
}

func TestInodeGet_BlobExtendInfoFlushPath(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(152)
	f := &File{super: s, ino: ino}
	ei := f.getOrCreateExtendInfo()
	ei.flag = syscall.O_WRONLY
	s.nodeCache[ino] = f
	s.ebsc[1] = &blobstore.BlobStoreClient{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: ino, StorageClass: proto.StorageClass_BlobStore, PoolId: 1}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.oec), "Flush", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyPrivateMethod(reflect.TypeOf((*File)(nil)), "openOECStream",
		func(_ *File, _ *proto.InodeInfo, flags uint32, _ uint64) error {
			require.Equal(t, uint32(syscall.O_WRONLY), flags&0x0f)
			return nil
		})

	_, err := s.InodeGet(ino)
	require.NoError(t, err)
}
