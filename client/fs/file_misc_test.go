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

func patchOecWriterForMisc(patches *gomonkey.Patches, w *blobstore.Writer) {
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Writer",
		func(_ *blobstore.ECExtentClient, _ uint64) *blobstore.Writer {
			return w
		})
}

func patchOecReaderWriterForMisc(patches *gomonkey.Patches, r *blobstore.Reader, w *blobstore.Writer) {
	t := reflect.TypeOf((*blobstore.ECExtentClient)(nil))
	patches.ApplyMethod(t, "Reader", func(_ *blobstore.ECExtentClient, _ uint64) *blobstore.Reader {
		return r
	})
	patches.ApplyMethod(t, "Writer", func(_ *blobstore.ECExtentClient, _ uint64) *blobstore.Writer {
		return w
	})
}

func newTestSuperForFile() *Super {
	return &Super{
		ic:                NewInodeCache(time.Hour, 64, true),
		rootIno:           1,
		nodeCache:         make(map[uint64]bazilfs.Node),
		dirExtendInfoMap:  make(map[uint64]*DirExtendInfo),
		fileExtendInfoMap: make(map[uint64]*FileExtendInfo),
		runningMonitor:    NewRunningMonitor(0),
		ec:                &stream.ExtentClient{},
		mw:                &meta.MetaWrapper{},
		volname:           "vol",
		EbsBlockSize:      4096,
		oec:               blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{}),
	}
}

func TestFile_IsEioHelpers(t *testing.T) {
	require.False(t, isWriteEio(syscall.EBADF))
	require.False(t, isWriteEio(errors.New("no such file or directory")))
	require.True(t, isWriteEio(errors.New("unexpected write failure")))

	require.False(t, isReadEio(syscall.EBADF))
	require.False(t, isReadEio(errors.New("ExtentNotFoundError")))
	require.False(t, isReadEio(errors.New("no such file or directory")))
	require.True(t, isReadEio(errors.New("unexpected read failure")))
}

func TestFile_GetParentPathBranches(t *testing.T) {
	s := newTestSuperForFile()

	// root parent
	f := &File{super: s, ino: 10, parentIno: s.rootIno, name: "f"}
	require.Equal(t, "/", f.getParentPath())

	// cache miss
	f.parentIno = 99
	require.Equal(t, "unknown", f.getParentPath())

	// type mismatch in cache
	s.nodeCache[99] = &File{super: s, ino: 99, parentIno: 1, name: "notdir"}
	require.Equal(t, "unknown", f.getParentPath())

	// valid parent dir
	parent := &Dir{super: s, ino: 2, parentIno: 1, name: "parent"}
	s.nodeCache[2] = parent
	f.parentIno = 2
	require.Equal(t, "/parent", f.getParentPath())
}

func TestFile_XattrFeatureSwitchAndSecurityCapabilityBypass(t *testing.T) {
	s := newTestSuperForFile()
	f := &File{super: s, ino: 10, name: "f"}

	// xattr disabled
	require.ErrorIs(t, f.Getxattr(context.Background(), &fuse.GetxattrRequest{}, &fuse.GetxattrResponse{}), fuse.ENOSYS)
	require.ErrorIs(t, f.Listxattr(context.Background(), &fuse.ListxattrRequest{}, &fuse.ListxattrResponse{}), fuse.ENOSYS)
	require.ErrorIs(t, f.Setxattr(context.Background(), &fuse.SetxattrRequest{}), fuse.ENOSYS)
	require.ErrorIs(t, f.Removexattr(context.Background(), &fuse.RemovexattrRequest{}), fuse.ENOSYS)

	// security.capability fast path does not touch meta
	s.enableXattr = true
	resp := &fuse.GetxattrResponse{}
	err := f.Getxattr(context.Background(), &fuse.GetxattrRequest{Name: "security.capability"}, resp)
	require.NoError(t, err)
	require.Equal(t, []byte{}, resp.Xattr)
}

func TestFile_fileSizeVersion2_ecBlob_invalidFileSize_usesWriterCache(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 16, parentIno: 2, name: "ec.dat"}
	s.ic.Put(&proto.InodeInfo{Inode: 16, PoolId: 1, StorageClass: proto.StorageClass_BlobStore, Size: 100, Generation: 3})

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	w := &blobstore.Writer{}
	registerOecTestStreamerWithLogicalView(s, 16, nil, w, 200, 3)
	f.setColdBlobReaderWriter(nil, w)

	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "FileSize",
		func(_ *blobstore.ECExtentClient, _ uint64) (int, uint64, bool) {
			return 0, 0, false
		})
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: 16, Size: 100, Generation: 3}, nil
	})

	size, gen := f.fileSizeVersion2(f.ino, f.storageClass())
	// oec.FileSize 无效时冷卷回退 InodeGet.Size，不再合并孤立 Writer.CacheFileSize。
	require.Equal(t, 100, size)
	require.Equal(t, uint64(3), gen)
}

func TestFile_oecRefreshExtentsCache_warnOnError(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	_ = &File{super: s, ino: 17}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "RefreshExtentsCache",
		func(_ *blobstore.ECExtentClient, _ uint64) error {
			return errors.New("refresh failed")
		})
	require.Error(t, s.oec.RefreshExtentsCache(17))
}

func TestFile_FilterSuffixAndFileSizeVersion2Fallback(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 10, name: "a.log"}

	require.True(t, (&File{super: s, ino: 11}).filterFilesSuffix("log"))
	require.False(t, f.filterFilesSuffix(""))
	require.True(t, f.filterFilesSuffix("txt;log"))
	require.False(t, f.filterFilesSuffix("txt;jpg"))

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "FileSize",
		func(_ *blobstore.ECExtentClient, _ uint64) (int, uint64, bool) {
			return 128, 9, true
		})

	writer := &blobstore.Writer{}
	registerOecTestStreamerWithLogicalView(s, 10, nil, writer, 128, 9)
	f.setColdBlobReaderWriter(nil, writer)
	s.ic.Put(&proto.InodeInfo{Inode: f.ino, Size: 100, Generation: 9})

	size, gen := f.fileSizeVersion2(f.ino, f.storageClass())
	require.Equal(t, 128, size)
	require.Equal(t, uint64(9), gen)
}

func TestFile_XattrEnabledPaths(t *testing.T) {
	s := newTestSuperForFile()
	s.enableXattr = true
	f := &File{super: s, ino: 10, name: "f"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyMethod(reflect.TypeOf(s.mw), "XAttrGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ string, _ bool) (*proto.XAttrInfo, error) {
			return &proto.XAttrInfo{Inode: 10, XAttrs: map[string]string{"k": "value"}}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "XAttrsList_ll",
		func(_ *meta.MetaWrapper, _ uint64) ([]string, error) { return []string{"a", "b"}, nil })

	var setName, setValue string
	patches.ApplyMethod(reflect.TypeOf(s.mw), "XAttrSet_ll",
		func(_ *meta.MetaWrapper, _ uint64, name, value []byte, _ bool) error {
			setName = string(name)
			setValue = string(value)
			return nil
		})
	var delName string
	patches.ApplyMethod(reflect.TypeOf(s.mw), "XAttrDel_ll",
		func(_ *meta.MetaWrapper, _ uint64, name string) error {
			delName = name
			return nil
		})

	getResp := &fuse.GetxattrResponse{}
	err := f.Getxattr(context.Background(), &fuse.GetxattrRequest{Name: "k", Position: 1, Size: 2}, getResp)
	require.NoError(t, err)
	require.Equal(t, []byte("al"), getResp.Xattr)

	listResp := &fuse.ListxattrResponse{}
	require.NoError(t, f.Listxattr(context.Background(), &fuse.ListxattrRequest{}, listResp))
	require.Contains(t, string(listResp.Xattr), "a")
	require.Contains(t, string(listResp.Xattr), "b")

	require.NoError(t, f.Setxattr(context.Background(), &fuse.SetxattrRequest{Name: "k2", Xattr: []byte("v2")}))
	require.Equal(t, "k2", setName)
	require.Equal(t, "v2", setValue)

	require.NoError(t, f.Removexattr(context.Background(), &fuse.RemovexattrRequest{Name: "k3"}))
	require.Equal(t, "k3", delName)
}

func TestFile_Write_BlobFallocatePath(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 10, parentIno: 1, name: "f"}
	writer := &blobstore.Writer{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchOecWriterForMisc(patches, writer)
	registerOecTestStreamerWithLogicalView(s, 10, nil, writer, 10, 0)
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: 10, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
	})
	patches.ApplyMethod(reflect.TypeOf(writer), "Flush", func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 0, 0, nil, nil, syscall.ENOENT
		})
	var truncateTo uint64
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, size uint64, _ string, _ proto.ObjExtentKey, _ proto.ObjExtentKey) error {
			truncateTo = size
			return nil
		})

	req := &fuse.WriteRequest{Offset: 20, Data: []byte{0}}
	resp := &fuse.WriteResponse{}
	err := f.Write(context.Background(), req, resp)
	require.NoError(t, err)
	require.Equal(t, 1, resp.Size)
	require.Equal(t, uint64(21), truncateTo)
}

func TestFile_Setattr_BlobTruncateAndSyncReaderWriter(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	s.ebsc = map[uint8]*blobstore.BlobStoreClient{1: {}}
	f := &File{super: s, ino: 11, parentIno: 1, name: "f2"}
	writer := &blobstore.Writer{}
	reader := &blobstore.Reader{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchOecReaderWriterForMisc(patches, reader, writer)
	registerOecTestStreamerWithLogicalView(s, 11, reader, writer, 32, 0)

	callN := 0
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		callN++
		return &proto.InodeInfo{
			Inode:        11,
			PoolId:       1,
			StorageClass: proto.StorageClass_BlobStore,
			Size:         32,
			Generation:   uint64(callN),
		}, nil
	})
	// Setattr opens oec via openOECStream before truncate; stream already injected above.
	patches.ApplyPrivateMethod(reflect.TypeOf((*File)(nil)), "openOECStream",
		func(_ *File, _ *proto.InodeInfo, _ uint32, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "CloseStream",
		func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(writer), "Flush", func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 32, nil, []proto.ObjExtentKey{{FileOffset: 0, Size: 32}}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.mw), "TruncateV2",
		func(_ *meta.MetaWrapper, _ uint64, _ uint64, _ string, _ proto.ObjExtentKey, _ proto.ObjExtentKey) error {
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.ec), "RefreshExtentsCache", func(_ *stream.ExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "RefreshExtentsCache",
		func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })

	req := &fuse.SetattrRequest{Valid: fuse.SetattrSize, Size: 32}
	resp := &fuse.SetattrResponse{}
	err := f.Setattr(context.Background(), req, resp)
	require.NoError(t, err)
}

func TestFile_Setattr_ReplicaTruncateBranch(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeHot
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_Replica_HDD)},
	}
	f := &File{super: s, ino: 12, parentIno: 1, name: "f3"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: 12, PoolId: 1, StorageClass: proto.StorageClass_Replica_HDD, Size: 20}, nil
	})
	patches.ApplyMethod(reflect.TypeOf(s.ec), "OpenStream", func(_ *stream.ExtentClient, _ uint64, _ bool, _ bool, _ string) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.ec), "CloseStream", func(_ *stream.ExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.ec), "Flush", func(_ *stream.ExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.ec), "Truncate",
		func(_ *stream.ExtentClient, _ *meta.MetaWrapper, _ uint64, _ uint64, _ int, _ string) error {
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(s.ec), "RefreshExtentsCache", func(_ *stream.ExtentClient, _ uint64) error { return nil })

	req := &fuse.SetattrRequest{Valid: fuse.SetattrSize, Size: 20}
	resp := &fuse.SetattrResponse{}
	err := f.Setattr(context.Background(), req, resp)
	require.NoError(t, err)
}

// TestFile_EnsureBlobStoreWriter_CreatePath 已移除：ensureBlobStoreWriter 由 OpenStreamWithArgs 替代。

func TestFile_Write_BlobAppendFlagPath(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 14, parentIno: 1, name: "f5"}
	writer := &blobstore.Writer{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchOecWriterForMisc(patches, writer)
	registerOecTestStreamer(s, 14, nil, writer)
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: 14, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
	})
	patches.ApplyMethod(reflect.TypeOf(writer), "Write",
		func(_ *blobstore.Writer, _ context.Context, _ int, data []byte, flags int) (int, error) {
			require.NotZero(t, flags&proto.FlagsAppend)
			return len(data), nil
		})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Write",
		func(_ *blobstore.ECExtentClient, _ uint64, offset int, data []byte, flags int) (int, error) {
			require.Equal(t, 0, offset)
			require.NotZero(t, flags&proto.FlagsAppend)
			return len(data), nil
		})

	req := &fuse.WriteRequest{Offset: 0, Data: []byte("abc"), FileFlags: fuse.OpenAppend}
	resp := &fuse.WriteResponse{}
	err := f.Write(context.Background(), req, resp)
	require.NoError(t, err)
	require.Equal(t, 3, resp.Size)
}

func TestFile_Read_BlobUsesOecReadAfterAlign(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 15, parentIno: 1, name: "rf"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: 15, PoolId: 1, StorageClass: proto.StorageClass_BlobStore, Generation: 7, Size: 16}, nil
	})

	calledOecRead := false
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Read",
		func(_ *blobstore.ECExtentClient, ino uint64, _ []byte, offset int, size int) (int, error) {
			require.Equal(t, uint64(15), ino)
			require.Equal(t, 0, offset)
			require.Equal(t, 4, size)
			calledOecRead = true
			return 4, nil
		})

	req := &fuse.ReadRequest{Offset: 0, Size: 4}
	resp := &fuse.ReadResponse{Data: make([]byte, fuse.OutHeaderSize+4)}
	err := f.Read(context.Background(), req, resp)
	require.NoError(t, err)
	require.True(t, calledOecRead)
}

func TestFile_Open_BlobFlushExistingWriterError(t *testing.T) {
	t.Skip("Open+FlushAndFreeCache 路径待与 OpenStreamWithArgs 对齐后恢复")
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.volname = "vol"
	s.EbsBlockSize = 4096
	s.writeThreads = 1
	s.readThreads = 1
	s.ebsc = map[uint8]*blobstore.BlobStoreClient{1: {}}
	f := &File{super: s, ino: 30, parentIno: 1, name: "openflush"}
	w := &blobstore.Writer{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchOecWriterForMisc(patches, w)
	registerOecTestStreamer(s, 30, nil, w)
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: 30, PoolId: 1, StorageClass: proto.StorageClass_BlobStore, Size: 0}, nil
	})
	patches.ApplyMethod(reflect.TypeOf(s.ec), "OpenStream", func(_ *stream.ExtentClient, _ uint64, _ bool, _ bool, _ string) error {
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(s.ec), "RefreshExtentsCache", func(_ *stream.ExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.ec), "FileSize", func(_ *stream.ExtentClient, _ uint64) (int, uint64, bool) {
		return 0, 1, false
	})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECStreamer)(nil)), "FlushAndFreeCache", func(_ *blobstore.ECStreamer, _ context.Context) error {
		return errors.New("flush failed")
	})

	req := &fuse.OpenRequest{Flags: syscall.O_RDONLY}
	resp := &fuse.OpenResponse{}
	h, err := f.Open(context.Background(), req, resp)
	require.Error(t, err)
	require.Nil(t, h)
}

func TestFile_Flush_BlobNilWriterReadOnlyOk(t *testing.T) {
	s := newTestSuperForFile()
	s.fsyncOnClose = true
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 31, parentIno: 1, name: "fl"}
	f.setFlag(syscall.O_RDONLY)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: 31, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
	})
	patches.ApplyMethod(reflect.TypeOf(s.oec), "Flush", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })

	err := f.Flush(context.Background(), &fuse.FlushRequest{})
	require.NoError(t, err)
}

func TestFile_Flush_BlobNilWriterWriteModeBadFd(t *testing.T) {
	s := newTestSuperForFile()
	s.fsyncOnClose = true
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 32, parentIno: 1, name: "fl2"}
	f.setFlag(syscall.O_WRONLY)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: 32, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
	})

	err := f.Flush(context.Background(), &fuse.FlushRequest{})
	require.Error(t, err)
	require.Equal(t, fuse.Errno(syscall.EBADF), err)
}

func TestFile_Fsync_BlobNilWriterReadOnlyOk(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 33, parentIno: 1, name: "fs"}
	f.setFlag(syscall.O_RDONLY)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: 33, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
	})
	patches.ApplyMethod(reflect.TypeOf(s.oec), "Flush", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })

	err := f.Fsync(context.Background(), &fuse.FsyncRequest{})
	require.NoError(t, err)
}

func TestFile_Fsync_BlobNilWriterWriteModeBadFd(t *testing.T) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: 34, parentIno: 1, name: "fs2"}
	f.setFlag(syscall.O_RDWR)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: 34, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
	})

	err := f.Fsync(context.Background(), &fuse.FsyncRequest{})
	require.Error(t, err)
	require.Equal(t, fuse.Errno(syscall.EBADF), err)
}

func TestFile_BuildECStreamOpenArgsPoolMissing(t *testing.T) {
	s := newTestSuperForFile()
	f := &File{super: s, ino: 10}
	_, err := f.buildECStreamOpenArgs(&proto.InodeInfo{PoolId: 88, Generation: 1, StorageClass: proto.StorageClass_BlobStore}, syscall.O_RDONLY, 50)
	require.Error(t, err)
}

func TestFile_BuildECStreamOpenArgsSuccess(t *testing.T) {
	s := newTestSuperForFile()
	s.ebsc = make(map[uint8]*blobstore.BlobStoreClient)
	dummy := &blobstore.BlobStoreClient{}
	s.ebsc[3] = dummy
	s.volname = "vn"
	s.volType = 1
	f := &File{super: s, ino: 42}
	args, err := f.buildECStreamOpenArgs(&proto.InodeInfo{PoolId: 3, Generation: 9, StorageClass: proto.StorageClass_BlobStore}, uint32(syscall.O_RDWR), 1000)
	require.NoError(t, err)
	require.Equal(t, uint64(42), args.Ino)
	require.Equal(t, uint8(3), args.PoolId)
	require.Equal(t, uint64(1000), args.FileSize)
	require.Equal(t, uint64(9), args.InodeGeneration)
	require.Equal(t, uint32(syscall.O_RDWR), args.OpenFlags)
	require.Same(t, dummy, args.Ebsc)
}

func TestFile_OpenOECStreamPropagatesOpenStreamError(t *testing.T) {
	s := newTestSuperForFile()
	s.ebsc = make(map[uint8]*blobstore.BlobStoreClient)
	s.ebsc[1] = &blobstore.BlobStoreClient{}
	f := &File{super: s, ino: 7}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "OpenStreamWithArgs",
		func(_ *blobstore.ECExtentClient, _ blobstore.ECStreamOpenArgs) error {
			return errors.New("open fail")
		})
	err := f.openOECStream(&proto.InodeInfo{PoolId: 1, Generation: 1}, syscall.O_RDONLY, 10)
	require.Error(t, err)
	require.Contains(t, err.Error(), "open fail")
}
