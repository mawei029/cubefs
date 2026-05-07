package gosdk

import (
	"syscall"
	"testing"

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
