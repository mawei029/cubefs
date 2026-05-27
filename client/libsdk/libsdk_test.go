package main

import (
	"context"
	"reflect"
	"syscall"
	"testing"
	"unsafe"

	"github.com/agiledragon/gomonkey/v2"
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

	patches.ApplyMethod(reflect.TypeOf(&blobstore.Writer{}), "Write",
		func(_ *blobstore.Writer, _ context.Context, _ int, data []byte, flags int) (int, error) {
			require.NotZero(t, flags&proto.FlagsAppend)
			require.NotZero(t, flags&proto.FlagsSyncWrite)
			return len(data), nil
		})

	c := &client{
		id:      1,
		volType: proto.VolumeTypeCold,
		fdmap:   make(map[uint]*file),
	}
	gClientManager.mu.Lock()
	if gClientManager.clients == nil {
		gClientManager.clients = make(map[int64]*client)
	}
	gClientManager.clients[1] = c
	gClientManager.mu.Unlock()
	defer func() {
		gClientManager.mu.Lock()
		delete(gClientManager.clients, 1)
		gClientManager.mu.Unlock()
	}()

	fd := uint(3)
	c.fdmap[fd] = &file{
		fd:           fd,
		ino:          1,
		flags:        uint32(syscall.O_WRONLY | syscall.O_APPEND),
		storageClass: proto.StorageClass_BlobStore,
		fileWriter:   &blobstore.Writer{},
	}

	buf := []byte("abc")
	ret := cfs_write(1, 3, unsafe.Pointer(&buf[0]), 3, 0)
	require.Equal(t, 3, int(ret))
}

func TestCfsWriteColdBlobNonAppend_NoAppendFlags(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyMethod(reflect.TypeOf(&blobstore.Writer{}), "Write",
		func(_ *blobstore.Writer, _ context.Context, _ int, data []byte, flags int) (int, error) {
			require.Zero(t, flags&proto.FlagsAppend)
			require.Zero(t, flags&proto.FlagsSyncWrite)
			return len(data), nil
		})

	c := &client{
		id:      2,
		volType: proto.VolumeTypeCold,
		fdmap:   make(map[uint]*file),
	}
	gClientManager.mu.Lock()
	if gClientManager.clients == nil {
		gClientManager.clients = make(map[int64]*client)
	}
	gClientManager.clients[2] = c
	gClientManager.mu.Unlock()
	defer func() {
		gClientManager.mu.Lock()
		delete(gClientManager.clients, 2)
		gClientManager.mu.Unlock()
	}()

	fd := uint(4)
	c.fdmap[fd] = &file{
		fd:           fd,
		ino:          1,
		flags:        uint32(syscall.O_WRONLY),
		storageClass: proto.StorageClass_BlobStore,
		fileWriter:   &blobstore.Writer{},
	}

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
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Reader", func(_ *blobstore.ECExtentClient, _ uint64) *blobstore.Reader { return &blobstore.Reader{} })
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Writer", func(_ *blobstore.ECExtentClient, _ uint64) *blobstore.Writer { return &blobstore.Writer{} })
	patches.ApplyMethod(reflect.TypeOf(c.oec), "CloseStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(c.oec), "EvictStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })

	require.NoError(t, c.openOECStream(f, info, syscall.O_RDWR, info.Size))
	f.fileReader = c.oec.Reader(f.ino)
	f.fileWriter = c.oec.Writer(f.ino)
	require.NotNil(t, f.fileReader)
	c.closeStream(f)
	require.Nil(t, f.fileReader)
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
