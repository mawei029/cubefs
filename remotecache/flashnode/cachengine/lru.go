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
	"container/list"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/stat"
)

const (
	LRUUpdateChanSize          = 1000000
	MaxPushFrontConsumption    = 100000
	BackgroundCleanupItemCount = 50
	StatChanSize               = 100000

	HighWaterCntRatio            = 0.95
	LowWaterCntRatio             = 0.85
	HighWaterSizeRatio           = 0.90
	LowWaterSizeRatio            = 0.80
	HardLimitSizeRatio           = 0.98
	HardLimitEmergencyEvictCount = 5
	WatermarkEvictBatchSize      = 256
	WatermarkEvictLockBudget     = 2 * time.Millisecond
	WatermarkEvictTickInterval   = 100 * time.Millisecond
)

const (
	StatHit = iota
	StatMiss
	StatEvict
	StatSize
	StatPreheatReadBytes
	StatPreheatErrorCount
)

type StatUpdate struct {
	Key   interface{}
	Type  int
	Count int64
}

type LruCache interface {
	Get(key interface{}) (interface{}, error)
	Peek(key interface{}) (interface{}, bool)
	// Set inserts or updates an entry. The returned n is only the number of
	// entries synchronously unlinked by the hard-limit emergency evict path;
	// regular watermark eviction runs asynchronously in the background.
	Set(key interface{}, value interface{}, expiration time.Duration) (n int, err error)
	// BatchSet inserts or updates multiple entries. n has the same meaning as Set.
	BatchSet(keys []interface{}, values []interface{}, expirations []time.Duration) (n int, err error)
	Evict(key interface{}) bool
	EvictAll(cacheEvictWorkerNum int)
	Close() error
	Status() *Status
	StatusAll() *Status
	Len() int
	GetRateStat() RateStat
	GetAllocated() int64
	GetExpiredTime(key interface{}) (time.Time, bool)
	AddMisses(key interface{})
	CheckDiskSpace(dataPath string, key interface{}, size int64, reservedSpace int64) (n int, err error)
	FreePreAllocatedSize(key interface{})
	GetCreateTime(key interface{}) (time.Time, bool)
	SetCapacity(capacity int)
	SetRemoteCacheDisableTTL(remoteCacheDisableTTLMap map[string]bool)
	GetRemoteCacheDisableTTLMap() map[string]bool
	IsVolumeDisableTTL(volume string) bool
	SetStatCh(ch chan StatUpdate)
}

type Status struct {
	Allocated int64
	Length    int
	HitRate   RateStat
	Keys      []interface{}
}

type RateStat struct {
	Hits, Misses, Evicts int32
	HitRate              float64
}

type evictItem struct {
	key    interface{}
	value  interface{}
	reason string
}

// fCache implements a thread-safe fixed size cache with watermark eviction.
type fCache struct {
	cacheType          int
	capacity           int
	maxSize            int64
	allocated          int64
	length             int64
	preAllocated       int64
	preAllocatedKeyMap map[interface{}]int64

	highWaterCnt  int64
	lowWaterCnt   int64
	highWaterSize int64
	lowWaterSize  int64
	hardLimitSize int64

	hits              int32
	misses            int32
	evicts            int32 // evict by set
	cleanupRunning    int32
	watermarkEvicting int32 // 1 while a high->low watermark eviction wave is in progress
	recent            *RateStat

	ttl   time.Duration
	lock  sync.RWMutex
	lru   *list.List
	items map[interface{}]*list.Element

	onDelete OnDeleteF
	onClose  OnCloseF

	// volMap stores volume -> remoteCacheDisableTTL mapping
	volMap sync.Map // volume (string) -> disableTTL (bool)

	closeOnce sync.Once
	closeCh   chan struct{}
	disk      *Disk

	lruUpdateChan chan *entry
	evictSignal   chan struct{}
	statCh        chan StatUpdate
}

// entry in the cache.
type entry struct {
	key       interface{}
	value     interface{}
	createAt  time.Time
	expiredAt time.Time
}

type (
	OnDeleteF func(v interface{}, reason string, removeOuter bool) error
	OnCloseF  func(v interface{}) error
)

// NewCache constructs a new LruCache of the given size that is not safe for
// concurrent use. If it will be panic, if size is not a positive integer.
func NewCache(cacheType int, capacity int, maxSize int64, ttl time.Duration, onDelete OnDeleteF, onClose OnCloseF) LruCache {
	if capacity <= 0 {
		panic("must provide a positive capacity")
	}
	c := &fCache{
		cacheType:          cacheType,
		capacity:           capacity,
		maxSize:            maxSize,
		preAllocatedKeyMap: make(map[interface{}]int64),
		ttl:                ttl,
		lru:                list.New(),
		hits:               1,
		recent:             &RateStat{},
		onDelete:           onDelete,
		onClose:            onClose,
		closeCh:            make(chan struct{}),
		items:              make(map[interface{}]*list.Element),
		lruUpdateChan:      make(chan *entry, LRUUpdateChanSize),
		evictSignal:        make(chan struct{}, 1),
	}
	c.initWatermarks()
	go func() {
		tick := time.NewTicker(time.Second * 60)
		defer tick.Stop()
		for {
			c.replaceRecent()
			select {
			case <-tick.C:
			case <-c.closeCh:
				return
			}
		}
	}()

	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			c.PushFrontAll()
			select {
			case <-tick.C:
			case <-c.closeCh:
				return
			}
		}
	}()

	go c.watermarkEvictorLoop()
	return c
}

func (c *fCache) initWatermarks() {
	highCnt := int64(float64(c.capacity) * HighWaterCntRatio)
	lowCnt := int64(float64(c.capacity) * LowWaterCntRatio)
	if highCnt < 1 {
		highCnt = 1
	}
	if lowCnt < 1 {
		lowCnt = 1
	}
	atomic.StoreInt64(&c.highWaterCnt, highCnt)
	atomic.StoreInt64(&c.lowWaterCnt, lowCnt)
	if c.maxSize > 0 {
		atomic.StoreInt64(&c.highWaterSize, int64(float64(c.maxSize)*HighWaterSizeRatio))
		atomic.StoreInt64(&c.lowWaterSize, int64(float64(c.maxSize)*LowWaterSizeRatio))
		atomic.StoreInt64(&c.hardLimitSize, int64(float64(c.maxSize)*HardLimitSizeRatio))
	}
	if log.EnableInfo() {
		log.LogInfof("[initWatermarks] highWaterCnt (%v) lowWaterCnt (%v) highWaterSize (%v) lowWaterSize (%v) "+
			"hardLimitSize (%v)",
			atomic.LoadInt64(&c.highWaterCnt), atomic.LoadInt64(&c.lowWaterCnt),
			atomic.LoadInt64(&c.highWaterSize), atomic.LoadInt64(&c.lowWaterSize),
			atomic.LoadInt64(&c.hardLimitSize))
	}
}

func (c *fCache) overHardLimit() bool {
	if atomic.LoadInt64(&c.length) > int64(c.capacity) {
		return true
	}
	if c.maxSize > 0 && atomic.LoadInt64(&c.allocated) > atomic.LoadInt64(&c.hardLimitSize) {
		return true
	}
	return false
}

// finishInsert triggers watermark eviction and performs a small emergency sync
// unlink when hard limit is exceeded. The returned n counts only emergency
// evictions; watermark evictions are handled asynchronously by watermarkEvictorLoop.
func (c *fCache) finishInsert() (n int) {
	if c.overHighWater() {
		c.triggerEvictor()
	}
	if !c.overHardLimit() {
		return 0
	}
	victims := c.collectVictims(HardLimitEmergencyEvictCount, "emergency evict")
	n = len(victims)
	if n > 0 {
		c.drainVictims(victims)
	}
	if c.overHardLimit() {
		c.triggerEvictor()
	}
	return n
}

func (c *fCache) overHighWater() bool {
	if atomic.LoadInt64(&c.length) > atomic.LoadInt64(&c.highWaterCnt) {
		return true
	}
	if c.maxSize > 0 && atomic.LoadInt64(&c.allocated) > atomic.LoadInt64(&c.highWaterSize) {
		return true
	}
	return false
}

func (c *fCache) reachedLowWater() bool {
	if atomic.LoadInt64(&c.length) > atomic.LoadInt64(&c.lowWaterCnt) {
		return false
	}
	if c.maxSize > 0 && atomic.LoadInt64(&c.allocated) > atomic.LoadInt64(&c.lowWaterSize) {
		return false
	}
	return true
}

func (c *fCache) triggerEvictor() {
	if !c.overHighWater() {
		return
	}
	select {
	case c.evictSignal <- struct{}{}:
	default:
	}
}

func (c *fCache) watermarkEvictorLoop() {
	tick := time.NewTicker(WatermarkEvictTickInterval)
	defer tick.Stop()
	for {
		select {
		case <-c.evictSignal:
		case <-tick.C:
		case <-c.closeCh:
			return
		}
		if c.reachedLowWater() {
			atomic.StoreInt32(&c.watermarkEvicting, 0)
			continue
		}
		// Start a new wave only when over high water; continue an in-progress wave until low water.
		if !c.overHighWater() && atomic.LoadInt32(&c.watermarkEvicting) == 0 {
			continue
		}
		atomic.StoreInt32(&c.watermarkEvicting, 1)

		for !c.reachedLowWater() {
			victims := c.collectVictims(WatermarkEvictBatchSize, "watermark evict")
			if len(victims) == 0 {
				break
			}
			c.drainVictims(victims)
		}

		if c.reachedLowWater() {
			atomic.StoreInt32(&c.watermarkEvicting, 0)
		} else {
			select {
			case c.evictSignal <- struct{}{}:
			default:
			}
		}
	}
}

func (c *fCache) collectVictims(limit int, reason string) []evictItem {
	c.lock.Lock()
	defer c.lock.Unlock()

	deadline := time.Now().Add(WatermarkEvictLockBudget)
	victims := make([]evictItem, 0, limit)
	for len(victims) < limit && c.lru.Len() > 0 {
		if time.Now().After(deadline) {
			break
		}
		ent := c.lru.Back()
		if ent == nil {
			break
		}
		e := ent.Value.(*entry)
		if c.cacheType == LRUCacheBlockCacheType {
			c.DeleteKeyFromPreAllocatedKeyMap(e.key)
		}
		value := c.deleteElement(ent)
		victims = append(victims, evictItem{
			key:    e.key,
			value:  value,
			reason: reason,
		})
	}
	return victims
}

func (c *fCache) drainVictims(victims []evictItem) {
	if len(victims) == 0 {
		return
	}
	go func(items []evictItem) {
		for _, item := range items {
			_ = c.onDelete(item.value, item.reason, true)
			if c.cacheType == LRUCacheBlockCacheType && log.EnableInfo() {
				log.LogInfof("delete(%s) for %s, len(%d) size(%d / %d)",
					item.key, item.reason, atomic.LoadInt64(&c.length),
					atomic.LoadInt64(&c.allocated), c.maxSize)
			}
		}
	}(victims)
}

func (c *fCache) SetStatCh(ch chan StatUpdate) {
	c.statCh = ch
}

func (c *fCache) sendStat(key interface{}, statType int, count int64) {
	if c.statCh != nil {
		select {
		case c.statCh <- StatUpdate{Key: key, Type: statType, Count: count}:
		default:
		}
	}
}

func (c *fCache) AttachDisk(d *Disk) {
	c.disk = d
}

func (c *fCache) PushFrontAll() {
	c.lock.Lock()
	bg := stat.BeginStat()
	defer func() {
		c.lock.Unlock()
		stat.EndStat("PushFrontAll", nil, bg, 1)
	}()
	processed := 0
	for processed < MaxPushFrontConsumption {
		select {
		case e := <-c.lruUpdateChan:
			if ent, ok := c.items[e.key]; ok {
				c.lru.MoveToFront(ent)
			}
			processed++
		case <-c.closeCh:
			return
		default:
			return
		}
	}
}

func (c *fCache) replaceRecent() {
	hits := atomic.SwapInt32(&c.hits, 1)
	misses := atomic.SwapInt32(&c.misses, 0)
	evicts := atomic.SwapInt32(&c.evicts, 0)
	rs := RateStat{
		Hits:    hits,
		Misses:  misses,
		Evicts:  evicts,
		HitRate: float64(hits) / float64(hits+misses),
	}
	c.recent = &rs
}

func (c *fCache) Status() *Status {
	c.lock.RLock()
	keys := make([]interface{}, 0, len(c.items))
	for _, i := range c.items {
		v := i.Value.(*entry)
		keys = append(keys, v.key)
	}
	c.lock.RUnlock()
	return &Status{
		Allocated: atomic.LoadInt64(&c.allocated),
		Length:    int(atomic.LoadInt64(&c.length)),
		HitRate:   *c.recent,
		Keys:      keys,
	}
}

func (c *fCache) StatusAll() *Status {
	c.lock.RLock()
	keys := make([]interface{}, 0, len(c.items))
	for _, i := range c.items {
		v := i.Value.(*entry)
		keyInfo := v.key.(string) + "  " + v.expiredAt.Format("2006-01-02 15:04:05")
		keys = append(keys, keyInfo)
	}
	c.lock.RUnlock()
	return &Status{
		Allocated: atomic.LoadInt64(&c.allocated),
		Length:    int(atomic.LoadInt64(&c.length)),
		HitRate:   *c.recent,
		Keys:      keys,
	}
}

func GenerateRandTime(expiration time.Duration) time.Duration {
	if expiration <= 0 {
		return expiration
	}

	// 计算过期时间前后 10% 的时间范围
	minDuration := expiration - time.Duration(float64(expiration)*0.1)
	maxDuration := expiration + time.Duration(float64(expiration)*0.1)
	diff := maxDuration - minDuration
	randomDuration := minDuration + time.Duration(rand.Int63n(int64(diff)))
	return randomDuration
}

func (c *fCache) DeleteKeyFromPreAllocatedKeyMap(key interface{}) {
	if size, ok := c.preAllocatedKeyMap[key]; ok {
		atomic.AddInt64(&c.preAllocated, -size)
		delete(c.preAllocatedKeyMap, key)
	}
}

func (c *fCache) FreePreAllocatedSize(key interface{}) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.cacheType != LRUCacheBlockCacheType {
		return
	}
	c.DeleteKeyFromPreAllocatedKeyMap(key)
}

func (c *fCache) CheckDiskSpace(dataPath string, key interface{}, size int64, reservedSpace int64) (n int, err error) {
	var diskSpaceLeft int64

	c.lock.Lock()
	if c.cacheType != LRUCacheBlockCacheType {
		c.lock.Unlock()
		return
	}

	fs := syscall.Statfs_t{}
	if err = syscall.Statfs(dataPath, &fs); err != nil {
		c.lock.Unlock()
		return 0, fmt.Errorf("[CheckDiskSpace] stats disk(%v): %s", dataPath, err.Error())
	}
	diskSpaceLeft = int64(fs.Bavail * uint64(fs.Bsize))
	if _, ok := c.preAllocatedKeyMap[key]; !ok {
		c.preAllocatedKeyMap[key] = size
		atomic.AddInt64(&c.preAllocated, size)
	}

	preAllocated := atomic.LoadInt64(&c.preAllocated)
	diskSpaceLeft -= preAllocated

	if diskSpaceLeft < reservedSpace {
		if log.EnableInfo() {
			log.LogInfof("[CheckDiskSpace] disk space left (%d bytes) is less than reserved space (%d bytes), starting background cleanup", diskSpaceLeft, reservedSpace)
		}
		if atomic.CompareAndSwapInt32(&c.cleanupRunning, 0, 1) {
			go c.backgroundCleanup(BackgroundCleanupItemCount, diskSpaceLeft)
		}
		c.triggerEvictor()
	}
	if diskSpaceLeft > 0 {
		c.lock.Unlock()
		return 0, nil
	}

	toEvicts := make(map[interface{}]interface{})
	for diskSpaceLeft <= 0 {
		ent := c.lru.Back()
		if ent == nil {
			break
		}
		k := ent.Value.(*entry).key
		c.DeleteKeyFromPreAllocatedKeyMap(k)
		toEvicts[k] = c.deleteElement(ent)
		n++
		diskSpaceLeft += ent.Value.(*entry).value.(*CacheBlock).getAllocSize()
	}
	c.lock.Unlock()
	for k, e := range toEvicts {
		_ = c.onDelete(e, fmt.Sprintf("lru disk space is full(%d / %d) diskSpaceLeft(%d)", atomic.LoadInt64(&c.allocated), c.maxSize, diskSpaceLeft), true)
		if log.EnableInfo() {
			log.LogInfof("delete(%s) cos disk space full, len(%d) size(%d / %d) diskSpaceLeft(%d) preAllocated(%d)", k, atomic.LoadInt64(&c.length), atomic.LoadInt64(&c.allocated), c.maxSize, diskSpaceLeft, preAllocated)
		}
	}
	if diskSpaceLeft <= 0 {
		return n, fmt.Errorf("diskSpaceLeft(%v) is not larger than 0, lru has no more entry can be deleted", diskSpaceLeft)
	}
	return n, nil
}

// Set inserts or updates an entry. n is only the number of emergency sync evicts
// triggered when hard limit is exceeded; watermark eviction is asynchronous.
func (c *fCache) Set(key, value interface{}, expiration time.Duration) (n int, err error) {
	if expiration == 0 {
		expiration = c.ttl
	}

	expiration = GenerateRandTime(expiration)
	c.lock.Lock()
	if ent, ok := c.items[key]; ok {
		c.lru.MoveToFront(ent)
		v := ent.Value.(*entry)
		v.value = value
		v.createAt = time.Now()
		v.expiredAt = time.Now().Add(expiration)
		c.lock.Unlock()
		return 0, nil
	}

	if c.cacheType == LRUCacheBlockCacheType {
		newCb := value.(*CacheBlock)
		cbSize := newCb.getAllocSize()
		atomic.AddInt64(&c.allocated, cbSize)
	}

	c.items[key] = c.lru.PushFront(&entry{
		key:       key,
		value:     value,
		createAt:  time.Now(),
		expiredAt: time.Now().Add(expiration),
	})
	atomic.AddInt64(&c.length, 1)
	c.lock.Unlock()
	n = c.finishInsert()
	return n, nil
}

// BatchSet inserts or updates multiple entries. n is only the number of emergency
// sync evicts triggered when hard limit is exceeded; watermark eviction is asynchronous.
func (c *fCache) BatchSet(keys []interface{}, values []interface{}, expirations []time.Duration) (n int, err error) {
	if len(keys) != len(values) || len(keys) != len(expirations) {
		return 0, fmt.Errorf("keys, values and expirations length mismatch")
	}

	c.lock.Lock()

	for i, key := range keys {
		value := values[i]
		expiration := expirations[i]
		if expiration == 0 {
			expiration = c.ttl
		}
		currentExpiration := GenerateRandTime(expiration)

		if ent, ok := c.items[key]; ok {
			c.lru.MoveToFront(ent)
			v := ent.Value.(*entry)
			v.value = value
			v.createAt = time.Now()
			v.expiredAt = time.Now().Add(currentExpiration)
			continue
		}

		if c.cacheType == LRUCacheBlockCacheType {
			newCb := value.(*CacheBlock)
			cbSize := newCb.getAllocSize()
			atomic.AddInt64(&c.allocated, cbSize)
		}

		c.items[key] = c.lru.PushFront(&entry{
			key:       key,
			value:     value,
			createAt:  time.Now(),
			expiredAt: time.Now().Add(currentExpiration),
		})
		atomic.AddInt64(&c.length, 1)
	}

	c.lock.Unlock()
	n = c.finishInsert()

	return n, nil
}

func (c *fCache) Get(key interface{}) (interface{}, error) {
	// Extract volume from key and check if remoteCacheDisableTTL is enabled
	disableTTL := false
	if keyStr, ok := key.(string); ok {
		volume := extractVolumeFromKey(keyStr)
		if volume != "" {
			if val, ok := c.volMap.Load(volume); ok {
				disableTTL = val.(bool)
			}
		}
	}

	c.lock.RLock()
	if ent, ok := c.items[key]; ok {
		v := ent.Value.(*entry)
		// If disableTTL is true, skip TTL check
		if disableTTL || v.expiredAt.After(time.Now()) {
			c.lock.RUnlock()
			atomic.AddInt32(&c.hits, 1)
			c.sendStat(key, StatHit, 1)
			c.lruUpdateChan <- v
			return v.value, nil
		}
		c.lock.RUnlock()
		atomic.AddInt32(&c.misses, 1)
		c.sendStat(key, StatMiss, 1)
		c.lock.Lock()
		var expiredValue interface{}
		var expiredReason string
		if newEnt, found := c.items[key]; found {
			newV := newEnt.Value.(*entry)
			// If disableTTL is true, skip TTL check
			if disableTTL || newV.expiredAt.After(time.Now()) {
				c.lock.Unlock()
				atomic.AddInt32(&c.hits, 1)
				c.sendStat(key, StatHit, 1)
				c.lruUpdateChan <- newV
				return newV.value, nil
			}
			if c.cacheType == LRUCacheBlockCacheType {
				log.LogInfof("delete(%s) on get, create_time:(%v)  expired_time:(%v)",
					key, newV.createAt.Format("2006-01-02 15:04:05"), newV.expiredAt.Format("2006-01-02 15:04:05"))
				c.DeleteKeyFromPreAllocatedKeyMap(key)
			}
			expiredValue = c.deleteElement(newEnt)
			expiredReason = fmt.Sprintf("created: %v get expired: %v", newV.createAt.Format("2006-01-02 15:04:05"),
				newV.expiredAt.Format("2006-01-02 15:04:05"))
		}
		c.lock.Unlock()
		if expiredValue != nil {
			go func(val interface{}, reason string) {
				_ = c.onDelete(val, reason, true)
			}(expiredValue, expiredReason)
		}
		return nil, fmt.Errorf("expired key[%v]", key)
	}
	c.lock.RUnlock()
	atomic.AddInt32(&c.misses, 1)
	c.sendStat(key, StatMiss, 1)
	return nil, fmt.Errorf("key[%s] not found", key)
}

// Peek returns the key value (or undefined if not found) without updating
// the "recently used"-ness of the key.
func (c *fCache) Peek(key interface{}) (interface{}, bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	if ent, ok := c.items[key]; ok {
		v := ent.Value.(*entry)
		return v.value, true
	}
	return nil, false
}

// EvictAll is used to completely clear the cache.
func (c *fCache) EvictAll(cacheEvictWorkerNum int) {
	c.lock.Lock()
	defer c.lock.Unlock()
	var wg sync.WaitGroup
	toEvicts := make(chan interface{}, cacheEvictWorkerNum)
	for i := 0; i < cacheEvictWorkerNum; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range toEvicts {
				_ = c.onDelete(e, "execute evictAll operation", false)
			}
		}()
	}
	for _, ent := range c.items {
		if c.cacheType == LRUCacheBlockCacheType {
			c.DeleteKeyFromPreAllocatedKeyMap(ent.Value.(*entry).key)
		}
		toEvicts <- c.deleteElement(ent)
	}
	close(toEvicts)
	wg.Wait()
	atomic.StoreInt64(&c.length, 0)
}

func (c *fCache) Evict(key interface{}) bool {
	var val interface{}
	c.lock.Lock()
	if ent, ok := c.items[key]; ok {
		if c.cacheType == LRUCacheBlockCacheType {
			log.LogInfof("delete(%s) manually", key)
			c.DeleteKeyFromPreAllocatedKeyMap(key)
		}
		val = c.deleteElement(ent)
	}
	c.lock.Unlock()
	if val != nil {
		go func(v interface{}) {
			_ = c.onDelete(v, "execute evict operation", false)
		}(val)
	}
	return true
}

func (c *fCache) deleteElement(ent *list.Element) interface{} {
	v := ent.Value.(*entry)
	c.removeElement(ent)
	atomic.AddInt32(&c.evicts, 1)
	c.sendStat(v.key, StatEvict, 1)
	return v.value
}

func (c *fCache) Len() int {
	return int(atomic.LoadInt64(&c.length))
}

// SetRemoteCacheDisableTTL updates the remoteCacheDisableTTL map for volumes
func (c *fCache) SetRemoteCacheDisableTTL(remoteCacheDisableTTLMap map[string]bool) {
	// Update volMap with remoteCacheDisableTTL for each volume
	for volume, disableTTL := range remoteCacheDisableTTLMap {
		if disableTTL {
			c.volMap.Store(volume, true)
		} else {
			c.volMap.Delete(volume)
		}
	}
	// Remove volumes that are not in the map (they should use default TTL behavior)
	c.volMap.Range(func(key, value interface{}) bool {
		vol := key.(string)
		if _, exists := remoteCacheDisableTTLMap[vol]; !exists {
			c.volMap.Delete(vol)
		}
		return true
	})
}

func (c *fCache) GetRemoteCacheDisableTTLMap() map[string]bool {
	remoteCacheDisableTTLMap := make(map[string]bool)
	c.volMap.Range(func(key, value interface{}) bool {
		volume := key.(string)
		disableTTL := value.(bool)
		if disableTTL {
			remoteCacheDisableTTLMap[volume] = true
		}
		return true
	})
	return remoteCacheDisableTTLMap
}

// IsVolumeDisableTTL checks if remoteCacheDisableTTL is enabled for a specific volume
func (c *fCache) IsVolumeDisableTTL(volume string) bool {
	if val, ok := c.volMap.Load(volume); ok {
		return val.(bool)
	}
	return false
}

// extractVolumeFromKey extracts volume name from cache block key
// For GenCacheBlockKey: format is "volume/inode#offset#version", volume is the first path component
// For GenCacheBlockKeyV2: format is "volume/key", volume is the first path component
func extractVolumeFromKey(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// removeElement is used to remove a given list element from the cache
func (c *fCache) removeElement(e *list.Element) {
	c.lru.Remove(e)
	kv := e.Value.(*entry)
	delete(c.items, kv.key)
	atomic.AddInt64(&c.length, -1)
	if c.cacheType == LRUCacheBlockCacheType {
		cb := kv.value.(*CacheBlock)
		atomic.AddInt64(&c.allocated, -cb.getAllocSize())
	}
}

func (c *fCache) Close() error {
	c.closeOnce.Do(func() {
		close(c.closeCh)
	})
	c.lock.Lock()
	defer c.lock.Unlock()
	chanItems := make(chan interface{}, 16)
	var (
		errCount int32
		wg       sync.WaitGroup
	)

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range chanItems {
				err := c.onClose(e)
				if err != nil {
					atomic.AddInt32(&errCount, 1)
				}
			}
		}()
	}
	for _, item := range c.items {
		kv := item.Value.(*entry)
		chanItems <- kv.value
	}
	close(chanItems)
	wg.Wait()
	if errCount > 0 {
		return fmt.Errorf("error count(%v) on close", errCount)
	}
	return nil
}

func (c *fCache) GetRateStat() RateStat {
	return *c.recent
}

func (c *fCache) GetAllocated() int64 {
	return atomic.LoadInt64(&c.allocated)
}

func (c *fCache) GetExpiredTime(key interface{}) (time.Time, bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	if ent, ok := c.items[key]; ok {
		v := ent.Value.(*entry)
		return v.expiredAt, true
	}
	return time.Time{}, false
}

func (c *fCache) GetCreateTime(key interface{}) (time.Time, bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	if ent, ok := c.items[key]; ok {
		v := ent.Value.(*entry)
		return v.createAt, true
	}
	return time.Time{}, false
}

func (c *fCache) AddMisses(key interface{}) {
	atomic.AddInt32(&c.misses, 1)
	c.sendStat(key, StatMiss, 1)
}

func (c *fCache) backgroundCleanup(itemCount int, diskSpaceLeft int64) {
	if log.EnableInfo() {
		log.LogInfof("[backgroundCleanup] Starting background cleanup of up to %d items", itemCount)
	}
	defer atomic.StoreInt32(&c.cleanupRunning, 0)

	startTime := time.Now()
	reason := fmt.Sprintf("background cleanup - disk space(%v) low", diskSpaceLeft)
	victims := c.collectVictims(itemCount, reason)
	if log.EnableInfo() {
		log.LogInfof("[backgroundCleanup] Completed unlink of %d items in %v", len(victims), time.Since(startTime))
	}
	c.drainVictims(victims)
}

func (c *fCache) SetCapacity(capacity int) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.capacity = capacity
	c.initWatermarks()
}
