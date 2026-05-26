package flashnode

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/remotecache/flashnode/cachengine"
	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/stretchr/testify/require"
)

func newTestFlashNode() *FlashNode {
	return &FlashNode{
		legacyMaster: 1,
		missCache:    cachengine.NewMissCache(_defaultMissEntryExpiration, _defaultMaxMissEntryCache),
	}
}

func TestSetDataHitRateMetricWithHits(t *testing.T) {
	f := newTestFlashNode()
	f.Hits = 80
	f.Misses = 20

	fm := &FlashNodeMetrics{
		flashNode:         f,
		MetricDataHitRate: exporter.NewGauge(MetricFlashNodeDataHitRate),
	}
	fm.setDataHitRateMetric()
	require.Equal(t, uint64(1), atomic.LoadUint64(&f.Hits))
	require.Equal(t, uint64(0), atomic.LoadUint64(&f.Misses))
}

func TestSetDataHitRateMetricOnlyMisses(t *testing.T) {
	f := newTestFlashNode()
	f.Hits = 0
	f.Misses = 30

	fm := &FlashNodeMetrics{
		flashNode:         f,
		MetricDataHitRate: exporter.NewGauge(MetricFlashNodeDataHitRate),
	}
	fm.setDataHitRateMetric()
	require.Equal(t, uint64(1), atomic.LoadUint64(&f.Hits))
	require.Equal(t, uint64(0), atomic.LoadUint64(&f.Misses))
}

func TestSetDataHitRateMetricZeroBoth(t *testing.T) {
	f := newTestFlashNode()
	f.Hits = 0
	f.Misses = 0

	fm := &FlashNodeMetrics{
		flashNode:         f,
		MetricDataHitRate: exporter.NewGauge(MetricFlashNodeDataHitRate),
	}
	fm.setDataHitRateMetric()
	require.Equal(t, uint64(1), atomic.LoadUint64(&f.Hits))
	require.Equal(t, uint64(0), atomic.LoadUint64(&f.Misses))
}

func TestSetDataHitRateMetricOnlyHits(t *testing.T) {
	f := newTestFlashNode()
	f.Hits = 100
	f.Misses = 0

	fm := &FlashNodeMetrics{
		flashNode:         f,
		MetricDataHitRate: exporter.NewGauge(MetricFlashNodeDataHitRate),
	}
	fm.setDataHitRateMetric()
	require.Equal(t, uint64(1), atomic.LoadUint64(&f.Hits))
	require.Equal(t, uint64(0), atomic.LoadUint64(&f.Misses))
}

func TestOpCacheReadMissesOnUnmarshalError(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	f := newTestFlashNode()

	p := proto.NewPacketReqID()
	p.Opcode = proto.OpFlashNodeCacheRead

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.opCacheRead(serverConn, p)
	}()

	r := proto.NewPacket()
	require.NoError(t, r.ReadFromConn(clientConn, 3))
	require.Equal(t, proto.OpErr, r.ResultCode)

	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("opCacheRead did not finish")
	}

	require.Equal(t, uint64(0), atomic.LoadUint64(&f.Hits))
	require.Equal(t, uint64(1), atomic.LoadUint64(&f.Misses))
}

func TestOpCacheObjectGetMissesOnUnmarshalError(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	f := newTestFlashNode()

	p := proto.NewPacketReqID()
	p.Opcode = proto.OpFlashNodeCacheReadObject
	p.Size = 1
	p.Data = []byte{'{'}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.opCacheObjectGet(serverConn, p)
	}()

	r := proto.NewPacket()
	require.NoError(t, r.ReadFromConn(clientConn, 3))
	require.Equal(t, proto.OpErr, r.ResultCode)

	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("opCacheObjectGet did not finish")
	}

	require.Equal(t, uint64(0), atomic.LoadUint64(&f.Hits))
	require.Equal(t, uint64(1), atomic.LoadUint64(&f.Misses))
}

func TestSmallObjectGetMissesOnBlockNotFound(t *testing.T) {
	root := t.TempDir()
	disk := &cachengine.Disk{Path: root, TotalSpace: 200 * util.MB, Capacity: 1024, Status: proto.ReadWrite}
	engine, err := cachengine.NewCacheEngine("", 0, cachengine.DefaultCacheMaxUsedRatio, []*cachengine.Disk{disk},
		1024, 1024, 0, 1, 1, nil, cachengine.DefaultExpireTime, nil, false, "", 1024, 100, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Stop()) }()

	f := newTestFlashNode()
	f.cacheEngine = engine

	req := &proto.BatchReadItem{
		Key:    "test-key-small-object-miss",
		Offset: 0,
		Size_:  1024,
		Slot:   1,
	}

	result, _ := f.smallObjectGet(req, "127.0.0.1", 0)
	require.Equal(t, uint32(proto.OpErr), result.ResultCode)

	require.Equal(t, uint64(0), atomic.LoadUint64(&f.Hits))
	require.Equal(t, uint64(1), atomic.LoadUint64(&f.Misses))
}

func newTestFlashNodeWithEngine(t *testing.T) (*FlashNode, *cachengine.CacheEngine) {
	t.Helper()
	root := t.TempDir()
	disk := &cachengine.Disk{Path: root, TotalSpace: 200 * util.MB, Capacity: 1024, Status: proto.ReadWrite}
	// Use high keyRateLimitThreshold so blocks don't get per-key rate limiting
	engine, err := cachengine.NewCacheEngine("", 0, cachengine.DefaultCacheMaxUsedRatio, []*cachengine.Disk{disk},
		1024, 1024, 0, 1, 1, nil, cachengine.DefaultExpireTime, nil, false, "", 1024*1024*1024, 0, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Stop()) })

	f := newTestFlashNode()
	f.cacheEngine = engine
	f.limitRead = util.NewIOLimiterEx(0, 1, 0, 0)
	f.handleReadTimeout = 30000
	f.keyRateLimitThreshold = int32(1024 * 1024 * 1024)
	return f, engine
}

func TestRegisterMetricsIncludesDataHitRate(t *testing.T) {
	f := newTestFlashNode()
	root := t.TempDir()
	disk := &cachengine.Disk{Path: root, TotalSpace: 200 * util.MB, Capacity: 1024, Status: proto.ReadWrite}
	engine, err := cachengine.NewCacheEngine("", 0, cachengine.DefaultCacheMaxUsedRatio, []*cachengine.Disk{disk},
		1024, 1024, 0, 1, 1, nil, cachengine.DefaultExpireTime, nil, false, "", 1024, 100, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Stop()) }()

	f.cacheEngine = engine
	f.registerMetrics([]*cachengine.Disk{disk})
	require.NotNil(t, f.metrics.MetricDataHitRate)
}

func TestDoStatIncludesDataHitRateMetric(t *testing.T) {
	root := t.TempDir()
	disk := &cachengine.Disk{Path: root, TotalSpace: 200 * util.MB, Capacity: 1024, Status: proto.ReadWrite}
	engine, err := cachengine.NewCacheEngine("", 0, cachengine.DefaultCacheMaxUsedRatio, []*cachengine.Disk{disk},
		1024, 1024, 0, 1, 1, nil, cachengine.DefaultExpireTime, nil, false, "", 1024, 100, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Stop()) }()

	f := newTestFlashNode()
	f.cacheEngine = engine
	f.clusterID = "test-cluster"
	f.localAddr = "127.0.0.1:1234"
	f.registerMetrics([]*cachengine.Disk{disk})

	f.Hits = 50
	f.Misses = 50
	f.metrics.doStat()
	require.Equal(t, uint64(1), atomic.LoadUint64(&f.Hits))
	require.Equal(t, uint64(0), atomic.LoadUint64(&f.Misses))
}

func TestSmallObjectGetCountsHits(t *testing.T) {
	f, engine := newTestFlashNodeWithEngine(t)

	uniKey := t.Name() + "_key"
	volume := cachengine.MapKeyToDirectory(uniKey)
	block, err, _, _ := engine.CreateBlockV2(volume, uniKey, uint64(cachengine.DefaultExpireTime/time.Second),
		proto.SMALL_OBJECT_BLOCK_SIZE, "127.0.0.1")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, block.Delete("test")) })

	writeSize := int64(proto.SMALL_OBJECT_BLOCK_SIZE)
	data := make([]byte, writeSize)
	for i := range data {
		data[i] = byte(i % 251)
	}
	crcBuf := make([]byte, proto.CACHE_BLOCK_CRC_SIZE)
	binary.BigEndian.PutUint32(crcBuf, crc32.ChecksumIEEE(data))
	require.NoError(t, block.WriteAtV2(&proto.FlashWriteParam{
		Offset:   0,
		Size:     writeSize,
		Data:     data,
		Crc:      crcBuf,
		DataSize: writeSize,
	}))
	require.NoError(t, block.MaybeWriteCompleted(writeSize))

	req := &proto.BatchReadItem{
		Key:    uniKey,
		Offset: 0,
		Size_:  uint64(writeSize),
		Slot:   1,
		Tid:    "test-tid",
	}

	result, buf := f.smallObjectGet(req, "127.0.0.1", uint64(time.Now().Add(time.Minute).UnixNano()))
	require.Equal(t, uint32(proto.OpOk), result.ResultCode)
	require.NotNil(t, buf)

	require.Equal(t, uint64(1), atomic.LoadUint64(&f.Hits))
	require.Equal(t, uint64(0), atomic.LoadUint64(&f.Misses))
}

func TestOpCacheObjectGetCountsHits(t *testing.T) {
	f, engine := newTestFlashNodeWithEngine(t)

	uniKey := t.Name() + "_key"
	volume := cachengine.MapKeyToDirectory(uniKey)
	block, err, _, _ := engine.CreateBlockV2(volume, uniKey, uint64(cachengine.DefaultExpireTime/time.Second),
		proto.CACHE_BLOCK_PACKET_SIZE, "127.0.0.1")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, block.Delete("test")) })

	data := make([]byte, proto.CACHE_BLOCK_PACKET_SIZE)
	for i := range data {
		data[i] = byte(i % 251)
	}
	crcBuf := make([]byte, proto.CACHE_BLOCK_CRC_SIZE)
	binary.BigEndian.PutUint32(crcBuf, crc32.ChecksumIEEE(data))
	require.NoError(t, block.WriteAtV2(&proto.FlashWriteParam{
		Offset:   0,
		Size:     proto.CACHE_BLOCK_PACKET_SIZE,
		Data:     data,
		Crc:      crcBuf,
		DataSize: proto.CACHE_BLOCK_PACKET_SIZE,
	}))
	require.NoError(t, block.MaybeWriteCompleted(proto.CACHE_BLOCK_PACKET_SIZE))

	serverConn, clientConn := net.Pipe()

	req := &proto.CacheReadRequestBase{
		Key:    uniKey,
		Offset: 0,
		Size_:  uint64(len(data)),
		TTL:    60,
	}
	p := proto.NewPacketReqID()
	p.Opcode = proto.OpFlashNodeCacheReadObject
	p.MarshalDataPb(req)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.opCacheObjectGet(serverConn, p)
	}()

	header := make([]byte, util.PacketHeaderSize)
	require.NoError(t, clientConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = io.ReadFull(clientConn, header)
	require.NoError(t, err)

	firstReply := proto.NewPacket()
	require.NoError(t, firstReply.UnmarshalHeader(header))
	require.Equal(t, proto.OpOk, firstReply.ResultCode)

	dataReply := proto.NewPacket()
	require.NoError(t, dataReply.ReadFromConn(clientConn, 3))
	require.Equal(t, proto.OpOk, dataReply.ResultCode)

	_ = clientConn.Close()
	_ = serverConn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("opCacheObjectGet did not finish")
	}

	require.Equal(t, uint64(1), atomic.LoadUint64(&f.Hits))
	require.Equal(t, uint64(0), atomic.LoadUint64(&f.Misses))
}
