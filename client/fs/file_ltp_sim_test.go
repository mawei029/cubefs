package fs

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/depends/bazil.org/fuse"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// 本文件目标：在 client/fs 侧用 UT 尽量替代「EC 卷上跑 LTP ftest01」，把概率问题变成可回归断言。
//
// 与 ftest01.c 日志的对应关系（便于搜日志对用例）：
//   -「fstat() mismatch; st_size=…, file_max=…」→ domisc(m_fstat) 与 TestFile_LtpSim_ec_fault_* / blob_attr_*。
//   -「bad verify @ … should be 0」→ 洞区读到旧图案；主循环里 assert + Read 路径「读上界不得越过权威 meta」。
//   - fork/wait → TestFile_LtpSimFtest01_forkWait（exec 子进程隔离 gomonkey）。
//
// 运行：go test ./client/fs -run 'TestFile_LtpSim' -v
//      go test ./client/fs -short  # fork 用例跳过
// 环境：CUBEFS_LTP_SIM_ITERATIONS  外层迭代；CUBEFS_LTP_SIM_RELAX_READ_GUARD=1 关闭读上界严格校验（仅排障）。
// -----------------------------------------------------------------------------

const (
	ltpSimEnvChild          = "CUBEFS_LTP_SIM_CHILD"
	ltpSimEnvMe             = "CUBEFS_LTP_SIM_ME"
	ltpSimEnvSeed           = "CUBEFS_LTP_SIM_SEED"
	ltpSimEnvIter           = "CUBEFS_LTP_SIM_ITERATIONS"
	ltpSimEnvRelaxReadGuard = "CUBEFS_LTP_SIM_RELAX_READ_GUARD" // 设为 1 时关闭 Read inodeView 上界校验（仅排障）
)

func ltpSimParsePositiveIntEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func ltpReadViewGuardEnabled() bool {
	return os.Getenv(ltpSimEnvRelaxReadGuard) != "1"
}

type ltpFtest01VM struct {
	mu  sync.Mutex
	buf []byte
	// meta 视图：与 Attr / Read 注入一致
	metaSize uint64
	metaGen  uint64
}

func (vm *ltpFtest01VM) truncate(target uint64) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if target < uint64(len(vm.buf)) {
		for i := target; i < vm.metaSize && i < uint64(len(vm.buf)); i++ {
			vm.buf[i] = 0
		}
	}
	vm.metaSize = target
	vm.metaGen++
}

func (vm *ltpFtest01VM) writeAt(off int, p []byte) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	end := off + len(p)
	if end > len(vm.buf) {
		end = len(vm.buf)
	}
	copy(vm.buf[off:end], p[:end-off])
	if uint64(end) > vm.metaSize {
		vm.metaSize = uint64(end)
	}
}

func (vm *ltpFtest01VM) readAt(off int, p []byte, inodeViewSize uint64) int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if off >= int(inodeViewSize) {
		return 0
	}
	lim := int64(inodeViewSize) - int64(off)
	if lim <= 0 {
		return 0
	}
	n := len(p)
	if int64(n) > lim {
		n = int(lim)
	}
	copy(p, vm.buf[off:off+n])
	return n
}

func ltpFtest01Super(ino uint64) (*Super, *File) {
	s := newTestSuperForFile()
	s.volType = proto.VolumeTypeCold
	s.poolCache = map[uint8]*proto.StoragePoolInfo{
		1: {Id: 1, StorageClass: uint8(proto.StorageClass_BlobStore)},
	}
	f := &File{super: s, ino: ino, parentIno: 1, name: "ltp_ftest01"}
	return s, f
}

func ltpSetattrSize(t *testing.T, f *File, size uint64) {
	t.Helper()
	req := &fuse.SetattrRequest{Valid: fuse.SetattrSize, Size: size}
	resp := &fuse.SetattrResponse{}
	require.NoError(t, f.Setattr(context.Background(), req, resp))
}

func ltpInstallFtest01Patches(t *testing.T, patches *gomonkey.Patches, s *Super, f *File, w *blobstore.Writer, vm *ltpFtest01VM) {
	t.Helper()
	patchOecWriterForMisc(patches, w)
	registerOecTestStreamer(s, f.ino, nil, w)

	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		vm.mu.Lock()
		defer vm.mu.Unlock()
		return &proto.InodeInfo{
			Inode: f.ino, PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: vm.metaSize, Generation: vm.metaGen, Mode: proto.Mode(0o644),
		}, nil
	})

	patches.ApplyPrivateMethod(reflect.TypeOf((*File)(nil)), "doECTruncateV2",
		func(_ *File, ino uint64, targetSize uint64, _ string) error {
			require.Equal(t, f.ino, ino)
			vm.truncate(targetSize)
			if ww := s.oec.Writer(ino); ww != nil {
				ww.SetFileSize(targetSize)
			}
			return nil
		})

	patches.ApplyMethod(reflect.TypeOf(w), "Flush",
		func(_ *blobstore.Writer, _ uint64, _ context.Context) error { return nil })

	// flushNonReplicaWriter 在 Writer 非空时会调 oec.Flush；真实 Flush 要求 streamers[ino] 非 nil。
	// 本 harness 已桩 Writer/Write，这里直接收敛 Flush，避免 EBADF（与 ftest01 的 fsync 语义一致：成功即可）。
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Flush",
		func(_ *blobstore.ECExtentClient, _ uint64) error { return nil })

	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Write",
		func(_ *blobstore.ECExtentClient, ino uint64, offset int, data []byte, _ int, _ func() error, _ uint8, _ uint32, _, _ bool) (int, error) {
			require.Equal(t, f.ino, ino)
			vm.writeAt(offset, data)
			if ww := s.oec.Writer(ino); ww != nil {
				vm.mu.Lock()
				sz := vm.metaSize
				vm.mu.Unlock()
				ww.SetFileSize(sz)
			}
			return len(data), nil
		})

	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "ReadWithInodeView",
		func(_ *blobstore.ECExtentClient, _ context.Context, ino uint64, data []byte, offset int, size int, _ uint8, _ bool, _ uint64, inodeViewSize uint64) (int, error) {
			require.Equal(t, f.ino, ino)
			if ltpReadViewGuardEnabled() {
				vm.mu.Lock()
				meta := vm.metaSize
				vm.mu.Unlock()
				var wmax uint64
				if ww := s.oec.Writer(f.ino); ww != nil {
					wmax = uint64(ww.CacheFileSize())
				}
				auth := meta
				if wmax > auth {
					auth = wmax
				}
				// File.Read 合并后的读上界不得大于「权威 meta ∪ 写缓存尾」；若大于则易读到截断前物理残留（LTP bad verify）。
				if inodeViewSize > auth {
					require.FailNowf(t, "ReadWithInodeView 读上界异常",
						"inodeViewSize=0x%x > auth(meta∪cache)=0x%x (EC ftest01 bad verify / 洞区非零 常见根因)", inodeViewSize, auth)
				}
			}
			return vm.readAt(offset, data[:size], inodeViewSize), nil
		})

	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "FileSize",
		func(_ *blobstore.ECExtentClient, _ uint64) (int, uint64, bool) {
			vm.mu.Lock()
			defer vm.mu.Unlock()
			sz := int(vm.metaSize)
			if ww := s.oec.Writer(f.ino); ww != nil {
				if c := ww.CacheFileSize(); c > sz {
					sz = c
				}
			}
			return sz, vm.metaGen, true
		})
}

// ltpFtest01ClearBitsFromTrunc 对齐 ftest01.c domisc(m_trunc) 对 bitmap 的尾部清理。
func ltpFtest01ClearBitsFromTrunc(bits []byte, truncChunk, nchunks int) {
	chunk := truncChunk
	for chunk%8 != 0 && chunk < nchunks {
		bits[chunk/8] &^= byte(1 << (chunk % 8))
		chunk++
	}
	for chunk < nchunks {
		bits[chunk/8] = 0
		chunk += 8
	}
}

// ltpFtest01Domisc 对齐 ftest01.c domisc：fsync → trunc → sync → fstat 轮转。
func ltpFtest01Domisc(t *testing.T, f *File, miscType *int, fileMax *int64, lastTrunc *int64,
	bits []byte, nchunks, csize int, rng *rand.Rand, truncCount *int,
) {
	t.Helper()
	switch *miscType {
	case 0: // m_fsync
		require.NoError(t, f.Fsync(context.Background(), &fuse.FsyncRequest{Flags: syscall.O_RDWR}))
	case 1: // m_trunc
		if *fileMax < int64(csize) {
			break
		}
		n := int(*fileMax / int64(csize))
		if n <= 0 {
			break
		}
		tc := rng.Intn(n)
		newMax := int64(tc * csize)
		*lastTrunc = newMax
		*fileMax = newMax
		ltpFtest01ClearBitsFromTrunc(bits, tc, nchunks)
		ltpSetattrSize(t, f, uint64(newMax))
		*truncCount++
	case 2: // m_sync — 无单文件 syscall，略过
	case 3: // m_fstat
		attr := &fuse.Attr{}
		require.NoError(t, f.Attr(context.Background(), attr))
		require.Equal(t, uint64(*fileMax), attr.Size,
			"LTP domisc(m_fstat): st_size 须等于 file_max（模拟 ftest01.c:518-521）")
	}
	*miscType++
	if *miscType > 3 {
		*miscType = 0
	}
}

// runLtpFtest01Dotest 单进程 ftest01 主循环（参数缩小以控制 UT 耗时）。
func runLtpFtest01Dotest(t *testing.T, me int, seed int64, iterations, maxSize, csize, miscIntvl int) {
	t.Helper()
	require.Positive(t, csize)
	require.Zero(t, maxSize%csize)
	nchunks := maxSize / csize

	ino := uint64(9200 + me)
	s, f := ltpFtest01Super(ino)
	w := &blobstore.Writer{}
	vm := &ltpFtest01VM{buf: make([]byte, maxSize)}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	ltpInstallFtest01Patches(t, patches, s, f, w, vm)

	f.setFlag(syscall.O_RDWR)
	rng := rand.New(rand.NewSource(seed))
	nchild := 5
	val := byte((64/nchild)*me + 1) // 对齐 ftest01.c: val = (64 / testers) * me + 1

	bits := make([]byte, (nchunks+7)/8)
	valBuf := bytes.Repeat([]byte{val}, csize)
	zeroBuf := make([]byte, csize)

	miscType := 0
	whenmisc := rng.Intn(miscIntvl) + 5
	var truncCount int

	var fileMax int64
	var lastTrunc int64 = -1

	for it := 0; it < iterations; it++ {
		// 对齐 ftruncate(fd,0)：逻辑长度归零并清空旧字节（由补丁内 vm.truncate 完成）
		ltpSetattrSize(t, f, 0)
		fileMax = 0
		for i := range bits {
			bits[i] = 0
		}
		count := 0
		collide := 0

		for count < nchunks {
			chunk := rng.Intn(nchunks)
			off := int64(chunk * csize)

			req := &fuse.ReadRequest{Offset: off, Size: csize}
			resp := &fuse.ReadResponse{Data: make([]byte, fuse.OutHeaderSize+csize)}
			require.NoError(t, f.Read(context.Background(), req, resp))
			xfr := len(resp.Data) - fuse.OutHeaderSize
			buf := resp.Data[fuse.OutHeaderSize:]

			if off >= fileMax {
				bits[chunk/8] |= 1 << (chunk % 8)
				count++
			} else if bits[chunk/8]&(1<<(chunk%8)) == 0 {
				require.Equal(t, csize, xfr, "zero-read: xfr!=csize @0x%x me=%d", off, me)
				require.Truef(t, bytes.Equal(buf, zeroBuf),
					"bad verify hole @0x%x me=%d val=%d file_max=0x%x last_trunc=0x%x (对齐 ftest01.c:356-368)",
					off, me, val, fileMax, lastTrunc)
				bits[chunk/8] |= 1 << (chunk % 8)
				count++
			} else {
				require.Equal(t, csize, xfr, "val-read: xfr!=csize @0x%x me=%d", off, me)
				require.Truef(t, bytes.Equal(buf, valBuf), "bad verify val @0x%x me=%d", off, me)
				collide++
			}

			wr := &fuse.WriteRequest{Offset: off, Data: append([]byte(nil), valBuf...)}
			wresp := &fuse.WriteResponse{}
			require.NoError(t, f.Write(context.Background(), wr, wresp))
			require.Equal(t, csize, wresp.Size)

			end := off + int64(csize)
			if end > fileMax {
				fileMax = end
			}

			if miscIntvl > 0 {
				whenmisc--
				if whenmisc <= 0 {
					ltpFtest01Domisc(t, f, &miscType, &fileMax, &lastTrunc, bits, nchunks, csize, rng, &truncCount)
					whenmisc = rng.Intn(miscIntvl) + 5
				}
			}
			if count+collide > 2*nchunks {
				break
			}
		}

		require.NoError(t, f.Fsync(context.Background(), &fuse.FsyncRequest{Flags: syscall.O_RDWR}))
		val++
	}
	if testing.Verbose() {
		t.Logf("me=%d trunc_domisc=%d", me, truncCount)
	}
}

// TestFile_LtpSimFtest01_forkWait 子进程隔离跑 ftest01 风格循环，对齐「fork and wait」。
func TestFile_LtpSimFtest01_forkWait(t *testing.T) {
	if os.Getenv(ltpSimEnvChild) == "1" {
		me, err := strconv.Atoi(os.Getenv(ltpSimEnvMe))
		require.NoError(t, err)
		seed, err := strconv.ParseInt(os.Getenv(ltpSimEnvSeed), 10, 64)
		require.NoError(t, err)
		iter := ltpSimParsePositiveIntEnv(ltpSimEnvIter, 6)
		// misc_intvl=3 → 约每 3~7 步写就轮转 fsync/trunc/sync/fstat，trunc 密度高于默认 LTP misc_intvl=10
		runLtpFtest01Dotest(t, me, seed, iter, 64*1024, 2048, 3)
		return
	}
	if testing.Short() {
		t.Skip("LTP 模拟 fork 用例在 -short 下跳过")
	}
	nchild := 5
	for me := 0; me < nchild; me++ {
		me := me
		t.Run(fmt.Sprintf("child_%d", me), func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(os.Args[0], "-test.run=^TestFile_LtpSimFtest01_forkWait$", "-test.count=1", "-test.v=false")
			cmd.Env = append(os.Environ(),
				ltpSimEnvChild+"=1",
				fmt.Sprintf("%s=%d", ltpSimEnvMe, me),
				fmt.Sprintf("%s=%d", ltpSimEnvSeed, int64(424242+me*997)))
			if v := os.Getenv(ltpSimEnvIter); v != "" {
				cmd.Env = append(cmd.Env, ltpSimEnvIter+"="+v)
			}
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "child %d: %s", me, string(out))
		})
	}
}

// TestFile_LtpSimFtest01_dotestSingleThread 默认 CI 快速路径（单线程、无 exec）。
func TestFile_LtpSimFtest01_dotestSingleThread(t *testing.T) {
	iter := ltpSimParsePositiveIntEnv(ltpSimEnvIter, 4)
	runLtpFtest01Dotest(t, 0, 314159, iter, 48*1024, 2048, 2)
}

// -----------------------------------------------------------------------------
// 以下：针对历史概率性失败的契约用例（与 file.go Attr/Read 合并规则对齐）。
// -----------------------------------------------------------------------------

func ltpContractSuper(ino uint64) (*Super, *File) {
	return ltpFtest01Super(ino)
}

func TestFile_LtpSim_blob_attr_raises_size_when_stream_matches_inode_gen(t *testing.T) {
	s, f := ltpContractSuper(9101)
	w := &blobstore.Writer{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchOecWriterForMisc(patches, w)
	registerOecTestStreamer(s, f.ino, nil, w)

	const inodeSize = 960512
	const logicalMax = 0xeb000
	gen := uint64(7)

	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: f.ino, PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: inodeSize, Generation: gen, Mode: proto.Mode(0o644),
		}, nil
	})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "FileSize",
		func(_ *blobstore.ECExtentClient, _ uint64) (int, uint64, bool) {
			return logicalMax, gen, true
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
	patchOecWriterForMisc(patches, w)
	registerOecTestStreamer(s, f.ino, nil, w)

	const inodeSize = 0xfa000
	inodeGen := uint64(20)
	staleStreamSize := uint64(1038336)
	staleStreamGen := uint64(9)

	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: f.ino, PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: inodeSize, Generation: inodeGen, Mode: proto.Mode(0o644),
		}, nil
	})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "FileSize",
		func(_ *blobstore.ECExtentClient, _ uint64) (int, uint64, bool) {
			return int(staleStreamSize), staleStreamGen, true
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
	patchOecReaderWriterForMisc(patches, r, w)
	registerOecTestStreamer(s, f.ino, r, w)

	const inodeSize = 0x39800
	inodeGen := uint64(20)
	const staleLz = 0xfc800
	const staleLg uint64 = 5

	var gotInodeGen, gotInodeSize uint64
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: f.ino, PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: inodeSize, Generation: inodeGen, Mode: proto.Mode(0o644),
		}, nil
	})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "FileSize",
		func(_ *blobstore.ECExtentClient, _ uint64) (int, uint64, bool) {
			return staleLz, staleLg, true
		})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "ReadWithInodeView",
		func(_ *blobstore.ECExtentClient, _ context.Context, ino uint64, _ []byte, _ int, _ int, _ uint8, _ bool, inodeGenArg, inodeSizeArg uint64) (int, error) {
			require.Equal(t, f.ino, ino)
			gotInodeGen = inodeGenArg
			gotInodeSize = inodeSizeArg
			return 0, nil
		})

	req := &fuse.ReadRequest{Offset: 0x3800, Size: 2048}
	resp := &fuse.ReadResponse{Data: make([]byte, fuse.OutHeaderSize+2048)}
	require.NoError(t, f.Read(context.Background(), req, resp))
	require.Equal(t, inodeGen, gotInodeGen)
	require.Equal(t, uint64(inodeSize), gotInodeSize)
}

func TestFile_LtpSim_blob_read_extends_read_size_when_stream_gen_matches_inode(t *testing.T) {
	s, f := ltpContractSuper(9104)
	w := &blobstore.Writer{}
	r := &blobstore.Reader{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchOecReaderWriterForMisc(patches, r, w)
	registerOecTestStreamer(s, f.ino, r, w)

	const inodeSize = 100_000
	const logicalMax = 200_000
	gen := uint64(3)

	var gotInodeSize uint64
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: f.ino, PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: inodeSize, Generation: gen, Mode: proto.Mode(0o644),
		}, nil
	})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "FileSize",
		func(_ *blobstore.ECExtentClient, _ uint64) (int, uint64, bool) {
			return logicalMax, gen, true
		})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "ReadWithInodeView",
		func(_ *blobstore.ECExtentClient, _ context.Context, _ uint64, _ []byte, _ int, _ int, _ uint8, _ bool, _, inodeSizeArg uint64) (int, error) {
			gotInodeSize = inodeSizeArg
			return 0, nil
		})

	req := &fuse.ReadRequest{Offset: 0, Size: 1}
	resp := &fuse.ReadResponse{Data: make([]byte, fuse.OutHeaderSize+1)}
	require.NoError(t, f.Read(context.Background(), req, resp))
	require.Equal(t, uint64(logicalMax), gotInodeSize)
}

func TestFile_LtpSim_blob_fstat_after_write_sequence(t *testing.T) {
	s, f := ltpContractSuper(9105)
	w := &blobstore.Writer{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchOecWriterForMisc(patches, w)
	registerOecTestStreamer(s, f.ino, nil, w)
	f.setFlag(syscall.O_RDWR)

	gen := uint64(4)
	inodeSize := uint64(0)
	patches.ApplyMethod(reflect.TypeOf(s), "InodeGet", func(_ *Super, _ uint64) (*proto.InodeInfo, error) {
		return &proto.InodeInfo{
			Inode: f.ino, PoolId: 1, StorageClass: proto.StorageClass_BlobStore,
			Size: inodeSize, Generation: gen, Mode: proto.Mode(0o644),
		}, nil
	})
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "Write",
		func(_ *blobstore.ECExtentClient, _ uint64, _ int, data []byte, _ int, _ func() error, _ uint8, _ uint32, _, _ bool) (int, error) {
			return len(data), nil
		})

	const chunk = 2048
	const off = 0x7e800
	writeReq := &fuse.WriteRequest{Offset: off, Data: make([]byte, chunk)}
	writeResp := &fuse.WriteResponse{}
	require.NoError(t, f.Write(context.Background(), writeReq, writeResp))
	require.Equal(t, chunk, writeResp.Size)

	logicalMax := uint64(off + chunk)
	w.SetFileSize(logicalMax)
	patches.ApplyMethod(reflect.TypeOf((*blobstore.ECExtentClient)(nil)), "FileSize",
		func(_ *blobstore.ECExtentClient, _ uint64) (int, uint64, bool) {
			return int(logicalMax), gen, true
		})

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

// TestFile_LtpSim_ec_fault_deterministic_trunc_then_read_hole
// 确定性脚本：稀疏写 → 截断缩短 → 读从未写过的洞 chunk，须全零（ftest01 在 trunc 后清 bitmap 再读洞）。
func TestFile_LtpSim_ec_fault_deterministic_trunc_then_read_hole(t *testing.T) {
	const csize = 2048
	const maxSize = 16 * csize
	ino := uint64(9301)
	s, f := ltpFtest01Super(ino)
	w := &blobstore.Writer{}
	vm := &ltpFtest01VM{buf: make([]byte, maxSize)}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	ltpInstallFtest01Patches(t, patches, s, f, w, vm)
	f.setFlag(syscall.O_RDWR)

	val := byte(54)
	valBuf := bytes.Repeat([]byte{val}, csize)
	// 稀疏写：只写 chunk 5、9，使 file_max 延伸到 10*csize
	for _, chunk := range []int{5, 9} {
		off := int64(chunk * csize)
		wr := &fuse.WriteRequest{Offset: off, Data: valBuf}
		resp := &fuse.WriteResponse{}
		require.NoError(t, f.Write(context.Background(), wr, resp))
		require.Equal(t, csize, resp.Size)
	}
	// 截短：丢掉高地址已写区，使 chunk2 仍在文件内且从未写过 → 洞
	const newMax = 6 * csize
	require.Less(t, newMax, 9*csize, "trunc 后应裁掉 chunk9")
	ltpSetattrSize(t, f, uint64(newMax))

	holeChunk := 2
	holeOff := int64(holeChunk * csize)
	require.Less(t, holeOff+int64(csize), int64(newMax), "洞 chunk 须完全落在截断后文件内")

	req := &fuse.ReadRequest{Offset: holeOff, Size: csize}
	resp := &fuse.ReadResponse{Data: make([]byte, fuse.OutHeaderSize+csize)}
	require.NoError(t, f.Read(context.Background(), req, resp))
	got := resp.Data[fuse.OutHeaderSize:]
	require.Truef(t, bytes.Equal(got, make([]byte, csize)),
		"截断后洞区须全零（对应 LTP bad verify @ 0x%x / val %d）", holeOff, val)
}
