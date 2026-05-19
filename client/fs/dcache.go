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

package fs

import (
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DentryCache defines the dentry cache.
type DentryCache struct {
	sync.Mutex
	cache        map[string]uint64
	expiration   time.Time
	acceleration bool
}

// NewDentryCache returns a new dentry cache.
func NewDentryCache(acceleration bool) *DentryCache {
	return &DentryCache{
		cache:        make(map[string]uint64),
		expiration:   time.Now().Add(DentryValidDuration),
		acceleration: acceleration,
	}
}

// Put puts an item into the cache.
func (dc *DentryCache) Put(name string, ino uint64) {
	if dc == nil {
		return
	}
	dc.Lock()
	defer dc.Unlock()
	if dc.cache == nil {
		dc.cache = make(map[string]uint64)
	}
	dc.cache[name] = ino
	dc.expiration = time.Now().Add(DentryValidDuration)
}

// Get gets the item from the cache based on the given key.
func (dc *DentryCache) Get(name string) (uint64, bool) {
	if dc == nil {
		return 0, false
	}

	dc.Lock()
	defer dc.Unlock()
	if dc.expiration.Before(time.Now()) && !dc.acceleration {
		dc.cache = make(map[string]uint64)
		return 0, false
	}
	ino, ok := dc.cache[name]
	return ino, ok
}

// Delete deletes the item based on the given key.
func (dc *DentryCache) Delete(name string) {
	if dc == nil {
		return
	}
	dc.Lock()
	defer dc.Unlock()
	delete(dc.cache, name)
}

func (dc *DentryCache) Len() int {
	if dc == nil {
		return 0
	}
	dc.Lock()
	defer dc.Unlock()
	return len(dc.cache)
}

func (dc *DentryCache) Clear() {
	if dc == nil {
		return
	}
	dc.Lock()
	defer dc.Unlock()
	dc.cache = nil
}

type negativeDentryEntry struct {
	parentIno      uint64
	name           string
	nextValidateAt int64
}

func negativeDentryKey(parentIno uint64, name string) string {
	return strconv.FormatUint(parentIno, 10) + "/" + name
}

func negativeDentryJitterNanos() int64 {
	maxJitter := int64(float64(NegativeDentryRevalidatePeriod) * NegativeDentryRevalidateJitterRatio)
	if maxJitter <= 0 {
		return 0
	}
	return rand.Int63n(maxJitter + 1)
}

func negativeDentryScheduleNextValidate(now int64) int64 {
	return now + int64(NegativeDentryRevalidatePeriod) + negativeDentryJitterNanos()
}

func negativeDentryDue(nextValidateAt int64, now int64) bool {
	return now >= nextValidateAt
}

func (s *Super) NegativeDentryPut(parentIno uint64, name string) {
	now := time.Now().UnixNano()
	entry := &negativeDentryEntry{
		parentIno:      parentIno,
		name:           name,
		nextValidateAt: negativeDentryScheduleNextValidate(now),
	}
	s.negativeDentryCache.Store(negativeDentryKey(parentIno, name), entry)
}

func (s *Super) negativeDentryBumpNextValidate(entry *negativeDentryEntry, now int64) {
	atomic.StoreInt64(&entry.nextValidateAt, negativeDentryScheduleNextValidate(now))
}

func (s *Super) NegativeDentryGet(parentIno uint64, name string) bool {
	_, ok := s.negativeDentryCache.Load(negativeDentryKey(parentIno, name))
	return ok
}

func (s *Super) NegativeDentryDelete(parentIno uint64, name string) {
	s.negativeDentryCache.Delete(negativeDentryKey(parentIno, name))
}

func (s *Super) NegativeDentryClear(parentIno uint64) {
	s.negativeDentryCache.Range(func(key, value interface{}) bool {
		entry := value.(*negativeDentryEntry)
		if entry.parentIno == parentIno {
			s.negativeDentryCache.Delete(key)
		}
		return true
	})
}
