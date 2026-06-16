package buf

import (
	"sync"
	"sync/atomic"
)

const DefaultEbsWriteCacheLimit int64 = 512 // 512 × 8MB = 4GB by default

var CachePool *FileCachePool

type FileCachePool struct {
	pool       *sync.Pool
	blockSize  int
	totalLimit int64
	count      int64
	mu         sync.Mutex
	wait       sync.Cond
}

func newWriterCachePool(blockSize int) *sync.Pool {
	return &sync.Pool{
		New: func() interface{} {
			return make([]byte, blockSize)
		},
	}
}

func newFileCachePool(blockSize int, blockLimit int64) *FileCachePool {
	p := &FileCachePool{
		pool:       newWriterCachePool(blockSize),
		blockSize:  blockSize,
		totalLimit: blockLimit,
	}
	p.wait.L = &p.mu
	return p
}

// InitCachePool configures the EC/Blob write buffer pool.
// blockLimit is the max number of outstanding blocks (default DefaultEbsWriteCacheLimit when <= 0).
func InitCachePool(blockSize int, blockLimit int64) {
	if blockSize == 0 {
		return
	}
	if blockLimit <= 0 {
		blockLimit = DefaultEbsWriteCacheLimit
	}
	CachePool = newFileCachePool(blockSize, blockLimit)
}

// Get borrows one block buffer; blocks when outstanding buffers reach totalLimit.
func (fileCachePool *FileCachePool) Get() []byte {
	if fileCachePool == nil {
		return nil
	}

	fileCachePool.mu.Lock()
	for atomic.LoadInt64(&fileCachePool.count) >= fileCachePool.totalLimit {
		fileCachePool.wait.Wait()
	}
	atomic.AddInt64(&fileCachePool.count, 1)
	fileCachePool.mu.Unlock()

	return fileCachePool.pool.Get().([]byte)
}

func (fileCachePool *FileCachePool) Put(data []byte) {
	if fileCachePool == nil || data == nil {
		return
	}

	fileCachePool.mu.Lock()
	atomic.AddInt64(&fileCachePool.count, -1)
	fileCachePool.wait.Signal()
	fileCachePool.mu.Unlock()

	fileCachePool.pool.Put(data[:fileCachePool.blockSize]) // nolint: staticcheck
}
