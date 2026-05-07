package main

import (
	"testing"

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
