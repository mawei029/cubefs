package fs

import (
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

func TestInodeGet_BlobEmptyOeksTriggersRefresh(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(99)
	registerOecTestStreamerWithLogicalView(s, ino, &blobstore.Reader{}, &blobstore.Writer{}, 0, 1)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: ino, StorageClass: proto.StorageClass_BlobStore, PoolId: 1, Size: 64}, nil
		})
	refreshCalled := false
	patches.ApplyMethod(reflect.TypeOf(s.oec), "RefreshExtentsCache",
		func(_ *blobstore.ECExtentClient, gotIno uint64) error {
			require.Equal(t, ino, gotIno)
			refreshCalled = true
			return nil
		})

	got, err := s.InodeGet(ino)
	require.NoError(t, err)
	require.True(t, refreshCalled)
	require.True(t, proto.IsStorageClassBlobStore(got.StorageClass))
}

func TestInodeGet_BlobNonEmptyOeksSkipsRefresh(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(98)
	registerOecTestStreamerWithLogicalView(s, ino, &blobstore.Reader{}, &blobstore.Writer{}, 64, 1)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: ino, StorageClass: proto.StorageClass_BlobStore, PoolId: 1, Size: 64}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.oec), "NeedRefreshObjExtents",
		func(_ *blobstore.ECExtentClient, _ uint64) bool { return false })
	refreshCalled := false
	patches.ApplyMethod(reflect.TypeOf(s.oec), "RefreshExtentsCache",
		func(_ *blobstore.ECExtentClient, _ uint64) error {
			refreshCalled = true
			return nil
		})

	got, err := s.InodeGet(ino)
	require.NoError(t, err)
	require.False(t, refreshCalled)
	require.Equal(t, uint64(64), got.Size)
}

func TestInodeGet_BlobRefreshExtentsError(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(102)
	registerOecTestStreamerWithLogicalView(s, ino, &blobstore.Reader{}, &blobstore.Writer{}, 0, 1)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{
				Inode:        ino,
				StorageClass: proto.StorageClass_BlobStore,
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.oec), "RefreshExtentsCache",
		func(_ *blobstore.ECExtentClient, _ uint64) error { return errors.New("refresh failed") })

	_, err := s.InodeGet(ino)
	require.Error(t, err)
}

func TestInodeGet_BlobNoStreamNoSideEffects(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(101)
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
				Size:         50,
				Generation:   2,
			}, nil
		})

	flushCalled, evictCalled, openCalled, refreshCalled := false, false, false, false
	patches.ApplyMethod(reflect.TypeOf(s.oec), "RefreshExtentsCache",
		func(_ *blobstore.ECExtentClient, _ uint64) error {
			refreshCalled = true
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.oec), "Flush",
		func(_ *blobstore.ECExtentClient, _ uint64) error {
			flushCalled = true
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.oec), "EvictStream",
		func(_ *blobstore.ECExtentClient, _ uint64) error {
			evictCalled = true
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*File)(nil)), "openOECStream",
		func(_ *File, _ *proto.InodeInfo, _ uint32, _ uint64) error {
			openCalled = true
			return errors.New("should not open")
		})

	got, err := s.InodeGet(ino)
	require.NoError(t, err)
	require.False(t, refreshCalled)
	require.False(t, flushCalled)
	require.False(t, evictCalled)
	require.False(t, openCalled)
	require.Equal(t, uint64(50), got.Size)
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

func TestInodeGet_ReplicaHasExtentsSkipsRefresh(t *testing.T) {
	s := newTestSuperForInode()
	ino := uint64(202)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{
				Inode:        ino,
				StorageClass: proto.StorageClass_Replica_HDD,
				Extents:      &proto.GetExtentsResponse{},
			}, nil
		})
	refreshCalled := false
	patches.ApplyMethod(reflect.TypeOf(s.ec), "RefreshExtentsCache",
		func(_ *stream.ExtentClient, _ uint64) error {
			refreshCalled = true
			return nil
		})

	_, err := s.InodeGet(ino)
	require.NoError(t, err)
	require.False(t, refreshCalled)
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
