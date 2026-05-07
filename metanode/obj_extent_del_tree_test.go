package metanode

import (
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
	require.True(t, items[0].Oek.IsEquals(&oek))

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
	require.True(t, items[0].Oek.IsEquals(&oek))
}

func TestNormalizeObjExtentDelTsMs(t *testing.T) {
	require.Equal(t, int64(9), normalizeObjExtentDelTsMs(0, 9))
	require.Equal(t, int64(1700000000*1000), normalizeObjExtentDelTsMs(1700000000, 1))
	require.Equal(t, int64(1700000000000), normalizeObjExtentDelTsMs(1700000000000, 1))
}
