package stream

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStreamer_evict_blockedByPendingRequest(t *testing.T) {
	ec := &ExtentClient{
		streamers: make(map[uint64]*Streamer),
	}
	s := &Streamer{
		client:  ec,
		inode:   42,
		refcnt:  0,
		request: make(chan interface{}, 2),
	}
	s.request <- &OpenRequest{done: make(chan struct{})}

	err := s.evict()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "refcnt(0)"))
	require.False(t, strings.Contains(err.Error(), "requestQLen"))
}
