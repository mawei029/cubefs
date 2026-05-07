package gosdk

import (
	"context"
	"reflect"
	"syscall"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
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
