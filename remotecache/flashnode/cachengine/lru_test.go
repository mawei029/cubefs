// Copyright 2018 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package cachengine

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubefs/cubefs/util"
	"github.com/stretchr/testify/require"
)

var (
	nilDeleteFunc = func(v interface{}, reason string, removeOuter bool) error { return nil }
	nilCloseFunc  = func(v interface{}) error { return nil }
)

func TestLRUManyThings(t *testing.T) {
	require.PanicsWithValue(t, "must provide a positive capacity", func() {
		_ = NewCache(LRUFileHandleCacheType, 0, util.MB*100, time.Hour, nilDeleteFunc, nilCloseFunc)
	})

	c := NewCache(LRUFileHandleCacheType, 10, util.MB*100, time.Hour, nilDeleteFunc, nilCloseFunc)
	defer c.Close()
	var (
		k  = 1
		v1 = &CacheBlock{blockKey: "block1"}
		v2 = &CacheBlock{blockKey: "block2"}
	)

	require.Equal(t, 0, c.Len())

	actual, err := c.Get(k)
	require.Error(t, err)
	require.Nil(t, actual)

	_, has := c.Peek(k)
	require.False(t, has)

	c.Set(k, v1, time.Hour)
	actual, err = c.Get(k)
	require.NoError(t, err)
	require.Equal(t, actual, v1)
	require.Equal(t, 1, c.Len())

	_, has = c.Peek(k)
	require.True(t, has)

	c.Set(k, v2, time.Minute)
	actual, err = c.Get(k)
	require.NoError(t, err)
	require.Equal(t, actual, v2)
	require.Equal(t, 1, c.Len())

	require.True(t, c.Evict(k))
	actual, err = c.Get(k)
	require.Error(t, err)
	require.Nil(t, actual)
	require.Equal(t, 0, c.Len())
	require.True(t, c.Evict(k))
}

func TestLRUCapacity(t *testing.T) {
	called := make(chan struct{}, 8)
	c := NewCache(LRUCacheBlockCacheType, 2, util.MB*100, time.Hour,
		func(v interface{}, reason string, removeOuter bool) error {
			select {
			case called <- struct{}{}:
			default:
			}
			return nil
		},
		nilCloseFunc)
	defer c.Close()

	c.Set(1, &CacheBlock{blockKey: "block1"}, 0)
	c.Set(2, &CacheBlock{blockKey: "block2"}, 0)
	n, err := c.Set(3, &CacheBlock{blockKey: "block3"}, 0)
	require.NoError(t, err)
	require.Greater(t, n, 0)
	require.LessOrEqual(t, c.Len(), 2)
	_, err = c.Get(1)
	require.Error(t, err)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("expected async onDelete during capacity eviction")
	}
	_, err = c.Set(4, &CacheBlock{blockKey: "block4"}, 0)
	require.NoError(t, err)
	_, err = c.Set(5, &CacheBlock{blockKey: "block5"}, 0)
	require.NoError(t, err)
	require.LessOrEqual(t, c.Len(), 2)
	t.Logf("%+v", c.Status())
}

func TestLRUExpired(t *testing.T) {
	c := NewCache(LRUFileHandleCacheType, 2, util.MB*100, time.Hour, nilDeleteFunc, nilCloseFunc)
	defer c.Close()
	e := -time.Hour
	c.Set(1, &CacheBlock{blockKey: "block1"}, e)
	c.Set(2, &CacheBlock{blockKey: "block2"}, 0)
	require.Equal(t, 2, c.Len())
	_, err := c.Get(1)
	require.Error(t, err)
	require.Equal(t, 1, c.Len())

	c.EvictAll(2)
	require.Equal(t, 0, c.Len())
}

func waitCacheUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout waiting for cache condition")
}

func setCacheBlocks(t *testing.T, c LruCache, start, count int) {
	t.Helper()
	for i := start; i < start+count; i++ {
		_, err := c.Set(i, &CacheBlock{blockKey: fmt.Sprintf("block%d", i)}, time.Hour)
		require.NoError(t, err)
	}
}

func cacheHighWaterCnt(fc *fCache) int64 {
	return atomic.LoadInt64(&fc.highWaterCnt)
}

func cacheLowWaterCnt(fc *fCache) int64 {
	return atomic.LoadInt64(&fc.lowWaterCnt)
}

func cacheHighWaterSize(fc *fCache) int64 {
	return atomic.LoadInt64(&fc.highWaterSize)
}

func cacheLowWaterSize(fc *fCache) int64 {
	return atomic.LoadInt64(&fc.lowWaterSize)
}

func cacheHardLimitSize(fc *fCache) int64 {
	return atomic.LoadInt64(&fc.hardLimitSize)
}

func TestInitWatermarks(t *testing.T) {
	const capacity = 100
	maxSize := int64(util.GB)
	c := NewCache(LRUCacheBlockCacheType, capacity, maxSize, time.Hour, nilDeleteFunc, nilCloseFunc)
	defer c.Close()

	fc := c.(*fCache)
	require.Equal(t, int64(float64(capacity)*HighWaterCntRatio), cacheHighWaterCnt(fc))
	require.Equal(t, int64(float64(capacity)*LowWaterCntRatio), cacheLowWaterCnt(fc))
	wantHighSize := int64(float64(maxSize) * HighWaterSizeRatio)
	wantLowSize := int64(float64(maxSize) * LowWaterSizeRatio)
	wantHardLimit := int64(float64(maxSize) * HardLimitSizeRatio)
	require.Equal(t, wantHighSize, cacheHighWaterSize(fc))
	require.Equal(t, wantLowSize, cacheLowWaterSize(fc))
	require.Equal(t, wantHardLimit, cacheHardLimitSize(fc))
	require.Greater(t, cacheHighWaterCnt(fc), cacheLowWaterCnt(fc))
	require.Greater(t, cacheHighWaterSize(fc), cacheLowWaterSize(fc))
}

func TestWatermarkThresholds(t *testing.T) {
	c := NewCache(LRUFileHandleCacheType, 100, util.MB*100, time.Hour, nilDeleteFunc, nilCloseFunc)
	defer c.Close()
	fc := c.(*fCache)

	require.True(t, fc.reachedLowWater())
	require.False(t, fc.overHighWater())

	setCacheBlocks(t, c, 0, 90)
	require.False(t, fc.overHighWater())
	require.False(t, fc.reachedLowWater())

	setCacheBlocks(t, c, 90, 10)
	require.True(t, fc.overHighWater())
	require.False(t, fc.reachedLowWater())
}

func TestWatermarkEvictor(t *testing.T) {
	const capacity = 10
	c := NewCache(LRUFileHandleCacheType, capacity, util.MB*100, time.Hour, nilDeleteFunc, nilCloseFunc)
	defer c.Close()

	fc := c.(*fCache)
	require.Equal(t, int64(9), cacheHighWaterCnt(fc))
	require.Equal(t, int64(8), cacheLowWaterCnt(fc))

	setCacheBlocks(t, c, 0, capacity)
	require.Equal(t, capacity, c.Len())

	waitCacheUntil(t, 2*time.Second, func() bool {
		return c.Len() <= int(cacheLowWaterCnt(fc))
	})
	require.LessOrEqual(t, c.Len(), int(cacheLowWaterCnt(fc)))
	require.Less(t, c.Len(), int(cacheHighWaterCnt(fc)))
	require.Equal(t, int32(0), atomic.LoadInt32(&fc.watermarkEvicting))
}

func TestWatermarkEvictsToLowNotHigh(t *testing.T) {
	const capacity = 100
	c := NewCache(LRUFileHandleCacheType, capacity, util.MB*100, time.Hour, nilDeleteFunc, nilCloseFunc)
	defer c.Close()

	fc := c.(*fCache)
	setCacheBlocks(t, c, 0, capacity)
	waitCacheUntil(t, 5*time.Second, func() bool {
		return c.Len() <= int(cacheLowWaterCnt(fc))
	})
	require.LessOrEqual(t, c.Len(), int(cacheLowWaterCnt(fc)))
	require.Less(t, c.Len(), int(cacheHighWaterCnt(fc)))
}

func TestWatermarkDeadBandNoEviction(t *testing.T) {
	const capacity = 100
	c := NewCache(LRUFileHandleCacheType, capacity, util.MB*100, time.Hour, nilDeleteFunc, nilCloseFunc)
	defer c.Close()

	fc := c.(*fCache)
	const inDeadBand = 90
	setCacheBlocks(t, c, 0, inDeadBand)
	require.Equal(t, inDeadBand, c.Len())
	require.Greater(t, c.Len(), int(cacheLowWaterCnt(fc)))
	require.LessOrEqual(t, c.Len(), int(cacheHighWaterCnt(fc)))
	require.False(t, fc.overHighWater())

	time.Sleep(4 * WatermarkEvictTickInterval)
	require.Equal(t, inDeadBand, c.Len())
	require.Equal(t, int32(0), atomic.LoadInt32(&fc.watermarkEvicting))
}

func TestWatermarkResumeEvictionBelowHigh(t *testing.T) {
	const capacity = 100
	c := NewCache(LRUFileHandleCacheType, capacity, util.MB*100, time.Hour, nilDeleteFunc, nilCloseFunc)
	defer c.Close()

	fc := c.(*fCache)
	const inBand = 92
	setCacheBlocks(t, c, 0, inBand)
	require.Equal(t, inBand, c.Len())
	require.False(t, fc.overHighWater())
	require.False(t, fc.reachedLowWater())

	// Simulate an in-progress high->low wave that has dropped below high but not low yet.
	atomic.StoreInt32(&fc.watermarkEvicting, 1)
	select {
	case fc.evictSignal <- struct{}{}:
	default:
	}

	waitCacheUntil(t, 5*time.Second, func() bool {
		return c.Len() <= int(cacheLowWaterCnt(fc))
	})
	require.LessOrEqual(t, c.Len(), int(cacheLowWaterCnt(fc)))
	require.Equal(t, int32(0), atomic.LoadInt32(&fc.watermarkEvicting))
}

func TestSetNoBulkEvictAtCapacity(t *testing.T) {
	const capacity = 10
	c := NewCache(LRUFileHandleCacheType, capacity, util.MB*100, time.Hour, nilDeleteFunc, nilCloseFunc)
	defer c.Close()

	for i := 0; i < capacity; i++ {
		n, err := c.Set(i, &CacheBlock{blockKey: fmt.Sprintf("block%d", i)}, time.Hour)
		require.NoError(t, err)
		require.Equal(t, 0, n)
	}
	require.Equal(t, capacity, c.Len())
}

func TestEmergencyEvictOnHardLimit(t *testing.T) {
	called := make(chan struct{}, 8)
	c := NewCache(LRUCacheBlockCacheType, 2, util.MB*100, time.Hour,
		func(v interface{}, reason string, removeOuter bool) error {
			select {
			case called <- struct{}{}:
			default:
			}
			return nil
		},
		nilCloseFunc)
	defer c.Close()

	_, err := c.Set(1, &CacheBlock{blockKey: "block1"}, 0)
	require.NoError(t, err)
	_, err = c.Set(2, &CacheBlock{blockKey: "block2"}, 0)
	require.NoError(t, err)
	n, err := c.Set(3, &CacheBlock{blockKey: "block3"}, 0)
	require.NoError(t, err)
	require.Greater(t, n, 0)
	require.LessOrEqual(t, c.Len(), 2)

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("expected async onDelete after emergency evict")
	}
}

func TestBlockCacheAsyncEvict(t *testing.T) {
	called := make(chan struct{}, 8)
	c := NewCache(LRUCacheBlockCacheType, 2, util.MB*100, time.Hour,
		func(v interface{}, reason string, removeOuter bool) error {
			select {
			case called <- struct{}{}:
			default:
			}
			return nil
		},
		nilCloseFunc)
	defer c.Close()

	_, err := c.Set(1, &CacheBlock{blockKey: "block1"}, 0)
	require.NoError(t, err)
	_, err = c.Set(2, &CacheBlock{blockKey: "block2"}, 0)
	require.NoError(t, err)
	_, err = c.Set(3, &CacheBlock{blockKey: "block3"}, 0)
	require.NoError(t, err)

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("expected async onDelete after block cache eviction")
	}
}
