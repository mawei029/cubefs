package stream

import (
	"context"

	"github.com/cubefs/cubefs/sdk/meta"
)

// ExtentClientAPI 按 inode 的卷级 extent 客户端外向 API：与 *ExtentClient 上 ec.Read / ec.Write / ec.Flush / ec.Truncate / ec.OpenStream 等用法一致；
// 副本 *ExtentClient 与 EC *blobstore.ECExtentClient 均实现本接口（EC 打开 Blob 流须走 File.openOECStream / OpenStreamWithArgs；单测可用 ECExtentClient.SetStreamer 注入假流）。
type ExtentClientAPI interface {
	OpenStream(inode uint64, openForWrite, isCache bool, fullPath string) error
	CloseStream(inode uint64) error
	EvictStream(inode uint64) error
	RefCnt(inode uint64) int32

	Read(inode uint64, data []byte, offset int, size int, poolId uint8, isMigration bool) (int, error)
	Write(inode uint64, offset int, data []byte, flags int, checkFunc func() error,
		poolId uint8, storageClass uint32, isMigration, waitForFlush bool) (int, error)
	Flush(inode uint64) error
	Truncate(mw *meta.MetaWrapper, parentIno uint64, inode uint64, size int, fullPath string) error
}

// ECStreamerAPI 单 inode EC 流上的外向 IO（Read/Write 不带 inode 参数，接收者即该 inode）；由 *blobstore.ECStreamer 实现。
type ECStreamerAPI interface {
	Inode() uint64
	Open(openForWrite bool) error
	Release() error
	RefCnt() int32
	Evict() error

	Read(ctx context.Context, dst []byte, offset int, size int, poolId uint8, isMigration bool) (int, error)
	Write(ctx context.Context, offset int, data []byte, flags int, checkFunc func() error, storageClass uint32, isMigration bool) (int, error)
	Flush(ctx context.Context) error
	Truncate(ctx context.Context, size int, fullPath string) error
}
