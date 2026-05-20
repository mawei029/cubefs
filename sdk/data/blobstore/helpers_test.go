package blobstore

import (
	"sync/atomic"

	"github.com/brahma-adshonor/gohook"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/meta"
)

// SeedLogicalViewForTest 供跨包单测（如 client/fs）注入 ECStreamer 上 logical 尾与 inoVersion。
func SeedLogicalViewForTest(s *ECStreamer, fileSize, inoGen uint64) {
	if s == nil {
		return
	}
	atomic.StoreUint64(&s.fileSize, fileSize)
	atomic.StoreUint64(&s.inoVersion, inoGen)
}

// mustTestECStreamer 构造测试用 ECStreamer（统一 ECStreamOpenArgs，避免各 _test.go 重复适配 API）。
func mustTestECStreamer(ino uint64, r *Reader, w *Writer) *ECStreamer {
	args := ECStreamOpenArgs{
		Ino:       ino,
		VolName:   "v",
		BlockSize: 8 << 20,
		Mw:        &meta.MetaWrapper{},
	}
	s, err := NewECStreamer(args, r, w)
	if err != nil {
		panic(err)
	}
	if fr := s.fReader; fr != nil {
		fr.ecStreamer = s
	}
	if fw := s.fWriter; fw != nil {
		fw.ecStreamer = s
	}
	wireStreamerMetaForFlush(s)
	return s
}

// wireStreamerMetaForFlush 为 CloseStream/Flush 等会走 updateMetaInfo 的单测打桩 GetObjExtents。
func wireStreamerMetaForFlush(s *ECStreamer) {
	if s == nil || s.mw == nil {
		return
	}
	_ = gohook.HookMethod(s.mw, "GetObjExtents", func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
		return 1, 0, nil, nil, nil
	}, nil)
}

// seedDirtyForTest 将 dirty 置 1，便于单测走 Read/Flush 脏路径。
func seedDirtyForTest(s *ECStreamer) {
	if s != nil {
		s.markDirty()
	}
}

// commitLogicalSizeForTest 包装 commitFileSize，替代已删除的 commitLogicalSize 单测入口。
func commitLogicalSizeForTest(s *ECStreamer, size uint64) {
	if s != nil {
		s.commitFileSize(size)
	}
}

// seedStreamerExtentsForTest 为单测设置 oeks 与 logical 尾（替代 Reader 侧已删除的 extent 缓存字段）。
func seedStreamerExtentsForTest(s *ECStreamer, metaSize uint64, oeks []proto.ObjExtentKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.oeks = append([]proto.ObjExtentKey(nil), oeks...)
	s.mu.Unlock()
	atomic.StoreUint64(&s.fileSize, logicalReadBound(metaSize, oeks))
}

// mustTestECStreamerWithEbsc 构造带 Ebsc 的测试流，供 Reader.Read / Writer.TruncateV2 等单测打桩。
func mustTestECStreamerWithEbsc(ino uint64, ebsc *BlobStoreClient, blockSize int) *ECStreamer {
	if blockSize <= 0 {
		blockSize = 8 << 20
	}
	args := ECStreamOpenArgs{
		Ino:       ino,
		BlockSize: blockSize,
		Mw:        &meta.MetaWrapper{},
		Ebsc:      ebsc,
		VolName:   "vol",
	}
	s, err := NewECStreamer(args, nil, nil)
	if err != nil {
		panic(err)
	}
	wireStreamerMetaForFlush(s)
	return s
}

// testWriterWithMwEbsc 返回带 mw/ebsc 的 (streamer, writer)，供 writer_test 打桩 EBS/meta。
func testWriterWithMwEbsc(ino uint64, ebsc *BlobStoreClient) (*ECStreamer, *Writer) {
	s := mustTestECStreamer(ino, nil, nil)
	s.mw = &meta.MetaWrapper{}
	if ebsc != nil {
		s.ebsc = ebsc
	} else {
		s.ebsc = &BlobStoreClient{}
	}
	return s, s.fWriter
}
