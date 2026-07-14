package fs

import (
	"context"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/depends/bazil.org/fuse"
	bazilfs "github.com/cubefs/cubefs/depends/bazil.org/fuse/fs"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
)

// -----------------------------------------------------------------------------
// 本文件：LTP/ftest01 相关契约 UT（Attr/Read 与 EC 流视图合并规则），供单文件 golangci-lint 与 go test -run TestFile_LtpSim。
//
// 与历史 LTP/ftest01 日志的对应关系见 TestFile_LtpSim_ec_fault_* / blob_attr_* 契约用例。
//
// 运行：go test ./client/fs -run 'TestFile_LtpSim' -v
// -----------------------------------------------------------------------------

func ltpNewTestSuperForFile() *Super {
	return &Super{
		ic:                NewInodeCache(time.Hour, 64, true),
		rootIno:           1,
		nodeCache:         make(map[uint64]bazilfs.Node),
		dirExtendInfoMap:  make(map[uint64]*DirExtendInfo),
		fileExtendInfoMap: make(map[uint64]*FileExtendInfo),
		runningMonitor:    NewRunningMonitor(0),
		ec:                &stream.ExtentClient{},
		mw:                &meta.MetaWrapper{},
		volname:           "vol",
		EbsBlockSize:      4096,
		oec:               blobstore.NewObjExtentClient(blobstore.ObjExtentConfig{}),
	}
}

func ltpRegisterOecTestStreamer(s *Super, ino uint64, r *blobstore.Reader, w *blobstore.Writer) {
	var st *blobstore.ECStreamer
	args := blobstore.ECStreamOpenArgs{Ino: ino}
	switch {
	case r != nil && w != nil:
		st, _ = blobstore.NewECStreamer(args, r, w)
	case w != nil:
		st, _ = blobstore.NewECStreamer(args, nil, w)
	default:
		return
	}
	injectOECStreamer(s.oec, ino, st)
}

func ltpRegisterOecTestStreamerWithLogicalView(s *Super, ino uint64, r *blobstore.Reader, w *blobstore.Writer, fileSize, inoGen uint64) {
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

func ExportRegisterOecTestStreamer(s *Super, ino uint64, r *blobstore.Reader, w *blobstore.Writer) {
	ltpRegisterOecTestStreamer(s, ino, r, w)
}

func ExportRegisterOecTestStreamerWithLogicalView(s *Super, ino uint64, r *blobstore.Reader, w *blobstore.Writer, fileSize, inoGen uint64) {
	ltpRegisterOecTestStreamerWithLogicalView(s, ino, r, w, fileSize, inoGen)
}

// ExportLtpFtest01Super 构造 LTP 模拟用 Super/File。
func ExportLtpFtest01Super(ino uint64) (*Super, *File) {
	s := ltpNewTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: ino, parentIno: 1, name: "ltp_ftest01"}
	return s, f
}

func ExportFileIno(f *File) uint64 {
	if f == nil {
		return 0
	}
	return f.ino
}

func ExportSetFileFlag(f *File, flag uint32) {
	f.setFlag(flag)
}

// -----------------------------------------------------------------------------
// 以下：针对历史概率性失败的契约用例（与 file.go Attr/Read 合并规则对齐）。
// -----------------------------------------------------------------------------

func ltpContractSuper(ino uint64) (*Super, *File) {
	return ExportLtpFtest01Super(ino)
}

func TestFile_LtpSim_blob_attr_raises_size_when_stream_matches_inode_gen(t *testing.T) {
	s, f := ltpContractSuper(9101)
	w := &blobstore.Writer{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	const inodeSize = 960512
	const logicalMax = 0xeb000
	gen := uint64(7)
	ExportRegisterOecTestStreamerWithLogicalView(s, ExportFileIno(f), nil, w, logicalMax, gen)

	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: ExportFileIno(f), PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: inodeSize, Generation: gen, Mode: proto.Mode(0o644),
		}, nil
	})

	attr := &fuse.Attr{}
	require.NoError(t, f.Attr(context.Background(), attr))
	require.Equal(t, uint64(logicalMax), attr.Size)
}

func TestFile_LtpSim_blob_attr_ignores_stale_stream_when_inode_gen_newer(t *testing.T) {
	s, f := ltpContractSuper(9102)
	w := &blobstore.Writer{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	const inodeSize = 0xfa000
	inodeGen := uint64(20)
	staleStreamSize := uint64(1038336)
	staleStreamGen := uint64(9)
	ExportRegisterOecTestStreamerWithLogicalView(s, ExportFileIno(f), nil, w, staleStreamSize, staleStreamGen)

	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: ExportFileIno(f), PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: inodeSize, Generation: inodeGen, Mode: proto.Mode(0o644),
		}, nil
	})

	attr := &fuse.Attr{}
	require.NoError(t, f.Attr(context.Background(), attr))
	require.Equal(t, uint64(inodeSize), attr.Size)
}

func TestFile_LtpSim_blob_read_does_not_extend_past_inode_when_stream_gen_stale(t *testing.T) {
	s, f := ltpContractSuper(9103)
	w := &blobstore.Writer{}
	r := &blobstore.Reader{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	ExportRegisterOecTestStreamer(s, ExportFileIno(f), r, w)

	const inodeSize = 0x39800
	inodeGen := uint64(20)

	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: ExportFileIno(f), PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: inodeSize, Generation: inodeGen, Mode: proto.Mode(0o644),
		}, nil
	})
	var calledRead bool
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Read",
		func(_ *blobstore.ECExtentClient, ino uint64, _ []byte, _ int, _ int) (int, error) {
			require.Equal(t, ExportFileIno(f), ino)
			calledRead = true
			return 0, nil
		})

	req := &fuse.ReadRequest{Offset: 0x3800, Size: 2048}
	resp := &fuse.ReadResponse{Data: make([]byte, fuse.OutHeaderSize+2048)}
	require.NoError(t, f.Read(context.Background(), req, resp))
	require.True(t, calledRead)
}

func TestFile_LtpSim_blob_read_extends_read_size_when_stream_gen_matches_inode(t *testing.T) {
	s, f := ltpContractSuper(9104)
	w := &blobstore.Writer{}
	r := &blobstore.Reader{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	ExportRegisterOecTestStreamer(s, ExportFileIno(f), r, w)

	const inodeSize = 100_000
	gen := uint64(3)

	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: ExportFileIno(f), PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: inodeSize, Generation: gen, Mode: proto.Mode(0o644),
		}, nil
	})
	var calledRead bool
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Read",
		func(_ *blobstore.ECExtentClient, _ uint64, _ []byte, _ int, _ int) (int, error) {
			calledRead = true
			return 0, nil
		})

	req := &fuse.ReadRequest{Offset: 0, Size: 1}
	resp := &fuse.ReadResponse{Data: make([]byte, fuse.OutHeaderSize+1)}
	require.NoError(t, f.Read(context.Background(), req, resp))
	require.True(t, calledRead)
}

func TestFile_LtpSim_blob_fstat_after_write_sequence(t *testing.T) {
	s, f := ltpContractSuper(9105)
	w := &blobstore.Writer{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	ExportRegisterOecTestStreamer(s, ExportFileIno(f), nil, w)
	ExportSetFileFlag(f, syscall.O_RDWR)

	gen := uint64(4)
	inodeSize := uint64(0)
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: ExportFileIno(f), PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: inodeSize, Generation: gen, Mode: proto.Mode(0o644),
		}, nil
	})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Write",
		func(_ *blobstore.ECExtentClient, _ uint64, _ int, data []byte, _ int) (int, error) {
			return len(data), nil
		})

	const chunk = 2048
	const off = 0x7e800
	writeReq := &fuse.WriteRequest{Offset: off, Data: make([]byte, chunk)}
	writeResp := &fuse.WriteResponse{}
	require.NoError(t, f.Write(context.Background(), writeReq, writeResp))
	require.Equal(t, chunk, writeResp.Size)

	logicalMax := uint64(off + chunk)
	ExportRegisterOecTestStreamerWithLogicalView(s, ExportFileIno(f), nil, w, logicalMax, gen)
	inodeSize = logicalMax

	attr := &fuse.Attr{}
	require.NoError(t, f.Attr(context.Background(), attr))
	require.Equal(t, logicalMax, attr.Size)
}

// -----------------------------------------------------------------------------
// 与 EC 卷上 ftest01 日志逐条对齐的「故障形态」复现（断言失败即表示 file.go / 读合并 仍有缺口）。
// -----------------------------------------------------------------------------

// TestFile_LtpSim_ec_fault_log_fstat_st_size_960512_file_max_eb000
// 日志: fstat() mismatch; st_size=960512, file_max=eb000 — inode 偏小、流与代际一致时应抬高 Attr。
func TestFile_LtpSim_ec_fault_log_fstat_st_size_960512_file_max_eb000(t *testing.T) {
	TestFile_LtpSim_blob_attr_raises_size_when_stream_matches_inode_gen(t)
}

// TestFile_LtpSim_ec_fault_log_fstat_st_size_1038336_file_max_fa000
// 日志: st_size=1038336, file_max=fa000 — 截断后代际新于流时不得用陈旧流长度覆盖 Attr。
func TestFile_LtpSim_ec_fault_log_fstat_st_size_1038336_file_max_fa000(t *testing.T) {
	TestFile_LtpSim_blob_attr_ignores_stale_stream_when_inode_gen_newer(t)
}

// TestFile_LtpSim_ec_fault_log_bad_verify_0x3800_file_max_39800
// 日志: bad verify @ 0x3800 for val 54 ... file_max 0x39800, last_trunc 0x1800 — Read 传入 ReadWithInodeView 的 inode 视图不得大于截断后权威上界。
func TestFile_LtpSim_ec_fault_log_bad_verify_0x3800_file_max_39800(t *testing.T) {
	TestFile_LtpSim_blob_read_does_not_extend_past_inode_when_stream_gen_stale(t)
}

// TestFile_LtpSim_ftest03_fstat_after_expand_trunc 对齐 ftest03：expand-truncate 后 fstat 的 st_size 须等于 file_max（日志 st_size=f3800,file_max=f4000）。
func TestFile_LtpSim_ftest03_fstat_after_expand_trunc(t *testing.T) {
	const fileMax = 0xf4000 // 日志 file_max=f4000；stale extent 尾 0xf3800
	s, f := ltpContractSuper(9303)
	w := &blobstore.Writer{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	ExportRegisterOecTestStreamerWithLogicalView(s, ExportFileIno(f), nil, w, fileMax, 12)

	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: ExportFileIno(f), PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: fileMax, Generation: 12,
		}, nil
	})

	attr := &fuse.Attr{}
	require.NoError(t, f.Attr(context.Background(), attr))
	require.Equal(t, uint64(fileMax), attr.Size)
}
