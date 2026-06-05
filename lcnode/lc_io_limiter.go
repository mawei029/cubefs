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

package lcnode

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/cubefs/cubefs/util"
	"golang.org/x/time/rate"
)

const (
	bytesPerMB            = 1024 * 1024
	defaultLcIoLimitBurst = 2 * util.BlockSize
	maxBurstExceedRetries = 128
)

type LcNodeIoLimitSnapshot struct {
	ReadMBps         int64 `json:"-"`
	WriteMBps        int64 `json:"-"`
	ReadBytesPerSec  int64 `json:"readBytesPerSec"`
	WriteBytesPerSec int64 `json:"writeBytesPerSec"`
}

type LcNodeIoLimiter struct {
	mu        sync.RWMutex
	readBps   int64
	writeBps  int64
	readRate  *rate.Limiter
	writeRate *rate.Limiter
	// readWaitHook is set by tests in this package to observe WaitRead byte accounting.
	readWaitHook func(int)
	// writeWaitHook is set by tests in this package to observe WaitWrite byte accounting.
	writeWaitHook func(int)
}

func NewLcNodeIoLimiter(readMBps, writeMBps int64) *LcNodeIoLimiter {
	limiter := &LcNodeIoLimiter{
		readRate:  rate.NewLimiter(rate.Inf, defaultLcIoLimitBurst),
		writeRate: rate.NewLimiter(rate.Inf, defaultLcIoLimitBurst),
	}
	limiter.UpdateByMBps(readMBps, writeMBps)
	return limiter
}

func (l *LcNodeIoLimiter) UpdateByMBps(readMBps, writeMBps int64) {
	l.Update(mbpsToBytesPerSec(readMBps), mbpsToBytesPerSec(writeMBps))
}

func (l *LcNodeIoLimiter) Update(readBps, writeBps int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.readBps = normalizeBandwidth(readBps)
	l.writeBps = normalizeBandwidth(writeBps)
	updateRateLimiter(l.readRate, l.readBps)
	updateRateLimiter(l.writeRate, l.writeBps)
}

func (l *LcNodeIoLimiter) WaitRead(ctx context.Context, n int) error {
	if l == nil {
		return nil
	}
	if err := l.wait(ctx, true, n); err != nil {
		return err
	}
	if l.readWaitHook != nil {
		l.readWaitHook(n)
	}
	return nil
}

func (l *LcNodeIoLimiter) WaitWrite(ctx context.Context, n int) error {
	if l == nil {
		return nil
	}
	if err := l.wait(ctx, false, n); err != nil {
		return err
	}
	if l.writeWaitHook != nil {
		l.writeWaitHook(n)
	}
	return nil
}

func (l *LcNodeIoLimiter) Snapshot() LcNodeIoLimitSnapshot {
	if l == nil {
		return LcNodeIoLimitSnapshot{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()

	return LcNodeIoLimitSnapshot{
		ReadMBps:         bytesPerSecToMBps(l.readBps),
		WriteMBps:        bytesPerSecToMBps(l.writeBps),
		ReadBytesPerSec:  l.readBps,
		WriteBytesPerSec: l.writeBps,
	}
}

func (l *LcNodeIoLimiter) wait(ctx context.Context, read bool, n int) error {
	if n <= 0 {
		return nil
	}

	burstRetries := 0
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}

		limiter, burst := l.currentLimiter(read)
		if limiter.Limit() == rate.Inf {
			return nil
		}
		waitN := n
		if waitN > burst {
			waitN = burst
		}
		if waitN <= 0 {
			return fmt.Errorf("rate limiter burst must be positive, remaining=%d", n)
		}
		if err := limiter.WaitN(ctx, waitN); err != nil {
			// Burst may be reduced by Update() after burst is observed but before WaitN().
			// In that case, retry with the latest burst instead of aborting migration flow.
			if isBurstExceedErr(err) {
				burstRetries++
				if burstRetries > maxBurstExceedRetries {
					return fmt.Errorf("rate limiter burst retry exceeded, remaining=%d: %w", n, err)
				}
				runtime.Gosched()
				continue
			}
			return err
		}
		burstRetries = 0
		n -= waitN
	}
	return nil
}

func isBurstExceedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "exceeds limiter's burst")
}

func (l *LcNodeIoLimiter) currentLimiter(read bool) (*rate.Limiter, int) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if read {
		return l.readRate, l.readRate.Burst()
	}
	return l.writeRate, l.writeRate.Burst()
}

func updateRateLimiter(limiter *rate.Limiter, bytesPerSec int64) {
	if bytesPerSec <= 0 {
		limiter.SetLimit(rate.Inf)
		limiter.SetBurst(defaultLcIoLimitBurst)
		return
	}

	burst := int(bytesPerSec)
	if burst < defaultLcIoLimitBurst {
		burst = defaultLcIoLimitBurst
	}
	limiter.SetLimit(rate.Limit(bytesPerSec))
	limiter.SetBurst(burst)
}

func mbpsToBytesPerSec(mbps int64) int64 {
	if mbps <= 0 {
		return 0
	}
	return mbps * bytesPerMB
}

func bytesPerSecToMBps(bytesPerSec int64) int64 {
	if bytesPerSec <= 0 {
		return 0
	}
	return bytesPerSec / bytesPerMB
}

func normalizeBandwidth(bytesPerSec int64) int64 {
	if bytesPerSec <= 0 {
		return 0
	}
	return bytesPerSec
}

func defaultLcIoLimiter() *LcNodeIoLimiter {
	return NewLcNodeIoLimiter(defaultLcReadBandwidthLimitMB, defaultLcWriteBandwidthLimitMB)
}

func ensureLcIoLimiter(l *LcNode) *LcNodeIoLimiter {
	if l.ioLimiter == nil {
		l.ioLimiter = defaultLcIoLimiter()
	}
	return l.ioLimiter
}
