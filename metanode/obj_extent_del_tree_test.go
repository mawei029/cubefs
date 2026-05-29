package metanode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/require"
)

func writeObjExtentDelV1DequeueHeader(buf *bytes.Buffer, cnt uint32) {
	_ = binary.Write(buf, binary.BigEndian, uint32(objExtentDelVersion1))
	_ = binary.Write(buf, binary.BigEndian, cnt)
}

func writeObjExtentDelV1PunishHeader(buf *bytes.Buffer, cnt uint32, newTsMs int64) {
	writeObjExtentDelV1DequeueHeader(buf, cnt)
	_ = binary.Write(buf, binary.BigEndian, newTsMs)
}

func encodeBatchDequeue(b batchObjExtentDelItems) ([]byte, error) {
	var buf bytes.Buffer
	if err := b.MarshalDequeue(&buf); err != nil {
		return nil, err
	}
	return append([]byte(nil), buf.Bytes()...), nil
}

func encodeBatchPunish(b batchObjExtentDelItems, newTsMs int64) ([]byte, error) {
	var buf bytes.Buffer
	if err := b.MarshalPunish(&buf, newTsMs); err != nil {
		return nil, err
	}
	return append([]byte(nil), buf.Bytes()...), nil
}

func TestObjExtentDelTreeEnqueueAndDequeue(t *testing.T) {
	ot := newObjExtentDelTree()
	oek := createTestObjExtentKey(0, 1024, 1)

	ot.EnqueueFromApply(42, 0, 7, []proto.ObjExtentKey{oek})
	require.Equal(t, 1, ot.Len())

	items := ot.PeekFirstN(1)
	require.Len(t, items.Items, 1)
	require.Equal(t, int64(7), items.Items[0].TsMs)
	require.Equal(t, uint64(42), items.Items[0].Inode)
	require.Equal(t, uint64(7), items.Items[0].RaftIdx)
	require.Len(t, items.Items[0].Oeks, 1)
	require.True(t, items.Items[0].Oeks[0].IsEquals(&oek))

	val, err := encodeBatchDequeue(items)
	require.NoError(t, err)
	require.Equal(t, byte(objExtentDelVersion1), val[3])
	require.NoError(t, ot.ApplyDequeuePayload(val))
	require.Equal(t, 0, ot.Len())
}

func TestObjExtentDelTreeApplyPunishPayload(t *testing.T) {
	ot := newObjExtentDelTree()
	oek := createTestObjExtentKey(128, 2048, 2)
	ot.EnqueueFromApply(99, 1700000000, 3, []proto.ObjExtentKey{oek})

	oldItems := ot.PeekFirstN(1)
	require.Len(t, oldItems.Items, 1)

	newTs := int64(1700000005000)
	payload, err := encodeBatchPunish(oldItems, newTs)
	require.NoError(t, err)
	require.Equal(t, byte(objExtentDelVersion1), payload[3])
	require.NoError(t, ot.ApplyPunishPayload(payload, 11))

	items := ot.PeekFirstN(1)
	require.Len(t, items.Items, 1)
	require.Equal(t, newTs, items.Items[0].TsMs)
	require.Equal(t, uint64(11), items.Items[0].RaftIdx)
	require.Equal(t, oldItems.Items[0].Inode, items.Items[0].Inode)
	require.Len(t, items.Items[0].Oeks, 1)
	require.True(t, items.Items[0].Oeks[0].IsEquals(&oek))
}

func TestNormalizeObjExtentDelTsMs(t *testing.T) {
	require.Equal(t, int64(9), normalizeObjExtentDelTsMs(0, 9))
	require.Equal(t, int64(1700000000*1000), normalizeObjExtentDelTsMs(1700000000, 1))
	require.Equal(t, int64(1700000000000), normalizeObjExtentDelTsMs(1700000000000, 1))
}

func TestObjExtentDelTreeEdgeBranches(t *testing.T) {
	var nilTree *objExtentDelTree
	require.Empty(t, nilTree.PeekFirstN(1).Items)
	nilTree.EnqueueFromApply(1, 0, 1, nil)
	require.NoError(t, nilTree.ApplyDequeuePayload(nil))
	require.NoError(t, nilTree.ApplyPunishPayload(nil, 1))

	ot := newObjExtentDelTree()
	ot.EnqueueFromApply(1, 0, 2, []proto.ObjExtentKey{{}})
	require.Equal(t, 0, ot.Len())
	require.Empty(t, ot.PeekFirstN(0).Items)
}

func TestObjExtentDelItemCopy_emptyOeks(t *testing.T) {
	it := &objExtentDelItem{TsMs: 1, Inode: 2, RaftIdx: 3, Oeks: nil}
	cp := it.Copy().(*objExtentDelItem)
	require.Nil(t, cp.Oeks)
	require.Equal(t, it.TsMs, cp.TsMs)
	require.NotSame(t, it, cp)
}

func TestObjExtentDelItem_CopyItem(t *testing.T) {
	it := &objExtentDelItem{TsMs: 1, Inode: 2, RaftIdx: 3, Oeks: []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)}}
	cp := it.CopyItem()
	require.NotSame(t, it, cp)
	require.True(t, cp.Oeks[0].IsEquals(&it.Oeks[0]))
}

func TestBatchObjExtentDelItems_MarshalEncodeErrors(t *testing.T) {
	it := &objExtentDelItem{TsMs: 1, Inode: 2, RaftIdx: 3, Oeks: []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)}}
	batch := batchObjExtentDelItems{Items: []*objExtentDelItem{it}}

	t.Run("MarshalDequeue write fails", func(t *testing.T) {
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyFunc(binary.Write, func(_ io.Writer, _ binary.ByteOrder, _ interface{}) error {
			return errors.New("write fail")
		})
		_, err := encodeBatchDequeue(batch)
		require.Error(t, err)
	})

	t.Run("MarshalPunish empty oeks encodes then unmarshal rejects", func(t *testing.T) {
		empty := batchObjExtentDelItems{Items: []*objExtentDelItem{{TsMs: 1, Inode: 2, RaftIdx: 3}}}
		raw, err := encodeBatchPunish(empty, 99)
		require.NoError(t, err)
		var decoded batchObjExtentDelItems
		err = decoded.UnmarshalPunish(raw)
		require.ErrorIs(t, err, ErrDelTreeUnsupported)
	})

	t.Run("MarshalPunish write fails", func(t *testing.T) {
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyFunc(binary.Write, func(_ io.Writer, _ binary.ByteOrder, _ interface{}) error {
			return errors.New("write fail")
		})
		_, err := encodeBatchPunish(batch, 99)
		require.Error(t, err)
	})
}

func TestBatchObjExtentDelItems_MarshalDequeueMultiItemWriteErrors(t *testing.T) {
	it1 := &objExtentDelItem{TsMs: 1, Inode: 2, RaftIdx: 3, Oeks: []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)}}
	it2 := &objExtentDelItem{TsMs: 4, Inode: 5, RaftIdx: 6, Oeks: []proto.ObjExtentKey{createTestObjExtentKey(10, 2, 2)}}
	batch := batchObjExtentDelItems{Items: []*objExtentDelItem{it1, it2}}

	failOnNthWrite := func(t *testing.T, n int) {
		t.Helper()
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		calls := 0
		patches.ApplyFunc(binary.Write, func(w io.Writer, order binary.ByteOrder, data interface{}) error {
			calls++
			if calls == n {
				return errors.New("write fail")
			}
			return binary.Write(w, order, data)
		})
		_, err := encodeBatchDequeue(batch)
		require.Error(t, err)
	}

	// version(1) + count(2) + first item ts/inode/raftIdx(3) => 6th write is second item TsMs
	t.Run("second item TsMs", func(t *testing.T) { failOnNthWrite(t, 6) })
	t.Run("second item Inode", func(t *testing.T) { failOnNthWrite(t, 7) })
	t.Run("second item RaftIdx", func(t *testing.T) { failOnNthWrite(t, 8) })
}

func TestBatchObjExtentDelItems_MarshalPunishPerOekErrors(t *testing.T) {
	a := createTestObjExtentKey(0, 10, 1)
	b := createTestObjExtentKey(10, 20, 2)
	batch := batchObjExtentDelItems{Items: []*objExtentDelItem{
		{TsMs: 1, Inode: 2, RaftIdx: 3, Oeks: []proto.ObjExtentKey{a, b}},
	}}

	t.Run("MarshalBinary fails", func(t *testing.T) {
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(&proto.ObjExtentKey{}), "MarshalBinary",
			func(_ *proto.ObjExtentKey) ([]byte, error) {
				return nil, errors.New("marshal fail")
			})
		_, err := encodeBatchPunish(batch, 99)
		require.Error(t, err)
	})

	t.Run("second oek length write fails", func(t *testing.T) {
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		calls := 0
		patches.ApplyFunc(binary.Write, func(w io.Writer, order binary.ByteOrder, data interface{}) error {
			calls++
			// version + cnt + newTs + item header(4) + first oek len(8) => 9th is second oek len
			if calls == 9 {
				return errors.New("write fail")
			}
			return binary.Write(w, order, data)
		})
		_, err := encodeBatchPunish(batch, 99)
		require.Error(t, err)
	})
}

func TestBatchObjExtentDelItems_MarshalPunishMultiItemWriteErrors(t *testing.T) {
	it1 := &objExtentDelItem{TsMs: 1, Inode: 2, RaftIdx: 3, Oeks: []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)}}
	it2 := &objExtentDelItem{TsMs: 4, Inode: 5, RaftIdx: 6, Oeks: []proto.ObjExtentKey{createTestObjExtentKey(10, 2, 2)}}
	batch := batchObjExtentDelItems{Items: []*objExtentDelItem{it1, it2}}

	failOnNthWrite := func(t *testing.T, n int) {
		t.Helper()
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		calls := 0
		patches.ApplyFunc(binary.Write, func(w io.Writer, order binary.ByteOrder, data interface{}) error {
			calls++
			if calls == n {
				return errors.New("write fail")
			}
			return binary.Write(w, order, data)
		})
		_, err := encodeBatchPunish(batch, 99)
		require.Error(t, err)
	}

	// version + cnt + newTs + first item(7 writes) + second item oldTs
	t.Run("second item oldTs", func(t *testing.T) { failOnNthWrite(t, 10) })
	t.Run("second item inode", func(t *testing.T) { failOnNthWrite(t, 11) })
	t.Run("second item raftIdx", func(t *testing.T) { failOnNthWrite(t, 12) })
}

func TestBatchObjExtentDelItems_UnmarshalPunish_invalidOekCount(t *testing.T) {
	var buf bytes.Buffer
	writeObjExtentDelV1PunishHeader(&buf, 1, 4)
	require.NoError(t, binary.Write(&buf, binary.BigEndian, int64(1)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint64(2)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint64(3)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(0)))

	var batch batchObjExtentDelItems
	err := batch.UnmarshalPunish(buf.Bytes())
	require.ErrorIs(t, err, ErrDelTreeUnsupported)
}

func TestUnmarshalDequeue_unsupportedVersion(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(99)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(0)))

	var batch batchObjExtentDelItems
	err := batch.UnmarshalDequeue(buf.Bytes())
	require.ErrorIs(t, err, ErrDelTreeUnsupported)
}

func TestUnmarshalPunish_unsupportedVersion(t *testing.T) {
	var buf bytes.Buffer
	writeObjExtentDelV1PunishHeader(&buf, 0, 0)
	buf.Bytes()[3] = 99 // patch version byte in big-endian uint32

	var batch batchObjExtentDelItems
	err := batch.UnmarshalPunish(buf.Bytes())
	require.ErrorIs(t, err, ErrDelTreeUnsupported)
}

func TestMarshalDequeue_emptyBatch(t *testing.T) {
	var batch batchObjExtentDelItems
	raw, err := encodeBatchDequeue(batch)
	require.NoError(t, err)
	require.Len(t, raw, 8)

	var decoded batchObjExtentDelItems
	require.NoError(t, decoded.UnmarshalDequeue(raw))
	require.Empty(t, decoded.Items)
}

func TestUnmarshalDequeue_truncatedWhenCountLarge(t *testing.T) {
	var buf bytes.Buffer
	writeObjExtentDelV1DequeueHeader(&buf, 64)

	var batch batchObjExtentDelItems
	require.Error(t, batch.UnmarshalDequeue(buf.Bytes()))
}

func TestObjExtentDelPunishReplace_mergesOnDuplicateNewKey(t *testing.T) {
	ot := newObjExtentDelTree()
	a := createTestObjExtentKey(0, 100, 1)
	b := createTestObjExtentKey(200, 50, 2)
	ot.EnqueueFromApply(7, 100, 3, []proto.ObjExtentKey{a})

	const newTs = int64(5000)
	itemA := &objExtentDelItem{TsMs: 100, Inode: 7, RaftIdx: 3, Oeks: []proto.ObjExtentKey{a}}
	itemB := &objExtentDelItem{TsMs: 200, Inode: 7, RaftIdx: 4, Oeks: []proto.ObjExtentKey{b}}
	ot.mu.Lock()
	ot.objExtentDelPunishReplace(11, itemA, newTs)
	ot.objExtentDelPunishReplace(11, itemB, newTs)
	ot.mu.Unlock()

	out := ot.PeekFirstN(1)
	require.Len(t, out.Items, 1)
	require.Len(t, out.Items[0].Oeks, 2)
}

func TestUnmarshalDequeue_readErrors(t *testing.T) {
	var batch batchObjExtentDelItems
	// version + cnt=1, no item body
	require.Error(t, batch.UnmarshalDequeue([]byte{0, 0, 0, 1, 0, 0, 0, 1}))

	var buf bytes.Buffer
	writeObjExtentDelV1DequeueHeader(&buf, 1)
	require.NoError(t, binary.Write(&buf, binary.BigEndian, int64(1)))
	require.Error(t, batch.UnmarshalDequeue(buf.Bytes()))

	var buf2 bytes.Buffer
	writeObjExtentDelV1DequeueHeader(&buf2, 1)
	require.NoError(t, binary.Write(&buf2, binary.BigEndian, int64(1)))
	require.NoError(t, binary.Write(&buf2, binary.BigEndian, uint64(2)))
	require.Error(t, batch.UnmarshalDequeue(buf2.Bytes()))
}

func TestObjExtentDelPunishReplace_noOeksAborts(t *testing.T) {
	ot := newObjExtentDelTree()
	item := &objExtentDelItem{TsMs: 100, Inode: 7, RaftIdx: 3, Oeks: nil}
	ot.mu.Lock()
	ot.objExtentDelPunishReplace(11, item, 5000)
	ot.mu.Unlock()
	require.Equal(t, 0, ot.Len())
}

func TestObjExtentDelTreeEncodeDecodeAndErrors(t *testing.T) {
	oek := createTestObjExtentKey(0, 64, 3)
	it := &objExtentDelItem{
		TsMs:    10,
		Inode:   11,
		RaftIdx: 12,
		Oeks:    []proto.ObjExtentKey{oek},
	}
	cp := it.Copy().(*objExtentDelItem)
	require.Len(t, cp.Oeks, 1)
	require.True(t, cp.Oeks[0].IsEquals(&oek))
	if len(cp.Oeks[0].Blobs) > 0 {
		cp.Oeks[0].Blobs[0].MinBid = 999
		require.NotEqual(t, cp.Oeks[0].Blobs[0].MinBid, it.Oeks[0].Blobs[0].MinBid)
	}

	var batch batchObjExtentDelItems
	batch.Items = []*objExtentDelItem{it}
	val, err := encodeBatchDequeue(batch)
	require.NoError(t, err)
	var decoded batchObjExtentDelItems
	require.NoError(t, decoded.UnmarshalDequeue(val))
	require.Len(t, decoded.Items, 1)
	require.Equal(t, it.TsMs, decoded.Items[0].TsMs)
	require.Equal(t, it.Inode, decoded.Items[0].Inode)
	require.Equal(t, it.RaftIdx, decoded.Items[0].RaftIdx)

	err = decoded.UnmarshalDequeue([]byte{1, 2, 3})
	require.Error(t, err)
	require.Contains(t, err.Error(), "too short")
}

func TestObjExtentDelTreeApplyPunishPayloadErrors(t *testing.T) {
	ot := newObjExtentDelTree()

	require.Error(t, ot.ApplyPunishPayload([]byte{1, 2, 3}, 1))

	var truncated bytes.Buffer
	writeObjExtentDelV1PunishHeader(&truncated, 1, 99)
	require.NoError(t, binary.Write(&truncated, binary.BigEndian, int64(1)))
	require.Error(t, ot.ApplyPunishPayload(truncated.Bytes(), 1))
}

func TestObjExtentDelTreeEnqueueMultipleOeksOneItem(t *testing.T) {
	ot := newObjExtentDelTree()
	a := createTestObjExtentKey(0, 100, 1)
	b := createTestObjExtentKey(100, 200, 2)
	ot.EnqueueFromApply(1, 0, 5, []proto.ObjExtentKey{a, b})
	require.Equal(t, 1, ot.Len())
	items := ot.PeekFirstN(1)
	require.Len(t, items.Items, 1)
	require.Len(t, items.Items[0].Oeks, 2)
	require.True(t, items.Items[0].Oeks[0].IsEquals(&a))
	require.True(t, items.Items[0].Oeks[1].IsEquals(&b))
}

func TestObjExtentDelTreeEnqueueReplaceSameBtreeKey(t *testing.T) {
	ot := newObjExtentDelTree()
	a := createTestObjExtentKey(0, 100, 1)
	b := createTestObjExtentKey(200, 50, 2)
	ot.EnqueueFromApply(7, 0, 3, []proto.ObjExtentKey{a})
	ot.EnqueueFromApply(7, 0, 3, []proto.ObjExtentKey{b})
	require.Equal(t, 1, ot.Len())
	items := ot.PeekFirstN(1)
	require.Len(t, items.Items[0].Oeks, 1)
	require.True(t, items.Items[0].Oeks[0].IsEquals(&b))
}

func TestObjExtentDelTreeApplyPunishPayloadMultiOek(t *testing.T) {
	ot := newObjExtentDelTree()
	a := createTestObjExtentKey(0, 100, 1)
	b := createTestObjExtentKey(100, 200, 2)
	ot.EnqueueFromApply(42, 1700000000, 9, []proto.ObjExtentKey{a, b})
	oldItems := ot.PeekFirstN(1)
	require.Len(t, oldItems.Items, 1)
	newTs := int64(1700000005000)
	payload, err := encodeBatchPunish(oldItems, newTs)
	require.NoError(t, err)
	require.NoError(t, ot.ApplyPunishPayload(payload, 21))
	items := ot.PeekFirstN(1)
	require.Len(t, items.Items, 1)
	require.Equal(t, newTs, items.Items[0].TsMs)
	require.Equal(t, uint64(21), items.Items[0].RaftIdx)
	require.Len(t, items.Items[0].Oeks, 2)
	require.True(t, items.Items[0].Oeks[0].IsEquals(&a))
	require.True(t, items.Items[0].Oeks[1].IsEquals(&b))
}

func TestApplyPunishPayload_truncatedWhenCountLarge(t *testing.T) {
	ot := newObjExtentDelTree()
	var buf bytes.Buffer
	writeObjExtentDelV1PunishHeader(&buf, 32, 0)
	require.Error(t, ot.ApplyPunishPayload(buf.Bytes(), 1))
}

func TestApplyPunishPayload_invalidOekCount(t *testing.T) {
	ot := newObjExtentDelTree()
	var buf bytes.Buffer
	writeObjExtentDelV1PunishHeader(&buf, 1, 4)
	require.NoError(t, binary.Write(&buf, binary.BigEndian, int64(1)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint64(2)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint64(3)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(0)))
	require.ErrorIs(t, ot.ApplyPunishPayload(buf.Bytes(), 1), ErrDelTreeUnsupported)
}

func TestApplyPunishPayload_truncatedOekBody(t *testing.T) {
	ot := newObjExtentDelTree()
	oek := createTestObjExtentKey(0, 64, 1)
	ob, err := oek.MarshalBinary()
	require.NoError(t, err)

	var buf bytes.Buffer
	writeObjExtentDelV1PunishHeader(&buf, 1, 13)
	require.NoError(t, binary.Write(&buf, binary.BigEndian, int64(10)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint64(11)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint64(12)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(1)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(len(ob)+1)))
	require.Error(t, ot.ApplyPunishPayload(buf.Bytes(), 2))
}

func TestApplyPunishPayload_singleItem_roundTrip(t *testing.T) {
	ot := newObjExtentDelTree()
	oek := createTestObjExtentKey(0, 50, 1)
	ot.EnqueueFromApply(3, 1000, 7, []proto.ObjExtentKey{oek})
	items := ot.PeekFirstN(1)
	payload, err := encodeBatchPunish(items, 2000)
	require.NoError(t, err)
	require.NoError(t, ot.ApplyPunishPayload(payload, 8))
	out := ot.PeekFirstN(1)
	require.Len(t, out.Items, 1)
	require.Equal(t, int64(2000), out.Items[0].TsMs)
}

func TestEncodeObjExtentGcPunish_multiOek(t *testing.T) {
	a := createTestObjExtentKey(0, 10, 1)
	b := createTestObjExtentKey(10, 20, 2)
	it := &objExtentDelItem{TsMs: 1, Inode: 2, RaftIdx: 3, Oeks: []proto.ObjExtentKey{a, b}}
	var batch batchObjExtentDelItems
	batch.Items = []*objExtentDelItem{it}
	payload, err := encodeBatchPunish(batch, 99)
	require.NoError(t, err)
	require.NotEmpty(t, payload)
	ot := newObjExtentDelTree()
	require.NoError(t, ot.ApplyPunishPayload(payload, 5))
}

func TestApplyPunishPayload_sameInodeMergedRaftIdx(t *testing.T) {
	ot := newObjExtentDelTree()
	a := createTestObjExtentKey(0, 100, 1)
	b := createTestObjExtentKey(200, 50, 2)
	ot.EnqueueFromApply(7, 100, 3, []proto.ObjExtentKey{a})
	ot.EnqueueFromApply(7, 200, 4, []proto.ObjExtentKey{b})
	require.Equal(t, 2, ot.Len())

	newTs := int64(5000)
	batch := ot.PeekFirstN(2)
	payload, err := encodeBatchPunish(batch, newTs)
	require.NoError(t, err)
	require.NoError(t, ot.ApplyPunishPayload(payload, 11))
	items := ot.PeekFirstN(10)
	require.Len(t, items.Items, 1)
	require.Equal(t, newTs, items.Items[0].TsMs)
	require.Equal(t, uint64(7), items.Items[0].Inode)
	require.Equal(t, uint64(11), items.Items[0].RaftIdx)
	require.Len(t, items.Items[0].Oeks, 2)
}

func TestBatchObjExtentDelItems_MarshalPunishRoundTrip(t *testing.T) {
	a := createTestObjExtentKey(0, 100, 1)
	b := createTestObjExtentKey(100, 200, 2)
	src := batchObjExtentDelItems{Items: []*objExtentDelItem{
		{TsMs: 1700000000, Inode: 42, RaftIdx: 9, Oeks: []proto.ObjExtentKey{a, b}},
	}}
	newTs := int64(1700000005000)
	raw, err := encodeBatchPunish(src, newTs)
	require.NoError(t, err)

	var dst batchObjExtentDelItems
	require.NoError(t, dst.UnmarshalPunish(raw))
	require.Len(t, dst.Items, 1)
	require.Equal(t, int64(1700000000), dst.Items[0].TsMs)
	require.Equal(t, uint64(42), dst.Items[0].Inode)
	require.Equal(t, uint64(9), dst.Items[0].RaftIdx)
	require.Equal(t, newTs, dst.NewTime)
	require.Len(t, dst.Items[0].Oeks, 2)
	require.True(t, dst.Items[0].Oeks[0].IsEquals(&a))
	require.True(t, dst.Items[0].Oeks[1].IsEquals(&b))
}

func TestObjExtentDelPunishReplace_usesPayloadOeks(t *testing.T) {
	ot := newObjExtentDelTree()
	treeOek := createTestObjExtentKey(0, 100, 1)
	payloadOek := createTestObjExtentKey(999, 1, 1)
	ot.EnqueueFromApply(7, 100, 3, []proto.ObjExtentKey{treeOek})

	item := &objExtentDelItem{
		TsMs:    100 * 1000,
		Inode:   7,
		RaftIdx: 3,
		Oeks:    []proto.ObjExtentKey{payloadOek},
	}
	ot.mu.Lock()
	ot.objExtentDelPunishReplace(11, item, 5000)
	ot.mu.Unlock()

	out := ot.PeekFirstN(1)
	require.Len(t, out.Items, 1)
	require.True(t, out.Items[0].Oeks[0].IsEquals(&payloadOek))
}

func TestObjExtentDelPunishReplace_requeues(t *testing.T) {
	ot := newObjExtentDelTree()
	oek := createTestObjExtentKey(0, 32, 1)
	ot.EnqueueFromApply(7, 100, 1, []proto.ObjExtentKey{oek})
	peek := ot.PeekFirstN(1)
	require.Len(t, peek.Items, 1)
	item := peek.Items[0]
	ot.mu.Lock()
	ot.objExtentDelPunishReplace(99, item, 200)
	ot.mu.Unlock()
	items := ot.PeekFirstN(1)
	require.Len(t, items.Items, 1)
	require.Equal(t, int64(200), items.Items[0].TsMs)
	require.Equal(t, uint64(99), items.Items[0].RaftIdx)
}

func TestPeekFirstN_returnsSnapshotNotTreePointer(t *testing.T) {
	ot := newObjExtentDelTree()
	oek := createTestObjExtentKey(0, 1, 1)
	ot.EnqueueFromApply(1, 0, 2, []proto.ObjExtentKey{oek})
	peek := ot.PeekFirstN(1)
	peek.Items[0].TsMs = 999
	again := ot.PeekFirstN(1)
	require.Equal(t, int64(2), again.Items[0].TsMs)
}

func TestObjExtentDelTree_FullFlowDequeue(t *testing.T) {
	ot := newObjExtentDelTree()
	a := createTestObjExtentKey(0, 100, 1)
	b := createTestObjExtentKey(100, 50, 2)
	ot.EnqueueFromApply(42, 1700000000, 100, []proto.ObjExtentKey{a})
	ot.EnqueueFromApply(99, 0, 101, []proto.ObjExtentKey{b})

	require.Equal(t, 2, ot.Len())
	batch := ot.PeekFirstN(32)
	require.Len(t, batch.Items, 2)
	require.Equal(t, int64(101), batch.Items[0].TsMs, "time-first: smaller TsMs first")
	require.Equal(t, uint64(99), batch.Items[0].Inode)

	payload, err := encodeBatchDequeue(batch)
	require.NoError(t, err)
	require.NoError(t, ot.ApplyDequeuePayload(payload))
	require.Equal(t, 0, ot.Len())
}

func TestObjExtentDelTree_FullFlowPunish(t *testing.T) {
	ot := newObjExtentDelTree()
	oek := createTestObjExtentKey(0, 1024, 1)
	ot.EnqueueFromApply(42, 1700000000, 50, []proto.ObjExtentKey{oek})

	batch := ot.PeekFirstN(1)
	penaltyTs := int64(1700000000000)
	payload, err := encodeBatchPunish(batch, penaltyTs)
	require.NoError(t, err)

	require.NoError(t, ot.ApplyPunishPayload(payload, 200))
	require.Equal(t, 1, ot.Len())

	after := ot.PeekFirstN(1)
	require.Equal(t, penaltyTs, after.Items[0].TsMs)
	require.Equal(t, uint64(200), after.Items[0].RaftIdx)
	require.Equal(t, uint64(42), after.Items[0].Inode)
}

func TestObjExtentDelItem_Less_ordering(t *testing.T) {
	a := &objExtentDelItem{TsMs: 1, Inode: 2, RaftIdx: 3}
	b := &objExtentDelItem{TsMs: 2, Inode: 2, RaftIdx: 3}
	c := &objExtentDelItem{TsMs: 1, Inode: 3, RaftIdx: 3}
	d := &objExtentDelItem{TsMs: 1, Inode: 2, RaftIdx: 4}
	require.True(t, a.Less(b))
	require.True(t, a.Less(c))
	require.True(t, a.Less(d))
}

func TestUnmarshalPunish_shortNewTime(t *testing.T) {
	var buf bytes.Buffer
	writeObjExtentDelV1DequeueHeader(&buf, 0)
	// missing newTsMs after header

	var batch batchObjExtentDelItems
	err := batch.UnmarshalPunish(buf.Bytes())
	require.Error(t, err)
}

func TestUnmarshalPunish_invalidOekBody(t *testing.T) {
	oek := createTestObjExtentKey(0, 64, 1)
	ob, err := oek.MarshalBinary()
	require.NoError(t, err)

	var buf bytes.Buffer
	writeObjExtentDelV1PunishHeader(&buf, 1, 1)
	require.NoError(t, binary.Write(&buf, binary.BigEndian, int64(10)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint64(11)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint64(12)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(1)))
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(len(ob)+1)))

	var batch batchObjExtentDelItems
	err = batch.UnmarshalPunish(buf.Bytes())
	require.Error(t, err)
}

func TestObjExtentDelItem_LessAndKeyItem(t *testing.T) {
	a := &objExtentDelItem{TsMs: 1, Inode: 2, RaftIdx: 3, Oeks: []proto.ObjExtentKey{{}}}
	b := &objExtentDelItem{TsMs: 1, Inode: 2, RaftIdx: 3}
	require.False(t, a.Less(b))
	require.False(t, b.Less(a))
	key := a.keyItem()
	require.Nil(t, key.Oeks)
	require.Equal(t, a.TsMs, key.TsMs)

	ot := newObjExtentDelTree()
	ot.EnqueueFromApply(2, 0, 3, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)})
	peek := ot.PeekFirstN(1)
	require.Equal(t, int64(3), peek.Items[0].TsMs)
}

func TestApplyDequeuePayload_unmarshalError(t *testing.T) {
	ot := newObjExtentDelTree()
	ot.EnqueueFromApply(1, 0, 1, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)})
	require.Equal(t, 1, ot.Len())

	err := ot.ApplyDequeuePayload([]byte{0, 1, 2})
	require.Error(t, err)
	require.Equal(t, 1, ot.Len())
}
