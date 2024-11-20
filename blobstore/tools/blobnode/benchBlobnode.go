package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	bnapi "github.com/cubefs/cubefs/blobstore/api/blobnode"
	cmapi "github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/common/config"
	errorcode "github.com/cubefs/cubefs/blobstore/common/errors"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/util/log"
	"github.com/cubefs/cubefs/util/errors"
)

// POST /shard/put/diskid/{diskid}/vuid/{vuid}/bid/{bid}/size/{size}?iotype={iotype}
// POST /shard/markdelete/diskid/:diskid/vuid/:vuid/bid/:bid
// POST /shard/delete/diskid/:diskid/vuid/:vuid/bid/:bid
// GET /shard/get/diskid/{diskid}/vuid/{vuid}/bid/{bid}?iotype={iotype}

// GET /shard/stat/diskid/{diskid}/vuid/{vuidValue}/bid/{bidValue}
// GET /shard/list/diskid/{diskid}/vuid/{vuid}/startbid/{bid}/status/{status}/count/{count}

// POST /chunk/create/diskid/:diskid/vuid/:vuid?chunksize={size}
// POST /chunk/release/diskid/:diskid/vuid/:vuid?force={flag}
// GET /chunk/stat/diskid/:diskid/vuid/:vuid

var (
	confFile      = flag.String("f", "bench_blobnode.conf", "config file path")
	dataSize      = flag.String("d", "128K", "data size[4B,4K,64K,128K,1M,4M,8M,16M]")
	readFile      = flag.String("s", "src.data", "src, read data from src file path")
	concurrency   = flag.Int("c", 1, "go concurrency per disk")
	getDelay      = flag.Int("delay", 100, "start get shard, after delay put count(max get range count)")
	mode          = flag.String("m", "put", "operation mode[all,put,get,delete,alloc,release,getput]")
	putIntervalMs = flag.Int("putItv", 100, "put interval")
	getIntervalMs = flag.Int("getItv", 20, "get interval")
	outDir        = flag.String("o", "location", "put bid location dir")
	//maxPut      = flag.Duration("max", math.MaxInt, "get delay number")
	//bidStart    = flag.Duration("bid", 0, "bid start")

	getCnt   uint64
	putCnt   uint64
	bidStart uint64
	dataBuff []byte
	conf     BlobnodeTestConf
	mgr      *BlobnodeMgr

	fileSize = map[string]int{
		"4B":   4,
		"4K":   4 << 10,
		"64K":  64 << 10,
		"128K": 128 << 10,
		"1M":   1 << 20,
		"4M":   4 << 20,
		"8M":   8 << 20,
		"16M":  16 << 20,
	}
)

type BlobnodeTestConf struct {
	LogLevel    log.Level                     `json:"log_level"`  // int
	ClusterID   proto.ClusterID               `json:"cluster_id"` // uint32
	Host        string                        `json:"host"`       // dist blobnode host
	ClusterMgr  cmapi.Config                  `json:"cluster_mgr"`
	BidStart    uint64                        `json:"bid_start"`
	PutBidStart uint64                        `json:"put_bid_start"`
	MaxCnt      uint64                        `json:"max_cnt"` // max count per concurrence
	PrintSec    int                           `json:"print_sec"`
	Vuids       map[proto.DiskID][]proto.Vuid `json:"vuids"`
}

type BlobnodeMgr struct {
	//hostMap   map[string][]*client.DiskInfoSimple
	//diskMap       map[proto.DiskID]*cmapi.BlobNodeDiskInfo      // disk id -> disk info

	conf         BlobnodeTestConf
	hostDiskMap  map[string][]*cmapi.BlobNodeDiskInfo // host -> disks
	diskMap      []*cmapi.BlobNodeDiskInfo            // all disks
	diskChunkMap map[proto.DiskID][]*cmapi.ChunkInfo  // disk id -> chunks
	chunkMap     map[cmapi.ChunkID]*cmapi.ChunkInfo   // chunk id -> chunk info

	clusterMgrCli *cmapi.Client
	blobnodeCli   bnapi.StorageAPI
}

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
	go loopPrintStat()

	// bid start
	log.Infof("mode=%s, (get/del)bid start=%d, put bid=%d, max count=%d", *mode, mgr.conf.BidStart, mgr.conf.PutBidStart, mgr.conf.MaxCnt)

	switch *mode {
	case "all":
		mgr.allOp(ctx)
	case "put":
		mgr.onlyPut(ctx)
	case "get":
		mgr.onlyGet(ctx)
	case "delete":
		mgr.onlyDelete(ctx)
	case "alloc":
		mgr.onlyAlloc(ctx)
	case "release":
		mgr.onlyRelease(ctx)
	case "getput":
		mgr.getAndPut(ctx)

	default:
		panic(errors.New("invalid op mode"))
	}

	log.Info("main sleep wait...")

	// wait for signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	sig := <-ch
	log.Infof("receive signal: %s, stop service...", sig.String())
	//close()
	log.Infof("putCntTotal=%d, getCntTotal=%d \n", atomic.LoadUint64(&putCnt), atomic.LoadUint64(&getCnt))
	os.Exit(0)
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
	if conf.MaxCnt == 0 { // fix it
		conf.MaxCnt = math.MaxInt64
	}

	return nil
}

func initConfMgr(ctx context.Context) {
	confBytes, err := os.ReadFile(*confFile)
	if err != nil {
		log.Fatalf("read config file failed, filename: %s, err: %v", *confFile, err)
	}
	log.Infof("LoadFile config file %s:\n%s", *confFile, confBytes)

	if err = config.LoadData(&conf, confBytes); err != nil {
		log.Fatalf("load config failed, error: %+v", err)
	}
	log.SetOutputLevel(conf.LogLevel)

	mgr = newBlobnodeMgr(ctx)
}

func initData() {
	if *mode != "put" && *mode != "all" && *mode != "getput" {
		return
	}

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
		hostDiskMap:   make(map[string][]*cmapi.BlobNodeDiskInfo),
		diskMap:       make([]*cmapi.BlobNodeDiskInfo, 0),
		diskChunkMap:  make(map[proto.DiskID][]*cmapi.ChunkInfo),
		chunkMap:      make(map[cmapi.ChunkID]*cmapi.ChunkInfo),
		blobnodeCli:   bnapi.New(&bnapi.Config{}),
		clusterMgrCli: cmapi.New(&conf.ClusterMgr),
	}
	bidStart = conf.BidStart

	disks, err := mgr.clusterMgrCli.ListHostDisk(ctx, conf.Host)
	if err != nil {
		log.Fatalf("Fail to list cluster disk, err: %+v", err)
	}

	allDisk := make(map[proto.DiskID]*cmapi.BlobNodeDiskInfo)
	for i := range disks {
		if mgr.conf.ClusterID != disks[i].ClusterID {
			log.Errorf("the disk does not belong to this cluster: cluster_id[%d], disk[%+v]", mgr.conf.ClusterID, disks[i])
			continue
		}

		// there may be previously expired diskID
		for _, disk := range disks {
			allDisk[disk.DiskID] = disk
		}
	}

	disks = mgr.removeRedundantDiskID(allDisk)
	mgr.diskMap = make([]*cmapi.BlobNodeDiskInfo, 0, len(disks))
	for i := range disks {
		mgr.addDisks(disks[i])
		mgr.addChunks(ctx, disks[i])
	}

	mgr.sortDisk()

	return mgr
}

func (mgr *BlobnodeMgr) removeRedundantDiskID(allDisks map[proto.DiskID]*cmapi.BlobNodeDiskInfo) []*cmapi.BlobNodeDiskInfo {
	uniq := make(map[string]proto.DiskID)
	for _, disk := range allDisks {
		id, exist := uniq[disk.Path]
		// this id is monotonically increasing, so we take the latest(maximum) diskID in the same path
		if !exist || id < disk.DiskID {
			uniq[disk.Path] = disk.DiskID
		}
	}

	disks := make([]*cmapi.BlobNodeDiskInfo, 0, len(uniq))
	for _, id := range uniq {
		disks = append(disks, allDisks[id])
	}
	return disks
}

//func (mgr *BlobnodeMgr) addDisks(disk *client.DiskInfoSimple) {
func (mgr *BlobnodeMgr) addDisks(disk *cmapi.BlobNodeDiskInfo) {
	host := disk.Host
	if _, ok := mgr.hostDiskMap[host]; !ok {
		mgr.hostDiskMap[host] = []*cmapi.BlobNodeDiskInfo{}
	}
	mgr.hostDiskMap[host] = append(mgr.hostDiskMap[host], disk)

	// mgr.diskMap[disk.DiskID] = disk
	mgr.diskMap = append(mgr.diskMap, disk)
}

func (mgr *BlobnodeMgr) addChunks(ctx context.Context, disk *cmapi.BlobNodeDiskInfo) {
	cis, err := mgr.blobnodeCli.ListChunks(ctx, conf.Host, &bnapi.ListChunkArgs{DiskID: disk.DiskID})
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
		//// sort by available size
		//sort.SliceStable(chunks, func(i, j int) bool {
		//	return chunks[i].Free > chunks[j].Free
		//})

		// sort by id
		sort.SliceStable(chunks, func(i, j int) bool {
			return chunks[i].Vuid < chunks[j].Vuid
		})
	}
}

func (mgr *BlobnodeMgr) allOp(ctx context.Context) {
	if mgr.conf.PutBidStart == 0 {
		mgr.conf.PutBidStart = genId()
	}

	// 生成唯一bid, 开始put, 记录location. 空间不够时候则申请chunk
	log.Infof("start put... put bid start: %d", mgr.conf.PutBidStart)
	var wg sync.WaitGroup
	mgr.put(ctx, &wg)
	wg.Wait()

	// 根据location, 开始Get
	log.Info("start get...")
	mgr.get(ctx)

	log.Info("wait delete...")
	ch := make(chan int)
	<-ch
	// delete shard bid
	log.Info("start delete...")
	mgr.delete(ctx)
}

func (mgr *BlobnodeMgr) onlyPut(ctx context.Context) {
	if mgr.conf.PutBidStart == 0 {
		mgr.conf.PutBidStart = genId()
	}
	log.Infof("start put... put bid start: %d", mgr.conf.PutBidStart)

	mgr.put(ctx, nil)
}

func (mgr *BlobnodeMgr) onlyGet(ctx context.Context) {
	if bidStart == 0 {
		log.Fatal("invalid get: invalid bid start")
	}
	log.Info("start get...")
	mgr.get(ctx)
}

func (mgr *BlobnodeMgr) onlyDelete(ctx context.Context) {
	if bidStart == 0 {
		log.Fatal("invalid get: invalid bid start")
	}
	log.Info("start delete...")
	mgr.delete(ctx)
}

func (mgr *BlobnodeMgr) onlyAlloc(ctx context.Context) {
	log.SetOutputLevel(0)
	mgr.alloc(ctx)
}

func (mgr *BlobnodeMgr) onlyRelease(ctx context.Context) {
	log.SetOutputLevel(0)
	mgr.release(ctx)
}

func (mgr *BlobnodeMgr) getAndPut(ctx context.Context) {
	log.Info("start get...")
	mgr.get(ctx)

	time.Sleep(time.Second)
	log.Infof("start put... put bid start: %d", mgr.conf.PutBidStart)
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

			if wg != nil {
				wg.Add(1)
			}

			cnt++
			go fn(idx, vuid, dkId, wg)
		}
	}
}

func (mgr *BlobnodeMgr) serialLoopSpecific(ctx context.Context, wg *sync.WaitGroup, fn func(int, proto.Vuid, proto.DiskID, *sync.WaitGroup)) {
	for dkId, chunks := range mgr.conf.Vuids {
		for idx, vuid := range chunks {
			fn(idx, vuid, dkId, wg)
		}
	}
}

// POST /shard/put/diskid/{diskid}/vuid/{vuid}/bid/{bid}/size/{size}?iotype={iotype}
func (mgr *BlobnodeMgr) put(ctx context.Context, wg *sync.WaitGroup) {
	if len(mgr.conf.Vuids) > 0 {
		mgr.loopSpecific(ctx, wg, mgr.singlePut)

		return
	}

	mgr.loopCommon(ctx, wg, mgr.singlePut)
}
func (mgr *BlobnodeMgr) get(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 { // for get
		mgr.loopSpecific(ctx, nil, mgr.singleGet)

		return
	}

	mgr.loopCommon(ctx, nil, mgr.singleGet)
}

func (mgr *BlobnodeMgr) delete(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 {
		mgr.loopSpecific(ctx, nil, mgr.singleDel)

		return
	}

	mgr.loopCommon(ctx, nil, mgr.singleDel)
}

func (mgr *BlobnodeMgr) alloc(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 {
		//mgr.loopSpecific(ctx, nil, mgr.singleAlloc)
		mgr.serialLoopSpecific(ctx, nil, mgr.singleAlloc)
		return
	}

	mgr.loopCommon(ctx, nil, mgr.singleAlloc)
}

func (mgr *BlobnodeMgr) release(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 {
		//mgr.loopSpecific(ctx, nil, mgr.singleRelease)
		mgr.serialLoopSpecific(ctx, nil, mgr.singleRelease)
		return
	}
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
		bid := mgr.conf.PutBidStart + off
		urlStr := fmt.Sprintf("%v/shard/put/diskid/%v/vuid/%v/bid/%v/size/%v?iotype=%d",
			mgr.conf.Host, diskId, vuid, bid, size, bnapi.NormalIO)
		errCode := doPost(urlStr, "Put")
		time.Sleep(time.Millisecond * time.Duration(*putIntervalMs))

		if errCode == errorcode.CodeChunkNoSpace {
			// panic(errCode)
			// alloc vuid, set vuid
			for idx := chunkIdx + *concurrency; idx < len(mgr.diskChunkMap[diskId]); idx++ {
				if mgr.diskChunkMap[diskId][idx].Free > uint64(size) {
					vuid = mgr.diskChunkMap[diskId][idx].Vuid
					chunkIdx = idx
					break
				}
			}
			continue
		}

		atomic.AddUint64(&putCnt, 1)
		off++
		file.WriteString(urlStr + "\n")
		if isWait && off > uint64(*getDelay) {
			isWait = false
			wg.Done()
		}

		if off > uint64(mgr.conf.MaxCnt) {
			return
		}
	}
}

func (mgr *BlobnodeMgr) singleGet(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID, wg *sync.WaitGroup) {
	off := uint64(0)
	for {
		// GET
		bid := bidStart + off
		urlStr := fmt.Sprintf("%v/shard/get/diskid/%v/vuid/%v/bid/%v?iotype=%d", mgr.conf.Host, diskId, vuid, bid, bnapi.NormalIO)
		eCode := doGet(urlStr, "Get")
		time.Sleep(time.Millisecond * time.Duration(*getIntervalMs))

		if eCode == errorcode.CodeBidNotFound {
			//continue
			panic(eCode)
		}

		atomic.AddUint64(&getCnt, 1)
		off++
		if off > uint64(*getDelay) {
			off = 0
		}
	}
}

func (mgr *BlobnodeMgr) singleDel(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID, wg *sync.WaitGroup) {
	for off := uint64(0); off < uint64(*getDelay); off++ {
		bid := bidStart + off
		urlStr := fmt.Sprintf("%v/shard/markdelete/diskid/%v/vuid/%v/bid/%v", mgr.conf.Host, diskId, vuid, bid)
		eCode := doPost(urlStr, "markDelete")
		urlStr = fmt.Sprintf("%v/shard/delete/diskid/%v/vuid/%v/bid/%v", mgr.conf.Host, diskId, vuid, bid)
		eCode = doPost(urlStr, "delete")
		time.Sleep(time.Millisecond * time.Duration(*getIntervalMs))

		if eCode != http.StatusOK {
			panic(eCode)
		}
	}
}

const (
	_16GB    = 1 << 34
	maxRetry = 5
)

func (mgr *BlobnodeMgr) singleAlloc(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID, wg *sync.WaitGroup) {
	urlStr, eCode := "", 0
	// vuid = 0xFFFFF000001 // 0xFFF FF 000001

	for i := 0; i < maxRetry; i++ {
		urlStr = fmt.Sprintf("%v/chunk/create/diskid/%v/vuid/%v?chunksize=%v", mgr.conf.Host, diskId, vuid, _16GB)
		eCode = doPost(urlStr, "alloc")

		if eCode == errorcode.CodeAlreadyExist {
			vuid++ // epoch+1
			continue
		}
		break
	}

	if eCode != http.StatusOK {
		panic(eCode)
	}
	urlStr = fmt.Sprintf("%v/chunk/stat/diskid/%v/vuid/%v", mgr.conf.Host, diskId, vuid)
	eCode = doGet(urlStr, "alloc")
}

func (mgr *BlobnodeMgr) singleRelease(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID, wg *sync.WaitGroup) {
	urlStr := fmt.Sprintf("%v/chunk/release/diskid/%v/vuid/%v?force=%v", mgr.conf.Host, diskId, vuid, true)
	eCode := doPost(urlStr, "release")
	if eCode != http.StatusOK {
		panic(eCode)
	}

	urlStr = fmt.Sprintf("%v/chunk/stat/diskid/%v/vuid/%v", mgr.conf.Host, diskId, vuid)
	eCode = doGet(urlStr, "alloc")
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
		//io.CopyN(ioutil.Discard, rsp.Body, rsp.ContentLength)
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
		log.Debugf("chunkName:%s, chunk:%+v", id.String(), *val)
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
		diskMap:      make([]*cmapi.BlobNodeDiskInfo, 1),
		diskChunkMap: make(map[proto.DiskID][]*cmapi.ChunkInfo),
	}

	mgr.diskMap[0] = &cmapi.BlobNodeDiskInfo{
		DiskHeartBeatInfo: cmapi.DiskHeartBeatInfo{
			DiskID: 168,
		},
	}
	mgr.diskChunkMap[168] = make([]*cmapi.ChunkInfo, 2)
	mgr.diskChunkMap[168][0] = &cmapi.ChunkInfo{Vuid: 1234}
	mgr.diskChunkMap[168][1] = &cmapi.ChunkInfo{Vuid: 5678}
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

	file, err := os.OpenFile(fPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		panic(err)
	}

	return file
}

func loopPrintStat() {
	go func() {
		if conf.PrintSec <= 0 {
			return
		}

		idx := 0
		tk := time.NewTicker(time.Second * time.Duration(conf.PrintSec))
		for {
			select {
			case <-tk.C:
				idx++
				fmt.Printf("idx:%d, putCnt=%d, getCnt=%d \n", idx, atomic.LoadUint64(&putCnt), atomic.LoadUint64(&getCnt))
			}
		}
	}()
}
