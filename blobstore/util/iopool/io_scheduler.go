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

package iopool

import (
	"github.com/cubefs/cubefs/blobstore/util/taskpool"
)

// TODO add ut
type IoScheduler interface {
	Schedule(task *IoTask)
	Close()
}

// queue
type QueueIoScheduler struct {
	queue chan func() IoPoolResult
	pool  taskpool.TaskPool
}

func NewQueueIoScheduler(queueLen, threadCnt int) *QueueIoScheduler {
	s := &QueueIoScheduler{
		queue: make(chan func() IoPoolResult, queueLen), // queueLen
		pool:  taskpool.New(threadCnt, threadCnt),
	}
	s.pool.Run(func() {
		s.runLoop()
	})
	return s
}

type IoPoolResult struct {
	offset int // 返回写入的位置
	n      int // 返回实际写的长度
	err    error
}

func (s *QueueIoScheduler) Schedule(taskFn func() IoPoolResult) {
	s.queue <- taskFn
}

func (s *QueueIoScheduler) Close() {
	s.pool.Close()
}

func (s *QueueIoScheduler) runLoop() {
	for task := range s.queue {
		//task.Exec()
		//if task.isSync {
		//	task.Sync()
		//}
		//task.Complete()
		task()
	}
}
