// Copyright 2026 The CubeFS Authors.
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

package flashnode

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/remotecache/flashnode/cachengine"
	"github.com/cubefs/cubefs/util"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func newFlashNodeWithDiskEngine(t *testing.T, diskSpaces ...int64) (*FlashNode, []*cachengine.Disk) {
	t.Helper()
	disks := make([]*cachengine.Disk, 0, len(diskSpaces))
	for _, space := range diskSpaces {
		dir := t.TempDir()
		disks = append(disks, &cachengine.Disk{
			Path:       dir,
			TotalSpace: space,
			Capacity:   100,
			Status:     proto.ReadWrite,
		})
	}
	ce, err := cachengine.NewCacheEngine("", 0, cachengine.DefaultCacheMaxUsedRatio, disks, 100, 500, 0, 1, 1, nil, cachengine.DefaultExpireTime, nil, false, "", 1024, 100, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ce.Stop()) })

	f := &FlashNode{
		cacheEngine:   ce,
		disks:         disks,
		lruCapacity:   100,
		lruFhCapacity: 500,
		readRps:       1000,
		readLimiter:   rate.NewLimiter(1000, 2000),
		limitWrite:    util.NewIOLimiterEx(0, 1, 100, _defaultFlashLimitHangTimeout),
		limitRead:     util.NewIOLimiterEx(0, 1, 100, _defaultFlashLimitHangTimeout),
	}
	return f, disks
}

func TestSetLruCapacity(t *testing.T) {
	t.Run("noopWhenInvalid", func(t *testing.T) {
		f, _ := newFlashNodeWithDiskEngine(t, util.MB)
		f.setLruCapacity(0)
		f.setLruCapacity(-1)
		f.cacheEngine = nil
		f.setLruCapacity(1000)
	})

	t.Run("noopWhenUnchanged", func(t *testing.T) {
		f, _ := newFlashNodeWithDiskEngine(t, util.MB)
		f.lruCapacity = 500
		f.setLruCapacity(500)
	})

	t.Run("proportionalMultiDisk", func(t *testing.T) {
		f, disks := newFlashNodeWithDiskEngine(t, 100, 300)
		f.setLruCapacity(1000)

		require.Equal(t, 1000, f.lruCapacity)
		require.Equal(t, 250, disks[0].Capacity)
		require.Equal(t, 750, disks[1].Capacity)

		stats := f.cacheEngine.Status()
		require.Len(t, stats, 2)
		capSum := 0
		for _, st := range stats {
			capSum += st.Capacity
		}
		require.Equal(t, 1000, capSum)
	})

	t.Run("tinyDiskGetsAtLeastOne", func(t *testing.T) {
		f, disks := newFlashNodeWithDiskEngine(t, 1, 1_000_000_000)
		f.setLruCapacity(500)
		require.GreaterOrEqual(t, disks[0].Capacity, 1)
		sum := disks[0].Capacity + disks[1].Capacity
		require.Equal(t, 500, sum)
	})
}

func TestSplitLruCapacityByDiskSpace(t *testing.T) {
	t.Run("sumEqualsTotal", func(t *testing.T) {
		spaces := []int64{100, 300, 400}
		caps := splitLruCapacityByDiskSpace(1000, spaces)
		require.Len(t, caps, 3)
		require.Equal(t, 1000, caps[0]+caps[1]+caps[2])
		require.Equal(t, 125, caps[0])
		require.Equal(t, 375, caps[1])
		require.Equal(t, 500, caps[2])
	})

	t.Run("eachAtLeastOneWhenTotalGteDiskCount", func(t *testing.T) {
		spaces := []int64{1, 1, 1}
		caps := splitLruCapacityByDiskSpace(10, spaces)
		require.Len(t, caps, 3)
		for _, c := range caps {
			require.GreaterOrEqual(t, c, 1)
		}
		require.Equal(t, 10, caps[0]+caps[1]+caps[2])
	})

	t.Run("smallDiskNotZero", func(t *testing.T) {
		caps := splitLruCapacityByDiskSpace(100, []int64{1, 99})
		require.Equal(t, 1, caps[0])
		require.Equal(t, 99, caps[1])
	})

	t.Run("truncationGoesToLastDisk", func(t *testing.T) {
		caps := splitLruCapacityByDiskSpace(7, []int64{1, 1, 1})
		require.Equal(t, 7, caps[0]+caps[1]+caps[2])
		require.Equal(t, 3, caps[2])
	})
}

func TestSetLruFhCapacity(t *testing.T) {
	f, _ := newFlashNodeWithDiskEngine(t, util.MB)

	f.setLruFhCapacity(0)
	f.setLruFhCapacity(1000000)
	require.Equal(t, 500, f.lruFhCapacity)

	f.setLruFhCapacity(2000)
	require.Equal(t, 2000, f.lruFhCapacity)
}

func TestOpFlashNodeHeartbeatAppliesLruConfig(t *testing.T) {
	f, _ := newFlashNodeWithDiskEngine(t, util.MB)

	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	go func() {
		buf := make([]byte, 8192)
		_, _ = srv.Read(buf)
	}()

	req := &proto.HeartBeatRequest{}
	req.FlashNodeLruCapacity = 800
	req.FlashNodeLruFhCapacity = 2000
	req.FlashNodeReadRps = 5000
	data, err := json.Marshal(&proto.AdminTask{Request: req})
	require.NoError(t, err)

	p := proto.NewPacket()
	p.Data = data
	require.NoError(t, f.opFlashNodeHeartbeat(cli, p))

	require.Equal(t, 800, f.lruCapacity)
	require.Equal(t, 2000, f.lruFhCapacity)
	require.Equal(t, 5000, f.readRps)
}
