// Copyright 2018 The CubeFS Authors.
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

package fs

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/depends/bazil.org/fuse"
	"github.com/cubefs/cubefs/depends/bazil.org/fuse/fs"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/stat"
)

// File defines the structure of a file.
type File struct {
	super     *Super
	ino       uint64
	parentIno uint64
	name      string
}

// Functions that File needs to implement
var (
	_ fs.Node              = (*File)(nil)
	_ fs.Handle            = (*File)(nil)
	_ fs.NodeForgetter     = (*File)(nil)
	_ fs.NodeOpener        = (*File)(nil)
	_ fs.HandleReleaser    = (*File)(nil)
	_ fs.HandleReader      = (*File)(nil)
	_ fs.HandleWriter      = (*File)(nil)
	_ fs.HandleFlusher     = (*File)(nil)
	_ fs.NodeFsyncer       = (*File)(nil)
	_ fs.NodeSetattrer     = (*File)(nil)
	_ fs.NodeReadlinker    = (*File)(nil)
	_ fs.NodeGetxattrer    = (*File)(nil)
	_ fs.NodeListxattrer   = (*File)(nil)
	_ fs.NodeSetxattrer    = (*File)(nil)
	_ fs.NodeRemovexattrer = (*File)(nil)
)

func isWriteEio(err error) bool {
	if err == syscall.EOPNOTSUPP || err == syscall.ENOTSUP || strings.Contains(err.Error(), syscall.ENOENT.Error()) {
		return false
	}

	if err == syscall.EBADF || err == syscall.EDQUOT {
		return false
	}

	if strings.Contains(err.Error(), "stream writer in error status") {
		return false
	}

	return true
}

func isReadEio(err error) bool {
	if err == syscall.EOPNOTSUPP || err == syscall.ENOTSUP || strings.Contains(err.Error(), "ExtentNotFoundError") || strings.Contains(err.Error(), syscall.ENOENT.Error()) {
		return false
	}

	if err == syscall.EBADF {
		return false
	}

	return true
}

// getStorageClassByPoolIdFromSuper returns storage class based on pool ID using Super's pool cache
// Otherwise, returns the storage class corresponding to the pool ID from pool cache
func getStorageClassByPoolIdFromSuper(s *Super, poolId uint8) *proto.StoragePoolInfo {
	pool, _ := s.getPoolInfo(poolId)
	return pool
}

// getStorageClassByPoolId returns storage class based on pool ID
// Otherwise, returns the storage class corresponding to the pool ID from pool cache
func (f *File) getStorageClassByPoolId(poolId uint8) *proto.StoragePoolInfo {
	return getStorageClassByPoolIdFromSuper(f.super, poolId)
}

// buildECStreamOpenArgs builds ECStreamOpenArgs for openOECStream/OpenStreamWithArgs (oec LimitManager).
func (f *File) buildECStreamOpenArgs(info *proto.InodeInfo, openFlags uint32, fileSize uint64) (blobstore.ECStreamOpenArgs, error) {
	ebsc, err := f.super.getBlobStoreClient(info.PoolId)
	if err != nil {
		return blobstore.ECStreamOpenArgs{}, err
	}
	aheadEn, aheadMin, aheadTotalMem := f.super.BlobStoreAheadReadForReader()
	return blobstore.ECStreamOpenArgs{
		Ino:              f.ino,
		PoolId:           info.PoolId,
		FileSize:         fileSize,
		InodeGeneration:  info.Generation,
		OpenFlags:        openFlags,
		VolName:          f.super.volname,
		VolType:          f.super.volType,
		BlockSize:        f.super.EbsBlockSize,
		Ebsc:             ebsc,
		Bc:               f.super.bc,
		Mw:               f.super.mw,
		EnableBcache:     f.super.enableBcache,
		WConcurrency:     f.super.writeThreads,
		ReadConcurrency:  f.super.readThreads,
		LimitManager:     f.super.oec.LimitManager,
		AheadReadEnable:  aheadEn,
		MinReadAheadSize: aheadMin,
		PrefetchTotalMem: aheadTotalMem,
	}, nil
}

// openOECStream opens or bumps Blob/EC stream ref via OpenStreamWithArgs (refCnt++; CloseStream resets once for meta refresh).
func (f *File) openOECStream(info *proto.InodeInfo, openFlags uint32, logicalSize uint64) error {
	args, err := f.buildECStreamOpenArgs(info, openFlags, logicalSize)
	if err != nil {
		return err
	}
	return f.super.oec.OpenStreamWithArgs(args)
}

// NewFile returns a new file.
func NewFile(s *Super, i *proto.InodeInfo, flag uint32, pino uint64, filename string) fs.Node {
	f := &File{
		super:     s,
		ino:       i.Inode,
		parentIno: pino,
		name:      filename,
	}
	f.setFlag(flag)
	// Get storage class from poolId if available, otherwise use existing StorageClass
	if proto.IsStorageClassBlobStore(i.StorageClass) {
		// Blob/EC: Reader/Writer attached on Open/Create via oec; here only store open flags.
		f.setFlag(flag)
		log.LogDebugf("Trace NewFile: blob ino(%v) flag(%v) (rw via oec on Open/Create)", i.Inode, flag)
		return f
	}
	log.LogDebugf("Trace NewFile:ino(%v) flag(%v) ", i, flag)
	return f
}

// get file parentPath
func (f *File) getParentPath() string {
	if f.parentIno == f.super.rootIno {
		return "/"
	}

	f.super.fslock.Lock()
	node, ok := f.super.nodeCache[f.parentIno]
	f.super.fslock.Unlock()
	if !ok {
		log.LogWarnf("Get node cache failed: ino(%v)", f.parentIno)
		return "unknown"
	}
	parentDir, ok := node.(*Dir)
	if !ok {
		log.LogErrorf("Type error: Can not convert node -> *Dir, ino(%v)", f.parentIno)
		return "unknown"
	}
	return parentDir.getCwd()
}

// Attr sets the attributes of a file.
func (f *File) Attr(ctx context.Context, a *fuse.Attr) error {
	var err error
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("Attr", err, bgTime, 1)
	}()

	ino := f.ino
	info, err := f.super.InodeGet(ino)
	if err != nil {
		log.LogErrorf("Attr: ino(%v) err(%v)", ino, err)
		return ParseError(err)
	}

	fillAttr(info, a)
	a.ParentIno = f.parentIno

	log.LogDebugf("Attr: ino(%v) inode.size(%v) inode.gen(%v)", ino, info.Size, info.Generation)
	// fstat size: replica and EC/Blob merge stream gen vs inode gen (gen>=inode.Generation); see replica_volume_read_and_fstat_flow_zh.md.
	if !proto.IsSymlink(info.Mode) {
		fileSize, gen := f.fileSizeVersion2(ino)
		log.LogDebugf("Attr: stream fileSize(%v) stream.gen(%v)", fileSize, gen)
		if gen >= info.Generation {
			a.Size = uint64(fileSize)
		}
	}
	if proto.IsSymlink(info.Mode) {
		a.Size = uint64(len(info.Target))
	}
	log.LogDebugf("TRACE Attr: inode(%v) attr(%v)", info, a)
	return nil
}

// Forget evicts the inode of the current file. This can only happen when the inode is on the orphan list.
func (f *File) Forget() {
	var err error
	bgTime := stat.BeginStat()

	ino := f.ino
	defer func() {
		stat.EndStat("Forget:file", err, bgTime, 1)
		log.LogDebugf("TRACE Forget: ino(%v) %v", ino, f.name)
	}()

	// TODO:why cannot close fwriter
	// log.LogErrorf("TRACE Forget: ino(%v)", ino)
	//	f.fWriter.Close()
	// }

	if DisableMetaCache {
		f.super.ic.Delete(ino)
		f.super.fslock.Lock()
		delete(f.super.nodeCache, ino)
		f.super.fslock.Unlock()
		fullPath := f.getParentPath() + f.name
		if proto.DataPlaneUsesBlobEC(f.super.volType, f.storageClass()) {
			// Evict oec only on cold/Blob; do not call ec.EvictStream.
			if err := f.super.oec.EvictStream(ino); err != nil {
				log.LogWarnf("Forget: oec EvictStream not ready, ino(%v) path(%v) err(%v)", ino, fullPath, err)
				return
			}
		} else {
			if err := f.super.ec.EvictStream(ino); err != nil {
				log.LogWarnf("Forget: stream not ready to evict, ino(%v) err(%v)", ino, err)
				return
			}
		}
	}

	if !f.super.orphan.Evict(ino) {
		return
	}
	fullPath := f.getParentPath() + f.name
	if err := f.super.mw.Evict(ino, fullPath, false); err != nil {
		log.LogWarnf("Forget Evict: ino(%v) err(%v)", ino, err)
	}
}

// Open handles the open request.
func (f *File) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (handle fs.Handle, err error) {
	bgTime := stat.BeginStat()
	var needBCache bool

	runningStat := f.super.runningMonitor.AddClientOp("fileopen", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Open", err, bgTime, 1)
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	ino := f.ino
	info, err := f.getInfo()
	if err != nil {
		return nil, ParseError(err)
	}
	if log.EnableDebug() {
		log.LogDebugf("TRACE open ino(%v) info(%v) fullPath(%v)", ino, info, path.Join(f.getParentPath(), f.name))
	}
	start := time.Now()

	if f.super.bcacheDir != "" && !f.filterFilesSuffix(f.super.bcacheFilterFiles) {
		parentPath := f.getParentPath()
		if log.EnableDebug() {
			log.LogDebugf("TRACE open ino(%v) fullpath(%v)", ino, path.Join(f.getParentPath(), f.name))
		}
		if parentPath != "" && !strings.HasSuffix(parentPath, "/") {
			parentPath = parentPath + "/"
		}
		log.LogDebugf("TRACE open ino(%v) parentPath(%v)", ino, parentPath)
		if strings.HasPrefix(parentPath, f.super.bcacheDir) {
			needBCache = true
		}
	}
	openForWrite := false
	if req.Flags&0x0f != syscall.O_RDONLY {
		openForWrite = true
	}

	isCache := false
	// Hot or replica: ec.OpenStream (paired with ec.CloseStream in Release); cold/Blob skip ec here.
	if proto.IsHot(f.super.volType) || proto.IsStorageClassReplica(info.StorageClass) {
		if needBCache {
			f.super.ec.OpenStreamWithCache(ino, needBCache, openForWrite, isCache, path.Join(f.getParentPath(), f.name))
		} else {
			f.super.ec.OpenStream(ino, openForWrite, isCache, path.Join(f.getParentPath(), f.name))
		}
	}
	log.LogDebugf("TRACE open ino(%v) f.super.bcacheDir(%v) needBCache(%v)", ino, f.super.bcacheDir, needBCache)

	if proto.IsStorageClassReplica(info.StorageClass) {
		if f.super.metaCacheAcceleration && !openForWrite {
			inodeInfo, err1 := f.super.InodeGet(ino)
			if err1 == nil && inodeInfo != nil && inodeInfo.Extents != nil {
				f.super.ec.RefreshExtentsWithCache(inodeInfo)
			} else {
				f.super.ec.RefreshExtentsCache(ino)
			}
		} else {
			f.super.ec.RefreshExtentsCache(ino)
		}
	}

	// ec stream oeks
	if proto.IsStorageClassBlobStore(info.StorageClass) {
		f.super.oec.RefreshExtentsCache(ino)
	}

	if f.super.keepCache && resp != nil {
		resp.Flags |= fuse.OpenKeepCache
	}
	// Cold/Blob: openOECStream (oec refCnt++); Flush existing stream first (oec only, not ec.OpenStream).
	if proto.DataPlaneUsesBlobEC(f.super.volType, info.StorageClass) {
		log.LogDebugf("TRANCE open ino(%v) info(%v), poolId(%v)", ino, info, info.PoolId)

		if s := f.super.oec.GetStreamer(ino); s != nil {
			if flushErr := s.Flush(context.Background()); flushErr != nil {
				log.LogErrorf("Open: oec flush before replace stream ino(%v) err(%v)", ino, flushErr)
				return nil, ParseError(flushErr)
			}
		}
		fileSize, _ := f.fileSizeVersion2(ino)

		if err := f.openOECStream(info, uint32(req.Flags&0x0f), uint64(fileSize)); err != nil {
			log.LogErrorf("Open: openOECStream ino(%v) err: %v", ino, err)
			return nil, err
		}
		log.LogDebugf("TRACE file open (oec), ino(%v) req.Flags(%v) streamer(%v) writer(%v)",
			ino, req.Flags, f.super.oec.GetStreamer(ino) != nil, f.super.oec.HasWriter(ino))
	}

	elapsed := time.Since(start)
	f.setFlag(uint32(req.Flags))
	log.LogDebugf("TRACE Open: ino(%v) req(%v) resp(%v) flags(%v) (%v)ns", ino, req, resp, f.getFlag(), elapsed.Nanoseconds())

	return f, nil
}

// Release handles the release request.
func (f *File) Release(ctx context.Context, req *fuse.ReleaseRequest) (err error) {
	ino := f.ino
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("filerelease", req.Hdr().Pid)

	defer func() {
		stat.EndStat("Release:file", err, bgTime, 1)
		oecRef := f.super.oec.RefCnt(ino)
		if f.super.ec.RefCnt(ino) == 0 && oecRef == 0 && !f.super.metaCacheAcceleration {
			// keep nodeCache hold the latest inode info
			f.super.fslock.Lock()
			delete(f.super.nodeCache, ino)
			f.super.fslock.Unlock()
			if DisableMetaCache {
				f.super.ic.Delete(ino)
			}
			f.removeParentDcacheEntry()
			f.deleteExtendInfo()
		}
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()
	log.LogDebugf("TRACE Release enter: ino(%v) req(%v)", ino, req)

	start := time.Now()
	var errEc, errOec error
	// Release pairs with Create/Open to drop data-plane ref (storage-class specific, mutually exclusive paths):
	//
	//   | Class / vol | Create (dir.go)       | File.Open              | Release              | Forget (DisableMetaCache) |
	//   | Cold or Blob | openOECStream        | openOECStream (Flush first) | oec.CloseStream     | oec.EvictStream          |
	//   | Hot or replica | ec.OpenStream     | ec.OpenStream*         | ec.CloseStream      | ec.EvictStream           |
	//
	// *Open: hot/replica use ec; cold/Blob use oec only (no ec.OpenStream on same inode).
	info, getErr := f.getInfo()
	if getErr != nil {
		log.LogWarnf("Release: getInfo ino(%v) err(%v)", ino, getErr)
		return ParseError(getErr)
	}

	if proto.DataPlaneUsesBlobEC(f.super.volType, info.StorageClass) {
		if errOec = f.super.oec.CloseStream(ino); errOec != nil {
			log.LogErrorf("Release: oec CloseStream ino(%v) req(%v) err(%v)", ino, req, errOec)
			// oec.CloseStream rolls back refCnt on Flush failure; FreeCache writer buffer here, no Evict.
			if w := f.super.oec.Writer(ino); w != nil {
				w.FreeCache()
			}
			return ParseError(errOec)
		}
	} else {
		if errEc = f.super.ec.CloseStream(ino); errEc != nil {
			log.LogErrorf("Release: ec CloseStream ino(%v) req(%v) err(%v)", ino, req, errEc)
			return ParseError(errEc)
		}
	}

	if log.EnableDebug() {
		elapsed := time.Since(start)
		log.LogDebugf("TRACE FileRelease: ino(%v) req(%v) name(%v)(%v)ns", ino, req, path.Join(f.getParentPath(), f.name), elapsed.Nanoseconds())
	}

	return nil
}

// Read handles the read request.
func (f *File) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) (err error) {
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("fileread", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Read", err, bgTime, 1)
		stat.StatBandWidth("Read", uint32(req.Size))
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	info, err := f.getInfo()
	if err != nil {
		return ParseError(err)
	}
	// Get storage class from poolId if available, otherwise use existing StorageClass
	pool := f.getStorageClassByPoolId(info.PoolId)
	storageClass := uint32(pool.StorageClass)
	log.LogDebugf("TRACE Read enter: ino(%v) poolId(%v) storageClass(%v) offset(%v) filesize(%v) reqsize(%v) req(%v)",
		f.ino, info.PoolId, storageClass, req.Offset, info.Size, req.Size, req)

	start := time.Now()
	metric := exporter.NewTPCnt("fileread")
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: f.super.volname})
	}()

	var size int
	if proto.IsStorageClassReplica(storageClass) {
		f.super.ec.GetStreamer(f.ino).SetParentInode(f.parentIno)
		// Use storageClass derived from poolId
		size, err = f.super.ec.Read(f.ino, resp.Data[fuse.OutHeaderSize:], int(req.Offset),
			req.Size, info.PoolId, false)
	} else {
		// Blob/EC read/fstat bounds use oec FileSizeView (includes unflushed writer); sync in ECStreamer.Read.
		size, err = f.super.oec.Read(f.ino, resp.Data[fuse.OutHeaderSize:], int(req.Offset), req.Size)
	}
	if err != nil && err != io.EOF {
		msg := fmt.Sprintf("Read: ino(%v) req(%v) err(%v) size(%v)", f.ino, req, err, size)
		f.super.handleError("Read", msg)
		errMetric := exporter.NewCounter("fileReadFailed")
		if !isReadEio(err) {
			errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "NOTSUP"})
		} else {
			errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "EIO"})
		}
		return ParseError(err)
	}

	// last read request of file
	if info.Size > uint64(req.Offset) && uint64(req.Offset+int64(req.Size)) >= info.Size {
		// at least read bytes: info.Size - req.Offset
		if size > 0 && uint64(size) < info.Size-uint64(req.Offset) {
			log.LogWarnf("Read: error data size, ino(%v) offset(%v) filesize(%v) reqsize(%v) size(%v)\n", f.ino, req.Offset, info.Size, req.Size, size)
		}
	}

	if size > req.Size {
		msg := fmt.Sprintf("Read: read size larger than request size, ino(%v) req(%v) size(%v)", f.ino, req, size)
		f.super.handleError("Read", msg)
		errMetric := exporter.NewCounter("fileReadFailed")
		errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "ERANGE"})
		return fuse.ERANGE
	}

	if size > 0 {
		resp.Data = resp.Data[:size+fuse.OutHeaderSize]
	} else if size <= 0 && req.Size > 0 {
		resp.Data = resp.Data[:fuse.OutHeaderSize]
	}

	elapsed := time.Since(start)
	log.LogDebugf("TRACE Read: ino(%v) offset(%v) reqsize(%v) req(%v) size(%v) (%v)ns", f.ino, req.Offset, req.Size, req, size, elapsed.Nanoseconds())

	return nil
}

// Write handles the write request.
func (f *File) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) (err error) {
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("filewrite", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Write", err, bgTime, 1)
		stat.StatBandWidth("Write", uint32(len(req.Data)))
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	ino := f.ino
	reqlen := len(req.Data)

	f.super.BeginDirMutation(f.parentIno)
	defer f.super.EndDirMutation(f.parentIno)

	info, err := f.getInfo()
	if err != nil {
		return ParseError(err)
	}
	// Get storage class from poolId if available, otherwise use existing StorageClass
	pool := f.getStorageClassByPoolId(info.PoolId)
	storageClass := uint32(pool.StorageClass)

	log.LogDebugf("TRACE Write enter: ino(%v) poolId(%v) storageClass(%v) offset(%v) len(%v) flags(%v) fileflags(%v) quotaIds(%v) req(%v)",
		ino, info.PoolId, storageClass, req.Offset, reqlen, req.Flags, req.FileFlags, info.QuotaInfos, req)
	if proto.IsHot(f.super.volType) || proto.IsStorageClassReplica(storageClass) {
		filesize, _ := f.fileSize(ino)
		if req.Offset > int64(filesize) && reqlen == 1 && req.Data[0] == 0 {

			// workaround: posix_fallocate would write 1 byte if fallocate is not supported.
			fullPath := path.Join(f.getParentPath(), f.name)
			err = f.super.ec.Truncate(f.super.mw, f.parentIno, ino, int(req.Offset)+reqlen, fullPath)
			if err == nil {
				resp.Size = reqlen
			}
			log.LogDebugf("fallocate: ino(%v) origFilesize(%v) req(%v) err(%v)", f.ino, filesize, req, err)
			return
		}
	} else {
		filesize, _ := f.fileSizeVersion2(ino)
		if req.Offset > int64(filesize) && reqlen == 1 && req.Data[0] == 0 {
			// Special case:posix_fallocate may degrade to writing one trailing zero byte when fallocate is unsupported; keep behavior aligned with Hot/Replica branch.
			fullPath := path.Join(f.getParentPath(), f.name)
			target := uint64(req.Offset) + uint64(reqlen)
			if err = f.doECTruncateV2(ino, target, fullPath); err != nil {
				return ParseError(err)
			}
			resp.Size = reqlen
			log.LogDebugf("fallocate(blob): ino(%v) origFilesize(%v) target(%v) req(%v)", f.ino, filesize, target, req)
			return nil
		}
	}

	defer func() {
		f.super.ic.Delete(ino)
	}()

	var waitForFlush bool
	var flags int

	if isDirectIOEnabled(req.FileFlags) || (req.FileFlags&fuse.OpenSync != 0) {
		waitForFlush = true
		if f.super.enSyncWrite {
			flags |= proto.FlagsSyncWrite
		}
		// Cold/Blob still set FlagsSyncWrite for writer sync path; do not clear waitForFlush,
		// or FUSE returns success before durable write under O_SYNC/O_DIRECT (LTP rwtest/POSIX).
		if proto.IsCold(f.super.volType) || proto.IsStorageClassBlobStore(storageClass) {
			flags |= proto.FlagsSyncWrite
		}
	}

	if req.FileFlags&fuse.OpenAppend != 0 {
		flags |= proto.FlagsAppend
	}

	start := time.Now()
	metric := exporter.NewTPCnt("filewrite")
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: f.super.volname})
	}()

	checkFunc := func() error {
		if !f.super.mw.EnableQuota {
			return nil
		}
		if ok := f.super.ec.UidIsLimited(req.Uid); ok {
			return ParseError(syscall.ENOSPC)
		}
		var quotaIds []uint32
		for quotaId := range info.QuotaInfos {
			quotaIds = append(quotaIds, quotaId)
		}
		if limited := f.super.mw.IsQuotaLimited(quotaIds); limited {
			return ParseError(syscall.ENOSPC)
		}
		return nil
	}

	var size int
	if proto.IsStorageClassReplica(storageClass) {
		f.super.ec.GetStreamer(ino).SetParentInode(f.parentIno)
		// Use storageClass derived from poolId
		if size, err = f.super.ec.Write(ino, int(req.Offset), req.Data, flags, checkFunc, pool.Id,
			info.StorageClass, false, waitForFlush); err == ParseError(syscall.ENOSPC) {
			return
		}
	} else {
		f.storeIdle(0)
		size, err = f.super.oec.Write(ino, int(req.Offset), req.Data, flags)
	}

	if err != nil {
		msg := fmt.Sprintf("Write: ino(%v) offset(%v) len(%v) err(%v)", ino, req.Offset, reqlen, err)
		f.super.handleError("Write", msg)
		errMetric := exporter.NewCounter("fileWriteFailed")

		log.LogErrorf("Write: ino(%v) offset(%v) len(%v) err(%v)", ino, req.Offset, reqlen, err)

		if !isWriteEio(err) {
			errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "NOTSUP"})
		} else {
			errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "EIO"})
		}
		if err == syscall.EOPNOTSUPP {
			return fuse.ENOTSUP
		}
		return fuse.EIO
	}

	// POSIX/LTP: each Write must write full request or error; short writes fail rwtest/iogen.
	if size != reqlen {
		log.LogErrorf("Write: short write ino(%v) offset(%v) len(%v) got(%v)", ino, req.Offset, reqlen, size)
		return fuse.EIO
	}
	resp.Size = size

	// O_SYNC/O_DIRECT post-write flush: ec for hot/replica, oec for cold/Blob; never ec.Flush on Blob (EBADF→EIO).
	if waitForFlush {
		if proto.IsHot(f.super.volType) || proto.IsStorageClassReplica(storageClass) {
			err = f.super.ec.Flush(ino)
		} else {
			f.super.oec.Flush(ino)
		}
		if err != nil {
			msg := fmt.Sprintf("Write: failed to wait for flush, ino(%v) offset(%v) len(%v) err(%v) req(%v)", ino, req.Offset, reqlen, err, req)
			f.super.handleError("Wrtie", msg)
			errMetric := exporter.NewCounter("fileWriteFailed")
			if !isWriteEio(err) {
				errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "NOTSUP"})
			} else {
				errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "EIO"})
			}
			return ParseError(err)
		}
	}
	// Under RDWR we can call syncBlobReaderAfterMetaChange right after successful writes to align Reader immediately; this branch keeps it disabled,
	// and read path relies on InodeGet + readAfterFlush ensureAlignedForRead on each Read to refresh ObjExtents.
	elapsed := time.Since(start)
	log.LogDebugf("TRACE Write: ino(%v) offset(%v) len(%v) flags(%v) fileflags(%v) req(%v) (%v) ",
		ino, req.Offset, reqlen, req.Flags, req.FileFlags, req, elapsed.String())
	return nil
}

// Flush only when fsyncOnClose is enabled.
func (f *File) Flush(ctx context.Context, req *fuse.FlushRequest) (err error) {
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("filesync", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Flush", err, bgTime, 1)
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	if !f.super.fsyncOnClose {
		return fuse.ENOSYS
	}
	log.LogDebugf("TRACE Flush enter: ino(%v)", f.ino)
	start := time.Now()

	metric := exporter.NewTPCnt("filesync")
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: f.super.volname})
	}()

	openForWrite := false
	if req.Flags&0x0f != syscall.O_RDONLY {
		openForWrite = true
	}
	if openForWrite {
		f.super.BeginDirMutation(f.parentIno)
		defer f.super.EndDirMutation(f.parentIno)
	}
	info, infoErr := f.getInfo()
	if infoErr != nil {
		return ParseError(infoErr)
	}
	// Get storage class from poolId if available, otherwise use existing StorageClass
	pool := f.getStorageClassByPoolId(info.PoolId)
	storageClass := uint32(pool.StorageClass)

	if proto.IsHot(f.super.volType) || proto.IsStorageClassReplica(storageClass) {
		err = f.super.ec.Flush(f.ino)
	} else {
		err = f.super.oec.Flush(f.ino)
	}
	log.LogDebugf("TRACE Flush: ino(%v) err(%v)", f.ino, err)
	if err != nil {
		msg := fmt.Sprintf("Flush: ino(%v) err(%v)", f.ino, err)
		f.super.handleError("Flush", msg)
		log.LogErrorf("TRACE Flush err: ino(%v) err(%v)", f.ino, err)

		errMetric := exporter.NewCounter("fileWriteFailed")
		if !isReadEio(err) {
			errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "NOTSUP"})
		} else {
			errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "EIO"})
		}

		return ParseError(err)
	}

	if DisableMetaCache && openForWrite {
		f.super.ic.Delete(f.ino)
	}

	elapsed := time.Since(start)
	log.LogDebugf("TRACE Flush: ino(%v) (%v)ns", f.ino, elapsed.Nanoseconds())

	return nil
}

// Fsync handles the fsync request.
func (f *File) Fsync(ctx context.Context, req *fuse.FsyncRequest) (err error) {
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("filefsnyc", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Fsync", err, bgTime, 1)
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	openForWrite := false
	if req.Flags&0x0f != syscall.O_RDONLY {
		openForWrite = true
	}
	if openForWrite {
		f.super.BeginDirMutation(f.parentIno)
		defer f.super.EndDirMutation(f.parentIno)
	}

	log.LogDebugf("TRACE Fsync enter: ino(%v)", f.ino)
	start := time.Now()
	info, infoErr := f.getInfo()
	if infoErr != nil {
		return ParseError(infoErr)
	}
	// Get storage class from poolId if available, otherwise use existing StorageClass
	pool := f.getStorageClassByPoolId(info.PoolId)
	storageClass := uint32(pool.StorageClass)

	if proto.IsHot(f.super.volType) || proto.IsStorageClassReplica(storageClass) {
		err = f.super.ec.Flush(f.ino)
	} else {
		err = f.super.oec.Flush(f.ino)
	}
	if err != nil {
		msg := fmt.Sprintf("Fsync: ino(%v) err(%v)", f.ino, err)
		f.super.handleError("Fsync", msg)

		errMetric := exporter.NewCounter("fileWriteFailed")
		if !isWriteEio(err) {
			errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "NOTSUP"})
		} else {
			errMetric.AddWithLabels(1, map[string]string{exporter.Vol: f.super.volname, exporter.Err: "EIO"})
		}

		return ParseError(err)
	}

	f.super.ic.Delete(f.ino)
	elapsed := time.Since(start)
	log.LogDebugf("TRACE Fsync: ino(%v) (%v)ns", f.ino, elapsed.Nanoseconds())
	return nil
}

// Setattr handles the setattr request.
func (f *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	var err error
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("filesetattr", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Setattr", err, bgTime, 1)
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	ino := f.ino
	start := time.Now()
	// Truncate must use fresh meta: stale ic Size can skip doECTruncateV2 when req.Size==info.Size (LTP ftest01).
	if req.Valid.Size() {
		f.super.ic.Delete(ino)
	}
	info, err := f.getInfo()
	if err != nil {
		return ParseError(err)
	}
	// Get storage class from poolId if available, otherwise use existing StorageClass
	pool := f.getStorageClassByPoolId(info.PoolId)
	storageClass := uint32(pool.StorageClass)

	openForWrite := false
	if req.Flags&0x0f != syscall.O_RDONLY {
		openForWrite = true
	}

	f.super.BeginDirMutation(f.parentIno)
	defer f.super.EndDirMutation(f.parentIno)

	isCache := false
	if proto.IsCold(f.super.volType) || proto.IsStorageClassBlobStore(storageClass) {
		isCache = true
	}

	log.LogDebugf("Setattr: ino(%v) openForWrite(%v) isCache(%v) targetSize(%v) isHot(%v) storageClass(%v)",
		ino, openForWrite, isCache, req.Valid.Size(), proto.IsHot(f.super.volType), storageClass)

	if req.Valid.Size() {
		fullPath := path.Join(f.getParentPath(), f.name)
		switch {
		case proto.IsHot(f.super.volType) || proto.IsStorageClassReplica(storageClass):
			// when use trunc param in open request through nfs client and mount on cfs mountPoint, cfs client may not recv open message but only setAttr,
			// the streamer may not open and cause io error finally,so do a open no matter the stream be opened or not
			// Replica extents: NFS may only send Setattr without Open, so open stream first then truncate.
			if err := f.super.ec.OpenStream(ino, openForWrite, isCache, path.Join(f.getParentPath(), f.name)); err != nil {
				log.LogErrorf("Setattr: OpenStream ino(%v) size(%v) err(%v)", ino, req.Size, err)
				return ParseError(err)
			}
			defer f.super.ec.CloseStream(ino)

			if err := f.super.ec.Flush(ino); err != nil {
				log.LogErrorf("Setattr: truncate wait for flush ino(%v) size(%v) err(%v)", ino, req.Size, err)
				return ParseError(err)
			}
			fullPath := path.Join(f.getParentPath(), f.name)
			if err := f.super.ec.Truncate(f.super.mw, f.parentIno, ino, int(req.Size), fullPath); err != nil {
				log.LogErrorf("Setattr: truncate ino(%v) size(%v) err(%v)", ino, req.Size, err)
				return ParseError(err)
			}
			f.super.ic.Delete(ino)
			f.super.ec.RefreshExtentsCache(ino)
		case proto.IsStorageClassBlobStore(storageClass):
			if err := f.openOECStream(info, uint32(req.Flags&0x0f), info.Size); err != nil {
				log.LogErrorf("Setattr: openOECStream ino(%v) size(%v) err(%v)", ino, req.Size, err)
				return ParseError(err)
			}
			defer func() {
				if closeErr := f.super.oec.CloseStream(ino); closeErr != nil {
					log.LogWarnf("Setattr: oec CloseStream ino(%v) err(%v)", ino, closeErr)
				}
			}()
			if err = f.doECTruncateV2(ino, req.Size, fullPath); err != nil {
				log.LogErrorf("Setattr: doECTruncateV2 ino(%v) size(%v) err(%v)", ino, req.Size, err)
				return ParseError(err)
			}
			f.super.ic.Delete(ino)
			// doECTruncateV2 already refreshed extents in truncateV2Locked; then refresh inode view.
		default:
			// Size changes for other storage classes are not handled here; they are left to later setattr(meta) paths if any.
		}
	}

	info, err = f.super.InodeGet(ino)
	if err != nil {
		log.LogErrorf("Setattr: InodeGet failed, ino(%v) err(%v)", ino, err)
		return ParseError(err)
	}

	if req.Valid.Size() && (proto.IsHot(f.super.volType) || proto.IsStorageClassReplica(storageClass) ||
		proto.IsStorageClassBlobStore(storageClass)) {
		if req.Size != info.Size {
			log.LogWarnf("Setattr: truncate ino(%v) reqSize(%v) inodeSize(%v)", ino, req.Size, info.Size)
		}
	}

	valid := setattr(info, req)
	if valid != 0 {
		err = f.super.mw.Setattr(ino, valid, info.Mode, info.Uid, info.Gid, info.AccessTime.Unix(),
			info.ModifyTime.Unix())
		if err != nil {
			f.super.ic.Delete(ino)
			return ParseError(err)
		}
	}

	fillAttr(info, &resp.Attr)

	elapsed := time.Since(start)
	log.LogDebugf("TRACE Setattr: ino(%v) req(%v) valid(%v) cost(%v)ns", ino, req, valid, elapsed.Nanoseconds())
	return nil
}

// doECTruncateV2 handles truncate for EC/BlobStore volumes.
// Call chain: ensureBlobStoreWriter -> oec.Truncate -> ECStreamer.truncateV2 -> Writer.Flush / mw.GetObjExtents / (shrink: BeforeEBSShrinkHook+Writer.TruncateV2) -> mw.TruncateV2。
func (f *File) doECTruncateV2(ino uint64, targetSize uint64, fullPath string) error {
	return f.super.oec.Truncate(f.parentIno, ino, targetSize, fullPath)
}

// Readlink handles the readlink request.
func (f *File) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (string, error) {
	var err error
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("filereadlink", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Readlink", err, bgTime, 1)
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	ino := f.ino
	info, err := f.super.InodeGet(ino)
	if err != nil {
		log.LogErrorf("Readlink: ino(%v) err(%v)", ino, err)
		return "", ParseError(err)
	}
	if log.EnableDebug() {
		log.LogDebugf("TRACE Readlink: ino(%v) target(%v)", ino, string(info.Target))
	}
	return string(info.Target), nil
}

// Getxattr has not been implemented yet.
func (f *File) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	var err error
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("filegetxattr", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Getxattr", err, bgTime, 1)
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	if !f.super.enableXattr {
		return fuse.ENOSYS
	}
	ino := f.ino
	name := req.Name
	size := req.Size
	pos := req.Position

	// Optimize: directly return empty value for security.capability to avoid frequent server queries
	// This avoids backend service access for system xattr that is frequently queried during write operations
	if name == "security.capability" {
		resp.Xattr = []byte{}
		log.LogDebugf("TRACE GetXattr: ino(%v) name(%v) (optimized, returning empty)", ino, name)
		return nil
	}

	info, err := f.super.mw.XAttrGet_ll(ino, name, false)
	if err != nil {
		log.LogErrorf("GetXattr: ino(%v) name(%v) err(%v)", ino, name, err)
		return ParseError(err)
	}
	value := info.Get(name)
	if pos > 0 {
		value = value[pos:]
	}
	if size > 0 && size < uint32(len(value)) {
		value = value[:size]
	}
	resp.Xattr = value
	log.LogDebugf("TRACE GetXattr: ino(%v) name(%v)", ino, name)
	return nil
}

// Listxattr has not been implemented yet.
func (f *File) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	var err error
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("filelistxattr", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Listxattr", err, bgTime, 1)
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	if !f.super.enableXattr {
		return fuse.ENOSYS
	}
	ino := f.ino
	_ = req.Size     // ignore currently
	_ = req.Position // ignore currently

	keys, err := f.super.mw.XAttrsList_ll(ino)
	if err != nil {
		log.LogErrorf("ListXattr: ino(%v) err(%v)", ino, err)
		return ParseError(err)
	}
	for _, key := range keys {
		resp.Append(key)
	}
	log.LogDebugf("TRACE Listxattr: ino(%v)", ino)
	return nil
}

// Setxattr has not been implemented yet.
func (f *File) Setxattr(ctx context.Context, req *fuse.SetxattrRequest) error {
	var err error
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("filesetxattr", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Setxattr", err, bgTime, 1)
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	if !f.super.enableXattr {
		return fuse.ENOSYS
	}
	ino := f.ino
	name := req.Name
	value := req.Xattr
	// TODO： implement flag to improve compatible (Mofei Zhang)
	if err = f.super.mw.XAttrSet_ll(ino, []byte(name), []byte(value), false); err != nil {
		log.LogErrorf("Setxattr: ino(%v) name(%v) err(%v)", ino, name, err)
		return ParseError(err)
	}
	log.LogDebugf("TRACE Setxattr: ino(%v) name(%v)", ino, name)
	return nil
}

// Removexattr has not been implemented yet.
func (f *File) Removexattr(ctx context.Context, req *fuse.RemovexattrRequest) error {
	var err error
	bgTime := stat.BeginStat()
	runningStat := f.super.runningMonitor.AddClientOp("fileremovexattr", req.Hdr().Pid)
	defer func() {
		stat.EndStat("Removexattr", err, bgTime, 1)
		f.super.runningMonitor.SubClientOp(runningStat, err)
	}()

	if !f.super.enableXattr {
		return fuse.ENOSYS
	}
	ino := f.ino
	name := req.Name
	if err = f.super.mw.XAttrDel_ll(ino, name); err != nil {
		log.LogErrorf("Removexattr: ino(%v) name(%v) err(%v)", ino, name, err)
		return ParseError(err)
	}
	log.LogDebugf("TRACE RemoveXattr: ino(%v) name(%v)", ino, name)
	return nil
}

func (f *File) fileSize(ino uint64) (size int, gen uint64) {
	size, gen, valid := f.super.ec.FileSize(ino)
	if !valid {
		if info, err := f.super.InodeGet(ino); err == nil {
			size = int(info.Size)
			gen = info.Generation
		}
	}

	log.LogDebugf("TRANCE fileSize: ino(%v) fileSize(%v) gen(%v) valid(%v)", ino, size, gen, valid)
	return
}

func (f *File) fileSizeVersion2(ino uint64) (size int, gen uint64) {
	valid := false
	if proto.IsHot(f.super.volType) || proto.IsStorageClassReplica(f.storageClass()) {
		size, gen, valid = f.super.ec.FileSize(ino)
		if !valid {
			if info, err := f.super.InodeGet(ino); err == nil {
				size = int(info.Size)
				gen = info.Generation
			}
		}
	} else {
		// Blob/EC: when oec stream exists, FileSize is authoritative (includes unflushed writer tail).
		size, gen, valid = f.super.oec.FileSize(ino) // streamer logical size and generation
		if !valid {
			if info, err := f.super.InodeGet(ino); err == nil {
				size = int(info.Size)
				gen = info.Generation
			}
		}
	}

	log.LogDebugf("TRACE fileSizeVersion2: ino(%v) fileSize(%v) gen(%v) valid(%v)", ino, size, gen, valid)
	return
}

// return true mean this file will not cache in block cache
func (f *File) filterFilesSuffix(filterFiles string) bool {
	if f.name == "" {
		log.LogWarnf("this file inode[%v], name is nil", f.ino)
		return true
	}
	if filterFiles == "" {
		return false
	}
	suffixs := strings.Split(filterFiles, ";")
	for _, suffix := range suffixs {
		// .py means one type of file
		suffix = "." + suffix
		if suffix != "." && strings.Contains(f.name, suffix) {
			log.LogDebugf("fileName:%s,filter:%s,suffix:%s,suffixs:%v", f.name, filterFiles, suffix, suffixs)
			return true
		}
	}
	return false
}
