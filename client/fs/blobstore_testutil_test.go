package fs

import (
	"github.com/cubefs/cubefs/sdk/data/blobstore"
)

// registerOecTestStreamerWithLogicalView 注册带 Open 快照的测试流。
func registerOecTestStreamerWithLogicalView(s *Super, ino uint64, r *blobstore.Reader, w *blobstore.Writer, fileSize, inoGen uint64) {
	args := blobstore.ECStreamOpenArgs{
		Ino:             ino,
		FileSize:        fileSize,
		InodeGeneration: inoGen,
	}
	var st *blobstore.ECStreamer
	switch {
	case r != nil && w != nil:
		st, _ = blobstore.NewECStreamer(args, r, w)
	case w != nil:
		st, _ = blobstore.NewECStreamer(args, nil, w)
	case r != nil:
		st, _ = blobstore.NewECStreamer(args, r, nil)
	default:
		return
	}
	s.oec.SetStreamer(ino, st)
}
