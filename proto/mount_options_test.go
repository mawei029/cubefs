package proto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMountOptions_ebsCacheAndAheadReadDefaults(t *testing.T) {
	opts := NewMountOptions()
	InitMountOptions(opts)
	require.Equal(t, int64(512), opts[EbsBufferCacheLimit].GetInt64())
	require.Equal(t, int64(4), opts[AheadReadTotalMemGB].GetInt64())
}
