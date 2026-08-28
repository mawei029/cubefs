package blobstore

import (
	"context"
	"fmt"
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

// newTestMetaWrapper returns an empty MetaWrapper with streamer test stubs.
// finishIO calls InodeGet_ll on IO errors; a raw MetaWrapper panics in getPartitionByInode.
func newTestMetaWrapper() *meta.MetaWrapper {
	mw := &meta.MetaWrapper{}
	wireMetaWrapperForStreamerTest(mw)
	return mw
}

func wireMetaWrapperForStreamerTest(mw *meta.MetaWrapper) {
	if mw == nil {
		return
	}
	_ = gohook.HookMethod(mw, "GetObjExtents", func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
		return 1, 0, nil, nil, nil
	}, nil)
	_ = gohook.HookMethod(mw, "InodeGet_ll", func(_ *meta.MetaWrapper, ino uint64, _ bool) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{Inode: ino}, nil
	}, nil)
}

func stubInodeGetLL(mw *meta.MetaWrapper, fn func(*meta.MetaWrapper, uint64, bool) (*proto.InodeInfo, error)) {
	if mw == nil {
		return
	}
	_ = gohook.HookMethod(mw, "InodeGet_ll", fn, nil)
}

// wireStreamerMetaForFlush stubs GetObjExtents and InodeGet_ll for CloseStream/Flush/finishIO.
func wireStreamerMetaForFlush(s *ECStreamer) {
	if s == nil {
		return
	}
	wireMetaWrapperForStreamerTest(s.mw)
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
	s.oeks = NewReadOnlyOeks(oeks)
	s.mu.Unlock()
	atomic.StoreUint64(&s.fileSize, logicalReadBound(metaSize, oeks))
}

// mustTestECStreamerWithEbsc 构造带 Ebsc 的测试流，供 Reader.Read / Writer.TruncateV2 等单测打桩。
func mustTestECStreamerWithEbsc(ino uint64, ebsc *BlobStoreClient, blockSize int, args ...ECStreamOpenArgs) *ECStreamer {
	var arg ECStreamOpenArgs
	if len(args) > 0 {
		arg = args[0]
	}

	if blockSize <= 0 {
		blockSize = 8 << 20
	}
	arg.Ino = ino
	arg.BlockSize = blockSize
	arg.Mw = &meta.MetaWrapper{}
	arg.Ebsc = ebsc
	arg.VolName = "vol"

	s, err := NewECStreamer(arg, nil, nil)
	if err != nil {
		panic(err)
	}
	wireStreamerMetaForFlush(s)
	return s
}

// readUnderStreamerMu runs Reader.Read as ECStreamer.Read does: hold s.mu for OeksLocked / fileSizeView.
func readUnderStreamerMu(s *ECStreamer, ctx context.Context, dst []byte, offset, size int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fReader.Read(ctx, dst, offset, size)
}

// testWriterWithMwEbsc 返回带 mw/ebsc 的 (streamer, writer)，供 writer_test 打桩 EBS/meta。
func testWriterWithMwEbsc(ino uint64, ebsc *BlobStoreClient) (*ECStreamer, *Writer) {
	s := mustTestECStreamer(ino, nil, nil)
	s.mw = newTestMetaWrapper()
	if ebsc != nil {
		s.ebsc = ebsc
	} else {
		s.ebsc = &BlobStoreClient{}
	}
	return s, s.fWriter
}

func setStreamerForTest(c *ECExtentClient, ino uint64, s *ECStreamer) {
	if c == nil || s == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s.client = c
	c.streamers[ino] = s
}

func needsReadViewSyncForTest(c *ECExtentClient, ino uint64) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	s := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		return false
	}
	return s.isDirty()
}

func ensureReaderForTest(s *ECStreamer, cfg ClientConfig) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fReader == nil {
		cfg.ECStreamer = s
		s.fReader = NewReader(cfg)
	}
}

func ensureWriterForTest(s *ECStreamer, cfg ClientConfig) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fWriter == nil {
		cfg.ECStreamer = s
		s.fWriter = NewWriter(cfg)
	}
}

func truncateV2ForTest(w *Writer, ctx context.Context, targetSize uint64) (proto.ObjExtentKey, proto.ObjExtentKey, error) {
	if w == nil || w.ecStreamer == nil || w.ecStreamer.mw == nil || w.ecStreamer.ebsc == nil {
		return proto.ObjExtentKey{}, proto.ObjExtentKey{}, fmt.Errorf("Writer.TruncateV2: writer/mw/ebsc nil")
	}
	objExtents := w.ecStreamer.OeksLocked()
	currentSize := w.ecStreamer.fileSizeViewLocked()
	return w.TruncateV2FromExtents(ctx, targetSize, currentSize, objExtents)
}

// flushAndFreeCacheForTest runs writer Flush then drops IO caches (UT for CloseStream-adjacent path).
func flushAndFreeCacheForTest(s *ECStreamer, ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.fWriter; w != nil {
		if err := w.Flush(s.ino, ctx); err != nil {
			return err
		}
	}
	s.dropIOCachesLocked()
	return s.updateMetaInfo(nil)
}
