package fs

import (
	"sync"
	"sync/atomic"

	"github.com/cubefs/cubefs/depends/bazil.org/fuse"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
)

type DirExtendInfo struct {
	dcache    *DentryCache
	dctx      *DirContexts
	openCnt   int64
	missCount uint32
	lastDoing int32
	lastTime  int64
}

type FileExtendInfo struct {
	sync.RWMutex
	idle int32
	// coldBlobReader/coldBlobWriter: legacy cold path without ObjExtentClient; EC/BlobStore always uses Super.oec.
	coldBlobReader *blobstore.Reader
	coldBlobWriter *blobstore.Writer
	flag           uint32
}

func (d *Dir) getInfo() (*proto.InodeInfo, error) {
	return d.super.InodeGet(d.ino)
}

func (d *Dir) getExtendInfo() (*DirExtendInfo, bool) {
	s := d.super
	if s == nil {
		return nil, false
	}

	s.dirExtendInfoLock.RLock()
	ei, ok := s.dirExtendInfoMap[d.ino]
	s.dirExtendInfoLock.RUnlock()
	return ei, ok
}

func (d *Dir) getOrCreateExtendInfo() *DirExtendInfo {
	s := d.super
	if s == nil {
		return nil
	}

	s.dirExtendInfoLock.RLock()
	ei, ok := s.dirExtendInfoMap[d.ino]
	s.dirExtendInfoLock.RUnlock()
	if ok && ei != nil {
		return ei
	}

	s.dirExtendInfoLock.Lock()
	defer s.dirExtendInfoLock.Unlock()
	if s.dirExtendInfoMap == nil {
		s.dirExtendInfoMap = make(map[uint64]*DirExtendInfo)
	}
	ei, ok = s.dirExtendInfoMap[d.ino]
	if ok && ei != nil {
		return ei
	}

	ei = &DirExtendInfo{
		dctx: NewDirContexts(),
	}
	s.dirExtendInfoMap[d.ino] = ei
	return ei
}

func (d *Dir) deleteExtendInfo() {
	s := d.super
	if s == nil {
		return
	}
	s.dirExtendInfoLock.Lock()
	delete(s.dirExtendInfoMap, d.ino)
	s.dirExtendInfoLock.Unlock()
}

func (d *Dir) loadNlink() uint32 {
	info, err := d.getInfo()
	if err != nil || info == nil {
		return 0
	}
	return info.Nlink
}

func (d *Dir) loadOpenCnt() int64 {
	ei, ok := d.getExtendInfo()
	if !ok || ei == nil {
		return 0
	}
	return atomic.LoadInt64(&ei.openCnt)
}

func (d *Dir) addMissCount(delta uint32) uint32 {
	ei := d.getOrCreateExtendInfo()
	if ei == nil {
		return 0
	}
	return atomic.AddUint32(&ei.missCount, delta)
}

func (d *Dir) loadMissCount() uint32 {
	ei, ok := d.getExtendInfo()
	if !ok || ei == nil {
		return 0
	}
	return atomic.LoadUint32(&ei.missCount)
}

func (d *Dir) resetMissCount() {
	ei := d.getExtendInfoOrNil()
	if ei == nil {
		return
	}
	atomic.StoreUint32(&ei.missCount, 0)
}

func (d *Dir) loadLastDoing() int32 {
	ei, ok := d.getExtendInfo()
	if !ok || ei == nil {
		return 0
	}
	return atomic.LoadInt32(&ei.lastDoing)
}

func (d *Dir) storeLastDoing(v int32) {
	ei := d.getOrCreateExtendInfo()
	if ei == nil {
		return
	}
	atomic.StoreInt32(&ei.lastDoing, v)
}

func (d *Dir) loadLastTime() int64 {
	ei, ok := d.getExtendInfo()
	if !ok || ei == nil {
		return 0
	}
	return atomic.LoadInt64(&ei.lastTime)
}

func (d *Dir) storeLastTime(v int64) {
	ei := d.getOrCreateExtendInfo()
	if ei == nil {
		return
	}
	atomic.StoreInt64(&ei.lastTime, v)
}

func (d *Dir) getExtendInfoOrNil() *DirExtendInfo {
	ei, ok := d.getExtendInfo()
	if !ok {
		return nil
	}
	return ei
}

func (d *Dir) getDirContext(handle fuse.HandleID) DirContext {
	ei := d.getOrCreateExtendInfo()
	if ei == nil || ei.dctx == nil {
		return DirContext{}
	}
	return ei.dctx.GetCopy(handle)
}

func (d *Dir) putDirContext(handle fuse.HandleID, dirCtx *DirContext) {
	ei := d.getOrCreateExtendInfo()
	if ei == nil || ei.dctx == nil {
		return
	}
	ei.dctx.Put(handle, dirCtx)
}

func (d *Dir) getDcache() *DentryCache {
	ei, ok := d.getExtendInfo()
	if !ok || ei == nil {
		return nil
	}
	return ei.dcache
}

func (d *Dir) setDcache(dcache *DentryCache) {
	ei := d.getOrCreateExtendInfo()
	if ei == nil {
		return
	}
	ei.dcache = dcache
}

func (d *Dir) getDcacheLen() int {
	dcache := d.getDcache()
	if dcache == nil {
		return 0
	}
	return dcache.Len()
}

func (d *Dir) getDcacheEntry(name string) (uint64, bool) {
	dcache := d.getDcache()
	if dcache == nil {
		return 0, false
	}
	return dcache.Get(name)
}

func (d *Dir) putDcacheEntry(name string, ino uint64) {
	dcache := d.getOrCreateDcache(d.super.metaCacheAcceleration)
	if dcache == nil {
		return
	}
	dcache.Put(name, ino)
}

func (d *Dir) deleteDcacheEntry(name string) {
	dcache := d.getDcache()
	if dcache == nil {
		return
	}
	dcache.Delete(name)
}

func (d *Dir) getOrCreateDcache(acceleration bool) *DentryCache {
	if d.super.disableDcache {
		return nil
	}
	ei := d.getOrCreateExtendInfo()
	if ei == nil {
		return nil
	}
	if ei.dcache == nil {
		ei.dcache = NewDentryCache(acceleration)
	}
	return ei.dcache
}

func (f *File) getInfo() (*proto.InodeInfo, error) {
	return f.super.InodeGet(f.ino)
}

func (f *File) getExtendInfo() (*FileExtendInfo, bool) {
	s := f.super
	if s == nil {
		return nil, false
	}

	s.fileExtendInfoLock.RLock()
	ei, ok := s.fileExtendInfoMap[f.ino]
	s.fileExtendInfoLock.RUnlock()
	return ei, ok
}

func (f *File) getOrCreateExtendInfo() *FileExtendInfo {
	s := f.super
	if s == nil {
		return nil
	}

	s.fileExtendInfoLock.RLock()
	ei, ok := s.fileExtendInfoMap[f.ino]
	s.fileExtendInfoLock.RUnlock()
	if ok && ei != nil {
		return ei
	}

	s.fileExtendInfoLock.Lock()
	defer s.fileExtendInfoLock.Unlock()
	if s.fileExtendInfoMap == nil {
		s.fileExtendInfoMap = make(map[uint64]*FileExtendInfo)
	}
	ei, ok = s.fileExtendInfoMap[f.ino]
	if ok && ei != nil {
		return ei
	}

	ei = &FileExtendInfo{}
	s.fileExtendInfoMap[f.ino] = ei
	return ei
}

func (f *File) deleteExtendInfo() {
	s := f.super
	if s == nil {
		return
	}
	s.fileExtendInfoLock.Lock()
	delete(s.fileExtendInfoMap, f.ino)
	s.fileExtendInfoLock.Unlock()
}

func (f *File) getFlag() uint32 {
	if ei, ok := f.getExtendInfo(); ok && ei != nil {
		return ei.flag
	}
	return 0
}

func (f *File) setFlag(flag uint32) {
	ei := f.getOrCreateExtendInfo()
	if ei == nil {
		return
	}
	ei.flag = flag
}

func (f *File) coldBlobReaderWriter() (*blobstore.Reader, *blobstore.Writer) {
	ei, ok := f.getExtendInfo()
	if !ok || ei == nil {
		return nil, nil
	}
	ei.RLock()
	defer ei.RUnlock()
	return ei.coldBlobReader, ei.coldBlobWriter
}

func (f *File) coldBlobReader() *blobstore.Reader {
	r, _ := f.coldBlobReaderWriter()
	return r
}

func (f *File) coldBlobWriter() *blobstore.Writer {
	_, w := f.coldBlobReaderWriter()
	return w
}

func (f *File) setColdBlobReaderWriter(reader *blobstore.Reader, writer *blobstore.Writer) {
	ei := f.getOrCreateExtendInfo()
	if ei == nil {
		return
	}
	ei.Lock()
	ei.coldBlobReader = reader
	ei.coldBlobWriter = writer
	ei.Unlock()
}

func (f *File) storeIdle(v int32) {
	ei := f.getOrCreateExtendInfo()
	if ei == nil {
		return
	}
	atomic.StoreInt32(&ei.idle, v)
}

func (f *File) removeParentDcacheEntry() {
	f.super.fslock.Lock()
	node, ok := f.super.nodeCache[f.parentIno]
	f.super.fslock.Unlock()
	if !ok {
		return
	}
	parent, ok := node.(*Dir)
	if !ok {
		return
	}
	parent.deleteDcacheEntry(f.name)
}

// storageClass returns inode storage class from icache or InodeGet via getInfo.
// Do not call while holding s.fslock (InodeGet may lock fslock again — scheduleFlush deadlock).
// Otherwise self-deadlock with InodeGet (historical scheduleFlush issue).
func (f *File) storageClass() uint32 {
	if info := f.super.ic.Get(f.ino); info != nil {
		return info.StorageClass
	}
	info, err := f.getInfo()
	if err != nil || info == nil {
		return 0
	}
	return info.StorageClass
}
