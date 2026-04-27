// Copyright 2024 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package metanode

import (
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	raftstoremock "github.com/cubefs/cubefs/metanode/mocktest/raftstore"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/raftstore"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
)

// newTestMetaPartition creates a metaPartition for testing
func newTestMetaPartition(rootDir string, ctrl *gomock.Controller) *metaPartition {
	config := &MetaPartitionConfig{
		PartitionId:   10001,
		VolName:       "test_vol",
		Start:         0,
		End:           100,
		PartitionType: proto.VolumeTypeHot,
		RootDir:       rootDir,
		StoreMode:     proto.StoreModeMem,
		NodeId:        1,
		Peers:         []proto.Peer{{ID: 1, Addr: "127.0.0.1"}},
	}
	mp := newPartition(config, newManager())
	mp.stopC = make(chan bool)
	mp.objExtDelCh = make(chan []proto.ObjExtentKey, 100)

	// Mock raft: leader + Submit applies through FSM (obj extent GC dequeue/punish).
	if ctrl != nil {
		var applyIdx uint64 = 100
		raft := raftstoremock.NewMockPartition(ctrl)
		raft.EXPECT().LeaderTerm().Return(uint64(1), uint64(1)).AnyTimes()
		raft.EXPECT().Status().Return(&raftstore.PartitionStatus{RestoringSnapshot: false}).AnyTimes()
		raft.EXPECT().Submit(gomock.Any()).DoAndReturn(func(cmd []byte) (interface{}, error) {
			idx := atomic.AddUint64(&applyIdx, 1)
			_, err := mp.Apply(cmd, idx)
			return nil, err
		}).AnyTimes()
		mp.raftPartition = raft
	}

	return mp
}

// createTestObjExtentKey creates a test ObjExtentKey
func createTestObjExtentKey(fileOffset, size, bid uint64) proto.ObjExtentKey {
	return proto.ObjExtentKey{
		FileOffset: fileOffset,
		Size:       size,
		Cid:        1,
		CodeMode:   1,
		BlobSize:   1024,
		BlobsLen:   1,
		Blobs: []proto.Blob{
			{MinBid: bid, Count: 1, Vid: 1},
		},
		Crc: 12345,
	}
}

// TestRunObjExtentDelTreeGCOnce_Dequeue enqueues pending keys, runs one GC tick, and Raft-dequeues after EBS delete succeeds.
func TestRunObjExtentDelTreeGCOnce_Dequeue(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_gc")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mp := newTestMetaPartition(rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}

	oek := createTestObjExtentKey(0, 1024, 1)
	mp.objExtentDelTree.EnqueueFromApply(42, 1700000000, 7, []proto.ObjExtentKey{oek})
	require.Equal(t, 1, mp.objExtentDelTree.Len())

	patches := gomonkey.NewPatches()
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, oeks []proto.ObjExtentKey) error {
			require.Len(t, oeks, 1)
			return nil
		})
	defer patches.Reset()

	mp.runObjExtentDelTreeGCOnce()
	require.Equal(t, 0, mp.objExtentDelTree.Len(), "dequeue should remove item after successful delete")
}

// TestRunObjExtentDelTreeGCOnce_PunishRequeue on EBS failure submits punish op; FSM re-inserts with later TsMs.
func TestRunObjExtentDelTreeGCOnce_PunishRequeue(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_punish")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mp := newTestMetaPartition(rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}

	oek := createTestObjExtentKey(0, 1024, 1)
	mp.objExtentDelTree.EnqueueFromApply(99, 1700000001, 3, []proto.ObjExtentKey{oek})
	require.Equal(t, 1, mp.objExtentDelTree.Len())

	patches := gomonkey.NewPatches()
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error {
			return os.ErrInvalid
		})
	patches.ApplyFunc(time.Now, func() time.Time {
		return time.UnixMilli(1_700_000_000_000)
	})
	defer patches.Reset()

	mp.runObjExtentDelTreeGCOnce()
	require.Equal(t, 1, mp.objExtentDelTree.Len(), "punish should keep one pending entry")

	peek := mp.objExtentDelTree.PeekFirstN(1)
	require.Len(t, peek, 1)
	require.GreaterOrEqual(t, peek[0].TsMs, int64(1_700_000_000_000)+objExtentDelGcPenaltyMs-1)
}
