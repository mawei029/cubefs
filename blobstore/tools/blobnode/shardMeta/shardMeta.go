package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bnapi "github.com/cubefs/cubefs/blobstore/api/blobnode"
	cmapi "github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/blobnode/core"
	"github.com/cubefs/cubefs/blobstore/blobnode/core/disk"
	"github.com/cubefs/cubefs/blobstore/blobnode/core/storage"
	"github.com/cubefs/cubefs/blobstore/blobnode/db"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/util/log"
)

const (
	dataPath   = "data"
	metaPath   = "meta/superblock"
	timeFormat = "2006-01-02 15:04:05"
)

var (
	logLevel = flag.Int("level", 2, "log level[0:debug,1:info,2:warn,3:err]")
	diskDir  = flag.String("disk", "/home/service/var/data1", "disk dir root path")
	// metaPath = flag.String("meta", "meta/superblock", "meta super block path")

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
	chunkId := findChunkName(args)
	cfg := core.Config{MetaConfig: db.MetaConfig{
		Sync:         true,
		LRUCacheSize: 16 << 20, // 256 M
	}}
	s, err := loadSuperBlock(args.mPath, &cfg)
	if err != nil {
		panic(err)
	}
	defer s.Close(ctx)
	sDb := s.GetDb()

	cm, err := storage.NewChunkMeta(ctx, &cfg, core.VuidMeta{DiskID: args.diskId, ChunkID: chunkId}, sDb)
	log.Infof("cm=%+v, err=%+v", cm, err)
	fmt.Println("get chunk meta db success")

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
	dPath  string
	mPath  string
}

func checkConf() *opArgs {
	// *metaPath = "/home/mw/Documents/testChunk/meta/superblock"
	// *diskDir = "/home/mw/Documents/testChunk"
	// *vuid = 15612424224777
	// *bid = 1473238

	if !proto.Vuid(*vuid).IsValid() {
		panic("err invalid vuid")
	}
	if *bid == 0 {
		panic("err bid")
	}
	dPath := filepath.Join(*diskDir, dataPath)
	mPath := filepath.Join(*diskDir, metaPath)
	checkPathExist(dPath)
	checkPathExist(mPath)

	return &opArgs{
		diskId: proto.DiskID(*diskId),
		vuid:   proto.Vuid(*vuid),
		bid:    proto.BlobID(*bid),
		dPath:  dPath,
		mPath:  mPath,
	}
}

func findChunkName(args *opArgs) cmapi.ChunkID {
	files, err := os.ReadDir(args.dPath)
	if err != nil {
		panic(err)
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}
		chunkId := parseChunkNameStr(file.Name())
		// vuidStr := fmt.Sprintf(chunkId.VolumeUnitId().ToString())
		if chunkId.VolumeUnitId() == args.vuid {
			fmt.Printf("find vuid:%d, chunk file:%s \n", args.vuid, chunkId)
			return *chunkId // find vuid
		}
	}

	panic("can not find vuid -> chunk file")
}

func parseChunkNameStr(name string) *cmapi.ChunkID {
	chunkId := &cmapi.ChunkID{}
	if err := chunkId.Unmarshal([]byte(name)); err != nil {
		panic(err)
	}

	natureTm := time.Unix(0, int64(chunkId.UnixTime())).Format(timeFormat)
	log.Infof("chunkStr=%s, vuid=%d, idx=%d, time=%s", chunkId, chunkId.VolumeUnitId(), chunkId.VolumeUnitId().Index(), natureTm)
	return chunkId
}

func loadSuperBlock(metaPath string, cfg *core.Config) (s *disk.SuperBlock, err error) {
	// load super block，create or open
	sb, err := disk.NewSuperBlock(metaPath, cfg)
	if err != nil {
		panic(err)
	}
	return sb, nil
}

func checkPathExist(path string) {
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Fatal("path does not exist.")
		} else {
			log.Fatal("Error checking path existence: %+v", err)
		}
	}
}
