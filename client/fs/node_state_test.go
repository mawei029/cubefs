package fs

import (
	"testing"
	"time"

	bazilfs "github.com/cubefs/cubefs/depends/bazil.org/fuse/fs"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/stretchr/testify/require"
)

func newSuperForFileMetaTest() *Super {
	return &Super{
		ic:                NewInodeCache(time.Hour, 64, true),
		fileExtendInfoMap: make(map[uint64]*FileExtendInfo),
		ec:                stream.NewTestExtentClient(nil),
		oec:               blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{}),
	}
}

func TestCoverageDirExtendInfoStateFlow(t *testing.T) {
	s := &Super{
		ic:                    NewInodeCache(time.Minute, 16, true),
		dirExtendInfoMap:      make(map[uint64]*DirExtendInfo),
		metaCacheAcceleration: true,
	}
	s.ic.Put(&proto.InodeInfo{Inode: 1, Nlink: 2})
	d := &Dir{super: s, ino: 1}

	require.Equal(t, uint32(2), d.loadNlink())
	require.Equal(t, int64(0), d.loadOpenCnt())

	require.Equal(t, uint32(2), d.addMissCount(2))
	require.Equal(t, uint32(2), d.loadMissCount())
	d.resetMissCount()
	require.Equal(t, uint32(0), d.loadMissCount())

	d.storeLastDoing(3)
	require.Equal(t, int32(3), d.loadLastDoing())
	d.storeLastTime(100)
	require.Equal(t, int64(100), d.loadLastTime())

	d.putDcacheEntry("a", 11)
	ino, ok := d.getDcacheEntry("a")
	require.True(t, ok)
	require.Equal(t, uint64(11), ino)
	require.Equal(t, 1, d.getDcacheLen())
	d.deleteDcacheEntry("a")
	_, ok = d.getDcacheEntry("a")
	require.False(t, ok)

	d.deleteExtendInfo()
	_, ok = d.getExtendInfo()
	require.False(t, ok)
}

func TestCoverageFileExtendInfoAndStorageClass(t *testing.T) {
	s := &Super{
		ic:                NewInodeCache(time.Minute, 16, true),
		nodeCache:         make(map[uint64]bazilfs.Node),
		fileExtendInfoMap: make(map[uint64]*FileExtendInfo),
		dirExtendInfoMap:  make(map[uint64]*DirExtendInfo),
	}
	parent := &Dir{super: s, ino: 2}
	parent.setDcache(NewDentryCache(true))
	parent.putDcacheEntry("f", 3)
	s.nodeCache[2] = parent

	f := &File{super: s, ino: 3, parentIno: 2, name: "f"}
	require.Equal(t, uint32(0), f.getFlag())
	f.setFlag(7)
	require.Equal(t, uint32(7), f.getFlag())

	reader := &blobstore.Reader{}
	writer := &blobstore.Writer{}
	f.setColdBlobReaderWriter(reader, writer)
	require.Equal(t, reader, f.coldBlobReader())
	require.Equal(t, writer, f.coldBlobWriter())
	r2, w2 := f.coldBlobReaderWriter()
	require.Equal(t, reader, r2)
	require.Equal(t, writer, w2)

	f.storeIdle(1)
	f.removeParentDcacheEntry()
	_, ok := parent.getDcacheEntry("f")
	require.False(t, ok)

	s.ic.Put(&proto.InodeInfo{Inode: 3, StorageClass: proto.StorageClass_BlobStore})
	require.Equal(t, uint32(proto.StorageClass_BlobStore), f.storageClass())

	f.deleteExtendInfo()
	_, ok = f.getExtendInfo()
	require.False(t, ok)
}

func TestFile_refreshFileMeta_setsDispatchCache(t *testing.T) {
	s := newSuperForFileMetaTest()
	f := &File{super: s, ino: 100}

	f.refreshFileMeta(2, proto.StorageClass_Replica_SSD)

	ei, ok := f.getExtendInfo()
	require.True(t, ok)
	require.NotNil(t, ei)
	ei.RLock()
	require.True(t, ei.metaCached)
	require.Equal(t, uint8(2), ei.cachedPoolId)
	require.Equal(t, uint32(proto.StorageClass_Replica_SSD), ei.cachedStorageCls)
	ei.RUnlock()
}

func TestFile_refreshFileMeta_nilSuperNoop(t *testing.T) {
	f := &File{ino: 101}
	require.NotPanics(t, func() {
		f.refreshFileMeta(1, proto.StorageClass_Replica_HDD)
	})
}

func TestFile_getInfo_refreshesFileMetaCache(t *testing.T) {
	s := newSuperForFileMetaTest()
	const ino = uint64(110)
	s.ic.Put(&proto.InodeInfo{
		Inode:        ino,
		PoolId:       4,
		StorageClass: proto.StorageClass_BlobStore,
	})
	f := &File{super: s, ino: ino}

	info, err := f.getInfo()
	require.NoError(t, err)
	require.Equal(t, uint8(4), info.PoolId)

	ei, ok := f.getExtendInfo()
	require.True(t, ok)
	ei.RLock()
	require.True(t, ei.metaCached)
	require.Equal(t, uint8(4), ei.cachedPoolId)
	require.Equal(t, uint32(proto.StorageClass_BlobStore), ei.cachedStorageCls)
	ei.RUnlock()
}

func TestFile_dataPlaneMeta_cacheHit(t *testing.T) {
	s := newSuperForFileMetaTest()
	const ino = uint64(200)
	f := &File{super: s, ino: ino}

	f.refreshFileMeta(3, proto.StorageClass_Replica_HDD)
	// No ic entry: a cache hit must not depend on InodeGet.
	s.ic.Delete(ino)

	poolId, storageCls, err := f.getFileMeta()
	require.NoError(t, err)
	require.Equal(t, uint8(3), poolId)
	require.Equal(t, uint32(proto.StorageClass_Replica_HDD), storageCls)
}

func TestFile_dataPlaneMeta_cacheMiss_backfillsFromInodeCache(t *testing.T) {
	s := newSuperForFileMetaTest()
	const ino = uint64(201)
	s.ic.Put(&proto.InodeInfo{
		Inode:        ino,
		PoolId:       5,
		StorageClass: proto.StorageClass_BlobStore,
	})
	f := &File{super: s, ino: ino}

	poolId, storageCls, err := f.getFileMeta()
	require.NoError(t, err)
	require.Equal(t, uint8(5), poolId)
	require.Equal(t, uint32(proto.StorageClass_BlobStore), storageCls)

	ei, ok := f.getExtendInfo()
	require.True(t, ok)
	ei.RLock()
	require.True(t, ei.metaCached)
	require.Equal(t, uint8(5), ei.cachedPoolId)
	require.Equal(t, uint32(proto.StorageClass_BlobStore), ei.cachedStorageCls)
	ei.RUnlock()

	// Second call uses the extend-info cache without touching ic.
	s.ic.Delete(ino)
	poolId, storageCls, err = f.getFileMeta()
	require.NoError(t, err)
	require.Equal(t, uint8(5), poolId)
	require.Equal(t, uint32(proto.StorageClass_BlobStore), storageCls)
}

func TestFile_dataPlaneMeta_cacheMiss_withExtendInfoNotCached(t *testing.T) {
	s := newSuperForFileMetaTest()
	const ino = uint64(202)
	s.ic.Put(&proto.InodeInfo{
		Inode:        ino,
		PoolId:       7,
		StorageClass: proto.StorageClass_Replica_SSD,
	})
	f := &File{super: s, ino: ino}
	f.setFlag(1) // creates extend info with metaCached still false

	poolId, storageCls, err := f.getFileMeta()
	require.NoError(t, err)
	require.Equal(t, uint8(7), poolId)
	require.Equal(t, uint32(proto.StorageClass_Replica_SSD), storageCls)
}
