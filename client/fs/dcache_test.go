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
	"sync/atomic"
	"testing"
	"time"
)

func TestNegativeDentryGetDoesNotRenewNextValidateAt(t *testing.T) {
	s := &Super{}
	s.NegativeDentryPut(1, "missing")

	v, _ := s.negativeDentryCache.Load(negativeDentryKey(1, "missing"))
	entry := v.(*negativeDentryEntry)
	before := atomic.LoadInt64(&entry.nextValidateAt)

	if !s.NegativeDentryGet(1, "missing") {
		t.Fatal("expected cache hit")
	}
	if atomic.LoadInt64(&entry.nextValidateAt) != before {
		t.Fatal("expected Get not to renew nextValidateAt")
	}

	for i := 0; i < 3; i++ {
		if !s.NegativeDentryGet(1, "missing") {
			t.Fatal("expected cache hit on repeated Get")
		}
	}
	atomic.StoreInt64(&entry.nextValidateAt, time.Now().Add(-time.Millisecond).UnixNano())
	if !negativeDentryDue(atomic.LoadInt64(&entry.nextValidateAt), time.Now().UnixNano()) {
		t.Fatal("expected entry due for background probe when nextValidateAt is in the past")
	}
}

func TestNegativeDentryDueAfterRevalidatePeriod(t *testing.T) {
	now := time.Now().UnixNano()
	next := negativeDentryScheduleNextValidate(now)
	minNext := now + int64(NegativeDentryRevalidatePeriod)
	maxNext := minNext + int64(float64(NegativeDentryRevalidatePeriod)*NegativeDentryRevalidateJitterRatio)
	if next < minNext || next > maxNext {
		t.Fatalf("nextValidateAt %v out of [%v, %v]", next, minNext, maxNext)
	}
	if negativeDentryDue(next, now) {
		t.Fatal("expected not due before nextValidateAt")
	}
	if !negativeDentryDue(next, next) {
		t.Fatal("expected due at nextValidateAt")
	}
}

func TestNegativeDentryNextValidateAtJitterSpread(t *testing.T) {
	now := time.Now().UnixNano()
	seen := make(map[int64]struct{})
	for i := 0; i < 32; i++ {
		seen[negativeDentryScheduleNextValidate(now)] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatal("expected jitter to spread nextValidateAt across multiple values")
	}
}

func TestNegativeDentryDeleteAndClear(t *testing.T) {
	s := &Super{}
	s.NegativeDentryPut(1, "a")
	s.NegativeDentryDelete(1, "a")
	if s.NegativeDentryGet(1, "a") {
		t.Fatal("expected miss after delete")
	}

	s.NegativeDentryPut(1, "b")
	s.NegativeDentryPut(1, "c")
	s.NegativeDentryClear(1)
	if s.NegativeDentryGet(1, "b") || s.NegativeDentryGet(1, "c") {
		t.Fatal("expected miss after clear")
	}
}
