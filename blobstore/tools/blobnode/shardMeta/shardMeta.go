package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	bnapi "github.com/cubefs/cubefs/blobstore/api/blobnode"
	"github.com/cubefs/cubefs/blobstore/blobnode/core"
	"github.com/cubefs/cubefs/blobstore/blobnode/core/disk"
	"github.com/cubefs/cubefs/blobstore/blobnode/core/storage"
	"github.com/cubefs/cubefs/blobstore/blobnode/db"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/util/log"
)

var (
	logLevel = flag.Int("level", 1, "log level[0:debug,1:info,2:warn,3:err]")
	metaPath = flag.String("meta", "/home/service/var/data1/meta/superblock", "meta super block path")

	diskId = flag.Int("diskid", 1, "disk id")
	vuid   = flag.Int64("vuid", 0, "vuid")
	bid    = flag.Int64("bid", 0, "shard bid")
	mode   = flag.Int("mode", 0, "mode[0:get, 1:set]")
	setVal = flag.Int("setVal", 0, "shard status[0:default, 1:normal, 2:mark delete]")
)

func main() {
	flag.Parse()
	args := checkConf()
	log.SetOutputLevel(log.Level(*logLevel))

	ctx := context.Background()
	chunkId := bnapi.NewChunkId(args.vuid)
	cfg := core.Config{MetaConfig: db.MetaConfig{
		Sync:         true,
		LRUCacheSize: 16 << 20, // 256 M
	}}
	s, err := loadSuperBlock(&cfg)
	if err != nil {
		panic(err)
	}
	defer s.Close(ctx)
	sDb := s.GetDb()

	cm, err := storage.NewChunkMeta(ctx, &cfg, core.VuidMeta{DiskID: args.diskId, ChunkId: chunkId}, sDb)
	fmt.Printf("cm=%+v, err=%+v \n", cm, err)

	shardMeta, err := cm.Read(ctx, args.bid)
	switch *mode {
	case 0:
		fmt.Printf("rdb shard meta=%+v, err=%+v \n", shardMeta, err)
		return
	case 1:
		if err != nil {
			panic(err)
		}
		shardMeta.Flag = bnapi.ShardStatus(*setVal)
		err = cm.Write(ctx, args.bid, shardMeta)
		fmt.Printf("opration args:%+v, setVal:%d, err:%+v \n", args, *setVal, err)
		return
	}
}

type opArgs struct {
	diskId proto.DiskID
	vuid   proto.Vuid
	bid    proto.BlobID
}

func checkConf() *opArgs {
	if proto.Vuid(*vuid).IsValid() {
		panic("err invalid vuid")
	}

	return &opArgs{
		diskId: proto.DiskID(*diskId),
		vuid:   proto.Vuid(*vuid),
		bid:    proto.BlobID(*bid),
	}
}

func loadSuperBlock(cfg *core.Config) (s *disk.SuperBlock, err error) {
	checkPathExist(*metaPath)

	// load super block，create or open
	sb, err := disk.NewSuperBlock(*metaPath, cfg)
	if err != nil {
		panic(err)
		return nil, err
	}
	return sb, nil
}

func checkPathExist(path string) bool {
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Fatal("path does not exist.")
		} else {
			log.Fatal("Error checking path existence: %+v", err)
		}
		return false
	}
	return true
}
