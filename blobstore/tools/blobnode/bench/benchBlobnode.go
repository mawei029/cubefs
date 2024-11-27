package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	"sync/atomic"
	"time"

	bnapi "github.com/cubefs/cubefs/blobstore/api/blobnode"
	cmapi "github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/common/config"
	errorcode "github.com/cubefs/cubefs/blobstore/common/errors"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	statistic "github.com/cubefs/cubefs/blobstore/tools/common"
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
// GET /chunk/list/diskid/:diskid

type opMode int

const (
	opModePut = opMode(iota + 1)
	opModeGet
	opModeDel
	opModeAlloc
	opModeRelease

	overloadSleepMs = 100
)

var (
	confFile = flag.String("f", "bench_blobnode.conf", "config file path")

	//bidStart uint64
	getCnt   uint64
	putCnt   uint64
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
	LogLevel   log.Level       `json:"log_level"`  // int
	ClusterID  proto.ClusterID `json:"cluster_id"` // uint32
	Host       string          `json:"host"`       // dist blobnode host
	ClusterMgr cmapi.Config    `json:"cluster_mgr"`

	Mode       opMode `json:"mode"` // operation mode[0:invalid,put,get,delete,alloc,release]
	BidStart   uint64 `json:"bid_start"`
	MaxCnt     uint64 `json:"max_cnt"`      // max bid count, per concurrence
	MaxReadCnt uint64 `json:"max_read_cnt"` // max read count, per concurrence
	PerDisk    int    `json:"per_disk"`     // concurrence for per disk
	PerVuid    int    `json:"per_vuid"`     // concurrence for per vuid/chunk
	Interval   int    `json:"interval"`     // do request interval

	DataSize string `json:"data_size"` // data size[4B,4K,64K,128K,1M,4M,8M,16M]
	SrcFile  string `json:"src_file"`  // src, read data from src file path
	OutDir   string `json:"out_dir"`   // put bid location dir
	PrintSec int    `json:"print_sec"`

	DiskId proto.DiskID                  `json:"disk_id"` // specific single disk id
	Vuids  map[proto.DiskID][]proto.Vuid `json:"vuids"`
}

type BlobnodeMgr struct {
	//hostMap   map[string][]*client.DiskInfoSimple
	//diskMap       map[proto.DiskID]*cmapi.BlobNodeDiskInfo      // disk id -> disk info

	conf         BlobnodeTestConf
	hostDiskMap  map[string][]*cmapi.BlobNodeDiskInfo // host -> disks
	diskMap      []*cmapi.BlobNodeDiskInfo            // all disks
	diskChunkMap map[proto.DiskID][]*cmapi.ChunkInfo  // disk id -> chunks
	chunkMap     map[cmapi.ChunkID]*cmapi.ChunkInfo   // chunk id -> chunk info

	stat *statistic.TimeStatistic
	done chan struct{}

	clusterMgrCli *cmapi.Client
	blobnodeCli   bnapi.StorageAPI
}

func main() {
	// 1. flag parse
	flag.Parse()

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
	now := time.Now()
	log.Infof("mode=%d, bid start=%d, max count=%d", mgr.conf.Mode, mgr.conf.BidStart, mgr.conf.MaxCnt)

	switch mgr.conf.Mode {
	case opModePut:
		mgr.onlyPut(ctx)
	case opModeGet:
		mgr.onlyGet(ctx)
	case opModeDel:
		mgr.onlyDelete(ctx)
	case opModeAlloc:
		mgr.onlyAlloc(ctx)
	case opModeRelease:
		mgr.onlyRelease(ctx)

	default:
		panic(errors.New("invalid op mode"))
	}

	log.Info("main function wait...")
	for i := 0; i < cap(mgr.done); i++ {
		<-mgr.done
	}
	mgr.stat.Report()

	log.Infof("putCntTotal=%d, getCntTotal=%d, timeCost=%d ms\n", atomic.LoadUint64(&putCnt), atomic.LoadUint64(&getCnt), time.Since(now).Milliseconds())
	os.Exit(1)
}

func checkConfig() error {
	if conf.DataSize == "" {
		conf.DataSize = "128K"
	}
	if conf.SrcFile == "" {
		conf.SrcFile = "src.data"
	}
	if conf.OutDir == "" {
		conf.OutDir = "location"
	}
	if conf.PrintSec <= 0 {
		conf.PrintSec = 1
	}

	if _, ok := fileSize[strings.ToUpper(conf.DataSize)]; !ok {
		return errors.NewErrorf("not support data size %d", conf.DataSize)
	}
	if conf.MaxCnt == 0 { // fix it
		conf.MaxCnt = math.MaxInt64
	}
	if conf.MaxReadCnt == 0 || conf.MaxReadCnt < conf.MaxCnt {
		conf.MaxReadCnt = conf.MaxCnt
	}
	if conf.PerDisk <= 0 {
		conf.PerDisk = 1
	}
	if conf.PerVuid <= 0 {
		conf.PerVuid = 1
	}
	if conf.BidStart <= 0 {
		conf.BidStart = 1
	}
	//if conf.Interval <= 0 {
	//	conf.Interval = 10
	//}

	return nil
}

func initConfMgr(ctx context.Context) {
	confBytes, err := os.ReadFile(*confFile)
	if err != nil {
		log.Fatalf("read config file failed, filename: %s, err: %v", *confFile, err)
	}

	if err = config.LoadData(&conf, confBytes); err != nil {
		log.Fatalf("load config failed, error: %+v", err)
	}
	log.SetOutputLevel(conf.LogLevel)

	if err = checkConfig(); err != nil {
		panic(err)
	}
	log.Infof("LoadFile config file %s:\n%+v", *confFile, conf)

	mgr = newBlobnodeMgr(ctx)
}

func initData() {
	if conf.Mode != opModePut { // put, all, putGet
		return
	}

	size := fileSize[strings.ToUpper(conf.DataSize)]
	f, err := os.Open(conf.SrcFile)
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
	intervalNum := int(conf.MaxCnt) * conf.PerDisk
	if intervalNum > 100 {
		intervalNum = 100
	}

	mgr = &BlobnodeMgr{
		conf:          conf,
		hostDiskMap:   make(map[string][]*cmapi.BlobNodeDiskInfo),
		diskMap:       make([]*cmapi.BlobNodeDiskInfo, 0),
		diskChunkMap:  make(map[proto.DiskID][]*cmapi.ChunkInfo),
		chunkMap:      make(map[cmapi.ChunkID]*cmapi.ChunkInfo),
		blobnodeCli:   bnapi.New(&bnapi.Config{}),
		clusterMgrCli: cmapi.New(&conf.ClusterMgr),

		stat: statistic.NewTimeStatistic("bench", 20000*time.Microsecond, 300),
	}
	//bidStart = conf.BidStart

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
		// sort by available size, for put
		sort.SliceStable(chunks, func(i, j int) bool {
			return chunks[i].Free > chunks[j].Free
		})

		// sort by id
		//sort.SliceStable(chunks, func(i, j int) bool {
		//	return chunks[i].Vuid < chunks[j].Vuid
		//})
	}
}

func (mgr *BlobnodeMgr) onlyPut(ctx context.Context) {
	if mgr.conf.BidStart == 0 {
		mgr.conf.BidStart = genId()
	}
	log.Infof("start put... put bid start: %d", mgr.conf.BidStart)

	mgr.put(ctx)
}

func (mgr *BlobnodeMgr) onlyGet(ctx context.Context) {
	if mgr.conf.BidStart == 0 {
		log.Fatal("invalid get: invalid bid start")
	}
	log.Info("start get...")
	mgr.get(ctx)
}

func (mgr *BlobnodeMgr) onlyDelete(ctx context.Context) {
	if mgr.conf.BidStart == 0 {
		log.Fatal("invalid get: invalid bid start")
	}
	log.Info("start delete...")
	mgr.delete(ctx)
}

func (mgr *BlobnodeMgr) onlyAlloc(ctx context.Context) {
	log.SetOutputLevel(0)
	mgr.alloc(ctx)
	os.Exit(1)
}

func (mgr *BlobnodeMgr) onlyRelease(ctx context.Context) {
	log.SetOutputLevel(0)
	mgr.release(ctx)
	os.Exit(1)
}

func (mgr *BlobnodeMgr) loopAllDisk(ctx context.Context, fn func(int, proto.Vuid, proto.DiskID)) {
	for _, disk := range mgr.diskMap {
		dkId := disk.DiskID
		cnt := 0

		for idx, chunk := range mgr.diskChunkMap[dkId] {
			if cnt >= mgr.conf.PerDisk {
				break
			}

			cnt++
			go fn(idx, chunk.Vuid, dkId)
		}
	}
}

func (mgr *BlobnodeMgr) loopSpecificDiskVuid(ctx context.Context, fn func(int, proto.Vuid, proto.DiskID)) {
	total := 0
	for dkId, chunks := range mgr.conf.Vuids {
		cnt := 0
		for idx, vuid := range chunks {
			if cnt >= mgr.conf.PerDisk {
				break
			}

			cnt++
			go fn(idx, vuid, dkId)
		}
		total += cnt
	}
	mgr.done = make(chan struct{}, total)
}

func (mgr *BlobnodeMgr) serialLoopSpecific(ctx context.Context, fn func(int, proto.Vuid, proto.DiskID)) {
	for dkId, chunks := range mgr.conf.Vuids {
		for idx, vuid := range chunks {
			fn(idx, vuid, dkId)
		}
	}
}

// POST /shard/put/diskid/{diskid}/vuid/{vuid}/bid/{bid}/size/{size}?iotype={iotype}
func (mgr *BlobnodeMgr) put(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 {
		mgr.loopSpecificDiskVuid(ctx, mgr.multiPut) // mgr.singlePut)
		return
	}

	// all disk
	mgr.loopAllDisk(ctx, mgr.singlePut)
}
func (mgr *BlobnodeMgr) get(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 { // for get
		mgr.loopSpecificDiskVuid(ctx, mgr.multiGet) // mgr.singleGet)
		return
	}

	mgr.loopAllDisk(ctx, mgr.singleGet)
}

func (mgr *BlobnodeMgr) delete(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 {
		mgr.loopSpecificDiskVuid(ctx, mgr.singleDel)
		return
	}

	mgr.loopAllDisk(ctx, mgr.singleDel)
}

func (mgr *BlobnodeMgr) alloc(ctx context.Context) {
	defer log.Infof("alloc done...")

	if mgr.conf.DiskId != 0 {
		loc := SingleDisk{Vuids: make([]SingleChunk, mgr.conf.PerDisk)}
		diskId := mgr.conf.DiskId
		vid := proto.Vid(rand.Uint32() + 1)
		for i := 0; i < mgr.conf.PerDisk; i++ {
			// vuid := proto.Vuid(time.Now().UnixNano()) // vid + index + epoch
			vuid, _ := proto.NewVuid(vid, uint8(i), 1)
			mgr.singleAlloc(0, vuid, diskId)
			loc.Vuids[i].Vuid = vuid
		}
		loc.dump(diskId)
		return
	}

	if len(mgr.conf.Vuids) > 0 {
		//mgr.loopSpecificDiskVuid(ctx, nil, mgr.singleAlloc)
		mgr.serialLoopSpecific(ctx, mgr.singleAlloc)
		return
	}

	mgr.loopAllDisk(ctx, mgr.singleAlloc)
}

func (mgr *BlobnodeMgr) release(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 {
		//mgr.loopSpecificDiskVuid(ctx, nil, mgr.singleRelease)
		mgr.serialLoopSpecific(ctx, mgr.singleRelease)
		return
	}
}

func (mgr *BlobnodeMgr) multiPut(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
	var wg sync.WaitGroup
	step := uint64(mgr.conf.PerVuid)

	singlePut := func(bidIdx int, vuid proto.Vuid, diskId proto.DiskID) {
		bid, off := mgr.conf.BidStart+uint64(bidIdx), uint64(0)
		size := fileSize[strings.ToUpper(mgr.conf.DataSize)]
		defer func() {
			if bidIdx == int(step) {
				file := getFile(vuid, diskId)
				file.WriteString(fmt.Sprintf("diskID=%d, vuid=%d, bid start=%d, count=%d, size=%d\n", diskId, vuid, mgr.conf.BidStart, off, size))
			}
			wg.Done()
		}()

		for {
			url := fmt.Sprintf("%v/shard/put/diskid/%v/vuid/%v/bid/%v/size/%v?iotype=%d",
				mgr.conf.Host, diskId, vuid, bid+off, size, bnapi.NormalIO)
			start := time.Now()
			eCode := doPost(url, "Put")
			mgr.stat.Set(time.Since(start))

			switch eCode {
			case errorcode.CodeOverload:
				time.Sleep(time.Millisecond * overloadSleepMs)
				continue
			case errorcode.CodeChunkNoSpace:
				log.Warnf("chunk no space, errCode:%d, last bid:%d", eCode, bid+off)
				return
			case http.StatusOK:
				atomic.AddUint64(&putCnt, 1)
				off += step
				if off >= mgr.conf.MaxCnt {
					return
				}
			default:
				panic(fmt.Errorf("errCode=%d", eCode))
			}

			time.Sleep(time.Millisecond * time.Duration(mgr.conf.Interval))
		}
	}

	for i := 0; i < mgr.conf.PerVuid; i++ {
		wg.Add(1)
		bidIdx := i
		go singlePut(bidIdx, vuid, diskId)
	}
	wg.Wait()
	mgr.done <- struct{}{}
}

func (mgr *BlobnodeMgr) multiGet(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
	var wg sync.WaitGroup
	step := uint64(mgr.conf.PerVuid)

	singleGet := func(bidIdx int, vuid proto.Vuid, diskId proto.DiskID) {
		bid, off, cnt := mgr.conf.BidStart+uint64(bidIdx), uint64(0), uint64(0)
		url := fmt.Sprintf("%s/shard/stat/diskid/%d/vuid/%d/bid/%d", mgr.conf.Host, diskId, vuid, mgr.conf.BidStart)
		defer func() {
			if bidIdx == int(step) {
				log.Infof("diskID=%d, vuid=%d, bid start=%d, maxOff=%d, maxCnt=%d, statUrl=%s", diskId, vuid, mgr.conf.BidStart, off, cnt, url)
			}
			wg.Done()
		}()

		for {
			urlStr := fmt.Sprintf("%v/shard/get/diskid/%v/vuid/%v/bid/%v?iotype=%d", mgr.conf.Host, diskId, vuid, bid+off, bnapi.NormalIO)
			start := time.Now()
			eCode := doGet(urlStr, "Get")
			mgr.stat.Set(time.Now().Sub(start))

			switch eCode {
			case errorcode.CodeOverload:
				time.Sleep(time.Millisecond * overloadSleepMs)
				continue
			case errorcode.CodeBidNotFound:
				log.Warnf("bid not found, errCode:%d, last bid:%d", eCode, bid+off)
				return
			case http.StatusOK:
				atomic.AddUint64(&getCnt, 1)
				cnt += step
				if cnt >= mgr.conf.MaxReadCnt {
					return
				}
				off += step
				if off >= mgr.conf.MaxCnt {
					off = 0
				}
			default:
				panic(fmt.Errorf("errCode=%d", eCode))
			}
			time.Sleep(time.Millisecond * time.Duration(mgr.conf.Interval))
		}
	}

	for i := 0; i < mgr.conf.PerVuid; i++ {
		wg.Add(1)
		bidIdx := i
		go singleGet(bidIdx, vuid, diskId)
	}
	wg.Wait()
	mgr.done <- struct{}{}
}

func (mgr *BlobnodeMgr) singlePut(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
	off := uint64(0)
	size := fileSize[strings.ToUpper(mgr.conf.DataSize)]
	file := getFile(vuid, diskId)
	defer func() {
		//log.Infof("diskID=%d, vuid=%d, bid start=%d, count=%d", diskId, vuid, mgr.conf.BidStart, off)
		file.WriteString(fmt.Sprintf("diskID=%d, vuid=%d, bid start=%d, count=%d, size=%d\n", diskId, vuid, mgr.conf.BidStart, off, size))
		mgr.done <- struct{}{}
	}()

	// bid already exist?
	bidLast := mgr.conf.BidStart + mgr.conf.MaxCnt - 1
	url := fmt.Sprintf("%s/shard/stat/diskid/%d/vuid/%d/bid/%d", mgr.conf.Host, diskId, vuid, bidLast)
	errCode := doGet(url, "Get")
	//if errCode != errorcode.CodeBidNotFound {
	//	log.Warnf("bid already exist, errCode:%d, last bid:%d", errCode, bidLast)
	//	return
	//}

	for {
		bid := mgr.conf.BidStart + off // atomic.LoadUint64(&off)
		url = fmt.Sprintf("%v/shard/put/diskid/%v/vuid/%v/bid/%v/size/%v?iotype=%d",
			mgr.conf.Host, diskId, vuid, bid, size, bnapi.NormalIO)
		start := time.Now()
		errCode = doPost(url, "Put")
		mgr.stat.Set(time.Since(start))

		switch errCode {
		case errorcode.CodeOverload:
			time.Sleep(time.Millisecond * overloadSleepMs)
			continue
		case errorcode.CodeChunkNoSpace:
			log.Warnf("chunk no space, errCode:%d, last bid:%d", errCode, bid+off)
			return
			// alloc vuid, set vuid
			//isFind := false
			//for idx := chunkIdx + mgr.conf.PerDisk; idx < len(mgr.diskChunkMap[diskId]); idx++ {
			//	if mgr.diskChunkMap[diskId][idx].Free > uint64(size) {
			//		vuid = mgr.diskChunkMap[diskId][idx].Vuid
			//		chunkIdx = idx
			//		isFind = true
			//		break
			//	}
			//}
		case http.StatusOK:
			atomic.AddUint64(&putCnt, 1)
			off++ // atomic.AddUint64(&off, 1)
			// file.WriteString(urlStr + "\n")
			if off >= mgr.conf.MaxCnt {
				return
			}
		default:
			panic(fmt.Errorf("errCode=%d", errCode))
		}

		time.Sleep(time.Millisecond * time.Duration(mgr.conf.Interval))
	}
}

func (mgr *BlobnodeMgr) singleGet(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
	off, cnt := uint64(0), uint64(0)
	url := fmt.Sprintf("%s/shard/stat/diskid/%d/vuid/%d/bid/%d", mgr.conf.Host, diskId, vuid, mgr.conf.BidStart)
	//errCode := doGet(url, "Get")
	//if errCode != http.StatusOK {
	//	log.Warnf("bid stat error, errCode:%d, bid:%d", errCode, mgr.conf.BidStart)
	//	return
	//}
	defer func() {
		log.Infof("diskID=%d, vuid=%d, bid start=%d, maxOff=%d, maxCnt=%d, statUrl=%s", diskId, vuid, mgr.conf.BidStart, off, cnt, url)
		mgr.done <- struct{}{}
	}()

	for {
		// GET
		bid := mgr.conf.BidStart + off
		urlStr := fmt.Sprintf("%v/shard/get/diskid/%v/vuid/%v/bid/%v?iotype=%d", mgr.conf.Host, diskId, vuid, bid, bnapi.NormalIO)
		start := time.Now()
		eCode := doGet(urlStr, "Get")
		mgr.stat.Set(time.Now().Sub(start))

		switch eCode {
		case errorcode.CodeOverload:
			time.Sleep(time.Millisecond * overloadSleepMs)
			continue
		case errorcode.CodeBidNotFound:
			//panic(eCode)
			log.Warnf("bid not found, errCode:%d, last bid:%d", eCode, bid+off)
			return
		case http.StatusOK:
			atomic.AddUint64(&getCnt, 1)
			cnt++
			if cnt >= mgr.conf.MaxReadCnt {
				return
			}
			off++
			if off >= mgr.conf.MaxCnt {
				off = 0
			}
		default:
			panic(fmt.Errorf("errCode=%d", eCode))
		}

		time.Sleep(time.Millisecond * time.Duration(mgr.conf.Interval))
	}
}

func (mgr *BlobnodeMgr) singleDel(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
	for off := uint64(0); off < mgr.conf.MaxCnt; off++ {
		bid := mgr.conf.BidStart + off
		urlStr := fmt.Sprintf("%v/shard/markdelete/diskid/%v/vuid/%v/bid/%v", mgr.conf.Host, diskId, vuid, bid)
		eCode := doPost(urlStr, "markDelete")
		urlStr = fmt.Sprintf("%v/shard/delete/diskid/%v/vuid/%v/bid/%v", mgr.conf.Host, diskId, vuid, bid)
		eCode = doPost(urlStr, "delete")
		time.Sleep(time.Millisecond * time.Duration(mgr.conf.Interval))

		if eCode != http.StatusOK {
			panic(eCode)
		}
	}
}

const (
	_16GB    = 1 << 34
	maxRetry = 5
)

func (mgr *BlobnodeMgr) singleAlloc(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
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
	if eCode != http.StatusOK {
		panic(eCode)
	}
}

func (mgr *BlobnodeMgr) singleRelease(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
	urlStr := fmt.Sprintf("%v/chunk/release/diskid/%v/vuid/%v?force=%v", mgr.conf.Host, diskId, vuid, true)
	eCode := doPost(urlStr, "release")
	if eCode != http.StatusOK {
		panic(eCode)
	}

	urlStr = fmt.Sprintf("%v/chunk/stat/diskid/%v/vuid/%v", mgr.conf.Host, diskId, vuid)
	eCode = doGet(urlStr, "stat")
	if eCode != http.StatusOK {
		panic(eCode)
	}
}

type SingleChunk struct {
	Vuid  proto.Vuid
	Bid   proto.BlobID
	Count int
}

type SingleDisk struct {
	Vuids []SingleChunk
}

func (l *SingleDisk) dump(diskId proto.DiskID) {
	pwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	fPath := fmt.Sprintf("%s/%s/%d_alloc.log", pwd, conf.OutDir, diskId)
	err = os.MkdirAll(filepath.Dir(fPath), 0o755) // os.ModePerm)
	if err != nil {
		panic(err)
	}

	file, err := os.OpenFile(fPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) // os.Create(fPath)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	json.NewEncoder(file).Encode(l)
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
	// _4K    = 4096
	_256B = 256
	// _64B   = 64
)

func doPost(url string, operation string) int {
	log.Debugf("do post once, %s", url)
	buff := make([]byte, 0, 1)
	if operation == "Put" {
		buff = make([]byte, len(dataBuff))
		copy(buff, dataBuff)

		// mock random put data
		if len(dataBuff) >= _256B {
			rand.Seed(time.Now().UnixNano())
			for i := 0; i < len(dataBuff); i += _256B {
				buff[i] = strVal[rand.Intn(len(strVal))]
			}
		}
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(buff))
	if err != nil {
		panic(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(dataBuff))
	resp, err := http.DefaultClient.Do(req)
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
	rsp, err := http.DefaultClient.Get(url)
	if err != nil {
		panic(err)
	}
	defer rsp.Body.Close()

	if rsp.StatusCode != http.StatusOK {
		log.Warnf("fail to http get, status code:%d, operation: %s, url:%s", rsp.StatusCode, operation, url)
		return rsp.StatusCode
	}
	// buf := make([]byte, rsp.ContentLength)
	crc := rsp.Header.Get("Crc")
	if rsp.ContentLength > 0 && rsp.Body != nil {
		//io.LimitReader(rsp.Body, rsp.ContentLength).Read(buf) // not have limiter
		//io.CopyN(ioutil.Discard, rsp.Body, rsp.ContentLength)
		rd := io.LimitReader(rsp.Body, rsp.ContentLength)
		io.CopyN(ioutil.Discard, rd, rsp.ContentLength)
		// http get, operation: Get, url:http://ip:8889/shard/get/diskid/653/vuid/8127987705146507265/bid/47?iotype=0, response:1048576, dst:1048576, crc:1561641303
		// log.Infof("http get, operation: %s, url:%s, response:%d, dst:%d, crc:%s", operation, url, rsp.ContentLength, len(buf), crc)
	}
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

	fPath := fmt.Sprintf("%s/%s/%d_%d.log", pwd, conf.OutDir, diskId, vuid)
	log.Infof("file path: %s", fPath)
	err = os.MkdirAll(filepath.Dir(fPath), 0o644) // os.ModePerm)
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
