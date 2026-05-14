// Copyright 2026 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the License);
// you may not use this file except in compliance with the License.

package metanode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/cubefs/cubefs/proto"
)

const (
	// limit payload item count to avoid malformed input allocating huge memory.
	maxObjExtentDelBatch = 1 << 20
	// objExtentGcPunishPayloadMagicV2 marks punish Raft payloads that carry multiple ObjExtentKey
	// per btree item. Must exceed maxObjExtentDelBatch so it cannot collide with legacy leading uint32 (item count).
	objExtentGcPunishPayloadMagicV2 uint32 = 0xDEADC0DE
)

// objExtentDelItem is one pending EBS delete entry. Sort order: TsMs, Inode, Uniq (time-first GC).
type objExtentDelItem struct {
	TsMs  int64
	Inode uint64
	Uniq  uint64 // (raftApplyIndex << 20) | batchSlot (one slot per enqueue for replay idempotency)
	// Oeks are ObjExtentKeys to delete for this btree entry (same apply batch may be coalesced).
	Oeks []proto.ObjExtentKey
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
	return x.Uniq < y.Uniq
}

func (x *objExtentDelItem) Copy() BtreeItem {
	cp := *x
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

// keyItem returns a key-only item for btree Delete/Get (Oeks unset).
func (x *objExtentDelItem) keyItem() *objExtentDelItem {
	return &objExtentDelItem{TsMs: x.TsMs, Inode: x.Inode, Uniq: x.Uniq}
}

// objExtentDelTree holds pending ObjExtentKey deletes (memory btree, replicated dequeue via Raft).
type objExtentDelTree struct {
	mu sync.Mutex
	t  *BTree
}

var _ ObjExtentDelTree = (*objExtentDelTree)(nil)

func newObjExtentDelTree() *objExtentDelTree {
	return &objExtentDelTree{t: NewBtree()}
}

func (ot *objExtentDelTree) Len() int {
	ot.mu.Lock()
	defer ot.mu.Unlock()
	return ot.t.Len()
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
	uniq := raftApplyIndex << 20
	it := &objExtentDelItem{TsMs: tsMs, Inode: inode, Uniq: uniq, Oeks: list}
	ot.mu.Lock()
	defer ot.mu.Unlock()
	lookupKey := it.keyItem()
	if prev := ot.t.Get(lookupKey); prev != nil {
		prevItem := prev.(*objExtentDelItem)
		merged := make([]proto.ObjExtentKey, 0, len(prevItem.Oeks)+len(list))
		for _, o := range prevItem.Oeks {
			merged = append(merged, cloneObjExtentKeyBlobs(o))
		}
		merged = append(merged, list...)
		it.Oeks = merged
	}
	ot.t.ReplaceOrInsert(it, true)
}

// PeekFirstN returns up to n items with smallest keys (copies for caller; tree unchanged).
func (ot *objExtentDelTree) PeekFirstN(n int) []*objExtentDelItem {
	if ot == nil || n <= 0 {
		return nil
	}
	ot.mu.Lock()
	defer ot.mu.Unlock()
	out := make([]*objExtentDelItem, 0, n)
	ot.t.Ascend(func(it BtreeItem) bool {
		if len(out) >= n {
			return false
		}
		src := it.(*objExtentDelItem)
		cp := src.Copy().(*objExtentDelItem)
		out = append(out, cp)
		return true
	})
	return out
}

func (ot *objExtentDelTree) ApplyDequeuePayload(val []byte) error {
	if ot == nil {
		return nil
	}
	keys, err := decodeObjExtentGcDequeueKeys(val)
	if err != nil {
		return err
	}
	ot.mu.Lock()
	defer ot.mu.Unlock()
	for _, k := range keys {
		ot.t.Delete(k)
	}
	return nil
}

// ApplyPunishPayload deletes old keys and re-inserts with newTsMs and Uniq=(applyIndex<<20)|slot.
func (ot *objExtentDelTree) ApplyPunishPayload(val []byte, applyIndex uint64) error {
	if ot == nil {
		return nil
	}
	br := bytes.NewReader(val)
	var first uint32
	if err := binary.Read(br, binary.BigEndian, &first); err != nil {
		return err
	}
	ot.mu.Lock()
	defer ot.mu.Unlock()
	if first == objExtentGcPunishPayloadMagicV2 {
		return ot.applyPunishPayloadV2(br, applyIndex)
	}
	return ot.applyPunishPayloadLegacy(br, first, applyIndex)
}

func (ot *objExtentDelTree) applyPunishPayloadLegacy(br *bytes.Reader, cnt uint32, applyIndex uint64) error {
	if cnt > maxObjExtentDelBatch {
		return fmt.Errorf("punish: too many items %d", cnt)
	}
	for i := uint32(0); i < cnt; i++ {
		var oldTs int64
		var inode, oldUniq uint64
		var newTs int64
		if err := binary.Read(br, binary.BigEndian, &oldTs); err != nil {
			return err
		}
		if err := binary.Read(br, binary.BigEndian, &inode); err != nil {
			return err
		}
		if err := binary.Read(br, binary.BigEndian, &oldUniq); err != nil {
			return err
		}
		if err := binary.Read(br, binary.BigEndian, &newTs); err != nil {
			return err
		}
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
		ot.objExtentDelPunishReplace(oldTs, inode, oldUniq, newTs, (applyIndex<<20)|uint64(i), []proto.ObjExtentKey{oek})
	}
	return nil
}

func (ot *objExtentDelTree) applyPunishPayloadV2(br *bytes.Reader, applyIndex uint64) error {
	var cnt uint32
	if err := binary.Read(br, binary.BigEndian, &cnt); err != nil {
		return err
	}
	if cnt > maxObjExtentDelBatch {
		return fmt.Errorf("punish: too many items %d", cnt)
	}
	for i := uint32(0); i < cnt; i++ {
		var oldTs int64
		var inode, oldUniq uint64
		var newTs int64
		if err := binary.Read(br, binary.BigEndian, &oldTs); err != nil {
			return err
		}
		if err := binary.Read(br, binary.BigEndian, &inode); err != nil {
			return err
		}
		if err := binary.Read(br, binary.BigEndian, &oldUniq); err != nil {
			return err
		}
		if err := binary.Read(br, binary.BigEndian, &newTs); err != nil {
			return err
		}
		var nOeks uint32
		if err := binary.Read(br, binary.BigEndian, &nOeks); err != nil {
			return err
		}
		if nOeks == 0 || nOeks > maxObjExtentDelBatch {
			return fmt.Errorf("punish v2: invalid oek count %d", nOeks)
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
		ot.objExtentDelPunishReplace(oldTs, inode, oldUniq, newTs, (applyIndex<<20)|uint64(i), oeks)
	}
	return nil
}

func (ot *objExtentDelTree) objExtentDelPunishReplace(oldTs int64, inode, oldUniq uint64, newTs int64, newUniq uint64, oeks []proto.ObjExtentKey) {
	ot.t.Delete(&objExtentDelItem{TsMs: oldTs, Inode: inode, Uniq: oldUniq})
	nit := &objExtentDelItem{TsMs: newTs, Inode: inode, Uniq: newUniq, Oeks: make([]proto.ObjExtentKey, len(oeks))}
	for k := range oeks {
		nit.Oeks[k] = cloneObjExtentKeyBlobs(oeks[k])
	}
	ot.t.ReplaceOrInsert(nit, true)
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

func encodeObjExtentGcDequeueKeys(items []*objExtentDelItem) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	if err := binary.Write(buf, binary.BigEndian, uint32(len(items))); err != nil {
		return nil, err
	}
	for _, it := range items {
		if err := binary.Write(buf, binary.BigEndian, it.TsMs); err != nil {
			return nil, err
		}
		if err := binary.Write(buf, binary.BigEndian, it.Inode); err != nil {
			return nil, err
		}
		if err := binary.Write(buf, binary.BigEndian, it.Uniq); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func decodeObjExtentGcDequeueKeys(b []byte) ([]*objExtentDelItem, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("dequeue: short buf")
	}
	br := bytes.NewReader(b)
	var cnt uint32
	if err := binary.Read(br, binary.BigEndian, &cnt); err != nil {
		return nil, err
	}
	if cnt > maxObjExtentDelBatch {
		return nil, fmt.Errorf("dequeue: too many items %d", cnt)
	}
	out := make([]*objExtentDelItem, 0, cnt)
	for i := uint32(0); i < cnt; i++ {
		var it objExtentDelItem
		if err := binary.Read(br, binary.BigEndian, &it.TsMs); err != nil {
			return nil, err
		}
		if err := binary.Read(br, binary.BigEndian, &it.Inode); err != nil {
			return nil, err
		}
		if err := binary.Read(br, binary.BigEndian, &it.Uniq); err != nil {
			return nil, err
		}
		out = append(out, it.keyItem())
	}
	return out, nil
}

// encodeObjExtentGcPunish encodes punish payload. Legacy format (single ObjExtentKey per item) is used when
// every item has exactly one key, so old Raft entries remain decodable. Multi-key items use v2 (magic prefix).
func encodeObjExtentGcPunish(items []*objExtentDelItem, newTsMs int64) ([]byte, error) {
	useV2 := false
	for _, it := range items {
		if len(it.Oeks) != 1 {
			useV2 = true
			break
		}
	}
	buf := bytes.NewBuffer(nil)
	if useV2 {
		if err := binary.Write(buf, binary.BigEndian, objExtentGcPunishPayloadMagicV2); err != nil {
			return nil, err
		}
		if err := binary.Write(buf, binary.BigEndian, uint32(len(items))); err != nil {
			return nil, err
		}
		for _, it := range items {
			if err := binary.Write(buf, binary.BigEndian, it.TsMs); err != nil {
				return nil, err
			}
			if err := binary.Write(buf, binary.BigEndian, it.Inode); err != nil {
				return nil, err
			}
			if err := binary.Write(buf, binary.BigEndian, it.Uniq); err != nil {
				return nil, err
			}
			if err := binary.Write(buf, binary.BigEndian, newTsMs); err != nil {
				return nil, err
			}
			n := uint32(len(it.Oeks))
			if n == 0 || n > maxObjExtentDelBatch {
				return nil, fmt.Errorf("punish encode v2: invalid oek count %d", n)
			}
			if err := binary.Write(buf, binary.BigEndian, n); err != nil {
				return nil, err
			}
			for j := range it.Oeks {
				ob, err := it.Oeks[j].MarshalBinary()
				if err != nil {
					return nil, err
				}
				if err := binary.Write(buf, binary.BigEndian, uint32(len(ob))); err != nil {
					return nil, err
				}
				if _, err := buf.Write(ob); err != nil {
					return nil, err
				}
			}
		}
		return buf.Bytes(), nil
	}
	if err := binary.Write(buf, binary.BigEndian, uint32(len(items))); err != nil {
		return nil, err
	}
	for _, it := range items {
		if len(it.Oeks) != 1 {
			return nil, fmt.Errorf("punish encode legacy: expected exactly one ObjExtentKey per item")
		}
		if err := binary.Write(buf, binary.BigEndian, it.TsMs); err != nil {
			return nil, err
		}
		if err := binary.Write(buf, binary.BigEndian, it.Inode); err != nil {
			return nil, err
		}
		if err := binary.Write(buf, binary.BigEndian, it.Uniq); err != nil {
			return nil, err
		}
		if err := binary.Write(buf, binary.BigEndian, newTsMs); err != nil {
			return nil, err
		}
		ob, err := it.Oeks[0].MarshalBinary()
		if err != nil {
			return nil, err
		}
		if err := binary.Write(buf, binary.BigEndian, uint32(len(ob))); err != nil {
			return nil, err
		}
		if _, err := buf.Write(ob); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
