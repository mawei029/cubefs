package proto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppendObjExtentKeysRequestEkString(t *testing.T) {
	var nilReq *AppendObjExtentKeysRequest
	require.Equal(t, "", nilReq.EkString())

	req := &AppendObjExtentKeysRequest{
		VolName:        "vol",
		PartitionID:    1,
		Inode:          2,
		IsOverwrite:    true,
		Extents:        []ObjExtentKey{{FileOffset: 0, Size: 10}},
		DiscardExtents: []ObjExtentKey{{FileOffset: 0, Size: 8}, {}},
	}
	s := req.EkString()
	require.Contains(t, s, "vol:vol")
	require.Contains(t, s, "pid:1")
	require.Contains(t, s, "ino:2")
	require.Contains(t, s, "isOverwrite:true")
	require.Contains(t, s, "ek:")
	require.Contains(t, s, "dek:")
}
