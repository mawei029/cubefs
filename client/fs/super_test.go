// Copyright 2026 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the License);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package fs

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/blobstore/api/access"
	"github.com/cubefs/cubefs/blobstore/sdk"
	"github.com/cubefs/cubefs/depends/bazil.org/fuse"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/data/wrapper"
	masterSDK "github.com/cubefs/cubefs/sdk/master"
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

func TestMetaCacheAccelerationInitializesReadDirPool(t *testing.T) {
	t.Parallel()
	s := &Super{metaCacheAcceleration: true}
	s.initReadDirPool()
	require.NotNil(t, s.readDirPool)

	done := make(chan struct{})
	s.readDirPool.Run(func() {
		close(done)
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readDirPool task did not run")
	}
}

func TestMetaCacheAccelerationOffLeavesReadDirPoolNil(t *testing.T) {
	t.Parallel()
	s := &Super{metaCacheAcceleration: false}
	s.initReadDirPool()
	require.Nil(t, s.readDirPool)
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
		reflect.NewAt(dwField.Type(), unsafe.Pointer(dwField.UnsafeAddr())).Elem().Set(reflect.ValueOf(&wrapper.Wrapper{}))
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
		reflect.NewAt(dwField.Type(), unsafe.Pointer(dwField.UnsafeAddr())).Elem().Set(reflect.ValueOf(&wrapper.Wrapper{}))
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
		StreamRetryTimeout:                  180,
		VolStorageClass:                     proto.StorageClass_Replica_HDD,
		VolAllowedStorageClass:              []uint32{proto.StorageClass_Replica_HDD},
		EnableTransaction:                   "off",
		TrashRebuildGoroutineLimit:          1,
		TrashDeleteExpiredDirGoroutineLimit: 1,
	})
	require.NoError(t, err)
	require.Equal(t, 180, s.streamRetryTimeout)
	require.NotNil(t, s.ebsc)
	require.NotNil(t, s.oec)
	require.NotNil(t, s.runningMonitor)
	close(s.closeC)
	s.runningMonitor.Stop()
}

func TestGetBlobStoreClientPassesStreamRetryTimeout(t *testing.T) {
	const wantTimeout = 180
	s := &Super{
		ebsc:               make(map[uint8]*blobstore.BlobStoreClient),
		logpath:            t.TempDir(),
		streamRetryTimeout: wantTimeout,
		ebsConfig:          proto.DefaultEbsClientConfig(),
		poolCache: map[uint8]*proto.StoragePoolInfo{
			1: {Id: 1, ECAddr: "127.0.0.1:8500"},
		},
	}

	gotTimeout := -1
	var gotAccessCfg access.Config
	patches := gomonkey.ApplyFunc(blobstore.NewEbsClient, func(cfg access.Config, maxTimeoutSec int) (*blobstore.BlobStoreClient, error) {
		gotAccessCfg = cfg
		gotTimeout = maxTimeoutSec
		return &blobstore.BlobStoreClient{}, nil
	})
	defer patches.Reset()

	cli, err := s.getBlobStoreClient(1)
	require.NoError(t, err)
	require.NotNil(t, cli)
	require.Equal(t, wantTimeout, gotTimeout)
	require.Equal(t, "127.0.0.1:8500", gotAccessCfg.Consul.Address)
	require.Equal(t, access.NoLimitConnMode, gotAccessCfg.ConnMode)
	require.Equal(t, 60, gotAccessCfg.ServiceIntervalS)
	require.Equal(t, -1, gotAccessCfg.FailRetryIntervalS)
	require.Equal(t, MaxSizePutOnce, gotAccessCfg.MaxSizePutOnce)
	require.Equal(t, 6, gotAccessCfg.MaxHostRetry)
	require.Equal(t, int64(6000), gotAccessCfg.BodyBaseTimeoutMs)
	require.Equal(t, float64(2), gotAccessCfg.BodyBandwidthMBPs)
	require.Same(t, cli, s.ebsc[1])
}

func TestGetBlobStoreClientConsulAddressOverridesPoolECAddr(t *testing.T) {
	ec := proto.DefaultEbsClientConfig()
	ec.ConsulAddress = "consul_address:8500"

	s := &Super{
		ebsc:      make(map[uint8]*blobstore.BlobStoreClient),
		logpath:   t.TempDir(),
		ebsConfig: ec,
		poolCache: map[uint8]*proto.StoragePoolInfo{
			1: {Id: 1, ECAddr: "127.0.0.1:9999"},
		},
	}

	var gotConsul string
	patches := gomonkey.ApplyFunc(blobstore.NewEbsClient, func(cfg access.Config, _ int) (*blobstore.BlobStoreClient, error) {
		gotConsul = cfg.Consul.Address
		return &blobstore.BlobStoreClient{}, nil
	})
	defer patches.Reset()

	_, err := s.getBlobStoreClient(1)
	require.NoError(t, err)
	require.Equal(t, "consul_address:8500", gotConsul)
}

func TestGetBlobStoreClientToAccessConfigError(t *testing.T) {
	ec := proto.EbsClientConfig{}
	s := &Super{
		ebsc:      make(map[uint8]*blobstore.BlobStoreClient),
		logpath:   t.TempDir(),
		ebsConfig: ec,
		poolCache: map[uint8]*proto.StoragePoolInfo{
			1: {Id: 1, ECAddr: ""},
		},
	}

	_, err := s.getBlobStoreClient(1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ToAccessConfig failed")
}

// TestGetBlobStoreClientConcurrentSingleCreate: write lock serializes create; only one NewEbsClient.
func TestGetBlobStoreClientConcurrentSingleCreate(t *testing.T) {
	s := &Super{
		ebsc:      make(map[uint8]*blobstore.BlobStoreClient),
		logpath:   t.TempDir(),
		ebsConfig: proto.DefaultEbsClientConfig(),
		poolCache: map[uint8]*proto.StoragePoolInfo{
			1: {Id: 1, ECAddr: "127.0.0.1:8500"},
		},
	}

	want := &blobstore.BlobStoreClient{}
	var calls int32
	patches := gomonkey.ApplyFunc(blobstore.NewEbsClient, func(access.Config, int) (*blobstore.BlobStoreClient, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		return want, nil
	})
	defer patches.Reset()

	const n = 32
	var wg sync.WaitGroup
	clis := make([]*blobstore.BlobStoreClient, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			clis[idx], errs[idx] = s.getBlobStoreClient(1)
		}(i)
	}
	wg.Wait()

	require.Equal(t, int32(1), atomic.LoadInt32(&calls))
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		require.Same(t, want, clis[i])
	}
	require.Same(t, want, s.ebsc[1])
}

func TestGetBlobStoreClientCreatePanic(t *testing.T) {
	s := &Super{
		ebsc:      make(map[uint8]*blobstore.BlobStoreClient),
		logpath:   t.TempDir(),
		ebsConfig: proto.DefaultEbsClientConfig(),
		poolCache: map[uint8]*proto.StoragePoolInfo{
			3: {Id: 3, ECAddr: "127.0.0.1:8500"},
		},
	}
	patches := gomonkey.ApplyFunc(blobstore.NewEbsClient, func(access.Config, int) (*blobstore.BlobStoreClient, error) {
		panic("access client service discovery disconnect")
	})
	defer patches.Reset()

	_, err := s.getBlobStoreClient(3)
	require.Error(t, err)
	require.Contains(t, err.Error(), "panic creating blobstore client")
	require.Equal(t, fuse.EIO, ParseError(err))
	require.Nil(t, s.ebsc[3])
}

func TestGetBlobStoreClientEnableEbsSdk(t *testing.T) {
	const wantTimeout = 120
	ec := proto.DefaultEbsClientConfig()
	ec.Idc = "z0"
	ec.Region = "test-region"
	ec.RegionMagic = "test-region"
	ec.Clusters = []proto.EbsSdkCluster{{
		ClusterID: 50,
		Hosts:     []string{"http://10.0.0.1:9998"},
	}}
	s := &Super{
		ebsc:               make(map[uint8]*blobstore.BlobStoreClient),
		logpath:            t.TempDir(),
		streamRetryTimeout: wantTimeout,
		ebsConfig:          ec,
		enableEbsSdk:       true,
		poolCache: map[uint8]*proto.StoragePoolInfo{
			1: {Id: 1, ECAddr: "127.0.0.1:8500"},
		},
	}

	gotTimeout := -1
	var gotSdkCfg *sdk.Config
	newEbsClientCalled := false
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(blobstore.NewEbsClientSdk, func(cfg *sdk.Config, maxTimeoutSec int) (*blobstore.BlobStoreClient, error) {
		gotSdkCfg = cfg
		gotTimeout = maxTimeoutSec
		return &blobstore.BlobStoreClient{}, nil
	})
	patches.ApplyFunc(blobstore.NewEbsClient, func(access.Config, int) (*blobstore.BlobStoreClient, error) {
		newEbsClientCalled = true
		return &blobstore.BlobStoreClient{}, nil
	})

	cli, err := s.getBlobStoreClient(1)
	require.NoError(t, err)
	require.NotNil(t, cli)
	require.False(t, newEbsClientCalled)
	require.Equal(t, wantTimeout, gotTimeout)
	require.NotNil(t, gotSdkCfg)
	require.Equal(t, "127.0.0.1:8500", gotSdkCfg.ClusterConfig.ConsulAgentAddr)
	require.Len(t, gotSdkCfg.ClusterConfig.Clusters, 1)
	require.Equal(t, "z0", gotSdkCfg.IDC)
	require.Equal(t, "test-region", gotSdkCfg.ClusterConfig.RegionMagic)
	require.Equal(t, uint32(16<<20), gotSdkCfg.MaxBlobSize)
	require.Same(t, cli, s.ebsc[1])
}

func TestGetBlobStoreClientEnableEbsSdkConsulOnly(t *testing.T) {
	ec := proto.DefaultEbsClientConfig()
	ec.ConsulAddress = "consul_address:8500"
	ec.Idc = "z0"
	ec.RegionMagic = "rm"

	s := &Super{
		ebsc:         make(map[uint8]*blobstore.BlobStoreClient),
		logpath:      t.TempDir(),
		ebsConfig:    ec,
		enableEbsSdk: true,
		poolCache: map[uint8]*proto.StoragePoolInfo{
			1: {Id: 1, ECAddr: "127.0.0.1:9999"},
		},
	}

	var gotSdkCfg *sdk.Config
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(blobstore.NewEbsClientSdk, func(cfg *sdk.Config, maxTimeoutSec int) (*blobstore.BlobStoreClient, error) {
		gotSdkCfg = cfg
		return &blobstore.BlobStoreClient{}, nil
	})

	cli, err := s.getBlobStoreClient(1)
	require.NoError(t, err)
	require.NotNil(t, cli)
	require.NotNil(t, gotSdkCfg)
	require.Equal(t, "consul_address:8500", gotSdkCfg.ClusterConfig.ConsulAgentAddr)
	require.Empty(t, gotSdkCfg.ClusterConfig.Clusters)
}

func TestGetBlobStoreClientEnableEbsSdkBuildSdkConfigError(t *testing.T) {
	ec := proto.EbsClientConfig{}
	s := &Super{
		ebsc:         make(map[uint8]*blobstore.BlobStoreClient),
		logpath:      t.TempDir(),
		ebsConfig:    ec,
		enableEbsSdk: true,
		poolCache: map[uint8]*proto.StoragePoolInfo{
			1: {Id: 1, ECAddr: ""},
		},
	}

	_, err := s.getBlobStoreClient(1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "BuildSdkConfig failed")
}

func TestNewSuperStoresEbsConfigFromMountOptions(t *testing.T) {
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
				ObjBlockSize:        4096,
				VolType:             proto.VolumeTypeHot,
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
		reflect.NewAt(dwField.Type(), unsafe.Pointer(dwField.UnsafeAddr())).Elem().Set(reflect.ValueOf(&wrapper.Wrapper{}))
		return ec, nil
	})

	custom := proto.DefaultEbsClientConfig()
	hostTry := 40
	custom.HostTryTimes = &hostTry

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
		MinReadAheadSize:                    0,
		VolStorageClass:                     proto.StorageClass_Replica_HDD,
		VolAllowedStorageClass:              []uint32{proto.StorageClass_Replica_HDD},
		EnableTransaction:                   "off",
		TrashRebuildGoroutineLimit:          1,
		TrashDeleteExpiredDirGoroutineLimit: 1,
		EbsConfig:                           custom,
		EnableEbsSdk:                        true,
	})
	require.NoError(t, err)
	require.NotNil(t, s.ebsConfig.HostTryTimes)
	require.Equal(t, 40, *s.ebsConfig.HostTryTimes)
	require.True(t, s.enableEbsSdk)
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
	registerOecTestStreamerWithLogicalView(s, f.ino, nil, &blobstore.Writer{}, 0, 1)
	require.NoError(t, s.oec.OpenStreamWithArgs(blobstore.ECStreamOpenArgs{Ino: f.ino}))

	flushed := make(chan uint64, 2)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchECStreamerFlush(patches, func(st *blobstore.ECStreamer, _ context.Context) error {
		flushed <- st.Inode()
		return nil
	})

	runScheduleFlushOnceForTest(s)

	select {
	case got := <-flushed:
		require.Equal(t, f.ino, got)
	case <-time.After(2 * time.Second):
		t.Fatal("scheduleFlush iteration did not trigger ECStreamer.Flush in time")
	}
}

func TestSuper_scheduleFlush_eachIdleTickLaunchesFlush(t *testing.T) {
	s := newTestSuperForFile()
	s.oec = blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{})
	f := &File{super: s, ino: 61, parentIno: 1, name: "idle2.dat"}
	ei := f.getOrCreateExtendInfo()
	atomic.StoreInt32(&ei.idle, BlobWriterIdleTimeoutPeriod)
	s.fslock.Lock()
	s.nodeCache[f.ino] = f
	s.fslock.Unlock()
	registerOecTestStreamerWithLogicalView(s, f.ino, nil, &blobstore.Writer{}, 0, 1)
	require.NoError(t, s.oec.OpenStreamWithArgs(blobstore.ECStreamOpenArgs{Ino: f.ino}))

	var calls int32
	started := make(chan struct{})
	unblock := make(chan struct{})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchECStreamerFlush(patches, func(_ *blobstore.ECStreamer, _ context.Context) error {
		atomic.AddInt32(&calls, 1)
		select {
		case <-started:
		default:
			close(started)
		}
		<-unblock
		return nil
	})

	runScheduleFlushOnceForTest(s)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first idle flush did not start")
	}
	atomic.StoreInt32(&ei.idle, BlobWriterIdleTimeoutPeriod)
	runScheduleFlushOnceForTest(s)
	close(unblock)
	require.Eventually(t, func() bool { return atomic.LoadInt32(&calls) == 2 }, 2*time.Second, 10*time.Millisecond)
}

func TestSuper_scheduleFlush_idleBelowThresholdIncrements(t *testing.T) {
	s := newTestSuperForFile()
	s.oec = blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{})
	f := &File{super: s, ino: 62, parentIno: 1, name: "not-idle.dat"}
	ei := f.getOrCreateExtendInfo()
	atomic.StoreInt32(&ei.idle, 0)
	s.fslock.Lock()
	s.nodeCache[f.ino] = f
	s.fslock.Unlock()
	registerOecTestStreamerWithLogicalView(s, f.ino, nil, &blobstore.Writer{}, 0, 1)
	require.NoError(t, s.oec.OpenStreamWithArgs(blobstore.ECStreamOpenArgs{Ino: f.ino}))

	var calls int32
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchECStreamerFlush(patches, func(_ *blobstore.ECStreamer, _ context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})

	runScheduleFlushOnceForTest(s)
	require.Equal(t, int32(1), atomic.LoadInt32(&ei.idle))
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))
}

func TestSuper_scheduleFlush_skips_closed_stream(t *testing.T) {
	s := newTestSuperForFile()
	s.oec = blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{})
	f := &File{super: s, ino: 63, parentIno: 1, name: "closed.dat"}
	ei := f.getOrCreateExtendInfo()
	atomic.StoreInt32(&ei.idle, BlobWriterIdleTimeoutPeriod)
	s.fslock.Lock()
	s.nodeCache[f.ino] = f
	s.fslock.Unlock()

	var calls int32
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchECStreamerFlush(patches, func(_ *blobstore.ECStreamer, _ context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})

	runScheduleFlushOnceForTest(s)
	require.Equal(t, int32(0), atomic.LoadInt32(&ei.idle))
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))
}

func TestSuper_scheduleFlush_liveTicker_scansAndSingleFlights(t *testing.T) {
	s := newTestSuperForFile()
	s.oec = blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{})

	dir := &Dir{super: s, ino: 1, name: "d"}
	noEi := &File{super: s, ino: 70, parentIno: 1, name: "no-ei.dat"}
	nilEi := &File{super: s, ino: 71, parentIno: 1, name: "nil-ei.dat"}
	s.fileExtendInfoMap[71] = nil
	warming := &File{super: s, ino: 72, parentIno: 1, name: "warm.dat"}
	warmingEi := warming.getOrCreateExtendInfo()
	atomic.StoreInt32(&warmingEi.idle, 0)
	closed := &File{super: s, ino: 73, parentIno: 1, name: "closed.dat"}
	closedEi := closed.getOrCreateExtendInfo()
	atomic.StoreInt32(&closedEi.idle, BlobWriterIdleTimeoutPeriod)
	zeroRef := &File{super: s, ino: 74, parentIno: 1, name: "zeroref.dat"}
	zeroEi := zeroRef.getOrCreateExtendInfo()
	atomic.StoreInt32(&zeroEi.idle, BlobWriterIdleTimeoutPeriod)
	ready := &File{super: s, ino: 75, parentIno: 1, name: "ready.dat"}
	readyEi := ready.getOrCreateExtendInfo()
	atomic.StoreInt32(&readyEi.idle, BlobWriterIdleTimeoutPeriod)

	s.fslock.Lock()
	s.nodeCache[1] = dir
	s.nodeCache[70] = noEi
	s.nodeCache[71] = nilEi
	s.nodeCache[72] = warming
	s.nodeCache[73] = closed
	s.nodeCache[74] = zeroRef
	s.nodeCache[75] = ready
	s.fslock.Unlock()

	registerOecTestStreamerWithLogicalView(s, 74, nil, &blobstore.Writer{}, 0, 1)
	registerOecTestStreamerWithLogicalView(s, 75, nil, &blobstore.Writer{}, 0, 1)
	require.NoError(t, s.oec.OpenStreamWithArgs(blobstore.ECStreamOpenArgs{Ino: 75}))

	var calls int32
	started := make(chan struct{})
	unblock := make(chan struct{})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchECStreamerFlush(patches, func(st *blobstore.ECStreamer, _ context.Context) error {
		require.Equal(t, uint64(75), st.Inode())
		atomic.AddInt32(&calls, 1)
		select {
		case <-started:
		default:
			close(started)
		}
		<-unblock
		return nil
	})

	fast := time.NewTicker(5 * time.Millisecond)
	defer fast.Stop()
	patches.ApplyFunc(time.NewTicker, func(time.Duration) *time.Ticker { return fast })

	go s.scheduleFlush()

	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("scheduleFlush live ticker did not flush idle writer")
	}

	require.Equal(t, int32(1), atomic.LoadInt32(&warmingEi.idle))
	require.Equal(t, int32(0), atomic.LoadInt32(&readyEi.idle))
	require.Equal(t, int32(0), atomic.LoadInt32(&closedEi.idle))
	require.Equal(t, int32(0), atomic.LoadInt32(&zeroEi.idle))
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	fast.Stop()
	close(unblock)
}

func TestSuper_scheduleFlush_skipsWhenCannotFlush(t *testing.T) {
	s := newTestSuperForFile()
	s.oec = blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{})
	f := &File{super: s, ino: 76, parentIno: 1, name: "busy.dat"}
	ei := f.getOrCreateExtendInfo()
	atomic.StoreInt32(&ei.idle, BlobWriterIdleTimeoutPeriod)
	s.fslock.Lock()
	s.nodeCache[f.ino] = f
	s.fslock.Unlock()
	registerOecTestStreamerWithLogicalView(s, f.ino, nil, &blobstore.Writer{}, 0, 1)
	require.NoError(t, s.oec.OpenStreamWithArgs(blobstore.ECStreamOpenArgs{Ino: f.ino}))

	var flushCalls, gateCalls int32
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECStreamer)(nil)), "CanFlush",
		func(_ *blobstore.ECStreamer) bool {
			atomic.AddInt32(&gateCalls, 1)
			return false
		})
	patchECStreamerFlush(patches, func(_ *blobstore.ECStreamer, _ context.Context) error {
		atomic.AddInt32(&flushCalls, 1)
		return nil
	})

	fast := time.NewTicker(5 * time.Millisecond)
	defer fast.Stop()
	patches.ApplyFunc(time.NewTicker, func(time.Duration) *time.Ticker { return fast })
	go s.scheduleFlush()

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&gateCalls) >= 1
	}, 200*time.Millisecond, 5*time.Millisecond)
	fast.Stop()
	require.Equal(t, int32(0), atomic.LoadInt32(&flushCalls))
}

func TestSuper_scheduleFlush_flushErrorDoesNotPanic(t *testing.T) {
	s := newTestSuperForFile()
	s.oec = blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{})
	f := &File{super: s, ino: 77, parentIno: 1, name: "flush-err.dat"}
	ei := f.getOrCreateExtendInfo()
	atomic.StoreInt32(&ei.idle, BlobWriterIdleTimeoutPeriod)
	s.fslock.Lock()
	s.nodeCache[f.ino] = f
	s.fslock.Unlock()
	registerOecTestStreamerWithLogicalView(s, f.ino, nil, &blobstore.Writer{}, 0, 1)
	require.NoError(t, s.oec.OpenStreamWithArgs(blobstore.ECStreamOpenArgs{Ino: f.ino}))

	done := make(chan struct{})
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchECStreamerFlush(patches, func(_ *blobstore.ECStreamer, _ context.Context) error {
		defer close(done)
		return context.Canceled
	})

	fast := time.NewTicker(5 * time.Millisecond)
	defer fast.Stop()
	patches.ApplyFunc(time.NewTicker, func(time.Duration) *time.Ticker { return fast })
	go s.scheduleFlush()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("scheduleFlush did not call Flush")
	}
	fast.Stop()
}

// runScheduleFlushOnceForTest runs one Super.scheduleFlush tick without starting the ticker.
func runScheduleFlushOnceForTest(s *Super) {
	pending := make([]uint64, 0)
	s.fslock.Lock()
	for ino, node := range s.nodeCache {
		file, ok := node.(*File)
		if !ok {
			continue
		}
		ei, ok := file.getExtendInfo()
		if !ok || ei == nil {
			continue
		}
		if atomic.LoadInt32(&ei.idle) >= BlobWriterIdleTimeoutPeriod {
			atomic.StoreInt32(&ei.idle, 0)
			pending = append(pending, ino)
		} else {
			atomic.AddInt32(&ei.idle, 1)
		}
	}
	s.fslock.Unlock()
	for _, ino := range pending {
		st := s.oec.GetStreamer(ino)
		if st == nil || s.oec.RefCnt(ino) <= 0 || !st.CanFlush() {
			continue
		}
		go func(st *blobstore.ECStreamer) {
			_ = st.Flush(context.Background())
		}(st)
	}
}

func patchECStreamerFlush(patches *gomonkey.Patches, fn func(*blobstore.ECStreamer, context.Context) error) {
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECStreamer)(nil)), "Flush", fn)
}
