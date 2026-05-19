package metanode

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/require"
)

func TestObjExtentDelTreeEnqueueAndDequeue(t *testing.T) {
	ot := newObjExtentDelTree()
	oek := createTestObjExtentKey(0, 1024, 1)

	ot.EnqueueFromApply(42, 0, 7, []proto.ObjExtentKey{oek})
	require.Equal(t, 1, ot.Len())

	items := ot.PeekFirstN(1)
	require.Len(t, items, 1)
	require.Equal(t, int64(7), items[0].TsMs)
	require.Equal(t, uint64(42), items[0].Inode)
	require.Equal(t, uint64(7<<20), items[0].Uniq)
	require.Len(t, items[0].Oeks, 1)
	require.True(t, items[0].Oeks[0].IsEquals(&oek))

	val, err := encodeObjExtentGcDequeueKeys(items)
	require.NoError(t, err)
	require.NoError(t, ot.ApplyDequeuePayload(val))
	require.Equal(t, 0, ot.Len())
}

func TestObjExtentDelTreeApplyPunishPayload(t *testing.T) {
	ot := newObjExtentDelTree()
	oek := createTestObjExtentKey(128, 2048, 2)
	ot.EnqueueFromApply(99, 1700000000, 3, []proto.ObjExtentKey{oek})

	oldItems := ot.PeekFirstN(1)
	require.Len(t, oldItems, 1)

	newTs := int64(1700000005000)
	payload, err := encodeObjExtentGcPunish(oldItems, newTs)
	require.NoError(t, err)
	require.NoError(t, ot.ApplyPunishPayload(payload, 11))

	items := ot.PeekFirstN(1)
	require.Len(t, items, 1)
	require.Equal(t, newTs, items[0].TsMs)
	require.Equal(t, uint64(11<<20), items[0].Uniq)
	require.Equal(t, oldItems[0].Inode, items[0].Inode)
	require.Len(t, items[0].Oeks, 1)
	require.True(t, items[0].Oeks[0].IsEquals(&oek))
}

func TestNormalizeObjExtentDelTsMs(t *testing.T) {
	require.Equal(t, int64(9), normalizeObjExtentDelTsMs(0, 9))
	require.Equal(t, int64(1700000000*1000), normalizeObjExtentDelTsMs(1700000000, 1))
	require.Equal(t, int64(1700000000000), normalizeObjExtentDelTsMs(1700000000000, 1))
}

func TestObjExtentDelTreeEdgeBranches(t *testing.T) {
	var nilTree *objExtentDelTree
	require.Nil(t, nilTree.PeekFirstN(1))
	nilTree.EnqueueFromApply(1, 0, 1, nil)
	require.NoError(t, nilTree.ApplyDequeuePayload(nil))
	require.NoError(t, nilTree.ApplyPunishPayload(nil, 1))

	ot := newObjExtentDelTree()
	ot.EnqueueFromApply(1, 0, 2, []proto.ObjExtentKey{{}})
	require.Equal(t, 0, ot.Len())
	require.Nil(t, ot.PeekFirstN(0))
}

func TestObjExtentDelItemCopy_emptyOeks(t *testing.T) {
	it := &objExtentDelItem{TsMs: 1, Inode: 2, Uniq: 3, Oeks: nil}
	cp := it.Copy().(*objExtentDelItem)
	require.Nil(t, cp.Oeks)
	require.Equal(t, it.TsMs, cp.TsMs)
}

func TestObjExtentDelTreeEncodeDecodeAndErrors(t *testing.T) {
	oek := createTestObjExtentKey(0, 64, 3)
	it := &objExtentDelItem{
		TsMs:  10,
		Inode: 11,
		Uniq:  12,
		Oeks:  []proto.ObjExtentKey{oek},
	}
	// copy should deep-copy blobs
	cp := it.Copy().(*objExtentDelItem)
	require.Len(t, cp.Oeks, 1)
	require.True(t, cp.Oeks[0].IsEquals(&oek))
	if len(cp.Oeks[0].Blobs) > 0 {
		cp.Oeks[0].Blobs[0].MinBid = 999
		require.NotEqual(t, cp.Oeks[0].Blobs[0].MinBid, it.Oeks[0].Blobs[0].MinBid)
	}

	val, err := encodeObjExtentGcDequeueKeys([]*objExtentDelItem{it})
	require.NoError(t, err)
	keys, err := decodeObjExtentGcDequeueKeys(val)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, it.TsMs, keys[0].TsMs)
	require.Equal(t, it.Inode, keys[0].Inode)
	require.Equal(t, it.Uniq, keys[0].Uniq)

	_, err = decodeObjExtentGcDequeueKeys([]byte{1, 2, 3})
	require.Error(t, err)

	var tooManyBuf bytes.Buffer
	require.NoError(t, binary.Write(&tooManyBuf, binary.BigEndian, uint32(maxObjExtentDelBatch+1)))
	_, err = decodeObjExtentGcDequeueKeys(tooManyBuf.Bytes())
	require.Error(t, err)
}

func TestObjExtentDelTreeApplyPunishPayloadErrors(t *testing.T) {
	ot := newObjExtentDelTree()

	// short payload: missing count
	require.Error(t, ot.ApplyPunishPayload([]byte{1}, 1))

	var tooManyBuf bytes.Buffer
	require.NoError(t, binary.Write(&tooManyBuf, binary.BigEndian, uint32(maxObjExtentDelBatch+1)))
	require.Error(t, ot.ApplyPunishPayload(tooManyBuf.Bytes(), 1))

	// count=1 but payload truncated
	var truncated bytes.Buffer
	require.NoError(t, binary.Write(&truncated, binary.BigEndian, uint32(1)))
	require.NoError(t, binary.Write(&truncated, binary.BigEndian, int64(1))) // oldTs only
	require.Error(t, ot.ApplyPunishPayload(truncated.Bytes(), 1))
}

func TestObjExtentDelTreeEnqueueMultipleOeksOneItem(t *testing.T) {
	ot := newObjExtentDelTree()
	a := createTestObjExtentKey(0, 100, 1)
	b := createTestObjExtentKey(100, 200, 2)
	ot.EnqueueFromApply(1, 0, 5, []proto.ObjExtentKey{a, b})
	require.Equal(t, 1, ot.Len())
	items := ot.PeekFirstN(1)
	require.Len(t, items, 1)
	require.Len(t, items[0].Oeks, 2)
	require.True(t, items[0].Oeks[0].IsEquals(&a))
	require.True(t, items[0].Oeks[1].IsEquals(&b))
}

func TestObjExtentDelTreeEnqueueMergeSameBtreeKey(t *testing.T) {
	ot := newObjExtentDelTree()
	a := createTestObjExtentKey(0, 100, 1)
	b := createTestObjExtentKey(200, 50, 2)
	ot.EnqueueFromApply(7, 0, 3, []proto.ObjExtentKey{a})
	ot.EnqueueFromApply(7, 0, 3, []proto.ObjExtentKey{b})
	require.Equal(t, 1, ot.Len())
	items := ot.PeekFirstN(1)
	require.Len(t, items[0].Oeks, 2)
	require.True(t, items[0].Oeks[0].IsEquals(&a))
	require.True(t, items[0].Oeks[1].IsEquals(&b))
}

func TestObjExtentDelTreeApplyPunishPayloadV2MultiOek(t *testing.T) {
	ot := newObjExtentDelTree()
	a := createTestObjExtentKey(0, 100, 1)
	b := createTestObjExtentKey(100, 200, 2)
	ot.EnqueueFromApply(42, 1700000000, 9, []proto.ObjExtentKey{a, b})
	oldItems := ot.PeekFirstN(1)
	require.Len(t, oldItems, 1)
	newTs := int64(1700000005000)
	payload, err := encodeObjExtentGcPunish(oldItems, newTs)
	require.NoError(t, err)
	require.NoError(t, ot.ApplyPunishPayload(payload, 21))
	items := ot.PeekFirstN(1)
	require.Len(t, items, 1)
	require.Equal(t, newTs, items[0].TsMs)
	require.Equal(t, uint64(21<<20), items[0].Uniq)
	require.Len(t, items[0].Oeks, 2)
	require.True(t, items[0].Oeks[0].IsEquals(&a))
	require.True(t, items[0].Oeks[1].IsEquals(&b))
}
