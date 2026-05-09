package fs

import (
	"testing"
	"time"

	bazilfs "github.com/cubefs/cubefs/depends/bazil.org/fuse/fs"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/stretchr/testify/require"
)

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

	d.putNegativeDcache("missing")
	require.True(t, d.negativeDcacheHit("missing"))
	d.deleteNegativeDcache("missing")
	require.False(t, d.negativeDcacheHit("missing"))

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
