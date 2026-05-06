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
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubefs/cubefs/depends/bazil.org/fuse"
	"github.com/cubefs/cubefs/depends/bazil.org/fuse/fs"
	"github.com/cubefs/cubefs/proto"
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
