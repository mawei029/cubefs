package gosdk

import (
	"context"
	"reflect"
	"syscall"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
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

	patches.ApplyMethod(reflect.TypeOf(&blobstore.Writer{}), "Write",
		func(_ *blobstore.Writer, _ context.Context, _ int, data []byte, flags int) (int, error) {
			require.NotZero(t, flags&proto.FlagsAppend)
			require.NotZero(t, flags&proto.FlagsSyncWrite)
			return len(data), nil
		})

	c := &Client{volType: proto.VolumeTypeCold}
	f := &File{
		client:     c,
		flags:      syscall.O_WRONLY | syscall.O_APPEND,
		ino:        1,
		fileWriter: &blobstore.Writer{},
	}
	n, err := f.WriteFile([]byte("abc"), 0)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}

func TestFileWriteFileColdNonAppend_NoAppendFlags(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyMethod(reflect.TypeOf(&blobstore.Writer{}), "Write",
		func(_ *blobstore.Writer, _ context.Context, _ int, data []byte, flags int) (int, error) {
			require.Zero(t, flags&proto.FlagsAppend)
			require.Zero(t, flags&proto.FlagsSyncWrite)
			return len(data), nil
		})

	c := &Client{volType: proto.VolumeTypeCold}
	f := &File{
		client:     c,
		flags:      syscall.O_WRONLY,
		ino:        1,
		fileWriter: &blobstore.Writer{},
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
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Reader",
		func(_ *blobstore.ECExtentClient, _ uint64) *blobstore.Reader { return &blobstore.Reader{} })
	patches.ApplyMethod(reflect.TypeOf(c.oec), "Writer",
		func(_ *blobstore.ECExtentClient, _ uint64) *blobstore.Writer { return &blobstore.Writer{} })
	patches.ApplyMethod(reflect.TypeOf(c.oec), "CloseStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(c.oec), "EvictStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })

	require.NoError(t, c.openOECStream(f, info, syscall.O_RDWR, info.Size))
	f.fileReader = c.oec.Reader(f.ino)
	f.fileWriter = c.oec.Writer(f.ino)
	require.NotNil(t, f.fileReader)
	require.NotNil(t, f.fileWriter)

	require.NoError(t, c.closeStream(f))
	require.Nil(t, f.fileReader)
	require.Nil(t, f.fileWriter)
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
