package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cubefs/cubefs/blobstore/api/blobnode"
	cmapi "github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/common/config"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/util/log"
)

var (
	confFile    = flag.String("f", "blobnode_test.conf", "config file path")
	dataSize    = flag.String("d", "4B", "data size")
	readFile    = flag.String("r", "test.log", "read data from file path")
	concurrency = flag.Int("c", 1, "go concurrency")

	dataBuff []byte
	conf     BlobnodeTestConf
	mgr      *BlobnodeMgr

	fileSize = map[string]int{
		"4B":   4,
		"4K":   1 << 12,
		"64K":  1 << 15,
		"128K": 1 << 16,
		"1M":   1 << 20,
		"4M":   1 << 22,
		"8M":   1 << 23,
		"16M":  1 << 24,
	}
)

type BlobnodeTestConf struct {
	ClusterID  proto.ClusterID `json:"cluster_id"` // uint32
	LogLevel   log.Level       `json:"log_level"`  // int
	Host       string          `json:"host"`       // dist blobnode host
	ClusterMgr cmapi.Config    `json:"cluster_mgr"`
}

type BlobnodeMgr struct {
	clusterID proto.ClusterID
	host      string
	//hostMap   map[string][]*client.DiskInfoSimple
	hostDiskMap map[string][]*blobnode.DiskInfo // host -> disks
	//diskMap       map[proto.DiskID]*blobnode.DiskInfo      // disk id -> disk info
	diskMap       []*blobnode.DiskInfo
	diskChunkMap  map[proto.DiskID][]*blobnode.ChunkInfo   // disk id -> chunks
	chunkMap      map[blobnode.ChunkId]*blobnode.ChunkInfo // chunk id -> chunk info
	clusterMgrCli *cmapi.Client
	blobnodeCli   blobnode.StorageAPI
}

// POST /shard/put/diskid/{diskid}/vuid/{vuid}/bid/{bid}/size/{size}?iotype={iotype}
// GET /shard/stat/diskid/{diskid}/vuid/{vuidValue}/bid/{bidValue}
// GET /shard/list/diskid/{diskid}/vuid/{vuid}/startbid/{bid}/status/{status}/count/{count}
// GET /shard/get/diskid/{diskid}/vuid/{vuid}/bid/{bid}?iotype={iotype}
// POST /chunk/create/diskid/:diskid/vuid/:vuid"
// POST  /chunk/release/diskid/:diskid/vuid/:vuid
// POST "/shard/markdelete/diskid/:diskid/vuid/:vuid/bid/:bid"
// POST "/shard/delete/diskid/:diskid/vuid/:vuid/bid/:bid"
func main() {
	// 1. flag parse
	flag.Parse()
	if err := invalidArgs(); err != nil {
		panic(err)
	}

	// 2. init mgr, read data
	ctx := context.Background()
	initConfMgr(ctx) // 根据host拿到该节点的disk，拿到vuid
	initData()       // 用本地file的构造data数据

	printDebugInfo() // debug

	// 生成唯一bid, 开始put, 记录location. 空间不够时候则申请chunk
	mgr.put(ctx)

	// 根据location, 开始Get
	mgr.get(ctx)

	// delete shard bid
	mgr.delete(ctx)
}

func invalidArgs() error {
	return nil
}

func initConfMgr(ctx context.Context) {
	confBytes, err := ioutil.ReadFile(*confFile)
	if err != nil {
		log.Fatalf("read config file failed, filename: %s, err: %v", *confFile, err)
	}

	fmt.Printf("Config file %s:\n%s \n", *confFile, confBytes)
	if err = config.LoadData(&conf, confBytes); err != nil {
		log.Fatalf("load config failed, error: %+v", err)
	}
	log.SetOutputLevel(conf.LogLevel)
	fmt.Printf("Config: %+v \n", conf)

	//clusterMgrCli := client.NewClusterMgrClient(&conf.ClusterMgr)
	//disks, err := clusterMgrCli.ListClusterDisks(ctx)
	//if err != nil {
	//	log.Fatalf("Fail to list cluster disk, err: %+v", err)
	//}
	//mgr = newBlobnodeMgr(disks, conf.ClusterID)
	mgr = newBlobnodeMgr(ctx)
}

func initData() {
	size := fileSize[strings.ToUpper(*dataSize)]
	if size == 0 {
		log.Fatalf("not support data size %d", *dataSize)
	}

	f, err := os.Open(*readFile)
	if err != nil {
		log.Fatalf("%+v", err)
	}

	dataBuff = make([]byte, size) //buff := make([]byte, size)
	n, err := f.Read(dataBuff)
	if err != nil {
		log.Fatalf("%d, %+v", n, err)
	}
	log.Infof("read file, dataBuff len=%d (which size will be put) ", len(dataBuff))
}

//func newBlobnodeMgr(disks []*client.DiskInfoSimple, cid proto.ClusterID) *BlobnodeMgr {
func newBlobnodeMgr(ctx context.Context) *BlobnodeMgr {
	mgr = &BlobnodeMgr{
		clusterID:     conf.ClusterID,
		host:          conf.Host,
		hostDiskMap:   make(map[string][]*blobnode.DiskInfo),
		diskMap:       make([]*blobnode.DiskInfo, 0),
		diskChunkMap:  make(map[proto.DiskID][]*blobnode.ChunkInfo),
		chunkMap:      make(map[blobnode.ChunkId]*blobnode.ChunkInfo),
		blobnodeCli:   blobnode.New(&blobnode.Config{}),
		clusterMgrCli: cmapi.New(&conf.ClusterMgr),
	}

	disks, err := mgr.clusterMgrCli.ListHostDisk(ctx, conf.Host)
	if err != nil {
		log.Fatalf("Fail to list cluster disk, err: %+v", err)
	}

	mgr.diskMap = make([]*blobnode.DiskInfo, 0, len(disks))
	for i := range disks {
		if mgr.clusterID != disks[i].ClusterID {
			log.Errorf("the disk does not belong to this cluster: cluster_id[%d], disk[%+v]", mgr.clusterID, disks[i])
			continue
		}
		mgr.addDisks(disks[i])
		mgr.addChunks(ctx, disks[i])
	}

	mgr.sortDisk()

	return mgr
}

//func (mgr *BlobnodeMgr) addDisks(disk *client.DiskInfoSimple) {
func (mgr *BlobnodeMgr) addDisks(disk *blobnode.DiskInfo) {
	//idc := disk.Idc
	//if _, ok := mgr.diskMap[idc]; !ok {
	//	mgr.diskMap[disk.Idc] = []*client.DiskInfoSimple{}
	//}
	//mgr.diskMap[idc] = append(mgr.diskMap[idc], disk)
	//
	//if _, ok := mgr.hostMap[disk.Host]; !ok {
	//	mgr.hostMap[disk.Host] = []*client.DiskInfoSimple{}
	//}
	//mgr.hostMap[disk.Host] = append(mgr.hostMap[disk.Host], disk)
	host := disk.Host
	if _, ok := mgr.hostDiskMap[host]; !ok {
		mgr.hostDiskMap[host] = []*blobnode.DiskInfo{}
	}
	mgr.hostDiskMap[host] = append(mgr.hostDiskMap[host], disk)

	// mgr.diskMap[disk.DiskID] = disk
	mgr.diskMap = append(mgr.diskMap, disk)
}

func (mgr *BlobnodeMgr) addChunks(ctx context.Context, disk *blobnode.DiskInfo) {
	cis, err := mgr.blobnodeCli.ListChunks(ctx, conf.Host, &blobnode.ListChunkArgs{DiskID: disk.DiskID})
	if err != nil {
		log.Fatalf("Fail to list host disk chunks, err: %+v", err)
	}

	mgr.diskChunkMap[disk.DiskID] = cis
	for i, v := range cis {
		mgr.chunkMap[v.Id] = cis[i]
	}
}

func (mgr *BlobnodeMgr) sortDisk() {
	sort.SliceStable(mgr.diskMap, func(i, j int) bool {
		return mgr.diskMap[i].DiskID < mgr.diskMap[j].DiskID
	})

	for _, chunks := range mgr.diskChunkMap {
		sort.SliceStable(chunks, func(i, j int) bool {
			return chunks[i].Free > chunks[j].Free
		})
	}
}

// POST /shard/put/diskid/{diskid}/vuid/{vuid}/bid/{bid}/size/{size}?iotype={iotype}
func (mgr *BlobnodeMgr) put(ctx context.Context) {
	bidStart := genId()
	size := fileSize[strings.ToUpper(*dataSize)]
	cnt := uint64(0)

	//for _, disks := range mgr.hostDiskMap {
	//	for _, disk := range disks {
	for _, disk := range mgr.diskMap {
		go func() { // one disk, one go concurrency
			for _, chunk := range mgr.diskChunkMap[disk.DiskID] {
				if chunk.Free < uint64(size) {
					continue
				}

				urlStr := fmt.Sprintf("%v/shard/put/diskid/%v/vuid/%v/bid/%v/size/%v?iotype=%d",
					mgr.host, disk.DiskID, chunk.Vuid, bidStart+cnt, size, blobnode.NormalIO)
				doPost(urlStr)

				urlStr = fmt.Sprintf("%v/shard/markdelete/diskid/%v/vuid/%v/bid/%v", mgr.host, disk.DiskID, chunk.Vuid, bidStart+cnt)
				doPost(urlStr)
				urlStr = fmt.Sprintf("%v/shard/delete/diskid/%v/vuid/%v/bid/%v", mgr.host, disk.DiskID, chunk.Vuid, bidStart+cnt)
				doPost(urlStr)

				cnt++
				return
			}
		}()
	}
	//	}
	//}
}

func (mgr *BlobnodeMgr) get(ctx context.Context) {

}

func (mgr *BlobnodeMgr) delete(ctx context.Context) {

}

func genId() uint64 {
	//uid, _ := uuid.New().MarshalBinary()
	//return binary.LittleEndian.Uint64(uid[0:8])

	//uid := time.Now().UnixMilli() // +2
	uid := time.Now().Unix() // +5
	return uint64(uid*100000) + uint64(rand.Intn(10000))
}

func doPost(url string) {
	log.Infof("put once, %s", url)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(dataBuff))
	if err != nil {
		panic(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(dataBuff))
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	//req.ContentLength = args.Size
	//err = c.DoWith(ctx, req, ret, rpc.WithCrcEncode())
	//if err == nil {
	//	crc = ret.Crc
	//}

	if resp.StatusCode != http.StatusOK {
		log.Warnf("fail to put, status code:%d", resp.StatusCode)
	}

	buf := make([]byte, resp.ContentLength)
	if resp.ContentLength > 0 && resp.Body != nil {
		io.LimitReader(resp.Body, resp.ContentLength).Read(buf)
	}
	log.Debugf("do post, resp body: %s, resp:%+v", string(buf), resp)
}

func printDebugInfo() {
	log.Debugf("mgr, host: %s, clusterID: %d", mgr.host, mgr.clusterID)
	log.Debugf("hostDiskMap:%+v, diskMap:%+v, lenDisk:%d ", mgr.hostDiskMap, mgr.diskMap, len(mgr.diskMap))
	for idx, val := range mgr.diskMap {
		log.Debugf("idx:%d, ID:%d, disk:%+v", idx, val.DiskID, *val)
	}

	for id, val := range mgr.diskChunkMap {
		if len(val) > 0 {
			log.Debugf("diskId:%d, lenChunk:%d, chunk[0] free:%d", id, len(val), val[0].Free)
		} else {
			log.Debugf("diskId:%d, lenChunk:%d", id, len(val))
		}
	}

	for id, val := range mgr.chunkMap {
		log.Debugf("Id:%s, chunk:%+v", id.String(), *val)
		break
	}
}
