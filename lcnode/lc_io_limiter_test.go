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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestIsBurstExceedErr(t *testing.T) {
	require.False(t, isBurstExceedErr(nil))
	require.False(t, isBurstExceedErr(errors.New("other error")))

	lim := rate.NewLimiter(rate.Limit(1024), 16)
	err := lim.WaitN(context.Background(), 32)
	require.Error(t, err)
	require.True(t, isBurstExceedErr(err))
}

func TestWaitReadLargeRequestUnlimited(t *testing.T) {
	limiter := NewLcNodeIoLimiter(0, 0)
	var waits []int
	limiter.readWaitHook = func(n int) {
		waits = append(waits, n)
	}

	require.NoError(t, limiter.WaitRead(context.Background(), 8192))
	require.Equal(t, []int{8192}, waits)
}

func TestWaitWriteInvokesHook(t *testing.T) {
	limiter := NewLcNodeIoLimiter(0, 0)
	var got []int
	limiter.writeWaitHook = func(n int) {
		got = append(got, n)
	}
	require.NoError(t, limiter.WaitWrite(context.Background(), 16))
	require.Equal(t, []int{16}, got)
}

func TestWaitReadDoesNotSpinOnZeroBurst(t *testing.T) {
	limiter := NewLcNodeIoLimiter(0, 0)
	limiter.Update(1024, 1024)
	limiter.mu.Lock()
	limiter.readRate.SetBurst(0)
	limiter.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := limiter.WaitRead(ctx, 1024)
	require.Error(t, err)
	require.Contains(t, err.Error(), "burst must be positive")
}

func TestNilLcNodeIoLimiterWait(t *testing.T) {
	var limiter *LcNodeIoLimiter
	require.NoError(t, limiter.WaitRead(context.Background(), 1024))
	require.NoError(t, limiter.WaitWrite(context.Background(), 1024))
	require.Equal(t, LcNodeIoLimitSnapshot{}, limiter.Snapshot())
}

func TestWaitReadZeroOrNegativeN(t *testing.T) {
	limiter := NewLcNodeIoLimiter(1, 1)
	require.NoError(t, limiter.WaitRead(context.Background(), 0))
	require.NoError(t, limiter.WaitRead(context.Background(), -1))
}

func TestWaitReadContextCanceled(t *testing.T) {
	limiter := NewLcNodeIoLimiter(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := limiter.WaitRead(ctx, 1024*1024)
	require.Error(t, err)
	require.False(t, isBurstExceedErr(err))
}

func TestEnsureLcIoLimiterInitializesNilField(t *testing.T) {
	node := &LcNode{}
	limiter := ensureLcIoLimiter(node)
	require.NotNil(t, limiter)
	require.Same(t, limiter, node.ioLimiter)
}

func TestTransitionMgrLimiterOrDefault(t *testing.T) {
	tm := &TransitionMgr{}
	require.NotNil(t, tm.limiterOrDefault())

	tm.limiter = NewLcNodeIoLimiter(1, 1)
	require.Same(t, tm.limiter, tm.limiterOrDefault())
}
