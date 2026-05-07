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
