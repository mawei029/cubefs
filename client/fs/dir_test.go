// Copyright 2026 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package fs

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/depends/bazil.org/fuse"
	"github.com/cubefs/cubefs/depends/bazil.org/fuse/fs"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/stretchr/testify/require"
)

func newTestSuperForDir() *Super {
	return &Super{
		ic:                    NewInodeCache(time.Hour, 1024, true),
		rootIno:               1,
		nodeCache:             make(map[uint64]fs.Node),
		dirExtendInfoMap:      make(map[uint64]*DirExtendInfo),
		fileExtendInfoMap:     make(map[uint64]*FileExtendInfo),
		runningMonitor:        NewRunningMonitor(0),
		disableDcache:         false,
		metaCacheAcceleration: false,
	}
}

func TestDir_getCwd_nodeCacheMiss_immediate(t *testing.T) {
	t.Parallel()
	const rootIno uint64 = 1
	super := &Super{
		rootIno:   rootIno,
		nodeCache: make(map[uint64]fs.Node),
	}
	d := &Dir{
		super:     super,
		ino:       999,
		parentIno: 2,
		name:      "leaf",
	}
	require.Equal(t, "unknown/", d.getCwd())
}

func TestDir_getCwd_nodeCacheMiss_afterParentSegment(t *testing.T) {
	t.Parallel()
	const rootIno uint64 = 1
	super := &Super{
		rootIno:   rootIno,
		nodeCache: make(map[uint64]fs.Node),
	}
	leaf := &Dir{
		super:     super,
		ino:       100,
		parentIno: 50,
		name:      "leaf",
	}
	super.nodeCache[100] = leaf

	require.Equal(t, "unknown/leaf", leaf.getCwd())
}

func TestDir_getCwd_nodeInCacheButNotDir(t *testing.T) {
	t.Parallel()
	const rootIno uint64 = 1
	super := &Super{
		rootIno:   rootIno,
		nodeCache: make(map[uint64]fs.Node),
	}
	f := &File{
		super:     super,
		ino:       200,
		parentIno: rootIno,
		name:      "notadir",
	}
	super.nodeCache[200] = f

	d := &Dir{
		super:     super,
		ino:       200,
		parentIno: rootIno,
		name:      "x",
	}
	require.Equal(t, "unknown/", d.getCwd())
}

func TestDir_Lookup_metaCacheMissReadDirGate(t *testing.T) {
	t.Parallel()
	now := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	cooldownOk := now.Add(-6 * time.Minute).Unix()

	t.Run("triggers_when_idle", func(t *testing.T) {
		t.Parallel()
		require.True(t, dirLookupMetaCacheAccelerationGate(6, 0, now, 0))
	})

	t.Run("no_trigger_miss_count_not_above_5", func(t *testing.T) {
		t.Parallel()
		require.False(t, dirLookupMetaCacheAccelerationGate(5, 0, now, 0))
	})

	t.Run("no_trigger_while_lastDoing_set", func(t *testing.T) {
		t.Parallel()
		require.False(t, dirLookupMetaCacheAccelerationGate(6, 0, now, 1))
	})

	t.Run("no_trigger_within_5min_since_last", func(t *testing.T) {
		t.Parallel()
		recent := now.Add(-2 * time.Minute).Unix()
		require.False(t, dirLookupMetaCacheAccelerationGate(6, recent, now, 0))
	})

	t.Run("triggers_after_5min_cooldown", func(t *testing.T) {
		t.Parallel()
		require.True(t, dirLookupMetaCacheAccelerationGate(6, cooldownOk, now, 0))
	})

	t.Run("exactly_5min_since_last_triggers", func(t *testing.T) {
		t.Parallel()
		last := now.Add(-5 * time.Minute).Unix()
		require.True(t, dirLookupMetaCacheAccelerationGate(6, last, now, 0))
	})

	t.Run("just_under_5min_no_trigger", func(t *testing.T) {
		t.Parallel()
		last := now.Add(-5*time.Minute + time.Second).Unix()
		require.False(t, dirLookupMetaCacheAccelerationGate(6, last, now, 0))
	})
}

// superForDirMutationTest builds a minimal Super so Dir.* paths can run with inode
// metadata only from ic (InodeGet cache hit), without MetaWrapper / ExtentClient.
func superForDirMutationTest(t *testing.T) *Super {
	t.Helper()
	rm := NewRunningMonitor(0)
	return &Super{
		metaCacheAcceleration: true,
		volname:               "ut-vol",
		volType:               proto.VolumeTypeHot,
		rootIno:               1,
		ic:                    NewInodeCache(time.Hour, 10000, true),
		runningMonitor:        rm,
		nodeCache:             make(map[uint64]fs.Node),
		dirDirtyCache:         make(map[uint64]bool),
		dirDirtyCount:         make(map[uint64]int),
		// File paths resolve storage class via poolCache; test inode helpers use PoolId 0.
		poolCache: map[uint8]*proto.StoragePoolInfo{
			0: {Id: 0, StorageClass: uint8(proto.StorageClass_Replica_HDD)},
		},
	}
}

func dirInodeInfoForMutationTest(ino uint64) *proto.InodeInfo {
	now := time.Now()
	return &proto.InodeInfo{
		Inode:        ino,
		Mode:         uint32(os.ModeDir | 0o755),
		Nlink:        2,
		Uid:          1000,
		Gid:          1000,
		Size:         4096,
		AccessTime:   now,
		ModifyTime:   now,
		CreateTime:   now,
		Extents:      &proto.GetExtentsResponse{},
		StorageClass: proto.StorageClass_Replica_HDD,
	}
}

func fileInodeInfoForMutationTest(ino uint64) *proto.InodeInfo {
	now := time.Now()
	return &proto.InodeInfo{
		Inode:        ino,
		Mode:         uint32(0o644),
		Nlink:        1,
		Uid:          1000,
		Gid:          1000,
		Size:         0,
		AccessTime:   now,
		ModifyTime:   now,
		CreateTime:   now,
		Extents:      &proto.GetExtentsResponse{},
		StorageClass: proto.StorageClass_Replica_HDD,
	}
}

func TestDir_Setattr_metaAccel_beginEndPaired_inodeFromIcache(t *testing.T) {
	t.Parallel()
	const dirIno uint64 = 88001
	s := superForDirMutationTest(t)
	info := dirInodeInfoForMutationTest(dirIno)
	s.ic.Put(info)

	d := NewDir(s, info, 1, "utdir").(*Dir)

	req := &fuse.SetattrRequest{Header: fuse.Header{Pid: 4242}}
	resp := &fuse.SetattrResponse{}

	err := d.Setattr(context.Background(), req, resp)
	require.NoError(t, err)
	require.NotZero(t, resp.Attr.Inode)

	_, inCount := s.dirDirtyCount[dirIno]
	require.False(t, inCount, "EndDirMutation must clear count after Setattr returns")
}

func TestDir_Link_nonFileOld_returnsEPermBeforeBegin(t *testing.T) {
	t.Parallel()
	s := superForDirMutationTest(t)
	const parentIno = uint64(88010)
	srcDir := NewDir(s, dirInodeInfoForMutationTest(parentIno), 1, "p").(*Dir)
	dstDir := NewDir(s, dirInodeInfoForMutationTest(parentIno+1), 1, "q").(*Dir)

	_, err := srcDir.Link(context.Background(), &fuse.LinkRequest{
		Header:  fuse.Header{Pid: 1},
		NewName: "hard",
	}, dstDir)
	require.ErrorIs(t, err, fuse.EPERM)
	require.Empty(t, s.dirDirtyCount, "Link must reject non-*File before BeginDirMutation")
}

func TestDir_Link_nonRegularFile_returnsEPermBeforeBegin(t *testing.T) {
	t.Parallel()
	s := superForDirMutationTest(t)
	const parentIno = uint64(88020)
	srcDir := NewDir(s, dirInodeInfoForMutationTest(parentIno), 1, "p").(*Dir)

	old := NewFile(s, &proto.InodeInfo{
		Inode:        88021,
		Mode:         uint32(os.ModeSymlink | 0o777),
		Nlink:        1,
		StorageClass: proto.StorageClass_Replica_HDD,
	}, syscall.O_RDONLY, parentIno, "sym").(*File)

	_, err := srcDir.Link(context.Background(), &fuse.LinkRequest{
		Header:  fuse.Header{Pid: 1},
		NewName: "l",
	}, old)
	require.ErrorIs(t, err, fuse.EPERM)
	require.Empty(t, s.dirDirtyCount)
}

func TestDir_Mknod_rdevNonZero_returnsENOSYSBeforeBegin(t *testing.T) {
	t.Parallel()
	s := superForDirMutationTest(t)
	d := NewDir(s, dirInodeInfoForMutationTest(88030), 1, "d").(*Dir)

	_, err := d.Mknod(context.Background(), &fuse.MknodRequest{
		Header: fuse.Header{Pid: 1},
		Name:   "dev",
		Rdev:   1,
	})
	require.ErrorIs(t, err, fuse.ENOSYS)
	require.Empty(t, s.dirDirtyCount)
}

func TestDir_Rename_nonDirDst_returnsENOTSUPBeforeBegin(t *testing.T) {
	t.Parallel()
	s := superForDirMutationTest(t)
	src := NewDir(s, dirInodeInfoForMutationTest(88040), 1, "src").(*Dir)
	dstFile := NewFile(s, fileInodeInfoForMutationTest(88041), syscall.O_RDONLY, 88040, "notadir").(*File)

	err := src.Rename(context.Background(), &fuse.RenameRequest{
		Header:  fuse.Header{Pid: 1},
		OldName: "a",
		NewName: "b",
	}, dstFile)
	require.ErrorIs(t, err, fuse.ENOTSUP)
	require.Empty(t, s.dirDirtyCount)
}

func TestNewDirStoresOnlyNodeIdentity(t *testing.T) {
	t.Parallel()

	super := newTestSuperForDir()
	node := NewDir(super, &proto.InodeInfo{Inode: 100}, 1, "dir")
	dir, ok := node.(*Dir)
	require.True(t, ok)
	require.Same(t, super, dir.super)
	require.Equal(t, uint64(100), dir.ino)
	require.Equal(t, uint64(1), dir.parentIno)
	require.Equal(t, "dir", dir.name)
}

func TestDirAttrLoadsInfoFromInodeCache(t *testing.T) {
	t.Parallel()

	super := newTestSuperForDir()
	info := &proto.InodeInfo{
		Inode: 100,
		Mode:  uint32(os.ModeDir | 0o755),
		Nlink: 2,
		Uid:   11,
		Gid:   22,
	}
	super.ic.Put(info)
	dir := &Dir{super: super, ino: info.Inode, parentIno: 1, name: "dir"}

	var attr fuse.Attr
	require.NoError(t, dir.Attr(context.Background(), &attr))
	require.Equal(t, info.Inode, attr.Inode)
	require.Equal(t, info.Uid, attr.Uid)
	require.Equal(t, info.Gid, attr.Gid)
}

func TestDirOpenCreatesExtendInfoAndKeepsCacheFlag(t *testing.T) {
	t.Parallel()

	super := newTestSuperForDir()
	super.keepCache = true
	dir := &Dir{super: super, ino: 100, parentIno: 1, name: "dir"}

	resp := &fuse.OpenResponse{}
	handle, err := dir.Open(context.Background(), &fuse.OpenRequest{}, resp)
	require.NoError(t, err)
	require.Same(t, dir, handle)
	require.NotZero(t, resp.Flags&fuse.OpenKeepCache)

	ei, ok := dir.getExtendInfo()
	require.True(t, ok)
	require.NotNil(t, ei)
	require.Equal(t, int64(1), atomic.LoadInt64(&ei.openCnt))
	require.NotNil(t, ei.dctx)
	require.NotNil(t, ei.dcacheNoEnt)
}

func TestDirReleaseClearsExtendInfoWhenLastOpenClosed(t *testing.T) {
	t.Parallel()

	super := newTestSuperForDir()
	dir := &Dir{super: super, ino: 100, parentIno: 1, name: "dir"}
	ei := dir.getOrCreateExtendInfo()
	atomic.StoreInt64(&ei.openCnt, 1)
	ei.dcache = NewDentryCache(false)
	ei.dcache.Put("child", 101)
	ei.dcacheNoEnt.Put("missing")
	ei.dctx.Put(7, &DirContext{Name: "cursor"})

	require.NoError(t, dir.Release(context.Background(), &fuse.ReleaseRequest{Handle: 7}))
	_, ok := dir.getExtendInfo()
	require.False(t, ok)
}

func TestDirReleaseNormalizesNegativeOpenCount(t *testing.T) {
	t.Parallel()

	super := newTestSuperForDir()
	super.metaCacheAcceleration = true
	dir := &Dir{super: super, ino: 100, parentIno: 1, name: "dir"}
	ei := dir.getOrCreateExtendInfo()
	atomic.StoreInt64(&ei.openCnt, 0)

	require.NoError(t, dir.Release(context.Background(), nil))
	ei, ok := dir.getExtendInfo()
	require.True(t, ok)
	require.Equal(t, int64(0), atomic.LoadInt64(&ei.openCnt))
}

func TestDirForgetDropsNodeAndExtendInfo(t *testing.T) {
	t.Parallel()

	super := newTestSuperForDir()
	dir := &Dir{super: super, ino: 100, parentIno: 1, name: "dir"}
	super.nodeCache[dir.ino] = dir
	ei := dir.getOrCreateExtendInfo()
	ei.dcache = NewDentryCache(false)
	ei.dcacheNoEnt.Put("missing")
	ei.dctx.Put(7, &DirContext{Name: "cursor"})

	dir.Forget()

	_, ok := super.nodeCache[dir.ino]
	require.False(t, ok)
	_, ok = dir.getExtendInfo()
	require.False(t, ok)
}

func TestDirExtendInfoHelpers(t *testing.T) {
	t.Parallel()

	super := newTestSuperForDir()
	dir := &Dir{super: super, ino: 100, parentIno: 1, name: "dir"}

	require.Equal(t, uint32(0), dir.loadMissCount())
	require.Equal(t, uint32(1), dir.addMissCount(1))
	require.Equal(t, uint32(1), dir.loadMissCount())
	dir.resetMissCount()
	require.Equal(t, uint32(0), dir.loadMissCount())

	dir.storeLastDoing(1)
	require.Equal(t, int32(1), dir.loadLastDoing())
	dir.storeLastTime(123)
	require.Equal(t, int64(123), dir.loadLastTime())

	dir.putDirContext(9, &DirContext{Name: "next"})
	require.Equal(t, "next", dir.getDirContext(9).Name)

	dir.putDcacheEntry("child", 101)
	ino, ok := dir.getDcacheEntry("child")
	require.True(t, ok)
	require.Equal(t, uint64(101), ino)
	require.Equal(t, 1, dir.getDcacheLen())
	dir.deleteDcacheEntry("child")
	_, ok = dir.getDcacheEntry("child")
	require.False(t, ok)

	require.False(t, dir.negativeDcacheHit("missing"))
	dir.putNegativeDcache("missing")
	require.True(t, dir.negativeDcacheHit("missing"))
	dir.deleteNegativeDcache("missing")
	require.False(t, dir.negativeDcacheHit("missing"))
}

func TestDir_ForgetKeepsExtendInfoWhenOpenCountPositive(t *testing.T) {
	s := newTestSuperForDir()
	const ino uint64 = 77
	ei := &DirExtendInfo{}
	atomic.StoreInt64(&ei.openCnt, 1)
	s.dirExtendInfoMap[ino] = ei
	s.nodeCache[ino] = &Dir{super: s, ino: ino, parentIno: 1, name: "x"}

	d := &Dir{super: s, ino: ino, parentIno: 1, name: "x"}
	d.Forget()

	_, still := s.dirExtendInfoMap[ino]
	require.True(t, still, "Forget 在 openCnt>0 时应保留 DirExtendInfo，避免 Release 将计数打成负数")
	_, inNode := s.nodeCache[ino]
	require.False(t, inNode)
}

func TestDir_Create_ColdBlob_openOECStreamError(t *testing.T) {
	s := newTestSuperForDir()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	s.ebsc = map[uint8]*blobstore.BlobStoreClient{1: {}}
	d := &Dir{super: s, ino: 10, parentIno: 1, name: "dir"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "Create_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ string, _ uint32, _ uint32, _ uint32, _ []byte, _ string, _ bool, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: 89, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*File)(nil)), "openOECStream",
		func(_ *File, _ *proto.InodeInfo, _ uint32, _ uint64) error { return errors.New("create open fail") })

	req := &fuse.CreateRequest{Name: "bad", Flags: fuse.OpenFlags(syscall.O_CREAT)}
	_, _, err := d.Create(context.Background(), req, &fuse.CreateResponse{})
	require.Error(t, err)
}

func TestDir_Create_ColdBlob_openOECStream(t *testing.T) {
	s := newTestSuperForDir()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	s.ebsc = map[uint8]*blobstore.BlobStoreClient{1: {}}
	d := &Dir{super: s, ino: 10, parentIno: 1, name: "dir"}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "Create_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ string, _ uint32, _ uint32, _ uint32, _ []byte, _ string, _ bool, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: 88, PoolId: 1, StorageClass: proto.StorageClass_BlobStore, Size: 0}, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*File)(nil)), "openOECStream",
		func(_ *File, _ *proto.InodeInfo, _ uint32, _ uint64) error { return nil })

	req := &fuse.CreateRequest{Name: "newec", Flags: fuse.OpenFlags(syscall.O_CREAT | syscall.O_RDWR)}
	resp := &fuse.CreateResponse{}
	node, _, err := d.Create(context.Background(), req, resp)
	require.NoError(t, err)
	require.NotNil(t, node)
}

func TestDir_ForgetRemovesExtendInfoWhenOpenCountZero(t *testing.T) {
	s := newTestSuperForDir()
	const ino uint64 = 78
	ei := &DirExtendInfo{}
	atomic.StoreInt64(&ei.openCnt, 0)
	s.dirExtendInfoMap[ino] = ei
	s.nodeCache[ino] = &Dir{super: s, ino: ino, parentIno: 1, name: "y"}

	d := &Dir{super: s, ino: ino, parentIno: 1, name: "y"}
	d.Forget()

	_, still := s.dirExtendInfoMap[ino]
	require.False(t, still)
	_, inNode := s.nodeCache[ino]
	require.False(t, inNode)
}
