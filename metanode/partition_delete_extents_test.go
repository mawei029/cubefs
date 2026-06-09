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
	"bytes"
	"errors"
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
	"github.com/cubefs/cubefs/util/auditlog"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
)

type testMetaPartitionRaftSetup func(ctrl *gomock.Controller, mp *metaPartition)

func defaultTestMetaPartitionRaft(ctrl *gomock.Controller, mp *metaPartition) {
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

func stopTestMetaPartitionWorkers(mp *metaPartition) {
	if mp == nil {
		return
	}
	select {
	case <-mp.stopC:
	default:
		close(mp.stopC)
	}
	time.Sleep(20 * time.Millisecond)
}

// newTestMetaPartition creates a metaPartition for testing.
// Raft mock must be attached before initObjects: NewTransactionProcessor starts a
// background goroutine that reads mp.raftPartition via IsLeader.
func newTestMetaPartition(t *testing.T, rootDir string, ctrl *gomock.Controller, raftSetup ...testMetaPartitionRaftSetup) *metaPartition {
	t.Helper()
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
	manager := newManager()
	mp := &metaPartition{
		config:           config,
		stopC:            make(chan bool),
		storeChan:        make(chan *storeMsg, 100),
		freeList:         newFreeList(),
		freeHybridList:   newFreeList(),
		extDelCh:         make(chan []proto.ExtentKey, defaultDelExtentsCnt),
		objExtentDelTree: newObjExtentDelTree(),
		extReset:         make(chan struct{}),
		vol:              NewVol(),
		manager:          manager,
		verSeq:           config.VerSeq,
		rocksdbManager:   manager.rocksdbManager,
	}
	if ctrl != nil {
		if len(raftSetup) > 0 && raftSetup[0] != nil {
			raftSetup[0](ctrl, mp)
		} else {
			defaultTestMetaPartitionRaft(ctrl, mp)
		}
	}
	mp.config.Cursor = 0
	mp.config.End = 100000
	mp.uidManager = NewUidMgr(config.VolName, mp.config.PartitionId)
	mp.mqMgr = NewQuotaManager(config.VolName, mp.config.PartitionId)
	if err := mp.initObjects(true); err != nil {
		panic(err)
	}
	mp.vol.info = &proto.SimpleVolView{
		VolStorageClass: proto.StorageClass_Replica_SSD,
		Pools: map[uint8]*proto.StoragePoolInfo{
			proto.DefaultHDDPoolId: {
				Id:           proto.DefaultHDDPoolId,
				Name:         "default",
				StorageClass: uint8(proto.StorageClass_Replica_HDD),
			},
			proto.DefaultSSDPoolId: {
				Id:           proto.DefaultSSDPoolId,
				Name:         "default",
				StorageClass: uint8(proto.StorageClass_Replica_SSD),
			},
			proto.DefaultECPoolId: {
				Id:           proto.DefaultECPoolId,
				Name:         "default",
				StorageClass: uint8(proto.StorageClass_BlobStore),
			},
		},
		DefaultPoolId: proto.DefaultHDDPoolId,
	}
	t.Cleanup(func() { stopTestMetaPartitionWorkers(mp) })
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

// isObjExtentDelTreeGCBackoffSleep matches runObjExtentDelTreeGCWorker idle/error backoff.
func isObjExtentDelTreeGCBackoffSleep(d time.Duration) bool {
	return d == AsyncDeleteInterval
}

// TestRunObjExtentDelTreeGCOnce_Dequeue enqueues pending keys, runs one GC tick, and Raft-dequeues after EBS delete succeeds.
func TestRunObjExtentDelTreeGCOnce_Dequeue(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_gc")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mp := newTestMetaPartition(t, rootDir, ctrl)
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

	require.NoError(t, mp.runObjExtentDelTreeGCOnce())
	require.Equal(t, 0, mp.objExtentDelTree.Len(), "dequeue should remove item after successful delete")
}

// TestRunObjExtentDelTreeGCOnce_DequeueMultiOek verifies one btree item carrying multiple ObjExtentKeys is flattened for EBS delete.
func TestRunObjExtentDelTreeGCOnce_DequeueMultiOek(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_gc_multi")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mp := newTestMetaPartition(t, rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}

	oek1 := createTestObjExtentKey(0, 1024, 1)
	oek2 := createTestObjExtentKey(1024, 512, 2)
	mp.objExtentDelTree.EnqueueFromApply(42, 1700000000, 7, []proto.ObjExtentKey{oek1, oek2})
	require.Equal(t, 1, mp.objExtentDelTree.Len())

	patches := gomonkey.NewPatches()
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, oeks []proto.ObjExtentKey) error {
			require.Len(t, oeks, 2)
			return nil
		})
	defer patches.Reset()

	require.NoError(t, mp.runObjExtentDelTreeGCOnce())
	require.Equal(t, 0, mp.objExtentDelTree.Len(), "dequeue should remove item after successful delete")
}

// TestRunObjExtentDelTreeGCOnce_PunishRequeue on EBS failure submits punish op; FSM re-inserts with later TsMs.
func TestRunObjExtentDelTreeGCOnce_PunishRequeue(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_punish")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mp := newTestMetaPartition(t, rootDir, ctrl)
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

	require.NoError(t, mp.runObjExtentDelTreeGCOnce())
	require.Equal(t, 1, mp.objExtentDelTree.Len(), "punish should keep one pending entry")

	peek := mp.objExtentDelTree.PeekFirstN(1)
	require.Len(t, peek.Items, 1)
	require.GreaterOrEqual(t, peek.Items[0].TsMs, int64(1_700_000_000_000)+objExtentDelGcPenaltyMs-1)
}

func TestStartObjExtentDelTreeGC_StopImmediately(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_start")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	mp := newTestMetaPartition(t, rootDir, nil)
	mp.startObjExtentDelTreeGC()
	close(mp.stopC)
	time.Sleep(20 * time.Millisecond)
}

func TestRunObjExtentDelTreeGCOnce_EarlyReturnBranches(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_early")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	t.Run("nil tree", func(t *testing.T) {
		mp := newTestMetaPartition(t, rootDir, ctrl)
		mp.objExtentDelTree = nil
		mp.runObjExtentDelTreeGCOnce()
	})

	t.Run("restoring snapshot", func(t *testing.T) {
		mp := newTestMetaPartition(t, rootDir, ctrl, func(ctrl *gomock.Controller, mp *metaPartition) {
			raft := raftstoremock.NewMockPartition(ctrl)
			raft.EXPECT().Status().Return(&raftstore.PartitionStatus{RestoringSnapshot: true}).AnyTimes()
			raft.EXPECT().LeaderTerm().Return(uint64(1), uint64(1)).AnyTimes()
			mp.raftPartition = raft
		})
		mp.runObjExtentDelTreeGCOnce()
	})

	t.Run("not leader", func(t *testing.T) {
		mp := newTestMetaPartition(t, rootDir, ctrl, func(ctrl *gomock.Controller, mp *metaPartition) {
			raft := raftstoremock.NewMockPartition(ctrl)
			raft.EXPECT().Status().Return(&raftstore.PartitionStatus{RestoringSnapshot: false}).AnyTimes()
			raft.EXPECT().LeaderTerm().Return(uint64(2), uint64(1)).AnyTimes()
			mp.raftPartition = raft
		})
		mp.runObjExtentDelTreeGCOnce()
	})

	t.Run("empty tree", func(t *testing.T) {
		mp := newTestMetaPartition(t, rootDir, ctrl, func(ctrl *gomock.Controller, mp *metaPartition) {
			raft := raftstoremock.NewMockPartition(ctrl)
			raft.EXPECT().Status().Return(&raftstore.PartitionStatus{RestoringSnapshot: false}).AnyTimes()
			raft.EXPECT().LeaderTerm().Return(uint64(1), uint64(1)).AnyTimes()
			mp.raftPartition = raft
		})
		mp.runObjExtentDelTreeGCOnce()
	})
}

func TestRunObjExtentDelTreeGCOnce_ReturnsEncodeErrors(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_encode_err")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	t.Run("marshal punish", func(t *testing.T) {
		mp := newTestMetaPartition(t, rootDir, ctrl)
		mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}
		mp.objExtentDelTree.EnqueueFromApply(1, 1700000000, 1, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)})

		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
			func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error { return errors.New("delete failed") })
		patches.ApplyMethod(reflect.TypeOf(&batchObjExtentDelItems{}), "MarshalPunish",
			func(_ *batchObjExtentDelItems, _ *bytes.Buffer, _ int64) error {
				return errors.New("encode punish failed")
			})
		onceErr := mp.runObjExtentDelTreeGCOnce()
		require.ErrorContains(t, onceErr, "encode punish failed")
		require.Equal(t, 1, mp.objExtentDelTree.Len())
	})

	t.Run("marshal dequeue", func(t *testing.T) {
		mp := newTestMetaPartition(t, rootDir, ctrl)
		mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}
		mp.objExtentDelTree.EnqueueFromApply(1, 1700000001, 2, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 2)})

		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
			func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error { return nil })
		patches.ApplyMethod(reflect.TypeOf(&batchObjExtentDelItems{}), "MarshalDequeue",
			func(_ *batchObjExtentDelItems, _ *bytes.Buffer) error {
				return errors.New("encode dequeue failed")
			})
		onceErr := mp.runObjExtentDelTreeGCOnce()
		require.ErrorContains(t, onceErr, "encode dequeue failed")
		require.Equal(t, 1, mp.objExtentDelTree.Len())
	})
}

func TestRunObjExtentDelTreeGCWorker_EncodeErrorBackoff(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_encode_worker")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := newTestMetaPartition(t, rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}
	mp.objExtentDelTree.EnqueueFromApply(1, 1700000000, 1, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)})

	var slept int32
	stop := make(chan bool)
	mp.stopC = stop
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(time.Sleep, func(d time.Duration) {
		if isObjExtentDelTreeGCBackoffSleep(d) {
			atomic.StoreInt32(&slept, 1)
			select {
			case <-stop:
			default:
				close(stop)
			}
		}
	})
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error { return errors.New("delete failed") })
	patches.ApplyMethod(reflect.TypeOf(&batchObjExtentDelItems{}), "MarshalPunish",
		func(_ *batchObjExtentDelItems, _ *bytes.Buffer, _ int64) error {
			return errors.New("encode punish failed")
		})
	mp.runObjExtentDelTreeGCWorker()
	require.Equal(t, int32(1), atomic.LoadInt32(&slept))
}

func TestRunObjExtentDelTreeGCWorker_DrainsUntilEmpty(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_worker")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := newTestMetaPartition(t, rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error { return nil })

	for i := 0; i < int(objExtentDelTreeGcBatch)+5; i++ {
		oek := createTestObjExtentKey(uint64(i*100), 64, uint64(i+1))
		mp.objExtentDelTree.EnqueueFromApply(uint64(100+i), 1700000000, uint64(10+i), []proto.ObjExtentKey{oek})
	}
	require.Greater(t, mp.objExtentDelTree.Len(), int(objExtentDelTreeGcBatch))

	mp.runObjExtentDelTreeGCWorker()
	require.Equal(t, 0, mp.objExtentDelTree.Len())
}

func TestStartObjExtentDelTreeGC_DrainsInBackground(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_bg")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mp := newTestMetaPartition(t, rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}
	mp.objExtentDelTree.EnqueueFromApply(1, 1700000000, 1, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)})

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error { return nil })

	mp.startObjExtentDelTreeGC()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mp.objExtentDelTree.Len() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, 0, mp.objExtentDelTree.Len())
	close(mp.stopC)
	time.Sleep(20 * time.Millisecond)
}

func TestStartObjExtentDelTreeGC_PanicRecover(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_ticker")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mp2 := newTestMetaPartition(t, rootDir, ctrl)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	raft := raftstoremock.NewMockPartition(ctrl)
	raft.EXPECT().Status().Return(nil).AnyTimes() // trigger panic at status.RestoringSnapshot
	raft.EXPECT().LeaderTerm().Return(uint64(1), uint64(1)).AnyTimes()
	mp2.raftPartition = raft
	mp2.startObjExtentDelTreeGC()
	time.Sleep(30 * time.Millisecond) // panic should be recovered by defer in goroutine
	close(mp2.stopC)
}

func TestRunObjExtentDelTreeGCOnce_ItemsEmptyAndSubmitErrors(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_tree_submit")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := newTestMetaPartition(t, rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}
	oek := createTestObjExtentKey(0, 1024, 1)
	mp.objExtentDelTree.EnqueueFromApply(42, 1700000000, 7, []proto.ObjExtentKey{oek})

	t.Run("peek returns empty", func(t *testing.T) {
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(mp.objExtentDelTree), "PeekFirstN",
			func(_ *objExtentDelTree, _ int) batchObjExtentDelItems { return batchObjExtentDelItems{} })
		mp.runObjExtentDelTreeGCOnce()
	})

	t.Run("punish submit error", func(t *testing.T) {
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
			func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error { return errors.New("delete failed") })
		patches.ApplyMethod(reflect.TypeOf(mp.raftPartition), "Submit",
			func(_ *raftstoremock.MockPartition, _ []byte) (interface{}, error) {
				return nil, errors.New("submit failed")
			})
		onceErr := mp.runObjExtentDelTreeGCOnce()
		require.ErrorContains(t, onceErr, "submit failed")
		require.Equal(t, 1, mp.objExtentDelTree.Len())
	})

	t.Run("dequeue submit error", func(t *testing.T) {
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
			func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error { return nil })
		patches.ApplyMethod(reflect.TypeOf(mp.raftPartition), "Submit",
			func(_ *raftstoremock.MockPartition, _ []byte) (interface{}, error) {
				return nil, errors.New("submit failed")
			})
		onceErr := mp.runObjExtentDelTreeGCOnce()
		require.ErrorContains(t, onceErr, "submit failed")
		require.Equal(t, 1, mp.objExtentDelTree.Len())
	})
}

func TestRunObjExtentDelTreeGCWorker_entryGuards(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_worker_guard")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var sleptDelTreeBackoff, sleptMinute int32
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(time.Sleep, func(d time.Duration) {
		switch {
		case isObjExtentDelTreeGCBackoffSleep(d):
			atomic.StoreInt32(&sleptDelTreeBackoff, 1)
		case d == time.Minute:
			atomic.StoreInt32(&sleptMinute, 1)
		}
	})

	t.Run("nil tree", func(t *testing.T) {
		atomic.StoreInt32(&sleptDelTreeBackoff, 0)
		mp := newTestMetaPartition(t, rootDir, ctrl)
		mp.objExtentDelTree = nil
		mp.runObjExtentDelTreeGCWorker()
		require.Equal(t, int32(1), atomic.LoadInt32(&sleptDelTreeBackoff))
	})

	t.Run("not leader at entry", func(t *testing.T) {
		atomic.StoreInt32(&sleptDelTreeBackoff, 0)
		mp := newTestMetaPartition(t, rootDir, ctrl, func(ctrl *gomock.Controller, mp *metaPartition) {
			raft := raftstoremock.NewMockPartition(ctrl)
			raft.EXPECT().Status().Return(&raftstore.PartitionStatus{RestoringSnapshot: false}).AnyTimes()
			raft.EXPECT().LeaderTerm().Return(uint64(2), uint64(1)).AnyTimes()
			mp.raftPartition = raft
		})
		mp.runObjExtentDelTreeGCWorker()
		require.Equal(t, int32(1), atomic.LoadInt32(&sleptDelTreeBackoff))
	})

	t.Run("empty tree sleeps minute", func(t *testing.T) {
		atomic.StoreInt32(&sleptMinute, 0)
		mp := newTestMetaPartition(t, rootDir, ctrl, func(ctrl *gomock.Controller, mp *metaPartition) {
			raft := raftstoremock.NewMockPartition(ctrl)
			raft.EXPECT().Status().Return(&raftstore.PartitionStatus{RestoringSnapshot: false}).AnyTimes()
			raft.EXPECT().LeaderTerm().Return(uint64(1), uint64(1)).AnyTimes()
			mp.raftPartition = raft
		})
		mp.runObjExtentDelTreeGCWorker()
		require.Equal(t, int32(1), atomic.LoadInt32(&sleptMinute))
	})
}

func TestRunObjExtentDelTreeGCWorker_stopAndNotLeaderInLoop(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_worker_ctl")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := newTestMetaPartition(t, rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}
	mp.objExtentDelTree.EnqueueFromApply(1, 1700000000, 1, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)})

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(time.Sleep, func(time.Duration) {})
	leaderCalls := 0
	patches.ApplyMethod(reflect.TypeOf(mp), "IsLeader",
		func(_ *metaPartition) (string, bool) {
			leaderCalls++
			if leaderCalls <= 1 {
				return "leader", true
			}
			return "", false
		})
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error { return errors.New("fail") })
	mp.runObjExtentDelTreeGCWorker()

	mp2 := newTestMetaPartition(t, rootDir, ctrl)
	mp2.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}
	mp2.objExtentDelTree.EnqueueFromApply(2, 1700000000, 2, []proto.ObjExtentKey{createTestObjExtentKey(0, 2, 2)})
	close(mp2.stopC)
	mp2.runObjExtentDelTreeGCWorker()
}

func TestRunObjExtentDelTreeGCOnce_ReturnsDeleteOrSubmitError(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_once_err")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := newTestMetaPartition(t, rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}
	mp.objExtentDelTree.EnqueueFromApply(1, 1700000000, 1, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)})

	var slept int32
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(time.Sleep, func(d time.Duration) {
		if d == AsyncDeleteInterval {
			atomic.StoreInt32(&slept, 1)
		}
	})
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error {
			return errors.New("delete failed")
		})
	patches.ApplyMethod(reflect.TypeOf(mp.raftPartition), "Submit",
		func(_ *raftstoremock.MockPartition, _ []byte) (interface{}, error) {
			return nil, errors.New("submit failed")
		})

	onceErr := mp.runObjExtentDelTreeGCOnce()
	require.ErrorContains(t, onceErr, "submit failed")
	require.Equal(t, int32(0), atomic.LoadInt32(&slept), "backoff is worker responsibility, not Once")
	require.Equal(t, 1, mp.objExtentDelTree.Len())
}

func TestRunObjExtentDelTreeGCOnce_PunishAppliedReturnsNil(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_once_punish_ok")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := newTestMetaPartition(t, rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}
	mp.objExtentDelTree.EnqueueFromApply(1, 1700000000, 1, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)})

	var slept int32
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(time.Sleep, func(d time.Duration) {
		if d == AsyncDeleteInterval {
			atomic.StoreInt32(&slept, 1)
		}
	})
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error {
			return errors.New("delete failed")
		})
	patches.ApplyFunc(time.Now, func() time.Time {
		return time.UnixMilli(1_700_000_000_000)
	})

	onceErr := mp.runObjExtentDelTreeGCOnce()
	require.NoError(t, onceErr, "submit clears err after successful punish")
	require.Equal(t, int32(0), atomic.LoadInt32(&slept), "Once does not sleep")
	require.Equal(t, 1, mp.objExtentDelTree.Len())

	peek := mp.objExtentDelTree.PeekFirstN(1)
	require.Len(t, peek.Items, 1)
	require.Equal(t, int64(1_700_000_000_000)+objExtentDelGcPenaltyMs, peek.Items[0].TsMs)
}

func TestRunObjExtentDelTreeGCWorker_errorBackoffInLoop(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_worker_backoff")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := newTestMetaPartition(t, rootDir, ctrl)
	mp.blobClientWrapper = &BlobStoreClientWrapper{blobClient: &blobstore.BlobStoreClient{}}
	mp.objExtentDelTree.EnqueueFromApply(1, 1700000000, 1, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)})

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	stop := make(chan bool)
	mp.stopC = stop
	patches.ApplyFunc(time.Sleep, func(d time.Duration) {
		if isObjExtentDelTreeGCBackoffSleep(d) {
			select {
			case <-stop:
			default:
				close(stop)
			}
		}
	})
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, _ []proto.ObjExtentKey) error {
			return errors.New("delete failed")
		})
	patches.ApplyMethod(reflect.TypeOf(mp.raftPartition), "Submit",
		func(_ *raftstoremock.MockPartition, _ []byte) (interface{}, error) {
			return nil, errors.New("submit failed")
		})
	mp.runObjExtentDelTreeGCWorker()
}

func TestApply_objExtentGcFsmOps(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_fsm_apply")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := newTestMetaPartition(t, rootDir, ctrl)

	oek := createTestObjExtentKey(0, 1024, 1)
	mp.objExtentDelTree.EnqueueFromApply(42, 1700000000, 7, []proto.ObjExtentKey{oek})
	require.Equal(t, 1, mp.objExtentDelTree.Len())

	batch := mp.objExtentDelTree.PeekFirstN(1)
	deqPayload, err := encodeBatchDequeue(batch)
	require.NoError(t, err)
	deqItem := NewMetaItem(opFSMObjExtentGcDequeue, nil, deqPayload)
	deqCmd, err := deqItem.MarshalJson()
	require.NoError(t, err)
	_, err = mp.Apply(deqCmd, 200)
	require.NoError(t, err)
	require.Equal(t, 0, mp.objExtentDelTree.Len())

	mp.objExtentDelTree.EnqueueFromApply(99, 1700000001, 3, []proto.ObjExtentKey{oek})
	batch = mp.objExtentDelTree.PeekFirstN(1)
	punishPayload, err := encodeBatchPunish(batch, 1700000099999)
	require.NoError(t, err)
	punishItem := NewMetaItem(opFSMObjExtentGcPunishRequeue, nil, punishPayload)
	punishCmd, err := punishItem.MarshalJson()
	require.NoError(t, err)
	_, err = mp.Apply(punishCmd, 201)
	require.NoError(t, err)
	require.Equal(t, 1, mp.objExtentDelTree.Len())
	peek := mp.objExtentDelTree.PeekFirstN(1)
	require.Equal(t, int64(1700000099999), peek.Items[0].TsMs)
	require.Equal(t, uint64(201), peek.Items[0].RaftIdx)
}

// TestObjExtentDelTreeEnqueue covers cap-on behavior: full queue must not grow (by design, not data-loss bug).
func TestObjExtentDelTreeEnqueue(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "obj_extent_del_metrics")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	t.Run("queueFullRejectsEnqueue", func(t *testing.T) {
		defer swapDelTreeMaxItemLimitRaw(1)()
		mp := newTestMetaPartition(t, rootDir, nil)
		mp.enqueueObjExtentDelWrap(42, 1700000000, 7, []proto.ObjExtentKey{createTestObjExtentKey(0, 1024, 1)})
		require.Equal(t, 1, mp.objExtentDelTree.Len())

		mp.enqueueObjExtentDelWrap(99, 1700000001, 8, []proto.ObjExtentKey{createTestObjExtentKey(10, 512, 2)})
		require.Equal(t, 1, mp.objExtentDelTree.Len())
	})

	t.Run("queueFullAudit", func(t *testing.T) {
		defer swapDelTreeMaxItemLimitRaw(1)()
		mp := newTestMetaPartition(t, rootDir, nil)
		mp.enqueueObjExtentDelWrap(42, 1700000000, 7, []proto.ObjExtentKey{createTestObjExtentKey(0, 1024, 1)})

		dropped := []proto.ObjExtentKey{
			createTestObjExtentKey(10, 512, 2),
			createTestObjExtentKey(20, 256, 3),
		}
		var auditOp, auditMsg string
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyFunc(auditlog.LogOpMsg, func(op, msg string, err error) {
			auditOp = op
			auditMsg = msg
		})

		mp.enqueueObjExtentDelWrap(99, 1700000001, 8, dropped)
		require.Equal(t, "ObjExtentDelTreeFull", auditOp)
		require.Contains(t, auditMsg, "mp=10001")
		require.Contains(t, auditMsg, "inode=99")
		require.Contains(t, auditMsg, "oek[0]="+dropped[0].String())
		require.Contains(t, auditMsg, "oek[1]="+dropped[1].String())
	})

	t.Run("detachRemovesPartition", func(t *testing.T) {
		mp := newTestMetaPartition(t, rootDir, nil)
		mp.manager.partitions[mp.config.PartitionId] = mp
		mp.objExtentDelTree.EnqueueFromApply(1, 0, 1, []proto.ObjExtentKey{createTestObjExtentKey(0, 1, 1)})
		require.NoError(t, mp.manager.detachPartition(mp.config.PartitionId))
		_, ok := mp.manager.partitions[mp.config.PartitionId]
		require.False(t, ok)
	})

	t.Run("enqueueNoLimitWhenMaxIsZero", func(t *testing.T) {
		defer swapDelTreeMaxItemLimit(0)()
		mp := newTestMetaPartition(t, rootDir, nil)
		for i := 0; i < 3; i++ {
			mp.enqueueObjExtentDelWrap(uint64(100+i), 1700000000, uint64(i), []proto.ObjExtentKey{createTestObjExtentKey(0, 1, uint64(i+1))})
		}
		require.Equal(t, 3, mp.objExtentDelTree.Len())
	})
}
