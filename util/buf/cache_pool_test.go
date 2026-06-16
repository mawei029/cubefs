// Copyright 2023 The CubeFS Authors.
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

package buf

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func outstandingCacheBlocks() int64 {
	if CachePool == nil {
		return 0
	}
	return atomic.LoadInt64(&CachePool.count)
}

func cacheBlockLimit() int64 {
	if CachePool == nil {
		return 0
	}
	return CachePool.totalLimit
}

func checkCachePool(t *testing.T, pool *FileCachePool) {
	first := pool.Get()
	second := pool.Get()
	require.Equal(t, len(first), len(second))
	require.NotSame(t, &first[0], &second[0])
	pool.Put(second)
	second = pool.Get()
	require.NotSame(t, &second[0], &first[0])
	require.Equal(t, len(first), len(second))
	pool.Put(first)
	pool.Put(second)
}

func TestCachePool(t *testing.T) {
	InitCachePool(8388608, 512)
	checkCachePool(t, CachePool)
}

func TestInitCachePool_defaultBlockLimit(t *testing.T) {
	InitCachePool(1024, 0)
	require.Equal(t, DefaultEbsWriteCacheLimit, cacheBlockLimit())
}

func TestInitCachePool_customBlockLimit(t *testing.T) {
	InitCachePool(64, 3)
	require.Equal(t, int64(3), cacheBlockLimit())
}

func TestCachePool_blocksAtLimit(t *testing.T) {
	const blockSize = 32
	const limit = 2
	InitCachePool(blockSize, limit)

	b1 := CachePool.Get()
	b2 := CachePool.Get()
	require.Equal(t, int64(limit), outstandingCacheBlocks())

	acquired := make(chan []byte, 1)
	go func() {
		acquired <- CachePool.Get()
	}()

	select {
	case b := <-acquired:
		t.Fatalf("third Get should block, got %p", &b[0])
	case <-time.After(200 * time.Millisecond):
	}

	CachePool.Put(b1)

	var b3 []byte
	select {
	case b3 = <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Get did not wake after Put")
	}
	require.Equal(t, int64(limit), outstandingCacheBlocks())

	CachePool.Put(b2)
	CachePool.Put(b3)
	require.Equal(t, int64(0), outstandingCacheBlocks())
}

func TestCachePool_putBroadcastUnblocksWaiter(t *testing.T) {
	const blockSize = 16
	InitCachePool(blockSize, 1)

	held := CachePool.Get()
	require.Equal(t, int64(1), outstandingCacheBlocks())

	waiting := make(chan struct{})
	acquired := make(chan []byte, 1)
	go func() {
		close(waiting)
		acquired <- CachePool.Get()
	}()
	<-waiting
	time.Sleep(50 * time.Millisecond)

	CachePool.Put(held)

	var extra []byte
	select {
	case extra = <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Get did not wake after Put")
	}
	CachePool.Put(extra)
	require.Equal(t, int64(0), outstandingCacheBlocks())
}

func TestCachePool_concurrentNeverExceedsLimit(t *testing.T) {
	const blockSize = 64
	const limit = 8
	const workers = 32
	const iters = 200
	InitCachePool(blockSize, limit)

	stop := make(chan struct{})
	var peak int64
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		ticker := time.NewTicker(20 * time.Microsecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				n := outstandingCacheBlocks()
				for {
					old := atomic.LoadInt64(&peak)
					if n <= old {
						break
					}
					if atomic.CompareAndSwapInt64(&peak, old, n) {
						break
					}
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				b := CachePool.Get()
				if n := outstandingCacheBlocks(); n > limit {
					t.Errorf("outstanding blocks %d exceeds limit %d", n, limit)
				}
				CachePool.Put(b)
			}
		}()
	}
	wg.Wait()
	close(stop)
	sampler.Wait()

	require.LessOrEqual(t, atomic.LoadInt64(&peak), int64(limit))
	require.Equal(t, int64(0), outstandingCacheBlocks())
}
