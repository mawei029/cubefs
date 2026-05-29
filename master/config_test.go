// Copyright 2026 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package master

import (
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/proto"
)

func TestNewClusterConfig_defaults(t *testing.T) {
	t.Parallel()
	cfg := newClusterConfig()
	require.NotNil(t, cfg)
	require.Equal(t, defaultNumberOfDataPartitionsToLoad, cfg.numberOfDataPartitionsToLoad)
	require.EqualValues(t, defaultMetaPartitionTimeOutSec, cfg.MetaPartitionTimeOutSec)
	require.EqualValues(t, defaultMPLearnerNum, cfg.MaxMPLearnerNum)
	require.EqualValues(t, defaultFollowerReadLeaseTime, cfg.FollowerReadLeaseTime)
	require.EqualValues(t, proto.DefaultFollowerReadLeaseTimeSec, cfg.FollowerReadLeaseTime)
	require.True(t, cfg.EnableLeaderMetricsReset)
	require.Equal(t, defaultFlashNodeHandleReadTimeout, cfg.flashNodeHandleReadTimeout)
	require.Equal(t, defaultDpLimitSsdBaseCount, cfg.DpLimitSsdBaseCount)
	require.Equal(t, defaultDpLimitHddFactor, cfg.DpLimitHddFactor)
	require.Nil(t, cfg.peers)
	require.Nil(t, cfg.peerAddrs)
}

func TestClusterValue_carriesDelTreeMaxItemLimit(t *testing.T) {
	t.Parallel()
	cv := clusterValue{
		MetaNodeDelTreeMaxItemLimit: 150_000,
	}
	require.Equal(t, uint64(150_000), cv.MetaNodeDelTreeMaxItemLimit)
}

func TestUpdateMetaNodeDelTreeMaxItemLimitPolicy(t *testing.T) {
	t.Parallel()
	c := &Cluster{cfg: &clusterConfig{}}
	c.updateMetaNodeDelTreeMaxItemLimit(250_000)
	require.Equal(t, uint64(250_000), atomic.LoadUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit))
	c.updateMetaNodeDelTreeMaxItemLimit(10)
	require.Equal(t, proto.MinDelTreeMaxItemLimit, atomic.LoadUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit))
	c.updateMetaNodeDelTreeMaxItemLimit(0)
	require.Equal(t, uint64(0), atomic.LoadUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit))
	c.updateMetaNodeDelTreeMaxItemLimit(50)
	require.Equal(t, proto.MinDelTreeMaxItemLimit, atomic.LoadUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit))
}

func TestSetMetaNodeDelTreeMaxItemLimit_success(t *testing.T) {
	if server == nil || server.cluster == nil {
		t.Skip("master test server not initialized")
	}
	c := server.cluster
	oldVal := atomic.LoadUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit)
	t.Cleanup(func() {
		require.NoError(t, c.setMetaNodeDelTreeMaxItemLimit(oldVal))
	})
	require.NoError(t, c.setMetaNodeDelTreeMaxItemLimit(300_000))
	require.Equal(t, uint64(300_000), atomic.LoadUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit))
}

func TestSetMetaNodeDelTreeMaxItemLimit_rollbackOnSyncError(t *testing.T) {
	c := &Cluster{cfg: &clusterConfig{}}
	atomic.StoreUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit, 200_000)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(&Cluster{}), "syncPutCluster",
		func(_ *Cluster) error { return errors.New("sync put failed") })

	err := c.setMetaNodeDelTreeMaxItemLimit(300_000)
	require.ErrorIs(t, err, proto.ErrPersistenceByRaft)
	require.Equal(t, uint64(200_000), atomic.LoadUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit))
}

func TestSetMetaNodeDelTreeMaxItemLimit_rejectsBelowMin(t *testing.T) {
	t.Parallel()
	c := &Cluster{cfg: &clusterConfig{}}
	atomic.StoreUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit, 200_000)

	err := c.setMetaNodeDelTreeMaxItemLimit(50)
	require.Error(t, err)
	require.Contains(t, err.Error(), "set value 50 below min")
	require.Equal(t, uint64(200_000), atomic.LoadUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit))

	err = c.setMetaNodeDelTreeMaxItemLimit(50_000)
	require.Error(t, err)
	require.Contains(t, err.Error(), "set value 50000 below min")
	require.Equal(t, uint64(200_000), atomic.LoadUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit))
}

func TestSetMetaNodeDelTreeMaxItemLimit_disableWithZero(t *testing.T) {
	t.Parallel()
	c := &Cluster{cfg: &clusterConfig{}}
	atomic.StoreUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit, 200_000)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(&Cluster{}), "syncPutCluster",
		func(_ *Cluster) error { return nil })

	require.NoError(t, c.setMetaNodeDelTreeMaxItemLimit(0))
	require.Equal(t, uint64(0), atomic.LoadUint64(&c.cfg.MetaNodeDelTreeMaxItemLimit))
}

func TestSetMetaNodeDeleteWorkerSleepMs_success(t *testing.T) {
	if server == nil || server.cluster == nil {
		t.Skip("master test server not initialized")
	}
	c := server.cluster
	oldVal := atomic.LoadUint64(&c.cfg.MetaNodeDeleteWorkerSleepMs)
	t.Cleanup(func() {
		require.NoError(t, c.setMetaNodeDeleteWorkerSleepMs(oldVal))
	})
	require.NoError(t, c.setMetaNodeDeleteWorkerSleepMs(1234))
	require.Equal(t, uint64(1234), atomic.LoadUint64(&c.cfg.MetaNodeDeleteWorkerSleepMs))
}

func TestSetMetaNodeDeleteWorkerSleepMs_rollbackOnSyncError(t *testing.T) {
	c := &Cluster{cfg: &clusterConfig{}}
	atomic.StoreUint64(&c.cfg.MetaNodeDeleteWorkerSleepMs, 50)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(&Cluster{}), "syncPutCluster",
		func(_ *Cluster) error { return errors.New("sync put failed") })

	err := c.setMetaNodeDeleteWorkerSleepMs(80)
	require.ErrorIs(t, err, proto.ErrPersistenceByRaft)
	require.Equal(t, uint64(50), atomic.LoadUint64(&c.cfg.MetaNodeDeleteWorkerSleepMs))
}

func TestParsePeerAddr(t *testing.T) {
	t.Parallel()
	t.Run("ok", func(t *testing.T) {
		t.Parallel()
		id, ip, port, err := parsePeerAddr("1:192.168.1.1:8080")
		require.NoError(t, err)
		require.EqualValues(t, 1, id)
		require.Equal(t, "192.168.1.1", ip)
		require.EqualValues(t, 8080, port)
	})

	t.Run("invalid_id", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := parsePeerAddr("not-a-number:192.168.1.1:8080")
		require.Error(t, err)
	})

	t.Run("invalid_port", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := parsePeerAddr("1:192.168.1.1:not-a-port")
		require.Error(t, err)
	})
}

func TestUpdateFollowerReadLeaseTime(t *testing.T) {
	t.Parallel()
	c := &Cluster{cfg: newClusterConfig()}
	require.EqualValues(t, proto.DefaultFollowerReadLeaseTimeSec, c.cfg.FollowerReadLeaseTime)

	c.updateFollowerReadLeaseTime(0)
	require.EqualValues(t, proto.DefaultFollowerReadLeaseTimeSec, c.cfg.FollowerReadLeaseTime)

	c.updateFollowerReadLeaseTime(1800)
	require.EqualValues(t, 1800, c.cfg.FollowerReadLeaseTime)

	c.updateFollowerReadLeaseTime(proto.MaxFollowerReadLeaseTimeSec + 100)
	require.EqualValues(t, proto.MaxFollowerReadLeaseTimeSec, c.cfg.FollowerReadLeaseTime)

	c.updateFollowerReadLeaseTime(1)
	require.EqualValues(t, proto.MinFollowerReadLeaseTimeSec, c.cfg.FollowerReadLeaseTime)
}

func TestSetMetaNodeFollowerReadLeaseTime_validate(t *testing.T) {
	t.Parallel()
	c := &Cluster{cfg: newClusterConfig()}
	old := c.cfg.FollowerReadLeaseTime

	require.Error(t, c.setMetaNodeFollowerReadLeaseTime(0))
	require.EqualValues(t, old, c.cfg.FollowerReadLeaseTime)

	require.Error(t, c.setMetaNodeFollowerReadLeaseTime(proto.MaxFollowerReadLeaseTimeSec+1))
	require.EqualValues(t, old, c.cfg.FollowerReadLeaseTime)

	require.NoError(t, proto.ValidateFollowerReadLeaseTime(proto.DefaultFollowerReadLeaseTimeSec))
	require.NoError(t, proto.ValidateFollowerReadLeaseTime(proto.MaxFollowerReadLeaseTimeSec))
}

func TestClusterConfig_parsePeers(t *testing.T) {
	t.Parallel()
	t.Run("ok_single", func(t *testing.T) {
		t.Parallel()
		cfg := newClusterConfig()
		cfg.heartbeatPort = 5901
		cfg.replicaPort = 5902
		const peerLine = "7:10.0.0.7:17000"
		err := cfg.parsePeers(peerLine)
		require.NoError(t, err)
		require.Equal(t, []string{peerLine}, cfg.peerAddrs)
		require.Len(t, cfg.peers, 1)
		require.EqualValues(t, 7, cfg.peers[0].ID)
		require.Equal(t, "10.0.0.7", cfg.peers[0].Address)
		require.Equal(t, 5901, cfg.peers[0].HeartbeatPort)
		require.Equal(t, 5902, cfg.peers[0].ReplicaPort)
		require.Equal(t, "10.0.0.7:17000", AddrDatabase[7])
		delete(AddrDatabase, 7)
	})

	t.Run("ok_multiple", func(t *testing.T) {
		t.Parallel()
		cfg := newClusterConfig()
		cfg.heartbeatPort = 1
		cfg.replicaPort = 2
		line := "101:10.1.1.1:9101,102:10.1.1.2:9102"
		err := cfg.parsePeers(line)
		require.NoError(t, err)
		require.Len(t, cfg.peers, 2)
		require.Equal(t, "10.1.1.2:9102", AddrDatabase[102])
		delete(AddrDatabase, 101)
		delete(AddrDatabase, 102)
	})

	t.Run("err_bad_peer", func(t *testing.T) {
		t.Parallel()
		cfg := newClusterConfig()
		cfg.heartbeatPort = 1
		cfg.replicaPort = 2
		err := cfg.parsePeers("1:10.0.0.1:9000,bad-peer")
		require.Error(t, err)
	})
}

func TestClusterConfig_checkRaftPartitionCanUseDifferentPort(t *testing.T) {
	if server == nil || server.rocksDBStore == nil {
		t.Skip("no master server / rocksdb in this test run")
	}
	cfg := server.cluster.cfg
	before := cfg.raftPartitionCanUseDifferentPort.Load()
	t.Cleanup(func() {
		_ = cfg.checkRaftPartitionCanUseDifferentPort(server, before)
	})
	// Enable different port (idempotent when already on); avoids disable path that can fail if persisted cluster forbids it.
	require.NoError(t, cfg.checkRaftPartitionCanUseDifferentPort(server, true))
}
