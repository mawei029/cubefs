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

type Instance struct {
	mq chan task
}

type task struct {
	op *rwSlice
	fn func(op *rwSlice)
}

// New starts worker goroutines that drain mq. worker is clamped to at least 1; with 0 workers,
// parallel slice work would never run and callers would deadlock on wg.Wait (e.g. blob Reader readEbsRange).
func New(worker int, size int) Instance {
	if worker < 1 {
		worker = 1
	}
	if size < 0 {
		size = 0
	}
	mq := make(chan task, size)
	for i := 0; i < worker; i++ {
		go func() {
			for {
				task, ok := <-mq
				if !ok {
					break
				}
				task.fn(task.op)
			}
		}()
	}
	return Instance{mq}
}

func (r Instance) Execute(op *rwSlice, fn func(op *rwSlice)) {
	r.mq <- task{
		op: op,
		fn: fn,
	}
}

func (r Instance) Close() {
	close(r.mq)
}

type Executor struct {
	tokens chan int
}

func NewExecutor(maxConcurrency int) *Executor {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	exec := &Executor{
		tokens: make(chan int, maxConcurrency),
	}
	for i := 0; i < maxConcurrency; i++ {
		exec.tokens <- i
	}
	return exec
}

func (exec *Executor) Run(fn func()) {
	i := <-exec.tokens
	go func() {
		fn()
		exec.tokens <- i
	}()
}
