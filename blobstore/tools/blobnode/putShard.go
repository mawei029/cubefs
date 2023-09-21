package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cubefs/cubefs/blobstore/api/blobnode"
	cmapi "github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/common/config"
	errorcode "github.com/cubefs/cubefs/blobstore/common/errors"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/util/log"
	"github.com/cubefs/cubefs/util/errors"
)

var (
	confFile    = flag.String("f", "blobnode_test.conf", "config file path")
	dataSize    = flag.String("d", "4B", "data size")
	readFile    = flag.String("r", "test.log", "read data from file path")
	concurrency = flag.Int("c", 1, "go concurrency")
	getDelay    = flag.Int("delay", 10, "get delay number")
	mode        = flag.String("m", "put", "operation mode")
	maxPut      = flag.Duration("max", math.MaxInt, "get delay number")
	intervalMs  = flag.Int("interval", 100, "put interval")
	outDir      = flag.String("o", "location", "put bid location dir")
	//bidStart    = flag.Duration("bid", 0, "bid start")

	bidStart uint64
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
	LogLevel   log.Level                     `json:"log_level"`  // int
	ClusterID  proto.ClusterID               `json:"cluster_id"` // uint32
	Host       string                        `json:"host"`       // dist blobnode host
	ClusterMgr cmapi.Config                  `json:"cluster_mgr"`
	BidStart   uint64                        `json:"bid_start"`
	Vuids      map[proto.DiskID][]proto.Vuid `json:"vuids"`
}

type BlobnodeMgr struct {
	//hostMap   map[string][]*client.DiskInfoSimple
	//diskMap       map[proto.DiskID]*blobnode.DiskInfo      // disk id -> disk info

	conf         BlobnodeTestConf
	hostDiskMap  map[string][]*blobnode.DiskInfo // host -> disks
	diskMap      []*blobnode.DiskInfo
	diskChunkMap map[proto.DiskID][]*blobnode.ChunkInfo   // disk id -> chunks
	chunkMap     map[blobnode.ChunkId]*blobnode.ChunkInfo // chunk id -> chunk info

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

	//debugNewMgr()
	//*mode = "put"
	//*bidStart = 1234567890
	//*readFile = "/home/mw/code/cubefs/blobstore/tools/blobnode/blobnode_test.conf"
	//*readFile = "/home/oppo/code/cubefs/blobstore/tools/blobnode/blobnode_test.conf"

	// 2. init mgr, read data
	ctx := context.Background()
	initConfMgr(ctx) // 根据host拿到该节点的disk，拿到vuid
	initData()       // 用本地file的构造data数据
	printDebugInfo() // debug

	// bid start
	log.Infof("mode=%s, bid start=%d", *mode, mgr.conf.BidStart)

	switch *mode {
	case "all":
		mgr.allOp(ctx)
	case "put":
		mgr.onlyPut(ctx)
	case "get":
		mgr.onlyGet(ctx)
	case "delete":
		mgr.onlyDelete(ctx)
	default:
		panic(errors.New("invalid op mode"))
	}

	log.Info("main sleep wait...")
	ch := make(chan int)
	<-ch
}

func invalidArgs() error {
	if _, ok := fileSize[strings.ToUpper(*dataSize)]; !ok {
		return errors.NewErrorf("not support data size %d", *dataSize)
	}
	if *concurrency <= 0 {
		return errors.New("invalid concurrency")
	}
	if *getDelay <= 0 {
		return errors.New("invalid delay")
	}

	return nil
}

func initConfMgr(ctx context.Context) {
	confBytes, err := ioutil.ReadFile(*confFile)
	if err != nil {
		log.Fatalf("read config file failed, filename: %s, err: %v", *confFile, err)
	}

	log.Debugf("Config file %s:\n %s ", *confFile, confBytes)
	if err = config.LoadData(&conf, confBytes); err != nil {
		log.Fatalf("load config failed, error: %+v", err)
	}
	log.SetOutputLevel(conf.LogLevel)
	log.Infof("Config: %+v", conf)

	mgr = newBlobnodeMgr(ctx)
}

func initData() {
	size := fileSize[strings.ToUpper(*dataSize)]
	f, err := os.Open(*readFile)
	if err != nil {
		panic(err)
	}

	dataBuff = make([]byte, size) //buff := make([]byte, size)
	_, err = f.Read(dataBuff)
	if err != nil {
		panic(err)
	}
	log.Infof("read file, dataBuff len=%d (which size will be put)", len(dataBuff))
}

//func newBlobnodeMgr(disks []*client.DiskInfoSimple, cid proto.ClusterID) *BlobnodeMgr {
func newBlobnodeMgr(ctx context.Context) *BlobnodeMgr {
	mgr = &BlobnodeMgr{
		conf:          conf,
		hostDiskMap:   make(map[string][]*blobnode.DiskInfo),
		diskMap:       make([]*blobnode.DiskInfo, 0),
		diskChunkMap:  make(map[proto.DiskID][]*blobnode.ChunkInfo),
		chunkMap:      make(map[blobnode.ChunkId]*blobnode.ChunkInfo),
		blobnodeCli:   blobnode.New(&blobnode.Config{}),
		clusterMgrCli: cmapi.New(&conf.ClusterMgr),
	}
	bidStart = conf.BidStart

	disks, err := mgr.clusterMgrCli.ListHostDisk(ctx, conf.Host)
	if err != nil {
		log.Fatalf("Fail to list cluster disk, err: %+v", err)
	}

	mgr.diskMap = make([]*blobnode.DiskInfo, 0, len(disks))
	for i := range disks {
		if mgr.conf.ClusterID != disks[i].ClusterID {
			log.Errorf("the disk does not belong to this cluster: cluster_id[%d], disk[%+v]", mgr.conf.ClusterID, disks[i])
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

func (mgr *BlobnodeMgr) allOp(ctx context.Context) {
	bidStart = genId()
	log.Infof("bid start: %d", bidStart)

	// 生成唯一bid, 开始put, 记录location. 空间不够时候则申请chunk
	var wg sync.WaitGroup
	mgr.put(ctx, &wg)
	wg.Wait()

	// 根据location, 开始Get
	mgr.get(ctx)

	ch := make(chan int)
	<-ch
	// delete shard bid
	mgr.delete(ctx)
}

func (mgr *BlobnodeMgr) onlyGet(ctx context.Context) {
	if bidStart == 0 {
		log.Fatal("invalid get: invalid bid start")
	}
	mgr.get(ctx)
}

func (mgr *BlobnodeMgr) onlyDelete(ctx context.Context) {
	if bidStart == 0 {
		log.Fatal("invalid get: invalid bid start")
	}
	mgr.delete(ctx)
}

func (mgr *BlobnodeMgr) onlyPut(ctx context.Context) {
	bidStart = genId()
	log.Infof("bid start: %d", bidStart)

	mgr.put(ctx, nil)
}

func (mgr *BlobnodeMgr) loopCommon(ctx context.Context, wg *sync.WaitGroup, fn func(int, proto.Vuid, proto.DiskID, *sync.WaitGroup)) {
	for _, disk := range mgr.diskMap {
		dkId := disk.DiskID
		cnt := 0

		for idx, chunk := range mgr.diskChunkMap[dkId] {
			if cnt >= *concurrency {
				break
			}

			if wg != nil {
				wg.Add(1)
			}

			cnt++
			go fn(idx, chunk.Vuid, dkId, wg)
		}
	}
}

func (mgr *BlobnodeMgr) loopSpecific(ctx context.Context, wg *sync.WaitGroup, fn func(int, proto.Vuid, proto.DiskID, *sync.WaitGroup)) {
	for dkId, chunks := range mgr.conf.Vuids {
		cnt := 0

		for idx, vuid := range chunks {
			if cnt >= *concurrency {
				break
			}

			cnt++
			go fn(idx, vuid, dkId, wg)
		}
	}
}

// POST /shard/put/diskid/{diskid}/vuid/{vuid}/bid/{bid}/size/{size}?iotype={iotype}
func (mgr *BlobnodeMgr) put(ctx context.Context, wg *sync.WaitGroup) {
	mgr.loopCommon(ctx, wg, mgr.singlePut)
}
func (mgr *BlobnodeMgr) get(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 { // for get
		mgr.loopSpecific(ctx, nil, mgr.singleGet)

		return
	}

	mgr.loopCommon(ctx, nil, mgr.singleGet)
}

func (mgr *BlobnodeMgr) singlePut(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID, wg *sync.WaitGroup) {
	off := uint64(0)
	size := fileSize[strings.ToUpper(*dataSize)]
	isWait := false
	if wg != nil {
		isWait = true
	}

	file := getFile(vuid, diskId)
	defer file.Close()

	for {
		// PUT
		bid := bidStart + off
		urlStr := fmt.Sprintf("%v/shard/put/diskid/%v/vuid/%v/bid/%v/size/%v?iotype=%d",
			mgr.conf.Host, diskId, vuid, bid, size, blobnode.NormalIO)
		errCode := doPost(urlStr, "Put")
		time.Sleep(time.Millisecond * time.Duration(*intervalMs))

		if errCode == errorcode.CodeChunkNoSpace {
			panic(errCode)
			// alloc vuid, set vuid
			//for idx := chunkIdx + 1; idx < len(mgr.diskChunkMap[diskId]); idx++ {
			//	if mgr.diskChunkMap[diskId][idx].Free > uint64(size) {
			//		vuid = mgr.diskChunkMap[diskId][idx].Vuid
			//		chunkIdx = idx
			//		break
			//	}
			//}
			//continue
		}

		off++
		file.WriteString(urlStr + "\n")
		if isWait && off > uint64(*getDelay) {
			isWait = false
			wg.Done()
		}

		if off > uint64(*maxPut) {
			return
		}
	}
}

func (mgr *BlobnodeMgr) singleGet(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID, wg *sync.WaitGroup) {
	off := uint64(0)
	for {
		// GET
		bid := bidStart + off
		urlStr := fmt.Sprintf("%v/shard/get/diskid/%v/vuid/%v/bid/%v?iotype=%d", mgr.conf.Host, diskId, vuid, bid, blobnode.NormalIO)
		eCode := doGet(urlStr, "Get")
		time.Sleep(time.Millisecond * time.Duration(*intervalMs))

		if eCode == errorcode.CodeBidNotFound {
			//continue
			panic(eCode)
		}

		off++
		if off > uint64(*getDelay) {
			off = 0
		}
	}
}

func (mgr *BlobnodeMgr) delete(ctx context.Context) {
	mgr.loopCommon(ctx, nil, mgr.singleDel)
}

func (mgr *BlobnodeMgr) singleDel(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID, wg *sync.WaitGroup) {
	for off := uint64(0); off < 100; off++ {
		bid := bidStart + off
		urlStr := fmt.Sprintf("%v/shard/markdelete/diskid/%v/vuid/%v/bid/%v", mgr.conf.Host, diskId, vuid, bid)
		eCode := doPost(urlStr, "markDelete")
		urlStr = fmt.Sprintf("%v/shard/delete/diskid/%v/vuid/%v/bid/%v", mgr.conf.Host, diskId, vuid, bid)
		eCode = doPost(urlStr, "delete")
		time.Sleep(time.Millisecond * time.Duration(*intervalMs))

		if eCode != http.StatusOK {
			panic(eCode)
		}
	}
}

func genId() uint64 {
	//uid, _ := uuid.New().MarshalBinary()
	//return binary.LittleEndian.Uint64(uid[0:8])

	//uid := time.Now().UnixMilli() // +2
	uid := time.Now().Unix() // +5
	return uint64(uid*100000) + uint64(rand.Intn(10000))
}

const (
	strVal = "abcdefghijklmnopqrstuvwxyz_ABCDEFGHIJKLMNOPQRSTUVWXYZ-0123456789"
	_4K    = 4096
)

func doPost(url string, operation string) int {
	log.Debugf("do post once, %s", url)
	buff := make([]byte, len(dataBuff))
	copy(buff, dataBuff)

	if len(dataBuff) >= _4K {
		rand.Seed(time.Now().UnixNano())
		for i := 0; i < len(dataBuff); i += _4K {
			buff[i] = strVal[rand.Intn(len(strVal))]
		}
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(buff))
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

	if resp.StatusCode != http.StatusOK { // || resp.StatusCode == errorcode.CodeChunkNoSpace { // 627
		log.Warnf("fail to http post, status code:%d, operation: %s, url:%s", resp.StatusCode, operation, url)
		return resp.StatusCode
	}

	buf := make([]byte, resp.ContentLength)
	if resp.ContentLength > 0 && resp.Body != nil {
		io.LimitReader(resp.Body, resp.ContentLength).Read(buf)
	}
	log.Debugf("do http post, resp body: %s, resp:%+v", string(buf), resp)
	return http.StatusOK
}

func doGet(url string, operation string) int {
	log.Debugf("do get once, %s", url)
	client := http.Client{}
	rsp, err := client.Get(url)
	if err != nil {
		panic(err)
	}
	defer rsp.Body.Close()

	if rsp.StatusCode != http.StatusOK {
		log.Warnf("fail to http get, status code:%d, operation: %s, url:%s", rsp.StatusCode, operation, url)
		return rsp.StatusCode
	}
	buf := make([]byte, rsp.ContentLength)
	if rsp.ContentLength > 0 && rsp.Body != nil {
		io.LimitReader(rsp.Body, rsp.ContentLength).Read(buf)
		io.CopyN(ioutil.Discard, rsp.Body, rsp.ContentLength)
	}
	crc := rsp.Header.Get("Crc")
	log.Debugf("do http get, crc=%s", crc) // !(EXTRA []string=[119067115])
	//for key, val := range rsp.Header {
	//	if key == "Crc" {
	//		log.Debugf("do http get, crc=%v", val) // !(EXTRA []string=[119067115])
	//	}
	//}

	//log.Debugf("do http get, url:%s, resp body: %s, resp:%+v", url, string(buf), rsp)
	log.Debugf("do http get, url:%s, resp:%+v", url, rsp)
	return http.StatusOK
}

func printDebugInfo() {
	log.Debugf("mgr, host: %s, clusterID: %d", mgr.conf.Host, mgr.conf.ClusterID)
	log.Debugf("hostDiskMap:%+v, diskMap:%+v, lenDisk:%d ", mgr.hostDiskMap, mgr.diskMap, len(mgr.diskMap))
	for idx, val := range mgr.diskMap {
		log.Debugf("idx:%d, ID:%d, disk:%+v", idx, val.DiskID, *val)
	}

	for id, val := range mgr.diskChunkMap {
		if len(val) > 0 {
			log.Debugf("diskId:%d, lenChunk:%d, chunk[0] free:%d, chunk[0]: %+v", id, len(val), val[0].Free, val[0])
		} else {
			log.Debugf("diskId:%d, lenChunk:%d", id, len(val))
		}
	}

	for id, val := range mgr.chunkMap {
		log.Debugf("Id:%s, chunk:%+v", id.String(), *val)
		break
	}
}

func debugNewMgr() {
	mgr = &BlobnodeMgr{
		conf: BlobnodeTestConf{
			Vuids: map[proto.DiskID][]proto.Vuid{
				167: {1111},
				168: {1234},
			},
		},
		diskMap:      make([]*blobnode.DiskInfo, 1),
		diskChunkMap: make(map[proto.DiskID][]*blobnode.ChunkInfo),
	}

	mgr.diskMap[0] = &blobnode.DiskInfo{
		DiskHeartBeatInfo: blobnode.DiskHeartBeatInfo{
			DiskID: 168,
		},
	}
	mgr.diskChunkMap[168] = make([]*blobnode.ChunkInfo, 2)
	mgr.diskChunkMap[168][0] = &blobnode.ChunkInfo{Vuid: 1234}
	mgr.diskChunkMap[168][1] = &blobnode.ChunkInfo{Vuid: 5678}
}

func getFile(vuid proto.Vuid, diskId proto.DiskID) *os.File {
	pwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	fPath := fmt.Sprintf("%s/%s/%d_%d.log", pwd, *outDir, diskId, vuid)
	log.Infof("file path: %s", fPath)
	err = os.MkdirAll(filepath.Dir(fPath), os.ModePerm)
	if err != nil {
		panic(err)
	}

	file, err := os.OpenFile(fPath, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		panic(err)
	}

	return file
}
