package fs

import (
	"reflect"
	"unsafe"

	"github.com/cubefs/cubefs/sdk/data/blobstore"
)

// injectOECStreamer registers a test ECStreamer on oec (UT-only white-box write to unexported map).
func injectOECStreamer(c *blobstore.ECExtentClient, ino uint64, st *blobstore.ECStreamer) {
	if c == nil || st == nil {
		return
	}
	field := reflect.ValueOf(c).Elem().FieldByName("streamers")
	m := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	m.SetMapIndex(reflect.ValueOf(ino), reflect.ValueOf(st))
}

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
	injectOECStreamer(s.oec, ino, st)
}
