// Copyright 2026 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the License);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package fs

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/stretchr/testify/require"
)

func newSuperDirDirtyHarness(metaAccel bool) *Super {
	return &Super{
		metaCacheAcceleration: metaAccel,
		dirDirtyCache:         make(map[uint64]bool),
		dirDirtyCount:         make(map[uint64]int),
	}
}

func TestReadDirAllCacheBegin_metaAccelerationOff(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(false)
	const ino = 42
	require.False(t, s.readDirAllCacheBegin(ino))
	_, ok := s.dirDirtyCache[ino]
	require.False(t, ok, "should not touch dirDirtyCache when acceleration off")
}

func TestReadDirAllCacheBegin_countPositive_skips(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino = 100
	s.dirDirtyCount[ino] = 1
	require.True(t, s.readDirAllCacheBegin(ino))
	_, ok := s.dirDirtyCache[ino]
	require.False(t, ok, "skip path must not establish dirDirtyCache entry")
}

func TestReadDirAllCacheBegin_countZero_setsFalse(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino = 200
	require.False(t, s.readDirAllCacheBegin(ino))
	v, ok := s.dirDirtyCache[ino]
	require.True(t, ok)
	require.False(t, v)
}

func TestReleaseDirDirty_metaAccelerationOff(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(false)
	s.dirDirtyCache[7] = true
	s.ReleaseDirDirty(7)
	require.True(t, s.dirDirtyCache[7])
}

func TestReleaseDirDirty_deletesEntry(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	s.dirDirtyCache[11] = false
	s.dirDirtyCache[12] = true
	s.ReleaseDirDirty(11)
	_, ok := s.dirDirtyCache[11]
	require.False(t, ok)
	_, ok12 := s.dirDirtyCache[12]
	require.True(t, ok12)
}

func TestBeginEndDirMutation_metaAccelerationOff(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(false)
	const ino = 55
	s.BeginDirMutation(ino)
	s.EndDirMutation(ino)
	require.Empty(t, s.dirDirtyCount)
}

func TestBeginEndDirMutation_singlePair_clearsCount(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino = 300
	s.BeginDirMutation(ino)
	require.Equal(t, 1, s.dirDirtyCount[ino])
	s.EndDirMutation(ino)
	_, ok := s.dirDirtyCount[ino]
	require.False(t, ok)
}

func TestBeginEndDirMutation_nestedDecrement(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino = 400
	s.BeginDirMutation(ino)
	s.BeginDirMutation(ino)
	require.Equal(t, 2, s.dirDirtyCount[ino])

	s.EndDirMutation(ino)
	require.Equal(t, 1, s.dirDirtyCount[ino])

	s.EndDirMutation(ino)
	_, ok := s.dirDirtyCount[ino]
	require.False(t, ok)
}

func TestEndDirMutation_withDirDirtyCacheKey_nestedDecrementStillMarksDirty(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino = 450
	require.False(t, s.readDirAllCacheBegin(ino))

	s.BeginDirMutation(ino)
	s.BeginDirMutation(ino)

	s.EndDirMutation(ino)
	require.True(t, s.dirDirtyCache[ino])
	require.Equal(t, 1, s.dirDirtyCount[ino])

	s.EndDirMutation(ino)
	require.True(t, s.dirDirtyCache[ino])
	_, ok := s.dirDirtyCount[ino]
	require.False(t, ok)
}

func TestEndDirMutation_setsDirtyWhenDirDirtyCacheKeyExists(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino = 500
	require.False(t, s.readDirAllCacheBegin(ino))
	require.False(t, s.dirDirtyCache[ino])

	s.BeginDirMutation(ino)
	s.EndDirMutation(ino)

	require.True(t, s.dirDirtyCache[ino])
	_, ok := s.dirDirtyCount[ino]
	require.False(t, ok)
}

func TestEndDirMutation_withoutBegin_doesNotAlterCountMap(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino = 600
	s.EndDirMutation(ino)
	require.Empty(t, s.dirDirtyCount)
}

func TestCheckDirDirty_metaAccelerationOff_runsCallback(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(false)
	var ran int32
	s.CheckDirDirty(99, func() { atomic.StoreInt32(&ran, 1) })
	require.Equal(t, int32(1), atomic.LoadInt32(&ran))
}

func TestCheckDirDirty_skipsWhenCountPositive(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino = 700
	s.dirDirtyCount[ino] = 1
	var ran int32
	s.CheckDirDirty(ino, func() { atomic.StoreInt32(&ran, 1) })
	require.Equal(t, int32(0), atomic.LoadInt32(&ran))
}

func TestCheckDirDirty_skipsWhenDirtyTrue(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino = 800
	s.dirDirtyCache[ino] = true
	var ran int32
	s.CheckDirDirty(ino, func() { atomic.StoreInt32(&ran, 1) })
	require.Equal(t, int32(0), atomic.LoadInt32(&ran))
}

func TestCheckDirDirty_runsCallbackWhenClean(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino = 900
	var ran int32
	s.CheckDirDirty(ino, func() { atomic.StoreInt32(&ran, 1) })
	require.Equal(t, int32(1), atomic.LoadInt32(&ran))
}

func TestDirDirty_concurrentBeginEnd_noLostUpdates(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino uint64 = 1000
	var wg sync.WaitGroup
	n := 50
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			s.BeginDirMutation(ino)
			s.EndDirMutation(ino)
		}()
	}
	wg.Wait()
	require.Empty(t, s.dirDirtyCount)
}

func TestReadDirAllCacheBegin_resetsDirtyTrueToFalseWhenNoMutation(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino uint64 = 1100
	s.dirDirtyCache[ino] = true
	require.False(t, s.readDirAllCacheBegin(ino))
	v, ok := s.dirDirtyCache[ino]
	require.True(t, ok)
	require.False(t, v, "entry gate establishes fresh scan baseline")
}

func TestReadDirAllCacheBegin_ReleaseDirDirty_thenBeginAgain(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino uint64 = 1101
	require.False(t, s.readDirAllCacheBegin(ino))
	s.ReleaseDirDirty(ino)
	_, ok := s.dirDirtyCache[ino]
	require.False(t, ok)
	require.False(t, s.readDirAllCacheBegin(ino))
	_, ok = s.dirDirtyCache[ino]
	require.True(t, ok)
}

func TestReleaseDirDirty_missingKey_noOp(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	s.ReleaseDirDirty(99999)
	require.Empty(t, s.dirDirtyCache)
}

func TestEndDirMutation_whenDirDirtyCacheAlreadyTrue(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino uint64 = 1200
	s.dirDirtyCache[ino] = true
	s.BeginDirMutation(ino)
	s.EndDirMutation(ino)
	require.True(t, s.dirDirtyCache[ino])
	require.Empty(t, s.dirDirtyCount)
}

func TestEndDirMutation_doubleEndAfterSingleBegin_secondEndNoCount(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino uint64 = 1201
	s.BeginDirMutation(ino)
	s.EndDirMutation(ino)
	s.EndDirMutation(ino)
	require.Empty(t, s.dirDirtyCount)
}

func TestCheckDirDirty_skipsOnCountWhenDirtyWouldOtherwiseAllow(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino uint64 = 1300
	s.dirDirtyCount[ino] = 2
	s.dirDirtyCache[ino] = false
	var ran int32
	s.CheckDirDirty(ino, func() { atomic.StoreInt32(&ran, 1) })
	require.Equal(t, int32(0), atomic.LoadInt32(&ran))
}

func TestCheckDirDirty_skipsWhenBothCountAndDirtySet(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino uint64 = 1301
	s.dirDirtyCount[ino] = 1
	s.dirDirtyCache[ino] = true
	var ran int32
	s.CheckDirDirty(ino, func() { atomic.StoreInt32(&ran, 1) })
	require.Equal(t, int32(0), atomic.LoadInt32(&ran))
}

func TestCheckDirDirty_runsWhenExplicitFalseKeyAndZeroCount(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino uint64 = 1302
	s.dirDirtyCache[ino] = false
	var ran int32
	s.CheckDirDirty(ino, func() { atomic.StoreInt32(&ran, 1) })
	require.Equal(t, int32(1), atomic.LoadInt32(&ran))
}

func TestDirDirty_twoInodesIndependent(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const a, b uint64 = 1400, 1401
	s.BeginDirMutation(a)
	s.readDirAllCacheBegin(b)
	require.Equal(t, 1, s.dirDirtyCount[a])
	_, ok := s.dirDirtyCache[b]
	require.True(t, ok)
	s.EndDirMutation(a)
	require.Empty(t, s.dirDirtyCount)
	v, ok := s.dirDirtyCache[b]
	require.True(t, ok)
	require.False(t, v)
}

func TestBeginEnd_manyPairsSameIno(t *testing.T) {
	t.Parallel()
	s := newSuperDirDirtyHarness(true)
	const ino uint64 = 1500
	for i := 0; i < 25; i++ {
		s.BeginDirMutation(ino)
	}
	for i := 0; i < 25; i++ {
		s.EndDirMutation(ino)
	}
	require.Empty(t, s.dirDirtyCount)
}

func TestCheckDirDirty_tableDriven(t *testing.T) {
	t.Parallel()
	type row struct {
		name     string
		accel    bool
		count    int
		dirty    *bool
		wantRuns bool
	}
	trueB := true
	cases := []row{
		{"accel_off", false, 0, nil, true},
		{"accel_on_clean", true, 0, nil, true},
		{"accel_on_dirty_true", true, 0, &trueB, false},
		{"accel_on_count", true, 1, nil, false},
		{"accel_on_count_dirty", true, 2, &trueB, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSuperDirDirtyHarness(tc.accel)
			const ino uint64 = 1600
			if tc.count > 0 {
				s.dirDirtyCount[ino] = tc.count
			}
			if tc.dirty != nil {
				s.dirDirtyCache[ino] = *tc.dirty
			}
			var ran int32
			s.CheckDirDirty(ino, func() { atomic.StoreInt32(&ran, 1) })
			if tc.wantRuns {
				require.Equal(t, int32(1), atomic.LoadInt32(&ran))
			} else {
				require.Equal(t, int32(0), atomic.LoadInt32(&ran))
			}
		})
	}
}

func FuzzDirDirtyBeginEndBalanced(f *testing.F) {
	f.Add(uint64(77), byte(3))
	f.Add(uint64(1), byte(1))
	f.Fuzz(func(t *testing.T, ino uint64, depth byte) {
		if ino == 0 {
			ino = 1
		}
		n := int(depth%15) + 1
		s := newSuperDirDirtyHarness(true)
		for i := 0; i < n; i++ {
			s.BeginDirMutation(ino)
		}
		for i := 0; i < n; i++ {
			s.EndDirMutation(ino)
		}
		require.Empty(t, s.dirDirtyCount)
	})
}

func TestSuperBlobStoreAheadReadForReader(t *testing.T) {
	s := &Super{
		aheadReadEnable:   true,
		minReadAheadSize:  123,
		aheadReadTotalMem: 456,
	}
	enable, min, total := s.BlobStoreAheadReadForReader()
	require.True(t, enable)
	require.Equal(t, 123, min)
	require.Equal(t, int64(456), total)
}

func TestNewSuper_defaultMinReadAheadSizeWhenZero(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	mw := &meta.MetaWrapper{}
	mc := masterSDK.NewMasterClient([]string{"127.0.0.1:1"}, false)
	patches.ApplyFunc(meta.NewMetaWrapper, func(_ *meta.MetaConfig) (*meta.MetaWrapper, error) {
		return mw, nil
	})
	patches.ApplyMethod(reflect.TypeOf(mw), "GetRootIno",
		func(_ *meta.MetaWrapper, _ string) (uint64, error) { return 1, nil })
	{
		v := reflect.ValueOf(mw).Elem()
		mcField := v.FieldByName("mc")
		reflect.NewAt(mcField.Type(), unsafe.Pointer(mcField.UnsafeAddr())).Elem().Set(reflect.ValueOf(mc))
		clusterField := v.FieldByName("cluster")
		reflect.NewAt(clusterField.Type(), unsafe.Pointer(clusterField.UnsafeAddr())).Elem().SetString("test-cluster")
	}
	admin := mc.AdminAPI()
	patches.ApplyMethod(reflect.TypeOf(admin), "GetVolumeSimpleInfo",
		func(_ *masterSDK.AdminAPI, _ string) (*proto.SimpleVolView, error) {
			return &proto.SimpleVolView{
				VolType:             proto.VolumeTypeHot,
				ObjBlockSize:        4096,
				VolStorageClass:     proto.StorageClass_Replica_HDD,
				AllowedStorageClass: []uint32{proto.StorageClass_Replica_HDD},
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(admin), "GetClusterInfo",
		func(_ *masterSDK.AdminAPI) (*proto.ClusterInfo, error) {
			return &proto.ClusterInfo{
				EbsAddr: "http://127.0.0.1:8080", ServicePath: "/svc", Cluster: "test-cluster",
				DirChildrenNumLimit: proto.DefaultDirChildrenNumLimit,
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(admin), "ListStoragePools",
		func(_ *masterSDK.AdminAPI) ([]*proto.StoragePoolInfo, error) { return nil, nil })
	patches.ApplyFunc(stream.NewExtentClient, func(_ *stream.ExtentConfig) (*stream.ExtentClient, error) {
		ec := &stream.ExtentClient{}
		ev := reflect.ValueOf(ec).Elem()
		mvField := ev.FieldByName("multiVerMgr")
		reflect.NewAt(mvField.Type(), unsafe.Pointer(mvField.UnsafeAddr())).Elem().Set(reflect.ValueOf(&stream.MultiVerMgr{}))
		dwField := ev.FieldByName("dataWrapper")
		reflect.NewAt(dwField.Type(), unsafe.Pointer(dwField.UnsafeAddr())).Elem().Set(reflect.ValueOf(&datawrapper.Wrapper{}))
		return ec, nil
	})

	s, err := NewSuper(&proto.MountOptions{
		Volname: "vol", Owner: "owner", Master: "127.0.0.1:1", MountPoint: "/mnt/cubefs", SubDir: "/",
		InodeLruLimit: 1024, ReadThreads: 1, WriteThreads: 1, VolType: proto.VolumeTypeHot,
		EbsBlockSize: 4096, ClientOpTimeOut: 1, MetaCacheAcceleration: false, StopWarmMeta: true,
		AheadReadEnable: true, AheadReadTotalMem: 1024, AheadReadBlockTimeOut: 1, AheadReadWindowCnt: 1,
		MinReadAheadSize: 0,
		VolStorageClass:  proto.StorageClass_Replica_HDD, VolAllowedStorageClass: []uint32{proto.StorageClass_Replica_HDD},
		EnableTransaction: "off", TrashRebuildGoroutineLimit: 1, TrashDeleteExpiredDirGoroutineLimit: 1,
	})
	require.NoError(t, err)
	require.Equal(t, uint64(proto.DefaultMinReadAheadSize), s.minReadAheadSize)
	close(s.closeC)
	s.runningMonitor.Stop()
}

func TestNewSuper_CoversInitBranches(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	mw := &meta.MetaWrapper{}
	mc := masterSDK.NewMasterClient([]string{"127.0.0.1:1"}, false)
	patches.ApplyFunc(meta.NewMetaWrapper, func(_ *meta.MetaConfig) (*meta.MetaWrapper, error) {
		return mw, nil
	})
	patches.ApplyMethod(reflect.TypeOf(mw), "GetRootIno",
		func(_ *meta.MetaWrapper, _ string) (uint64, error) { return 1, nil })
	// Set unexported fields to avoid inlined getter patch issues.
	{
		v := reflect.ValueOf(mw).Elem()
		mcField := v.FieldByName("mc")
		reflect.NewAt(mcField.Type(), unsafe.Pointer(mcField.UnsafeAddr())).Elem().Set(reflect.ValueOf(mc))
		clusterField := v.FieldByName("cluster")
		reflect.NewAt(clusterField.Type(), unsafe.Pointer(clusterField.UnsafeAddr())).Elem().SetString("test-cluster")
	}

	admin := mc.AdminAPI()
	patches.ApplyMethod(reflect.TypeOf(admin), "GetVolumeSimpleInfo",
		func(_ *masterSDK.AdminAPI, _ string) (*proto.SimpleVolView, error) {
			return &proto.SimpleVolView{
				VolType:             proto.VolumeTypeHot,
				ObjBlockSize:        4096,
				VolStorageClass:     proto.StorageClass_Replica_HDD,
				AllowedStorageClass: []uint32{proto.StorageClass_Replica_HDD},
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(admin), "GetClusterInfo",
		func(_ *masterSDK.AdminAPI) (*proto.ClusterInfo, error) {
			return &proto.ClusterInfo{
				EbsAddr:             "http://127.0.0.1:8080",
				ServicePath:         "/svc",
				Cluster:             "test-cluster",
				DirChildrenNumLimit: proto.DefaultDirChildrenNumLimit,
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(admin), "ListStoragePools",
		func(_ *masterSDK.AdminAPI) ([]*proto.StoragePoolInfo, error) { return nil, nil })

	patches.ApplyFunc(stream.NewExtentClient, func(_ *stream.ExtentConfig) (*stream.ExtentClient, error) {
		ec := &stream.ExtentClient{}
		ev := reflect.ValueOf(ec).Elem()
		mvField := ev.FieldByName("multiVerMgr")
		reflect.NewAt(mvField.Type(), unsafe.Pointer(mvField.UnsafeAddr())).Elem().Set(reflect.ValueOf(&stream.MultiVerMgr{}))
		dwField := ev.FieldByName("dataWrapper")
		reflect.NewAt(dwField.Type(), unsafe.Pointer(dwField.UnsafeAddr())).Elem().Set(reflect.ValueOf(&datawrapper.Wrapper{}))
		return ec, nil
	})

	s, err := NewSuper(&proto.MountOptions{
		Volname:                             "vol",
		Owner:                               "owner",
		Master:                              "127.0.0.1:1",
		MountPoint:                          "/mnt/cubefs",
		SubDir:                              "/",
		InodeLruLimit:                       1024,
		ReadThreads:                         1,
		WriteThreads:                        1,
		VolType:                             proto.VolumeTypeHot,
		EbsBlockSize:                        4096,
		ClientOpTimeOut:                     1,
		MetaCacheAcceleration:               false,
		StopWarmMeta:                        true,
		AheadReadEnable:                     true,
		AheadReadTotalMem:                   1024,
		AheadReadBlockTimeOut:               1,
		AheadReadWindowCnt:                  1,
		MinReadAheadSize:                    1,
		VolStorageClass:                     proto.StorageClass_Replica_HDD,
		VolAllowedStorageClass:              []uint32{proto.StorageClass_Replica_HDD},
		EnableTransaction:                   "off",
		TrashRebuildGoroutineLimit:          1,
		TrashDeleteExpiredDirGoroutineLimit: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, s.ebsc)
	require.NotNil(t, s.oec)
	require.NotNil(t, s.runningMonitor)
	close(s.closeC)
	s.runningMonitor.Stop()
}

func TestSuper_scheduleFlush_idleWriterTriggersOecFlush(t *testing.T) {
	s := newTestSuperForFile()
	s.oec = blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{})
	f := &File{super: s, ino: 60, parentIno: 1, name: "idle.dat"}
	ei := f.getOrCreateExtendInfo()
	atomic.StoreInt32(&ei.idle, BlobWriterIdleTimeoutPeriod)
	s.fslock.Lock()
	s.nodeCache[f.ino] = f
	s.fslock.Unlock()
	// scheduleFlush 仅在 oec 流存在且 refCnt>0 时刷盘（与 File.storeIdle / oec 数据面一致）。
	registerOecTestStreamerWithLogicalView(s, f.ino, nil, &blobstore.Writer{}, 0, 1)
	require.NoError(t, s.oec.OpenStreamWithArgs(blobstore.ECStreamOpenArgs{Ino: f.ino}))

	flushed := make(chan uint64, 1)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.oec), "Flush",
		func(_ *blobstore.ECExtentClient, ino uint64) error {
			flushed <- ino
			return nil
		})

	go s.scheduleFlush()
	select {
	case got := <-flushed:
		require.Equal(t, f.ino, got)
	case <-time.After(6 * time.Second):
		t.Fatal("scheduleFlush did not trigger oec.Flush in time")
	}
}
