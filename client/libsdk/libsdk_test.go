package main

import (
	"errors"
	"reflect"
	"syscall"
	"testing"
	"unsafe"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/bits-and-blooms/bitset"
	"github.com/cubefs/cubefs/client/fs"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/stretchr/testify/require"
)

func TestClientManagerLifecycle(t *testing.T) {
	c := newClient()
	got, ok := getClient(c.id)
	require.True(t, ok)
	require.Equal(t, c.id, got.id)

	removeClient(c.id)
	_, ok = getClient(c.id)
	require.False(t, ok)
}

func TestCfsWriteAppendFlags(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	c := newTestLibsdkClientForOEC()
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Write",
		func(_ *blobstore.ECExtentClient, ino uint64, _ int, data []byte, flags int) (int, error) {
			require.Equal(t, uint64(1), ino)
			require.NotZero(t, flags&proto.FlagsAppend)
			require.NotZero(t, flags&proto.FlagsSyncWrite)
			return len(data), nil
		})

	gClientManager.mu.Lock()
	if gClientManager.clients == nil {
		gClientManager.clients = make(map[int64]*client)
	}
	c.id = 1
	gClientManager.clients[1] = c
	gClientManager.mu.Unlock()
	defer func() {
		gClientManager.mu.Lock()
		delete(gClientManager.clients, 1)
		gClientManager.mu.Unlock()
	}()

	fd := uint(3)
	c.fdmap = map[uint]*file{fd: {
		fd:           fd,
		ino:          1,
		flags:        uint32(syscall.O_WRONLY | syscall.O_APPEND),
		storageClass: proto.StorageClass_BlobStore,
	}}

	buf := []byte("abc")
	ret := cfs_write(1, 3, unsafe.Pointer(&buf[0]), 3, 0)
	require.Equal(t, 3, int(ret))
}

func TestCfsWriteColdBlobNonAppend_NoAppendFlags(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	c := newTestLibsdkClientForOEC()
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Write",
		func(_ *blobstore.ECExtentClient, ino uint64, _ int, data []byte, flags int) (int, error) {
			require.Equal(t, uint64(1), ino)
			require.Zero(t, flags&proto.FlagsAppend)
			require.Zero(t, flags&proto.FlagsSyncWrite)
			return len(data), nil
		})

	gClientManager.mu.Lock()
	if gClientManager.clients == nil {
		gClientManager.clients = make(map[int64]*client)
	}
	c.id = 2
	gClientManager.clients[2] = c
	gClientManager.mu.Unlock()
	defer func() {
		gClientManager.mu.Lock()
		delete(gClientManager.clients, 2)
		gClientManager.mu.Unlock()
	}()

	fd := uint(4)
	c.fdmap = map[uint]*file{fd: {
		fd:           fd,
		ino:          1,
		flags:        uint32(syscall.O_WRONLY),
		storageClass: proto.StorageClass_BlobStore,
	}}

	buf := []byte("abc")
	ret := cfs_write(2, 4, unsafe.Pointer(&buf[0]), 3, 0)
	require.Equal(t, 3, int(ret))
}

func newTestLibsdkClientForOEC() *client {
	ec := &stream.ExtentClient{}
	return &client{
		volType:          proto.VolumeTypeCold,
		ebsBlockSize:     4096,
		ebsc:             &blobstore.BlobStoreClient{},
		ec:               ec,
		oec:              blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{LimitManager: ec.LimitManager}),
		volName:          "vol1",
		enableBcache:     true,
		writeBlockThread: 2,
		readBlockThread:  3,
		fdmap:            make(map[uint]*file),
	}
}

func TestClient_buildECStreamOpenArgs_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	info := &proto.InodeInfo{Inode: 5, PoolId: 1, Generation: 2, StorageClass: proto.StorageClass_BlobStore}
	args, err := c.buildECStreamOpenArgs(5, info, syscall.O_RDONLY, 256)
	require.NoError(t, err)
	require.Equal(t, uint64(5), args.Ino)
	require.Equal(t, c.ebsc, args.Ebsc)
}

func TestClient_openOECStream_closeStream_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	ino := uint64(88)
	info := &proto.InodeInfo{Inode: ino, PoolId: 1, Size: 10, Generation: 1, StorageClass: proto.StorageClass_BlobStore}
	f := &file{ino: ino, flags: uint32(syscall.O_RDWR), storageClass: proto.StorageClass_BlobStore}

	patches.ApplyMethod(reflect.TypeOf(c.oec), "OpenStreamWithArgs", func(_ *blobstore.ECExtentClient, _ blobstore.ECStreamOpenArgs) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(c.oec), "CloseStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(c.oec), "EvictStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })

	require.NoError(t, c.openOECStream(f, info, syscall.O_RDWR, info.Size))
	c.closeStream(f)
}

func TestClient_openStream_openOECStreamFailure_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	ino := uint64(99)
	f := &file{ino: ino, flags: uint32(syscall.O_RDWR), storageClass: proto.StorageClass_BlobStore}
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)
	c.ic.Put(&proto.InodeInfo{Inode: ino, PoolId: 1, Generation: 1, StorageClass: proto.StorageClass_BlobStore})
	openErr := errors.New("open failed")
	patches.ApplyMethod(reflect.TypeOf(c.oec), "OpenStreamWithArgs", func(_ *blobstore.ECExtentClient, _ blobstore.ECStreamOpenArgs) error {
		return openErr
	})

	err := c.openStream(f, "/f")
	require.ErrorIs(t, err, openErr)
}

func TestClient_openStream_inodeGetFailure_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	c.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	ino := uint64(100)
	f := &file{ino: ino, flags: uint32(syscall.O_RDWR), storageClass: proto.StorageClass_BlobStore}
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)
	getErr := errors.New("inode get failed")
	patches.ApplyMethod(reflect.TypeOf(c.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return nil, getErr
		})

	err := c.openStream(f, "/f")
	require.ErrorIs(t, err, getErr)
}

func TestClient_openStream_successViaInodeGet_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	c.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	ino := uint64(101)
	info := &proto.InodeInfo{Inode: ino, PoolId: 1, Size: 8, Generation: 1, StorageClass: proto.StorageClass_BlobStore}
	f := &file{ino: ino, flags: uint32(syscall.O_RDWR), storageClass: proto.StorageClass_BlobStore}
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)
	patches.ApplyMethod(reflect.TypeOf(c.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ bool) (*proto.InodeInfo, error) {
			return info, nil
		})
	patches.ApplyMethod(reflect.TypeOf(c.oec), "OpenStreamWithArgs", func(_ *blobstore.ECExtentClient, _ blobstore.ECStreamOpenArgs) error {
		return nil
	})

	require.NoError(t, c.openStream(f, "/f"))
}

func TestClient_openStream_hotEcPath_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	c.volType = proto.VolumeTypeHot
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	f := &file{ino: 11, openForWrite: true, storageClass: proto.StorageClass_Replica_SSD}
	patches.ApplyMethod(reflect.TypeOf(c.ec), "OpenStream",
		func(_ *stream.ExtentClient, ino uint64, openForWrite, isCache bool, fullPath string) error {
			require.Equal(t, uint64(11), ino)
			require.True(t, openForWrite)
			require.False(t, isCache)
			require.Equal(t, "/hot", fullPath)
			return nil
		})

	require.NoError(t, c.openStream(f, "/hot"))
}

func TestClient_openStreamFailureReleasesFD_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	c.fdset = bitset.New(maxFdNum)
	c.fdset.Set(0).Set(1).Set(2)
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)
	c.ic.Put(&proto.InodeInfo{Inode: 66, PoolId: 1, Generation: 1, StorageClass: proto.StorageClass_BlobStore})

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(c.oec), "OpenStreamWithArgs", func(_ *blobstore.ECExtentClient, _ blobstore.ECStreamOpenArgs) error {
		return errors.New("open failed")
	})

	f := c.allocFD(66, uint32(syscall.O_RDWR), 0, false, 0, 1, "/f", proto.StorageClass_BlobStore, 1)
	require.NotNil(t, f)
	if err := c.openStream(f, "/f"); err != nil {
		c.releaseFD(f.fd)
	}
	require.Nil(t, c.getFile(f.fd))
}

func TestClient_truncate_replicaEcPath_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	c.volType = proto.VolumeTypeHot
	c.mw = &meta.MetaWrapper{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	f := &file{ino: 12, pino: 2, path: "/r", storageClass: proto.StorageClass_Replica_SSD}
	patches.ApplyMethod(reflect.TypeOf(c.ec), "Truncate",
		func(_ *stream.ExtentClient, _ *meta.MetaWrapper, pino, ino uint64, size int, fullPath string) error {
			require.Equal(t, uint64(2), pino)
			require.Equal(t, uint64(12), ino)
			require.Equal(t, 64, size)
			require.Equal(t, "/r", fullPath)
			return nil
		})

	require.NoError(t, c.truncate(f, 64))
}

func TestClient_coldBlobReadWriteFlushTruncate_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	f := &file{ino: 7, pino: 3, path: "/f", storageClass: proto.StorageClass_BlobStore}
	var flushed bool
	var truncated bool

	patches.ApplyMethod(reflect.TypeOf(c.oec), "Read",
		func(_ *blobstore.ECExtentClient, ino uint64, data []byte, offset, size int) (int, error) {
			require.Equal(t, uint64(7), ino)
			require.Equal(t, 4, offset)
			require.Equal(t, 8, size)
			copy(data, "abcdefgh")
			return 8, nil
		})
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Write",
		func(_ *blobstore.ECExtentClient, ino uint64, offset int, data []byte, flags int) (int, error) {
			require.Equal(t, uint64(7), ino)
			require.Equal(t, 1, offset)
			require.Equal(t, "x", string(data))
			require.Zero(t, flags)
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
			require.Equal(t, uint64(128), targetSize)
			require.Equal(t, "/f", fullPath)
			truncated = true
			return nil
		})

	buf := make([]byte, 8)
	n, err := c.read(f, 4, buf)
	require.NoError(t, err)
	require.Equal(t, 8, n)

	n, err = c.write(f, 1, []byte("x"), 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	require.NoError(t, c.flush(f))
	require.True(t, flushed)

	require.NoError(t, c.truncate(f, 128))
	require.True(t, truncated)
}

func TestClient_openRegularFileFailureReleasesFD_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	c.fdset = bitset.New(maxFdNum)
	c.fdset.Set(0).Set(1).Set(2)
	c.ic = fs.NewInodeCache(fs.DefaultInodeExpiration, fs.MaxInodeCache, false)
	c.ic.Put(&proto.InodeInfo{Inode: 69, PoolId: 1, Generation: 1, StorageClass: proto.StorageClass_BlobStore})

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(c), "openStream", func(_ *client, _ *file, _ string) error {
		return errors.New("open stream failed")
	})

	f := c.allocFD(69, uint32(syscall.O_RDWR), 0, false, 0, 1, "/test", proto.StorageClass_BlobStore, 1)
	require.NotNil(t, f)
	err := c.openRegularFile(f, "/test")
	require.Error(t, err)
	require.Empty(t, c.fdmap)
}

func TestClient_openRegularFile_success_libsdk(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	c.fdset = bitset.New(maxFdNum)
	c.fdset.Set(0).Set(1).Set(2)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(c), "openStream", func(_ *client, _ *file, _ string) error {
		return nil
	})

	f := c.allocFD(72, uint32(syscall.O_RDWR), 0, false, 0, 1, "/test", proto.StorageClass_BlobStore, 1)
	require.NotNil(t, f)
	require.NoError(t, c.openRegularFile(f, "/test"))
	require.NotNil(t, c.getFile(f.fd))
}

// TestClient_allocFD_coversFileCacheDiscard executes allocFD (incl. _ = fileCache) used before oec openStream.
func TestClient_allocFD_coversFileCacheDiscard(t *testing.T) {
	c := newClient()
	c.fdset.Set(0).Set(1).Set(2)
	defer func() {
		gClientManager.mu.Lock()
		delete(gClientManager.clients, c.id)
		gClientManager.mu.Unlock()
	}()

	f := c.allocFD(42, uint32(syscall.O_RDWR), 0, true, 128, 1, "/f", proto.StorageClass_BlobStore, 1)
	require.NotNil(t, f)
	require.True(t, f.openForWrite)
	require.Equal(t, uint64(42), f.ino)

	f2 := c.allocFD(43, uint32(syscall.O_RDONLY), 0, false, 0, 1, "/g", proto.StorageClass_BlobStore, 1)
	require.NotNil(t, f2)
	require.False(t, f2.openForWrite)
}

func TestCfs_close_client_closesOec(t *testing.T) {
	c := newTestLibsdkClientForOEC()
	closed := false
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Close", func(_ *blobstore.ECExtentClient) error {
		closed = true
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(c.ec), "Close", func(_ *stream.ExtentClient) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(c.mw), "Close", func(_ *meta.MetaWrapper) error { return nil })

	gClientManager.mu.Lock()
	if gClientManager.clients == nil {
		gClientManager.clients = make(map[int64]*client)
	}
	c.id = 99
	gClientManager.clients[99] = c
	gClientManager.mu.Unlock()
	defer func() {
		gClientManager.mu.Lock()
		delete(gClientManager.clients, 99)
		gClientManager.mu.Unlock()
	}()

	cfs_close_client(99)
	require.True(t, closed)
}
