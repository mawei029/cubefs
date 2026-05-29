// Copyright 2026 The CubeFS Authors.
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
	"path"
	"testing"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util"
	"github.com/stretchr/testify/require"
)

func newTestDiskEngine(t *testing.T, diskSpaces ...int64) (*CacheEngine, []*Disk) {
	t.Helper()
	disks := make([]*Disk, 0, len(diskSpaces))
	for _, space := range diskSpaces {
		dir := t.TempDir()
		disks = append(disks, &Disk{
			Path:       dir,
			TotalSpace: space,
			Capacity:   100,
			Status:     proto.ReadWrite,
		})
	}
	ce, err := NewCacheEngine("", 0, DefaultCacheMaxUsedRatio, disks, 100, 500, 0, 1, 1, nil, DefaultExpireTime, nil, false, "", 1024, 100, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ce.Stop()) })
	return ce, disks
}

func cacheItemAt(t *testing.T, ce *CacheEngine, disk *Disk) *lruCacheItem {
	t.Helper()
	fullPath := path.Join(disk.Path, DefaultCacheDirName)
	v, ok := ce.lruCacheMap.Load(fullPath)
	require.True(t, ok)
	return v.(*lruCacheItem)
}

func TestSetDiskCacheCapacity(t *testing.T) {
	ce, disks := newTestDiskEngine(t, util.MB)
	item := cacheItemAt(t, ce, disks[0])
	fullPath := path.Join(disks[0].Path, DefaultCacheDirName)

	require.NoError(t, ce.SetDiskCacheCapacity(fullPath, 2000))
	require.Equal(t, 2000, item.config.Capacity)
	require.Equal(t, 2000, disks[0].Capacity)

	require.NoError(t, ce.SetDiskCacheCapacity("", 3000))
	require.Equal(t, 3000, item.config.Capacity)

	require.Error(t, ce.SetDiskCacheCapacity("/no/such/cache", 1))
}

func TestSetFhCacheCapacity(t *testing.T) {
	ce, _ := newTestDiskEngine(t, util.MB)
	require.NotNil(t, ce.lruFhCache)

	ce.SetFhCacheCapacity(0)
	require.Equal(t, 500, ce.fhCapacity)

	ce.SetFhCacheCapacity(12000)
	require.Equal(t, 12000, ce.fhCapacity)
}

func TestEvictCacheByVolume(t *testing.T) {
	cache := NewCache(LRUFileHandleCacheType, 100, util.MB, time.Hour, nilDeleteFunc, nilCloseFunc)
	defer cache.Close()

	dir := t.TempDir()
	disk := &Disk{Path: dir, TotalSpace: util.MB, Capacity: 100, Status: proto.ReadWrite}
	fullPath := path.Join(dir, DefaultCacheDirName)
	ce := &CacheEngine{}
	ce.lruCacheMap.Store(fullPath, &lruCacheItem{
		lruCache: cache,
		config:   CacheConfig{Path: fullPath, Capacity: 100},
		disk:     disk,
	})

	_, err := cache.Set("vol-a/key1", 1, time.Hour)
	require.NoError(t, err)
	_, err = cache.Set("vol-a/key2", 2, time.Hour)
	require.NoError(t, err)
	_, err = cache.Set("vol-b/key3", 3, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 3, cache.Len())

	failed := ce.EvictCacheByVolume("vol-a")
	require.Empty(t, failed)
	require.Equal(t, 1, cache.Len())
	_, err = cache.Get("vol-b/key3")
	require.NoError(t, err)
}
