// Copyright 2022 The CubeFS Authors.
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

package blobstore

import (
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	objEks := make([]proto.ObjExtentKey, 0)
	objEkLen := rand.Intn(20)
	expectedFileSize := 0
	for i := 0; i < objEkLen; i++ {
		size := rand.Intn(1000)
		objEks = append(objEks, proto.ObjExtentKey{Size: uint64(size), FileOffset: uint64(expectedFileSize)})
		expectedFileSize += size
	}

	rSlices := make([]rwSlice, 0)
	for i := 0; i < objEkLen; i++ {
		rSlices = append(rSlices, rwSlice{
			index:        0,
			fileOffset:   0,
			size:         uint32(expectedFileSize),
			rOffset:      0,
			rSize:        0,
			read:         0,
			Data:         nil,
			objExtentKey: objEks[i],
		})
	}

	sliceSize := len(rSlices)

	assert.Equal(t, int(sliceSize), int(objEkLen))

	var wg sync.WaitGroup
	pool := New(3, sliceSize)
	wg.Add(sliceSize)
	for _, rs := range rSlices {
		// rs_ := rs
		pool.Execute(&rs, func(param *rwSlice) {
			// syslog.Printf("pool.Execute rs = %v", rs_)
			time.Sleep(1 * time.Second)
			wg.Done()
		})
	}
	wg.Wait()
	pool.Close()
}

func TestNewClampsNegativeSize(t *testing.T) {
	pool := New(1, -1)
	defer pool.Close()
	done := make(chan struct{}, 1)
	pool.Execute(&rwSlice{}, func(op *rwSlice) {
		done <- struct{}{}
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("New(1, negative size) must still accept tasks (size clamped to 0)")
	}
}

func TestNewClampsZeroWorkers(t *testing.T) {
	pool := New(0, 2)
	defer pool.Close()
	done := make(chan struct{}, 1)
	pool.Execute(&rwSlice{}, func(op *rwSlice) {
		done <- struct{}{}
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("New(0, n) must still run tasks (worker clamped to 1)")
	}
}

func TestTaskPoolInstanceExecuteAndClose(t *testing.T) {
	pool := New(1, 2)
	defer pool.Close()

	done := make(chan int, 1)
	pool.Execute(&rwSlice{index: 7}, func(op *rwSlice) {
		done <- op.index
	})

	select {
	case v := <-done:
		require.Equal(t, 7, v)
	case <-time.After(2 * time.Second):
		t.Fatal("task pool execute timeout")
	}
}

func TestExecutorRun(t *testing.T) {
	exec := NewExecutor(1)
	done := make(chan struct{}, 1)

	exec.Run(func() {
		done <- struct{}{}
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("executor run timeout")
	}
}

func TestNewExecutorClampsZeroConcurrency(t *testing.T) {
	exec := NewExecutor(0)
	done := make(chan struct{}, 1)
	exec.Run(func() {
		done <- struct{}{}
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("NewExecutor(0) must clamp maxConcurrency to 1")
	}
}
