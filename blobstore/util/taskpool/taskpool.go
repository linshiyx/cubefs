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

// Package taskpool provides limited pool running task
package taskpool

import (
	"sync"
	"sync/atomic"
)

// TaskPool limited pool
type TaskPool struct {
	pool  chan func()
	wg    *sync.WaitGroup
	doing *uint32
}

// New returns task pool with workerCount and poolSize
func New(workerCount, poolSize int) TaskPool {
	pool := make(chan func(), poolSize)
	wg := &sync.WaitGroup{}
	doing := uint32(0)
	tp := TaskPool{pool: pool, wg: wg, doing: &doing}

	wg.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func() {
			defer wg.Done()
			for {
				task, ok := <-pool
				if !ok {
					break
				}
				atomic.AddUint32(tp.doing, 1)
				task()
				atomic.AddUint32(tp.doing, ^uint32(0))
			}
		}()
	}
	return tp
}

// Run add task to pool, block if pool is full
func (tp TaskPool) Run(task func()) {
	tp.pool <- task
}

// TryRun try to add task to pool, return immediately
func (tp TaskPool) TryRun(task func()) bool {
	select {
	case tp.pool <- task:
		return true
	default:
		return false
	}
}

func (tp TaskPool) Running() uint32 {
	return atomic.LoadUint32(tp.doing)
}

// Close the pool, the function is concurrent unsafe
func (tp TaskPool) Close() {
	close(tp.pool)
	tp.wg.Wait()
}
