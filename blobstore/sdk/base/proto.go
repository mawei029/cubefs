package base

import (
	"io"

	"github.com/cubefs/cubefs/blobstore/common/codemode"
	"github.com/cubefs/cubefs/blobstore/common/proto"
)

type GetShardMode int

const (
	GetShardModeLeader = GetShardMode(iota + 1)
	GetShardModeRandom
)

type CreateBlobArgs struct {
	BlobName []byte
	CodeMode codemode.CodeMode
}

type ListBlobArgs struct {
	Prefix string
	Marker string
	Count  int
}

type ListBlobResponse struct {
	Blobs      []proto.Blob
	NextMarker string
}

type StatBlobArgs struct {
	BlobName  []byte
	ClusterID proto.ClusterID
}

type SealBlobArgs struct {
	BlobName  []byte
	ClusterID proto.ClusterID
	BlobSize  uint64
}

type GetBlobArgs struct {
	ClusterID proto.ClusterID
	BlobName  []byte
	Offset    uint64
	ReadSize  uint64
	Writer    io.Writer
	Mode      GetShardMode
	ShardKeys [][]byte
}

// IsValid is valid get args
func (args *GetBlobArgs) IsValid() bool {
	if args == nil {
		return false
	}
	return args.ClusterID != 0
}

type DelBlobArgs struct {
	BlobName  []byte
	ClusterID proto.ClusterID
}

type PutBlobArgs struct {
	BlobName []byte
	CodeMode codemode.CodeMode
	NeedSeal bool

	Size uint64
	Body io.Reader
}