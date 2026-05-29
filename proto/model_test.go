package proto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeDelTreeMaxItemLimit(t *testing.T) {
	t.Parallel()
	require.Equal(t, uint64(0), NormalizeDelTreeMaxItemLimit(0))
	require.Equal(t, MinDelTreeMaxItemLimit, NormalizeDelTreeMaxItemLimit(1))
	require.Equal(t, MinDelTreeMaxItemLimit, NormalizeDelTreeMaxItemLimit(50_000))
	require.Equal(t, MinDelTreeMaxItemLimit, NormalizeDelTreeMaxItemLimit(MinDelTreeMaxItemLimit))
	require.Equal(t, uint64(250_000), NormalizeDelTreeMaxItemLimit(250_000))
}
