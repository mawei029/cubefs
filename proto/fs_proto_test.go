package proto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppendObjExtentKeysRequestEkString(t *testing.T) {
	var nilReq *AppendObjExtentKeysRequest
	require.Equal(t, "", nilReq.EkString())

	req := &AppendObjExtentKeysRequest{
		VolName:       "vol",
		PartitionID:   1,
		Inode:         2,
		IsOverwrite:   true,
		Extents:       []ObjExtentKey{{FileOffset: 0, Size: 10}},
		DiscardExtent: ObjExtentKey{FileOffset: 0, Size: 8},
	}
	s := req.EkString()
	require.Contains(t, s, "vol:vol")
	require.Contains(t, s, "pid:1")
	require.Contains(t, s, "ino:2")
	require.Contains(t, s, "isOverwrite:true")
	require.Contains(t, s, "ek:")
	require.Contains(t, s, "dek:")
}

func TestTruncateRequest_JSONRoundTrip(t *testing.T) {
	req := &TruncateRequest{
		VolName:      "vol1",
		PartitionID:  99,
		Inode:        1001,
		Size:         150,
		Timestamp:    1700000000,
		TruncateV2:   true,
		NewObjExtent: ObjExtentKey{FileOffset: 100, Size: 50},
		ToDelete:     ObjExtentKey{FileOffset: 200, Size: 30},
	}
	req.FullPaths = []string{"/a/b"}

	data, err := json.Marshal(req)
	require.NoError(t, err)
	require.Contains(t, string(data), `"newObjExtent"`)
	require.Contains(t, string(data), `"toDelete"`)

	var decoded TruncateRequest
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.True(t, decoded.TruncateV2)
	require.Equal(t, uint64(150), decoded.Size)
	require.Equal(t, uint64(100), decoded.NewObjExtent.FileOffset)
	require.Equal(t, uint64(50), decoded.NewObjExtent.Size)
	require.Equal(t, uint64(200), decoded.ToDelete.FileOffset)
	require.Equal(t, []string{"/a/b"}, decoded.FullPaths)
}

func TestTruncateRequest_EmptyDeltasGrowPath(t *testing.T) {
	req := &TruncateRequest{VolName: "v", Inode: 1, Size: 4096, TruncateV2: true}
	data, err := json.Marshal(req)
	require.NoError(t, err)
	var decoded TruncateRequest
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.True(t, decoded.NewObjExtent.IsEmpty())
	require.True(t, decoded.ToDelete.IsEmpty())
}
