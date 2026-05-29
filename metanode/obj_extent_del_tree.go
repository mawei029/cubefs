// Copyright 2026 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the License);
// you may not use this file except in compliance with the License.

package metanode

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
)

const (
	objExtentDelVersion1      = 1
	minObjExtentDelPayload    = 8  // version + count
	minObjExtentDelSnapRecord = 28 // version + TsMs + Inode + RaftIdx + oek count
)

var (
	// ErrDelTreeItemsFull: queue at cap; new oek not enqueued by design (see enqueueObjExtentDelWrap).
	ErrDelTreeItemsFull   = errors.New("delete tree items full")
	ErrDelTreeUnsupported = errors.New("delete tree payload unsupported")
	ErrDelPayloadTooShort = errors.New("delete payload too short")
)

// objExtentDelTree: in-memory B-tree holding pending ObjExtentKeys to delete.
// objExtentDelItem: B-tree node entry holding pending ObjExtentKeys to delete.
// batchObjExtentDelItems: batch operation carrier holding pending ObjExtentKeys to delete, including new timestamps.

// objExtentDelItem is one pending EBS delete entry. Sort order: TsMs, Inode, RaftIdx (time-first GC).
type objExtentDelItem struct {
	TsMs    int64
	Inode   uint64
	RaftIdx uint64               // raftApplyIndex of the op that created or re-queued this entry (replay-idempotent btree key)
	Oeks    []proto.ObjExtentKey // Oeks are ObjExtentKeys to delete for this btree entry (same apply batch may be coalesced).
}

func cloneObjExtentKeyBlobs(oek proto.ObjExtentKey) proto.ObjExtentKey {
	if len(oek.Blobs) > 0 {
		b := make([]proto.Blob, len(oek.Blobs))
		copy(b, oek.Blobs)
		oek.Blobs = b
	}
	return oek
}

func (x *objExtentDelItem) Less(than BtreeItem) bool {
	y := than.(*objExtentDelItem)
	if x.TsMs != y.TsMs {
		return x.TsMs < y.TsMs
	}
	if x.Inode != y.Inode {
		return x.Inode < y.Inode
	}
	return x.RaftIdx < y.RaftIdx
}

func (x *objExtentDelItem) Copy() BtreeItem {
	cp := objExtentDelItem{TsMs: x.TsMs, Inode: x.Inode, RaftIdx: x.RaftIdx}
	if len(x.Oeks) > 0 {
		cp.Oeks = make([]proto.ObjExtentKey, len(x.Oeks))
		for i := range x.Oeks {
			cp.Oeks[i] = cloneObjExtentKeyBlobs(x.Oeks[i])
		}
	} else {
		cp.Oeks = nil
	}
	return &cp
}

func (x *objExtentDelItem) CopyItem() *objExtentDelItem {
	return x.Copy().(*objExtentDelItem)
}

// keyItem returns a key-only item for btree Delete/Get (Oeks unset).
func (x *objExtentDelItem) keyItem() *objExtentDelItem {
	return &objExtentDelItem{TsMs: x.TsMs, Inode: x.Inode, RaftIdx: x.RaftIdx}
}

// objExtentDelTree holds pending ObjExtentKey deletes (memory btree, replicated dequeue via Raft).
type objExtentDelTree struct {
	mu sync.Mutex
	t  *BTree
}

var _ ObjExtentDelTreeAPI = (*objExtentDelTree)(nil)

func newObjExtentDelTree() *objExtentDelTree {
	return &objExtentDelTree{t: NewBtree()}
}

func (ot *objExtentDelTree) Len() int {
	ot.mu.Lock()
	defer ot.mu.Unlock()
	return ot.t.Len()
}

func (ot *objExtentDelTree) GetTree() *BTree {
	if ot == nil || ot.t == nil {
		return NewBtree()
	}
	return ot.t.GetTree()
}

// EnqueueFromApply inserts discard keys during Raft Apply (same log index as append op → replay-idempotent ReplaceOrInsert).
func (ot *objExtentDelTree) EnqueueFromApply(inode uint64, modifyTimeSec int64, raftApplyIndex uint64, oeks []proto.ObjExtentKey) {
	if ot == nil || len(oeks) == 0 {
		return
	}

	tsMs := normalizeObjExtentDelTsMs(modifyTimeSec, raftApplyIndex)
	list := make([]proto.ObjExtentKey, 0, len(oeks))

	for i := range oeks {
		if oeks[i].IsEmpty() {
			continue
		}
		list = append(list, cloneObjExtentKeyBlobs(oeks[i]))
	}

	if len(list) == 0 {
		return
	}

	it := &objExtentDelItem{TsMs: tsMs, Inode: inode, RaftIdx: raftApplyIndex, Oeks: list}
	ot.mu.Lock()
	defer ot.mu.Unlock()

	// no need to check if the item already exists: the item is unique by (TsMs, Inode, RaftIdx)
	ot.t.ReplaceOrInsert(it, true)
}

// PeekFirstN returns up to n items with smallest keys (copies for caller; tree unchanged).
func (ot *objExtentDelTree) PeekFirstN(n int) batchObjExtentDelItems {
	if ot == nil || n <= 0 {
		return batchObjExtentDelItems{}
	}

	ot.mu.Lock()
	defer ot.mu.Unlock()

	out := batchObjExtentDelItems{Items: make([]*objExtentDelItem, 0, n)}
	ot.t.Ascend(func(it BtreeItem) bool {
		if len(out.Items) >= n {
			return false
		}
		// snapshot; do not expose in-tree pointer to caller。
		cp := it.(*objExtentDelItem).CopyItem()
		out.Items = append(out.Items, cp)
		return true
	})
	return out
}

func (ot *objExtentDelTree) ApplyDequeuePayload(val []byte) error {
	if ot == nil {
		return nil
	}

	var items batchObjExtentDelItems
	if err := items.UnmarshalDequeue(val); err != nil {
		log.LogErrorf("action[ApplyDequeuePayload] dequeue: %v", err)
		return err
	}

	ot.mu.Lock()
	defer ot.mu.Unlock()
	for _, k := range items.Items {
		ot.t.Delete(k)
	}
	return nil
}

// ApplyPunishPayload deletes old keys and re-inserts with newTsMs and RaftIdx=applyIndex.
func (ot *objExtentDelTree) ApplyPunishPayload(val []byte, applyIndex uint64) error {
	if ot == nil {
		return nil
	}

	var batch batchObjExtentDelItems
	if err := batch.UnmarshalPunish(val); err != nil {
		log.LogErrorf("action[ApplyPunishPayload] punish: %v", err)
		return err
	}

	ot.mu.Lock()
	defer ot.mu.Unlock()
	for _, it := range batch.Items {
		ot.objExtentDelPunishReplace(applyIndex, it, batch.NewTime)
	}
	return nil
}

func (ot *objExtentDelTree) Range(start, end *objExtentDelItem, cb func(it *objExtentDelItem) bool) error {
	if ot == nil || ot.t == nil {
		return nil
	}

	callback := func(i BtreeItem) bool {
		return cb(i.(*objExtentDelItem))
	}

	if start == nil {
		start = &objExtentDelItem{}
	}

	if end == nil {
		ot.t.AscendGreaterOrEqual(start, callback)
	} else {
		ot.t.AscendRange(start, end, callback)
	}
	return nil
}

func (it *objExtentDelItem) MarshalSnapshot(buf *bytes.Buffer) error {
	buf.Reset()
	if err := binary.Write(buf, binary.BigEndian, uint32(objExtentDelVersion1)); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, it.TsMs); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, it.Inode); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, it.RaftIdx); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, uint32(len(it.Oeks))); err != nil {
		return err
	}
	for j := range it.Oeks {
		ob, err := it.Oeks[j].MarshalBinary()
		if err != nil {
			return err
		}
		if err := binary.Write(buf, binary.BigEndian, uint32(len(ob))); err != nil {
			return err
		}
		if _, err := buf.Write(ob); err != nil {
			return err
		}
	}
	return nil
}

func (it *objExtentDelItem) UnmarshalSnapshot(data []byte) error {
	if len(data) < minObjExtentDelSnapRecord {
		return ErrDelPayloadTooShort
	}
	br := bytes.NewReader(data)
	var ver uint32
	if err := binary.Read(br, binary.BigEndian, &ver); err != nil {
		return err
	}
	if ver != objExtentDelVersion1 {
		log.LogErrorf("snapshot: %v, version %d", ErrDelTreeUnsupported, ver)
		return ErrDelTreeUnsupported
	}
	if err := binary.Read(br, binary.BigEndian, &it.TsMs); err != nil {
		return err
	}
	if err := binary.Read(br, binary.BigEndian, &it.Inode); err != nil {
		return err
	}
	if err := binary.Read(br, binary.BigEndian, &it.RaftIdx); err != nil {
		return err
	}
	var n uint32
	if err := binary.Read(br, binary.BigEndian, &n); err != nil {
		return err
	}
	it.Oeks = make([]proto.ObjExtentKey, 0, n)
	for i := uint32(0); i < n; i++ {
		var oekLen uint32
		if err := binary.Read(br, binary.BigEndian, &oekLen); err != nil {
			return err
		}
		if oekLen == 0 {
			continue
		}
		ob := make([]byte, oekLen)
		if _, err := io.ReadFull(br, ob); err != nil {
			return err
		}
		var oek proto.ObjExtentKey
		if err := oek.UnmarshalBinary(bytes.NewBuffer(ob)); err != nil {
			return err
		}
		it.Oeks = append(it.Oeks, oek)
	}
	return nil
}

func (ot *objExtentDelTree) objExtentDelPunishReplace(raftIdx uint64, item *objExtentDelItem, newTime int64) {
	oeks := make([]proto.ObjExtentKey, len(item.Oeks))
	for k := range item.Oeks {
		oeks[k] = cloneObjExtentKeyBlobs(item.Oeks[k])
	}

	// delete old key
	oldKey := item.keyItem()
	ot.t.Delete(oldKey)

	// defensive programming: no oeks to delete: Abnormal/Damaged Load, missing/empty oeks should be rejected by UnmarshalPunish
	if len(oeks) == 0 {
		log.LogErrorf("action[objExtentDelPunishReplace] inode[%v] item[%v] no oeks to delete",
			item.Inode, item.keyItem())
		return
	}

	// insert new key, merge oeks if the new key already exists
	nit := &objExtentDelItem{TsMs: newTime, Inode: item.Inode, RaftIdx: raftIdx, Oeks: oeks}
	newKey := nit.keyItem()
	if prev := ot.t.Get(newKey); prev != nil {
		prevItem := prev.(*objExtentDelItem)
		merged := make([]proto.ObjExtentKey, 0, len(prevItem.Oeks)+len(nit.Oeks))
		for _, o := range prevItem.Oeks {
			merged = append(merged, cloneObjExtentKeyBlobs(o))
		}
		merged = append(merged, nit.Oeks...)
		nit.Oeks = merged
	}
	ot.t.ReplaceOrInsert(nit, true)
}

func (ot *objExtentDelTree) restoreFromSnapshot(it *objExtentDelItem) {
	if ot == nil || it == nil {
		return
	}
	cp := it.CopyItem()
	ot.mu.Lock()
	defer ot.mu.Unlock()
	if ot.t == nil {
		ot.t = NewBtree()
	}
	ot.t.ReplaceOrInsert(cp, true)
}

// batchObjExtentDelItem : Encode/Decode for Raft payload (dequeue / punish), batch delete items.
type batchObjExtentDelItems struct {
	Items   []*objExtentDelItem // btree items (peek / dequeue / punish old keys)
	NewTime int64               // new schedule time (ms), set by UnmarshalPunish
}

func (b *batchObjExtentDelItems) MarshalDequeue(buf *bytes.Buffer) error {
	buf.Reset()

	if err := b.writeBatchItemsHeader(buf); err != nil {
		return err
	}

	for _, it := range b.Items {
		if err := binary.Write(buf, binary.BigEndian, it.TsMs); err != nil {
			return err
		}
		if err := binary.Write(buf, binary.BigEndian, it.Inode); err != nil {
			return err
		}
		if err := binary.Write(buf, binary.BigEndian, it.RaftIdx); err != nil {
			return err
		}
	}

	return nil
}

func (b *batchObjExtentDelItems) UnmarshalDequeue(data []byte) error {
	if len(data) < minObjExtentDelPayload {
		return ErrDelPayloadTooShort
	}

	br := bytes.NewReader(data)
	ver, cnt, err := b.readBatchItemsHeader(br)
	if err != nil {
		return err
	}

	switch ver {
	case objExtentDelVersion1:
		b.Items = make([]*objExtentDelItem, 0, cnt)
		for i := uint32(0); i < cnt; i++ {
			var it objExtentDelItem
			if err := binary.Read(br, binary.BigEndian, &it.TsMs); err != nil {
				return err
			}
			if err := binary.Read(br, binary.BigEndian, &it.Inode); err != nil {
				return err
			}
			if err := binary.Read(br, binary.BigEndian, &it.RaftIdx); err != nil {
				return err
			}
			b.Items = append(b.Items, it.keyItem())
		}
		return nil
	default:
		log.LogErrorf("dequeue: unsupported version %d", ver)
		return ErrDelTreeUnsupported
	}
}

// MarshalPunish encodes punish payload: version, cnt, newTsMs, then per item (old key, nOeks, oeks...).
func (b *batchObjExtentDelItems) MarshalPunish(buf *bytes.Buffer, newTsMs int64) error {
	buf.Reset()
	b.NewTime = newTsMs
	if err := b.writeBatchItemsPunishHeader(buf); err != nil {
		return err
	}

	for _, it := range b.Items {
		if err := binary.Write(buf, binary.BigEndian, it.TsMs); err != nil {
			return err
		}
		if err := binary.Write(buf, binary.BigEndian, it.Inode); err != nil {
			return err
		}
		if err := binary.Write(buf, binary.BigEndian, it.RaftIdx); err != nil {
			return err
		}

		if err := binary.Write(buf, binary.BigEndian, uint32(len(it.Oeks))); err != nil {
			return err
		}
		for j := range it.Oeks {
			ob, err := it.Oeks[j].MarshalBinary()
			if err != nil {
				return err
			}
			if err := binary.Write(buf, binary.BigEndian, uint32(len(ob))); err != nil {
				return err
			}
			if _, err := buf.Write(ob); err != nil {
				return err
			}
		}
	}
	return nil
}

// UnmarshalPunish decodes punish payload produced by MarshalPunish.
func (b *batchObjExtentDelItems) UnmarshalPunish(data []byte) error {
	if len(data) < minObjExtentDelPayload {
		return ErrDelPayloadTooShort
	}

	br := bytes.NewReader(data)
	ver, cnt, err := b.readBatchItemsPunishHeader(br)
	if err != nil {
		return err
	}

	switch ver {
	case objExtentDelVersion1:
		for i := uint32(0); i < cnt; i++ {
			var oldTs int64
			var inode, oldIdx uint64
			if err := binary.Read(br, binary.BigEndian, &oldTs); err != nil {
				return err
			}
			if err := binary.Read(br, binary.BigEndian, &inode); err != nil {
				return err
			}
			if err := binary.Read(br, binary.BigEndian, &oldIdx); err != nil {
				return err
			}
			var nOeks uint32
			if err := binary.Read(br, binary.BigEndian, &nOeks); err != nil {
				return err
			}

			if nOeks == 0 {
				log.LogErrorf("umarshal punish item, invalid oek count: %d", nOeks)
				return ErrDelTreeUnsupported
			}

			oeks := make([]proto.ObjExtentKey, 0, nOeks)
			for j := uint32(0); j < nOeks; j++ {
				var oekLen uint32
				if err := binary.Read(br, binary.BigEndian, &oekLen); err != nil {
					return err
				}
				ob := make([]byte, oekLen)
				if _, err := io.ReadFull(br, ob); err != nil {
					return err
				}
				var oek proto.ObjExtentKey
				if err := oek.UnmarshalBinary(bytes.NewBuffer(ob)); err != nil {
					return err
				}
				oeks = append(oeks, oek)
			}
			b.Items = append(b.Items, &objExtentDelItem{
				TsMs:    oldTs,
				Inode:   inode,
				RaftIdx: oldIdx,
				Oeks:    oeks,
			})
		}
		return nil
	default:
		log.LogErrorf("action[UnmarshalPunish] punish decode: unsupported version %d", ver)
		return ErrDelTreeUnsupported
	}
}

func (b *batchObjExtentDelItems) writeBatchItemsHeader(buf *bytes.Buffer) error {
	if err := binary.Write(buf, binary.BigEndian, uint32(objExtentDelVersion1)); err != nil {
		return err
	}
	return binary.Write(buf, binary.BigEndian, uint32(len(b.Items)))
}

func (b *batchObjExtentDelItems) readBatchItemsHeader(br *bytes.Reader) (version, cnt uint32, err error) {
	if err = binary.Read(br, binary.BigEndian, &version); err != nil {
		return
	}
	if err = binary.Read(br, binary.BigEndian, &cnt); err != nil {
		return
	}

	b.Items = make([]*objExtentDelItem, 0, cnt)
	return
}

func (b *batchObjExtentDelItems) writeBatchItemsPunishHeader(buf *bytes.Buffer) error {
	if err := b.writeBatchItemsHeader(buf); err != nil {
		return err
	}
	return binary.Write(buf, binary.BigEndian, b.NewTime)
}

func (b *batchObjExtentDelItems) readBatchItemsPunishHeader(br *bytes.Reader) (version, cnt uint32, err error) {
	version, cnt, err = b.readBatchItemsHeader(br)
	if err != nil {
		return
	}
	if err = binary.Read(br, binary.BigEndian, &b.NewTime); err != nil {
		return
	}
	return
}

func normalizeObjExtentDelTsMs(ts int64, raftApplyIndex uint64) int64 {
	if ts <= 0 {
		return int64(raftApplyIndex)
	}
	// 10^12 is around 2001-09-09 in milliseconds.
	// If ts is already in milliseconds, use it directly.
	if ts >= 1_000_000_000_000 {
		return ts
	}
	return ts * 1000
}
