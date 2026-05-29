// Copyright 2026 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package metanode

import (
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/proto"
)

func TestFollowerReadLeaseTime_defaultFallback(t *testing.T) {
	atomic.StoreUint64(&nodeInfo.followerReadLeaseTime, 0)
	t.Cleanup(func() {
		atomic.StoreUint64(&nodeInfo.followerReadLeaseTime, 0)
	})

	require.EqualValues(t, proto.DefaultFollowerReadLeaseTimeSec, FollowerReadLeaseTime())
	require.EqualValues(t, proto.DefaultFollowerReadLeaseTimeSec, DefaultFollowerReadLeaseTime)
}

func TestUpdateFollowerReadLeaseTime(t *testing.T) {
	t.Cleanup(func() {
		atomic.StoreUint64(&nodeInfo.followerReadLeaseTime, 0)
	})

	updateFollowerReadLeaseTime(proto.DefaultFollowerReadLeaseTimeSec)
	require.EqualValues(t, proto.DefaultFollowerReadLeaseTimeSec, FollowerReadLeaseTime())

	updateFollowerReadLeaseTime(proto.MaxFollowerReadLeaseTimeSec)
	require.EqualValues(t, proto.MaxFollowerReadLeaseTimeSec, FollowerReadLeaseTime())

	updateFollowerReadLeaseTime(proto.MaxFollowerReadLeaseTimeSec + 500)
	require.EqualValues(t, proto.MaxFollowerReadLeaseTimeSec, FollowerReadLeaseTime())

	updateFollowerReadLeaseTime(1)
	require.EqualValues(t, proto.MinFollowerReadLeaseTimeSec, FollowerReadLeaseTime())
}

func swapDelTreeMaxItemLimit(maxLimit uint64) func() {
	prev := atomic.LoadUint64(&delTreeMaxItemLimit)
	updateDelTreeMaxItemLimit(maxLimit)
	return func() {
		atomic.StoreUint64(&delTreeMaxItemLimit, prev)
	}
}

// swapDelTreeMaxItemLimitRaw stores cap without normalize (UT only, e.g. cap=1 queue-full).
func swapDelTreeMaxItemLimitRaw(maxLimit uint64) func() {
	prev := atomic.LoadUint64(&delTreeMaxItemLimit)
	atomic.StoreUint64(&delTreeMaxItemLimit, maxLimit)
	return func() {
		atomic.StoreUint64(&delTreeMaxItemLimit, prev)
	}
}

func TestDelTreeMaxItemLimitPolicy(t *testing.T) {
	defer swapDelTreeMaxItemLimit(0)()

	updateDelTreeMaxItemLimit(200_000)
	require.True(t, DelTreeEnqueueLimitEnabled())
	require.Equal(t, int64(200_000), DelTreeMaxItemLimit())

	updateDelTreeMaxItemLimit(10)
	require.True(t, DelTreeEnqueueLimitEnabled())
	require.Equal(t, int64(proto.MinDelTreeMaxItemLimit), DelTreeMaxItemLimit())

	updateDelTreeMaxItemLimit(0)
	require.False(t, DelTreeEnqueueLimitEnabled())
	require.Equal(t, int64(0), DelTreeMaxItemLimit())

	updateDelTreeMaxItemLimit(50)
	require.True(t, DelTreeEnqueueLimitEnabled())
	require.Equal(t, int64(proto.MinDelTreeMaxItemLimit), DelTreeMaxItemLimit())
}

func TestDelTreeEnqueueLimitDisabledWhenMaxIsZero(t *testing.T) {
	defer swapDelTreeMaxItemLimit(0)()
	updateDelTreeMaxItemLimit(0)
	require.False(t, DelTreeEnqueueLimitEnabled())
	require.Equal(t, int64(0), DelTreeMaxItemLimit())
}

func TestDelTreeMaxItemLimitZeroIgnoresEnqueueCap(t *testing.T) {
	defer swapDelTreeMaxItemLimit(0)()
	rootDir, err := os.MkdirTemp("", "del_tree_limit_off")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)
	mp := newTestMetaPartition(rootDir, nil)
	mp.manager = newMetaPartitionTestManager()
	for i := 0; i < 3; i++ {
		mp.enqueueObjExtentDelWrap(uint64(100+i), 0, uint64(i), []proto.ObjExtentKey{createTestObjExtentKey(0, 1, uint64(i+1))})
	}
	require.Equal(t, 3, mp.objExtentDelTree.Len())
}
