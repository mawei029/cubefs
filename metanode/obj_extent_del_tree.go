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
)

// objExtentDelItem is one pending EBS delete entry. Sort order: TsMs, Inode, Uniq (time-first GC).
type objExtentDelItem struct {
	TsMs  int64
	Inode uint64
	Uniq  uint64 // (raftApplyIndex << 20) | seqInBatch
	Oek   proto.ObjExtentKey
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
	if len(x.Oek.Blobs) > 0 {
		cp.Oek.Blobs = make([]proto.Blob, len(x.Oek.Blobs))
		copy(cp.Oek.Blobs, x.Oek.Blobs)
	}
	return &cp
}

// keyItem returns a key-only item for btree Delete/Get (Oek zeroed).
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
	ot.mu.Lock()
	defer ot.mu.Unlock()
	for i := range oeks {
		if oeks[i].IsEmpty() {
			continue
		}
		uniq := (raftApplyIndex << 20) | uint64(i)
		it := &objExtentDelItem{TsMs: tsMs, Inode: inode, Uniq: uniq, Oek: oeks[i]}
		if len(it.Oek.Blobs) > 0 {
			b := make([]proto.Blob, len(it.Oek.Blobs))
			copy(b, it.Oek.Blobs)
			it.Oek.Blobs = b
		}
		ot.t.ReplaceOrInsert(it, true)
	}
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
	var cnt uint32
	if err := binary.Read(br, binary.BigEndian, &cnt); err != nil {
		return err
	}
	if cnt > maxObjExtentDelBatch {
		return fmt.Errorf("punish: too many items %d", cnt)
	}
	ot.mu.Lock()
	defer ot.mu.Unlock()
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
		ot.t.Delete(&objExtentDelItem{TsMs: oldTs, Inode: inode, Uniq: oldUniq})
		newUniq := (applyIndex << 20) | uint64(i)
		nit := &objExtentDelItem{TsMs: newTs, Inode: inode, Uniq: newUniq, Oek: oek}
		if len(nit.Oek.Blobs) > 0 {
			b := make([]proto.Blob, len(nit.Oek.Blobs))
			copy(b, nit.Oek.Blobs)
			nit.Oek.Blobs = b
		}
		ot.t.ReplaceOrInsert(nit, true)
	}
	return nil
}

func (ot *objExtentDelTree) deleteItem(it *objExtentDelItem) {
	if ot == nil || it == nil {
		return
	}
	ot.mu.Lock()
	defer ot.mu.Unlock()
	ot.t.Delete(it.keyItem())
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

// encodeObjExtentGcPunish: count + repeated (oldTs, inode, oldUniq, newTsMs, oekMarshal)
func encodeObjExtentGcPunish(items []*objExtentDelItem, newTsMs int64) ([]byte, error) {
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
		if err := binary.Write(buf, binary.BigEndian, newTsMs); err != nil {
			return nil, err
		}
		ob, err := it.Oek.MarshalBinary()
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
