package gosdk

import (
	"errors"
	"reflect"
	"syscall"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/bits-and-blooms/bitset"
	"github.com/cubefs/cubefs/blobstore/api/access"
	"github.com/cubefs/cubefs/client/fs"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	masterSDK "github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/buf"
	"github.com/stretchr/testify/require"
)

func TestFileWriteFilePermissionChecks(t *testing.T) {
	f := &File{flags: syscall.O_RDONLY}
	n, err := f.WriteFile([]byte("x"), 0)
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, syscall.EACCES)

	f.closed = true
	n, err = f.WriteFile([]byte("x"), 0)
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, syscall.EBADFD)
}

func TestFileWriteFileAppendFlags(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	c := newTestClientForOEC()
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Write",
		func(_ *blobstore.ECExtentClient, ino uint64, _ int, data []byte, flags int) (int, error) {
			require.Equal(t, uint64(1), ino)
			require.NotZero(t, flags&proto.FlagsAppend)
			require.NotZero(t, flags&proto.FlagsSyncWrite)
			return len(data), nil
		})

	f := &File{
		client:       c,
		flags:        syscall.O_WRONLY | syscall.O_APPEND,
		ino:          1,
		storageClass: proto.StorageClass_BlobStore,
	}
	n, err := f.WriteFile([]byte("abc"), 0)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}

func TestFileWriteFileColdNonAppend_NoAppendFlags(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	c := newTestClientForOEC()
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Write",
		func(_ *blobstore.ECExtentClient, ino uint64, _ int, data []byte, flags int) (int, error) {
			require.Equal(t, uint64(1), ino)
			require.Zero(t, flags&proto.FlagsAppend)
			require.Zero(t, flags&proto.FlagsSyncWrite)
			return len(data), nil
		})

	f := &File{
		client:       c,
		flags:        syscall.O_WRONLY,
		ino:          1,
		storageClass: proto.StorageClass_BlobStore,
	}
	n, err := f.WriteFile([]byte("abc"), 0)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}

func newTestClientForOEC() *Client {
	ec := &stream.ExtentClient{}
	return &Client{
		volType:      proto.VolumeTypeCold,
		ebsBlockSize: 4096,
		ebsc:         &blobstore.BlobStoreClient{},
		ec:           ec,
		oec:          blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{LimitManager: ec.LimitManager}),
		cfg:          Config{VolName: "vol1", EnableBcache: true, WriteBlockThread: 2, ReadBlockThread: 3},
	}
}

func TestClient_buildECStreamOpenArgs(t *testing.T) {
	c := newTestClientForOEC()
	info := &proto.InodeInfo{Inode: 9, PoolId: 1, Generation: 2, StorageClass: proto.StorageClass_BlobStore}
	args, err := c.buildECStreamOpenArgs(9, info, syscall.O_RDWR, 128)
	require.NoError(t, err)
	require.Equal(t, uint64(9), args.Ino)
	require.Equal(t, uint8(1), args.PoolId)
	require.Equal(t, uint64(128), args.FileSize)
	require.Equal(t, c.ebsc, args.Ebsc)
	require.Equal(t, c.oec.LimitManager, args.LimitManager)
}

func TestClient_buildECStreamOpenArgs_noEbsc(t *testing.T) {
	c := newTestClientForOEC()
	c.ebsc = nil
	_, err := c.buildECStreamOpenArgs(1, &proto.InodeInfo{}, 0, 0)
	require.Error(t, err)
}

func TestClient_openOECStream_and_closeStream_coldBlob(t *testing.T) {
	c := newTestClientForOEC()
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	ino := uint64(77)
	info := &proto.InodeInfo{Inode: ino, PoolId: 1, Size: 64, Generation: 1, StorageClass: proto.StorageClass_BlobStore}
	f := &File{client: c, ino: ino, flags: syscall.O_RDWR, storageClass: proto.StorageClass_BlobStore}

	patches.ApplyMethod(reflect.TypeOf(c.oec), "OpenStreamWithArgs",
		func(_ *blobstore.ECExtentClient, args blobstore.ECStreamOpenArgs) error {
			require.Equal(t, ino, args.Ino)
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(c.oec), "CloseStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(c.oec), "EvictStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })

	require.NoError(t, c.openOECStream(f, info, syscall.O_RDWR, info.Size))
	require.NoError(t, c.closeStream(f))
}

func TestClient_openStream_openOECStreamFailure(t *testing.T) {
	c := newTestClientForOEC()
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	f := &File{client: c, ino: 55, flags: syscall.O_RDWR, storageClass: proto.StorageClass_BlobStore}
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)
	c.ic.Put(&proto.InodeInfo{Inode: 55, PoolId: 1, Generation: 1, StorageClass: proto.StorageClass_BlobStore})
	openErr := errors.New("open failed")
	patches.ApplyMethod(reflect.TypeOf(c.oec), "OpenStreamWithArgs", func(_ *blobstore.ECExtentClient, _ blobstore.ECStreamOpenArgs) error {
		return openErr
	})

	err := c.openStream(f, true, "/f")
	require.ErrorIs(t, err, openErr)
}

func TestClient_openStream_inodeGetFailure(t *testing.T) {
	c := newTestClientForOEC()
	c.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	f := &File{client: c, ino: 56, flags: syscall.O_RDWR, storageClass: proto.StorageClass_BlobStore}
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)
	getErr := errors.New("inode get failed")
	patches.ApplyMethod(reflect.TypeOf(c.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return nil, getErr
		})

	err := c.openStream(f, true, "/f")
	require.ErrorIs(t, err, getErr)
}

func TestClient_openStream_successViaInodeGet(t *testing.T) {
	c := newTestClientForOEC()
	c.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	ino := uint64(57)
	info := &proto.InodeInfo{Inode: ino, PoolId: 1, Size: 16, Generation: 1, StorageClass: proto.StorageClass_BlobStore}
	f := &File{client: c, ino: ino, flags: syscall.O_RDWR, storageClass: proto.StorageClass_BlobStore}
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)
	patches.ApplyMethod(reflect.TypeOf(c.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return info, nil
		})
	patches.ApplyMethod(reflect.TypeOf(c.oec), "OpenStreamWithArgs", func(_ *blobstore.ECExtentClient, _ blobstore.ECStreamOpenArgs) error {
		return nil
	})

	require.NoError(t, c.openStream(f, true, "/f"))
}

func TestClient_openStream_hotEcPath(t *testing.T) {
	c := newTestClientForOEC()
	c.volType = proto.VolumeTypeHot
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	f := &File{client: c, ino: 21, flags: syscall.O_RDWR, storageClass: proto.StorageClass_Replica_SSD}
	patches.ApplyMethod(reflect.TypeOf(c.ec), "OpenStream",
		func(_ *stream.ExtentClient, ino uint64, openForWrite, isCache bool, fullPath string) error {
			require.Equal(t, uint64(21), ino)
			require.True(t, openForWrite)
			require.False(t, isCache)
			require.Equal(t, "/hot", fullPath)
			return nil
		})

	require.NoError(t, c.openStream(f, true, "/hot"))
}

func TestClient_openStreamFailureReleasesFD(t *testing.T) {
	c := newTestClientForOEC()
	c.fdmap = make(map[uint]*File)
	c.fdset = bitset.New(maxFdNum)
	c.fdset.Set(0).Set(1).Set(2)
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)
	c.ic.Put(&proto.InodeInfo{Inode: 67, PoolId: 1, Generation: 1, StorageClass: proto.StorageClass_BlobStore})

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(c.oec), "OpenStreamWithArgs", func(_ *blobstore.ECExtentClient, _ blobstore.ECStreamOpenArgs) error {
		return errors.New("open failed")
	})

	f := c.allocFD(67, syscall.O_RDWR, 0, false, 0, 1, "/f", proto.StorageClass_BlobStore, 1)
	require.NotNil(t, f)
	if err := c.openStream(f, true, "/f"); err != nil {
		c.releaseFD(f.fd)
	}
	require.Nil(t, c.getFile(f.fd))
}

func TestClient_closeStream_hotEcPath(t *testing.T) {
	c := newTestClientForOEC()
	c.volType = proto.VolumeTypeHot
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	f := &File{client: c, ino: 22, storageClass: proto.StorageClass_Replica_SSD}
	closeErr := errors.New("close failed")
	patches.ApplyMethod(reflect.TypeOf(c.ec), "CloseStream", func(_ *stream.ExtentClient, ino uint64) error {
		require.Equal(t, uint64(22), ino)
		return closeErr
	})

	err := c.closeStream(f)
	require.ErrorIs(t, err, closeErr)
}

func TestClient_closeStream_hotEcSuccess(t *testing.T) {
	c := newTestClientForOEC()
	c.volType = proto.VolumeTypeHot
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	f := &File{client: c, ino: 24, storageClass: proto.StorageClass_Replica_SSD}
	patches.ApplyMethod(reflect.TypeOf(c.ec), "CloseStream", func(_ *stream.ExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(c.ec), "EvictStream", func(_ *stream.ExtentClient, ino uint64) error {
		require.Equal(t, uint64(24), ino)
		return nil
	})

	require.NoError(t, c.closeStream(f))
}

func TestClient_openRegularFile_success(t *testing.T) {
	c := newTestClientForOEC()
	c.fdmap = make(map[uint]*File)
	c.fdset = bitset.New(maxFdNum)
	c.fdset.Set(0).Set(1).Set(2)
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(c), "openStream",
		func(_ *Client, _ *File, _ bool, _ string) error {
			return nil
		})

	f := c.allocFD(71, syscall.O_RDWR, 0, false, 0, 1, "/test", proto.StorageClass_BlobStore, 1)
	require.NotNil(t, f)
	require.NoError(t, c.openRegularFile(f, true, "/test"))
	require.NotNil(t, c.getFile(f.fd))
}

func TestClient_OpenFile_openStreamFailureReleasesFD(t *testing.T) {
	c := newTestClientForOEC()
	c.fdmap = make(map[uint]*File)
	c.fdset = bitset.New(maxFdNum)
	c.fdset.Set(0).Set(1).Set(2)
	c.cwd = "/"
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	dirInfo := &proto.InodeInfo{Inode: 1, Mode: uint32(syscall.S_IFDIR | 0o755)}
	fileInfo := &proto.InodeInfo{Inode: 68, Mode: uint32(syscall.S_IFREG | 0o644), StorageClass: proto.StorageClass_BlobStore}
	patches.ApplyPrivateMethod(reflect.TypeOf(c), "lookupPath",
		func(_ *Client, path string) (*proto.InodeInfo, error) {
			switch path {
			case "/":
				return dirInfo, nil
			case "/test":
				return fileInfo, nil
			default:
				return nil, syscall.ENOENT
			}
		})
	openErr := errors.New("open stream failed")
	patches.ApplyPrivateMethod(reflect.TypeOf(c), "openStream",
		func(_ *Client, _ *File, _ bool, _ string) error {
			return openErr
		})

	f, err := c.OpenFile("/test", syscall.O_RDWR, 0o644)
	require.ErrorIs(t, err, openErr)
	require.Nil(t, f)
	require.Empty(t, c.fdmap)
}

func TestClient_truncate_replicaEcPath(t *testing.T) {
	c := newTestClientForOEC()
	c.volType = proto.VolumeTypeHot
	c.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	f := &File{client: c, ino: 23, pino: 4, path: "/r", storageClass: proto.StorageClass_Replica_SSD}
	patches.ApplyMethod(reflect.TypeOf(c.ec), "Truncate",
		func(_ *stream.ExtentClient, _ *meta.MetaWrapper, pino, ino uint64, size int, fullPath string) error {
			require.Equal(t, uint64(4), pino)
			require.Equal(t, uint64(23), ino)
			require.Equal(t, 32, size)
			require.Equal(t, "/r", fullPath)
			return nil
		})

	require.NoError(t, c.truncate(f, 32))
}

func TestClient_coldBlobReadWriteFlushTruncate(t *testing.T) {
	c := newTestClientForOEC()
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	f := &File{client: c, ino: 7, pino: 3, path: "/f", storageClass: proto.StorageClass_BlobStore}
	var flushed bool
	var truncated bool

	patches.ApplyMethod(reflect.TypeOf(c.oec), "Read",
		func(_ *blobstore.ECExtentClient, ino uint64, data []byte, offset, size int) (int, error) {
			require.Equal(t, uint64(7), ino)
			require.Equal(t, 2, offset)
			require.Equal(t, 4, size)
			copy(data, "abcd")
			return 4, nil
		})
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Write",
		func(_ *blobstore.ECExtentClient, ino uint64, offset int, data []byte, flags int) (int, error) {
			require.Equal(t, uint64(7), ino)
			require.Equal(t, 10, offset)
			return len(data), nil
		})
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Flush",
		func(_ *blobstore.ECExtentClient, ino uint64) error {
			require.Equal(t, uint64(7), ino)
			flushed = true
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Truncate",
		func(_ *blobstore.ECExtentClient, parentIno, ino, targetSize uint64, fullPath string) error {
			require.Equal(t, uint64(3), parentIno)
			require.Equal(t, uint64(7), ino)
			require.Equal(t, uint64(64), targetSize)
			require.Equal(t, "/f", fullPath)
			truncated = true
			return nil
		})

	buf := make([]byte, 4)
	n, err := c.read(f, 2, buf)
	require.NoError(t, err)
	require.Equal(t, 4, n)

	n, err = c.write(f, 10, []byte("z"), 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	require.NoError(t, c.flush(f))
	require.True(t, flushed)

	require.NoError(t, c.truncate(f, 64))
	require.True(t, truncated)
}

func testClientWithFDSet() *Client {
	c := &Client{
		fdmap: make(map[uint]*File),
		fdset: bitset.New(maxFdNum),
	}
	c.fdset.Set(0).Set(1).Set(2)
	return c
}

// TestClient_allocFD_coversFileCacheDiscard executes allocFD (incl. _ = fileCache) on the EC/Blob open path.
func TestClient_allocFD_coversFileCacheDiscard(t *testing.T) {
	c := testClientWithFDSet()
	f := c.allocFD(42, syscall.O_RDWR, 0, true, 128, 1, "/f", proto.StorageClass_BlobStore, 1)
	require.NotNil(t, f)
	require.Equal(t, uint64(42), f.ino)
	require.Equal(t, uint(3), f.fd)

	f2 := c.allocFD(43, syscall.O_RDONLY, 0, false, 0, 1, "/g", proto.StorageClass_BlobStore, 1)
	require.NotNil(t, f2)
	require.Equal(t, syscall.O_RDONLY, f2.flags&syscall.O_ACCMODE)
}

func TestClient_Close_closesOec(t *testing.T) {
	c := newTestClientForOEC()
	closed := false
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Close", func(_ *blobstore.ECExtentClient) error {
		closed = true
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(c.ec), "Close", func(_ *stream.ExtentClient) error { return nil })
	c.Close()
	require.True(t, closed)
}

func TestClient_loadConfFromMaster_initCachePool(t *testing.T) {
	const objBlockSize = 1 << 23
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	mc := masterSDK.NewMasterClient([]string{"127.0.0.1:1"}, false)
	patches.ApplyFunc(masterSDK.NewMasterClient, func(_ []string, _ bool) *masterSDK.MasterClient {
		return mc
	})
	admin := mc.AdminAPI()
	patches.ApplyMethod(reflect.TypeOf(admin), "GetVolumeSimpleInfo",
		func(_ *masterSDK.AdminAPI, vol string) (*proto.SimpleVolView, error) {
			require.Equal(t, "cold-vol", vol)
			return &proto.SimpleVolView{
				VolType:             proto.VolumeTypeCold,
				ObjBlockSize:        objBlockSize,
				VolStorageClass:     proto.StorageClass_BlobStore,
				AllowedStorageClass: []uint32{proto.StorageClass_BlobStore},
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(admin), "GetClusterInfo",
		func(_ *masterSDK.AdminAPI) (*proto.ClusterInfo, error) {
			return &proto.ClusterInfo{
				EbsAddr:             "http://ebs",
				ServicePath:         "/svc",
				Cluster:             "cluster1",
				DirChildrenNumLimit: proto.DefaultDirChildrenNumLimit,
			}, nil
		})

	c := &Client{cfg: Config{VolName: "cold-vol"}}
	require.NoError(t, c.loadConfFromMaster([]string{"127.0.0.1:1"}))
	require.Equal(t, objBlockSize, c.ebsBlockSize)
	require.Equal(t, proto.VolumeTypeCold, c.volType)
	require.NotNil(t, buf.CachePool)

	b := buf.CachePool.Get()
	require.Equal(t, objBlockSize, len(b))
	require.Equal(t, objBlockSize, cap(b))
	buf.CachePool.Put(b)
}

func TestClientStartNewEbsClientDefaultTimeout(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	gotTimeout := -1
	patches.ApplyFunc(blobstore.NewEbsClient, func(_ access.Config, maxTimeoutSec int) (*blobstore.BlobStoreClient, error) {
		gotTimeout = maxTimeoutSec
		return &blobstore.BlobStoreClient{}, nil
	})
	patches.ApplyPrivateMethod(reflect.TypeOf(&Client{}), "loadConfFromMaster", func(c *Client, _ []string) error {
		c.ebsEndpoint = "127.0.0.1:1"
		return nil
	})
	patches.ApplyPrivateMethod(reflect.TypeOf(&Client{}), "checkPermission", func(_ *Client) error {
		return nil
	})
	patches.ApplyFunc(meta.NewMetaWrapper, func(_ *meta.MetaConfig) (*meta.MetaWrapper, error) {
		return &meta.MetaWrapper{}, nil
	})
	patches.ApplyFunc(stream.NewExtentClient, func(_ *stream.ExtentConfig) (*stream.ExtentClient, error) {
		return &stream.ExtentClient{}, nil
	})
	patches.ApplyFunc(blobstore.NewObjExtentClient, func(_ blobstore.ObjExtentConfig) *blobstore.ECExtentClient {
		return &blobstore.ECExtentClient{}
	})

	c := New(Config{MasterAddr: "127.0.0.1:1", VolName: "test-vol"})
	require.NoError(t, c.Start())
	require.Equal(t, 0, gotTimeout)
}
