package flashnode

import (
	"context"
	"testing"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/remotecache/flashnode/cachengine"
	"github.com/cubefs/cubefs/util"
)

func newRateLimitTestBlock(t *testing.T, key string, allocSize uint64, keyRateLimitThreshold int32, keyLimiterFlow int64) (*cachengine.CacheBlock, *cachengine.CacheEngine) {
	t.Helper()

	disk := &cachengine.Disk{
		Path:       t.TempDir(),
		TotalSpace: 64 * util.MB,
		Capacity:   16,
		Status:     proto.ReadWrite,
	}
	engine, err := cachengine.NewCacheEngine("", 0, cachengine.DefaultCacheMaxUsedRatio, []*cachengine.Disk{disk}, 16, 16, 0, 1, 1, nil,
		cachengine.DefaultExpireTime, nil, false, "", keyRateLimitThreshold, keyLimiterFlow, 0)
	if err != nil {
		t.Fatalf("new cache engine failed: %v", err)
	}
	block, err, _, _ := engine.CreateBlockV2("test", key, proto.DefaultCacheTTLSec, uint32(allocSize), "127.0.0.1")
	if err != nil {
		_ = engine.Stop()
		t.Fatalf("create cache block failed: %v", err)
	}
	return block, engine
}

func TestCheckRateLimit(t *testing.T) {
	keyRateLimitThreshold := int32(1024 * 1024)
	keyLimiterFlow := int64(100)
	allocSize := uint64(2 * 1024 * 1024)
	block, engine := newRateLimitTestBlock(t, "testkey", allocSize, keyRateLimitThreshold, keyLimiterFlow)
	defer func() { _ = engine.Stop() }()
	err := block.CheckRateLimit(context.Background(), 10, uint64(keyRateLimitThreshold))
	if err != nil {
		t.Fatalf("Expected no error when KeyLimiter is available, got: %v", err)
	}

	smallAllocSize := uint64(512 * 1024)
	block2, engine2 := newRateLimitTestBlock(t, "testkey2", smallAllocSize, keyRateLimitThreshold, keyLimiterFlow)
	defer func() { _ = engine2.Stop() }()

	err = block2.CheckRateLimit(context.Background(), 10, uint64(keyRateLimitThreshold))
	if err != nil {
		t.Fatalf("Expected no error when KeyLimiter is nil, got: %v", err)
	}
}

func TestCheckRateLimitWithContextDeadline(t *testing.T) {
	keyRateLimitThreshold := int32(1024 * 1024)
	keyLimiterFlow := int64(100)
	allocSize := uint64(2 * 1024 * 1024)
	block, engine := newRateLimitTestBlock(t, "testkey-deadline", allocSize, keyRateLimitThreshold, keyLimiterFlow)
	defer func() { _ = engine.Stop() }()

	if err := block.CheckRateLimit(context.Background(), int(keyLimiterFlow/2), uint64(keyRateLimitThreshold)); err != nil {
		t.Fatalf("Expected initial token drain success, got: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := block.CheckRateLimit(ctx, 10, uint64(keyRateLimitThreshold))
	if err != util.LimitedFlowError {
		t.Fatalf("Expected LimitedFlowError when deadline expires, got: %v", err)
	}
}
