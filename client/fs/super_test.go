package fs

import (
	"reflect"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	datawrapper "github.com/cubefs/cubefs/sdk/data/wrapper"
	masterSDK "github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/sdk/meta"
)

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
