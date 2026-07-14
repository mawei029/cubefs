// Copyright 2019 The CubeFS Authors.
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

package objectnode

import (
	"bytes"
	"context"
	"crypto/md5"
	"hash"
	"io"
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
)

func injectOECStreamer(c *blobstore.ECExtentClient, ino uint64, st *blobstore.ECStreamer) {
	if c == nil || st == nil {
		return
	}
	field := reflect.ValueOf(c).Elem().FieldByName("streamers")
	m := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	m.SetMapIndex(reflect.ValueOf(ino), reflect.ValueOf(st))
}

func newTestVolumeForOEC(t *testing.T) *Volume {
	t.Helper()
	ec := &stream.ExtentClient{}
	return &Volume{
		oec:          blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{LimitManager: ec.LimitManager}),
		ec:           ec,
		mw:           &meta.MetaWrapper{},
		name:         "utvol",
		volType:      proto.VolumeTypeCold,
		ebsBlockSize: 131072,
		closeCh:      make(chan struct{}),
	}
}

func registerOecWriterForVolume(s *Volume, ino uint64, w *blobstore.Writer) {
	args := blobstore.ECStreamOpenArgs{Ino: ino, FileSize: 0, InodeGeneration: 0}
	st, _ := blobstore.NewECStreamer(args, nil, w)
	injectOECStreamer(s.oec, ino, st)
}

// TestVolume_oecStreamLifecycle mirrors ec OpenStream/CloseStream scope used by PutObject/readFile.
func TestVolume_oecStreamLifecycle(t *testing.T) {
	v := newTestVolumeForOEC(t)
	ino := uint64(42)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Volume)(nil)), "openOECStream",
		func(v *Volume, ino uint64, poolId uint8) error {
			return v.oec.OpenStreamWithArgs(blobstore.ECStreamOpenArgs{
				Ino: ino, PoolId: poolId, VolName: v.name, VolType: v.volType, BlockSize: v.ebsBlockSize,
			})
		})

	require.NoError(t, v.openOECStream(ino, 1))
	require.Equal(t, int32(1), v.oec.RefCnt(ino))
	require.NoError(t, v.oec.CloseStream(ino))
	require.Equal(t, int32(0), v.oec.RefCnt(ino))
}

func TestVolume_buildECStreamOpenArgs(t *testing.T) {
	v := newTestVolumeForOEC(t)
	args := v.buildECStreamOpenArgs(7, 2, 100, 3)
	require.Equal(t, uint64(7), args.Ino)
	require.Equal(t, uint8(2), args.PoolId)
	require.Equal(t, uint64(100), args.FileSize)
	require.Equal(t, uint64(3), args.InodeGeneration)
	require.Equal(t, v.name, args.VolName)
	require.Equal(t, v.oec.LimitManager, args.LimitManager)
}

func TestVolume_openOECStream_usesInodeMeta(t *testing.T) {
	v := newTestVolumeForOEC(t)
	ino := uint64(55)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(v.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, got uint64, _ bool) (*proto.InodeInfo, error) {
			require.Equal(t, ino, got)
			return &proto.InodeInfo{Inode: ino, Size: 200, Generation: 9}, nil
		})
	var captured blobstore.ECStreamOpenArgs
	patches.ApplyMethod(reflect.TypeOf(v.oec), "OpenStreamWithArgs",
		func(_ *blobstore.ECExtentClient, args blobstore.ECStreamOpenArgs) error {
			captured = args
			return nil
		})
	require.NoError(t, v.openOECStream(ino, 1))
	require.Equal(t, uint64(200), captured.FileSize)
	require.Equal(t, uint64(9), captured.InodeGeneration)
}

func TestVolume_ebsWrite(t *testing.T) {
	v := newTestVolumeForOEC(t)
	ino := uint64(10)
	w := &blobstore.Writer{}
	registerOecWriterForVolume(v, ino, w)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(v.oec), "WriteFromReader",
		func(_ *blobstore.ECExtentClient, _ context.Context, got uint64, _ io.Reader, _ hash.Hash) (uint64, error) {
			require.Equal(t, ino, got)
			return 5, nil
		})

	n, err := v.ebsWrite(ino, bytes.NewReader([]byte("hello")), md5.New(), 1)
	require.NoError(t, err)
	require.Equal(t, uint64(5), n)
}

func TestVolume_ebsWrite_noWriter(t *testing.T) {
	v := newTestVolumeForOEC(t)
	_, err := v.ebsWrite(999, bytes.NewReader(nil), md5.New(), 0)
	require.Error(t, err)
}

func TestVolume_readFile_useOEC(t *testing.T) {
	v := newTestVolumeForOEC(t)
	ino := uint64(20)
	r := &blobstore.Reader{}
	args := blobstore.ECStreamOpenArgs{Ino: ino}
	st, _ := blobstore.NewECStreamer(args, r, nil)
	injectOECStreamer(v.oec, ino, st)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Volume)(nil)), "openOECStream", func(_ *Volume, _ uint64, _ uint8) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(v.oec), "CloseStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(st), "Read",
		func(_ *blobstore.ECStreamer, _ context.Context, buf []byte, _ int, size int) (int, error) {
			n := size
			if n > 4 {
				n = 4
			}
			copy(buf[:n], []byte("data")[:n])
			return n, io.EOF
		})

	var out bytes.Buffer
	err := v.readFile(ino, 4, "/obj", &out, 0, 4, proto.StorageClass_BlobStore, 1)
	require.NoError(t, err)
	require.Equal(t, "data", out.String())
}

func TestVolume_Close_oec(t *testing.T) {
	v := newTestVolumeForOEC(t)
	closed := false
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(v.oec), "Close", func(_ *blobstore.ECExtentClient) error {
		closed = true
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(v.mw), "Close", func(_ *meta.MetaWrapper) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(v.ec), "Close", func(_ *stream.ExtentClient) error { return nil })
	require.NoError(t, v.Close())
	require.True(t, closed)
}

func TestVolume_applyInodeToExistDentry_evictsStreams(t *testing.T) {
	v := newTestVolumeForOEC(t)
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var ecEvict, oecEvict uint64
	patches.ApplyMethod(reflect.TypeOf(v.mw), "DentryUpdate_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ string, _ uint64, _ string) (uint64, error) {
			return 100, nil
		})
	patches.ApplyMethod(reflect.TypeOf(v.mw), "InodeUnlink_ll", func(_ *meta.MetaWrapper, _ uint64, _ string) (bool, error) {
		return true, nil
	})
	patches.ApplyMethod(reflect.TypeOf(v.mw), "Evict", func(_ *meta.MetaWrapper, _ uint64, _ string, _ bool) error {
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(v.ec), "EvictStream", func(_ *stream.ExtentClient, ino uint64) error {
		ecEvict = ino
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(v.oec), "EvictStream", func(_ *blobstore.ECExtentClient, ino uint64) error {
		oecEvict = ino
		return nil
	})

	err := v.applyInodeToExistDentry(1, "k", 2, false, "/p", proto.StorageClass_BlobStore)
	require.NoError(t, err)
	require.Equal(t, uint64(100), ecEvict)
	require.Equal(t, uint64(100), oecEvict)
}

func TestVolume_DeletePath_evictsEcAndOec(t *testing.T) {
	v := newTestVolumeForOEC(t)
	v.metaLoader = &strictMetaLoader{v: v}
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	ino := uint64(501)
	patches.ApplyPrivateMethod(reflect.TypeOf((*Volume)(nil)), "recursiveLookupTarget",
		func(_ *Volume, _ string, _ bool) (uint64, uint64, string, os.FileMode, error) {
			return 1, ino, "obj", 0o644, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*strictMetaLoader)(nil)), "loadObjectLock",
		func(_ *strictMetaLoader) (*ObjectLockConfig, error) {
			return nil, nil
		})
	patches.ApplyMethod(reflect.TypeOf(v.mw), "Delete_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ string, _ bool, _ string, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: ino}, nil
		})
	var ecEvict, oecEvict uint64
	patches.ApplyMethod(reflect.TypeOf(v.ec), "EvictStream", func(_ *stream.ExtentClient, got uint64) error {
		ecEvict = got
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(v.oec), "EvictStream", func(_ *blobstore.ECExtentClient, got uint64) error {
		oecEvict = got
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(v.mw), "Evict", func(_ *meta.MetaWrapper, _ uint64, _ string, _ bool) error {
		return nil
	})

	err := v.DeletePath("obj")
	require.NoError(t, err)
	require.Equal(t, ino, ecEvict)
	require.Equal(t, ino, oecEvict)
}

func TestVolume_readFile_replicaUsesEc(t *testing.T) {
	v := newTestVolumeForOEC(t)
	v.volType = proto.VolumeTypeHot
	ino := uint64(60)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(v.ec), "OpenStream", func(_ *stream.ExtentClient, _ uint64, _ bool, _ bool, _ string) error {
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(v.ec), "CloseStream", func(_ *stream.ExtentClient, _ uint64) error {
		return nil
	})
	patches.ApplyPrivateMethod(reflect.TypeOf((*Volume)(nil)), "read",
		func(_ *Volume, _ uint64, _ uint64, _ string, w io.Writer, _, _ uint64, _ uint8) error {
			_, err := w.Write([]byte("ec"))
			return err
		})

	var out bytes.Buffer
	err := v.readFile(ino, 2, "/o", &out, 0, 2, proto.StorageClass_Replica_SSD, 1)
	require.NoError(t, err)
	require.Equal(t, "ec", out.String())
}

func TestVolume_PutObject_useOECPath(t *testing.T) {
	v := newTestVolumeForOEC(t)
	v.metaLoader = &strictMetaLoader{v: v}
	ino := uint64(900)
	w := &blobstore.Writer{}
	registerOecWriterForVolume(v, ino, w)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Volume)(nil)), "recursiveMakeDirectory",
		func(_ *Volume, _ string) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(v.mw), "Lookup_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ string, _ bool) (uint64, uint32, error) {
			return 0, 0, syscall.ENOENT
		})
	patches.ApplyMethod(reflect.TypeOf(v.mw), "InodeCreate_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ uint32, _, _ uint32, _ []byte, _ []uint64, _ string) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{
				Inode: ino, PoolId: 1, StorageClass: proto.StorageClass_BlobStore, Size: 0, Mode: DefaultFileMode,
			}, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*Volume)(nil)), "applyInodeToDEntry",
		func(_ *Volume, _, _ string, _ uint64, _ bool, _ string, _ uint32) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(v.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, got uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: got, Size: 3, ModifyTime: time.Now()}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(v.mw), "BatchSetXAttr_ll", func(_ *meta.MetaWrapper, _ uint64, _ map[string]string) error {
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(v.oec), "WriteFromReader",
		func(_ *blobstore.ECExtentClient, _ context.Context, _ uint64, _ io.Reader, _ hash.Hash) (uint64, error) {
			return 3, nil
		})
	patches.ApplyMethod(reflect.TypeOf(v.oec), "CloseStream", func(_ *blobstore.ECExtentClient, _ uint64) error {
		return nil
	})

	_, err := v.PutObject("obj", bytes.NewReader([]byte("abc")), nil)
	require.NoError(t, err)
}

func TestVolume_WritePart_useOECPath(t *testing.T) {
	v := newTestVolumeForOEC(t)
	ino := uint64(910)
	w := &blobstore.Writer{}
	registerOecWriterForVolume(v, ino, w)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(v.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, got uint64, _ bool) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{Inode: got, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(v.mw), "InodeCreate_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ uint32, _, _ uint32, _ []byte, _ []uint64, _ string) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{
				Inode: ino, PoolId: 1, StorageClass: proto.StorageClass_BlobStore, Size: 0, Mode: DefaultFileMode,
			}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(v.mw), "AddMultipartPart_ll",
		func(_ *meta.MetaWrapper, _ string, _ string, _ uint16, _ uint64, _ string, _ *proto.InodeInfo) (uint64, bool, error) {
			return 0, false, nil
		})
	patches.ApplyMethod(reflect.TypeOf(v.oec), "WriteFromReader",
		func(_ *blobstore.ECExtentClient, _ context.Context, _ uint64, _ io.Reader, _ hash.Hash) (uint64, error) {
			return 2, nil
		})
	patches.ApplyMethod(reflect.TypeOf(v.oec), "CloseStream", func(_ *blobstore.ECExtentClient, _ uint64) error {
		return nil
	})

	_, err := v.WritePart("obj", "mp", 1, bytes.NewReader([]byte("ab")))
	require.NoError(t, err)
}

func TestVolume_CopyFile_useOECPath(t *testing.T) {
	sv := newTestVolumeForOEC(t)
	v := newTestVolumeForOEC(t)
	sIno, tIno := uint64(801), uint64(802)
	sr := &blobstore.Reader{}
	sw := &blobstore.Writer{}
	registerOecReaderForVolume(sv, sIno, sr)
	registerOecWriterForVolume(v, tIno, sw)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf((*Volume)(nil)), "recursiveLookupTarget",
		func(_ *Volume, path string, _ bool) (uint64, uint64, string, os.FileMode, error) {
			if path == "src" {
				return 1, sIno, "src", DefaultFileMode, nil
			}
			return 0, 0, "", 0, syscall.ENOENT
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*Volume)(nil)), "recursiveMakeDirectory",
		func(_ *Volume, _ string) (uint64, error) { return 1, nil })
	patches.ApplyMethod(reflect.TypeOf(sv.mw), "InodeGet_ll",
		func(_ *meta.MetaWrapper, ino uint64, _ bool) (*proto.InodeInfo, error) {
			if ino == sIno {
				return &proto.InodeInfo{Inode: sIno, Size: 4, PoolId: 1, StorageClass: proto.StorageClass_BlobStore}, nil
			}
			return &proto.InodeInfo{Inode: ino, ModifyTime: time.Now()}, nil
		})
	patches.ApplyMethod(reflect.TypeOf(v.mw), "InodeCreate_ll",
		func(_ *meta.MetaWrapper, _ uint64, _ uint32, _, _ uint32, _ []byte, _ []uint64, _ string) (*proto.InodeInfo, error) {
			return &proto.InodeInfo{
				Inode: tIno, PoolId: 1, StorageClass: proto.StorageClass_BlobStore, Size: 0, Mode: DefaultFileMode,
			}, nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf((*Volume)(nil)), "applyInodeToDEntry",
		func(_ *Volume, _, _ string, _ uint64, _ bool, _ string, _ uint32) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(v.mw), "BatchSetXAttr_ll", func(_ *meta.MetaWrapper, _ uint64, _ map[string]string) error {
		return nil
	})
	patches.ApplyMethod(reflect.TypeOf(sv.mw), "XAttrGetAll_ll", func(_ *meta.MetaWrapper, _ uint64) (*proto.XAttrInfo, error) {
		return &proto.XAttrInfo{XAttrs: map[string]string{}}, nil
	})
	patches.ApplyMethod(reflect.TypeOf(sv.oec), "Read",
		func(_ *blobstore.ECExtentClient, _ uint64, buf []byte, _, _ int) (int, error) {
			copy(buf, []byte("data"))
			return 4, io.EOF
		})
	tSt := v.oec.GetStreamer(tIno)
	patches.ApplyMethod(reflect.TypeOf(tSt), "WriteWithoutPool",
		func(_ *blobstore.ECStreamer, _ context.Context, _ int, _ []byte) (int, error) {
			return len([]byte("data")), nil
		})
	patches.ApplyMethod(reflect.TypeOf(tSt), "FlushWithoutPool",
		func(_ *blobstore.ECStreamer, _ uint64, _ context.Context) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(sv.oec), "CloseStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(v.oec), "CloseStream", func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })

	_, err := v.CopyFile(sv, "src", "dst", "", nil)
	require.NoError(t, err)
}

func registerOecReaderForVolume(s *Volume, ino uint64, r *blobstore.Reader) {
	args := blobstore.ECStreamOpenArgs{Ino: ino, FileSize: 4, InodeGeneration: 0}
	st, _ := blobstore.NewECStreamer(args, r, nil)
	injectOECStreamer(s.oec, ino, st)
}

func TestVolume_readEbs_2(t *testing.T) {
	v := newTestVolumeForOEC(t)
	t.Run("offset_at_or_past_eof", func(t *testing.T) {
		var out bytes.Buffer
		require.NoError(t, v.readEbs(1, 10, "/o", &out, 10, 5, 0))
		require.Empty(t, out.Bytes())
		require.NoError(t, v.readEbs(1, 10, "/o", &out, 11, 5, 0))
		require.Empty(t, out.Bytes())
	})
	t.Run("range_clamped_via_oec_read", func(t *testing.T) {
		ino := uint64(55)
		registerOecReaderForVolume(v, ino, &blobstore.Reader{})
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(v.oec), "Read",
			func(_ *blobstore.ECExtentClient, _ uint64, buf []byte, off, size int) (int, error) {
				require.Equal(t, 8, off)
				require.Equal(t, 2, size)
				copy(buf, []byte("xy"))
				return 2, nil
			})
		var out bytes.Buffer
		require.NoError(t, v.readEbs(ino, 10, "/o", &out, 8, 10, 0))
		require.Equal(t, "xy", out.String())
	})
}
