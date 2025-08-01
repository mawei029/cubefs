package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bnapi "github.com/cubefs/cubefs/blobstore/api/blobnode"
	cmapi "github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/common/config"
	errcode "github.com/cubefs/cubefs/blobstore/common/errors"
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

// POST /chunk/create/diskid/:diskid/vuid/:vuid?chunksize={size}  // alloc
// POST /chunk/release/diskid/:diskid/vuid/:vuid?force={flag}     // release
// GET /chunk/stat/diskid/:diskid/vuid/:vuid
// GET /chunk/list/diskid/:diskid

type opMode int

const (
	opModePut      = opMode(1 << iota) // 1
	opModeGet                          // 2
	opModeDel                          // 4
	opModeAlloc                        // 8
	opModeRelease                      // 16
	opModeFullDisk                     // 32

	opModePutFullDisk = opModePut | opModeFullDisk        // 33
	opModeGetFullDisk = opModeGet | opModeFullDisk        // 34
	opModeDelFullDisk = opModeDel | opModeFullDisk        // 36
	opModeMixed       = opModePut | opModeGet | opModeDel // 7 - Mixed read/write/delete operations

	overloadSleepMs = 100
)

var (
	confFile = flag.String("f", "bench_blobnode.conf", "config file path")

	// bidStart uint64
	getCnt   uint64
	putCnt   uint64
	delCnt   uint64
	dataBuff []byte
	conf     BlobnodeTestConf
	mgr      *BlobnodeMgr

	fileSize = map[string]int{
		"4B":   4,
		"4K":   4 << 10,
		"32K":  32 << 10,
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

	Mode        opMode `json:"mode"` // operation mode[0:invalid,put,get,delete,alloc,release]
	BidStart    uint64 `json:"bid_start"`
	MaxCnt      uint64 `json:"max_cnt"`      // max bid count, per concurrence
	MaxReadCnt  uint64 `json:"max_read_cnt"` // max read count, per concurrence, may be beyond MaxCnt
	MaxRound    int    `json:"max_round"`    // max put round, keep writing data until the disk is full
	MaxChunkCnt int    `json:"max_chunk_cnt"`
	PerDisk     int    `json:"per_disk"` // concurrence for per disk
	PerVuid     int    `json:"per_vuid"` // concurrence for per vuid/chunk
	Interval    int    `json:"interval"` // do request interval
	Random      bool   `json:"random"`   // random read bid ; random write src data
	Check       bool   `json:"check"`    // check read data crc32

	DataSize string `json:"data_size"` // data size[4B,4K,64K,128K,1M,4M,8M,16M]
	SrcFile  string `json:"src_file"`  // src, read data from src file path
	OutDir   string `json:"out_dir"`   // put bid location dir
	PrintSec int    `json:"print_sec"`

	DiskId proto.DiskID                  `json:"disk_id"` // specific single disk id
	Vuids  map[proto.DiskID][]proto.Vuid `json:"vuids"`
}

type BlobnodeMgr struct {
	// hostMap   map[string][]*client.DiskInfoSimple
	// diskMap       map[proto.DiskID]*cmapi.BlobNodeDiskInfo      // disk id -> disk info

	conf         BlobnodeTestConf
	hostDiskMap  map[string][]*bnapi.DiskInfo        // host -> disks
	diskMap      []*bnapi.DiskInfo                   // all disks
	diskChunkMap map[proto.DiskID][]*bnapi.ChunkInfo // disk id -> chunks
	chunkMap     map[bnapi.ChunkId]*bnapi.ChunkInfo  // chunk id -> chunk info

	stat *statistic.TimeStatistic
	done chan struct{}

	clusterMgrCli *cmapi.Client
	blobnodeCli   bnapi.StorageAPI
}

func main() {
	// 1. flag parse
	flag.Parse()

	// debugNewMgr()
	// *mode = "put"
	// *bidStart = 1234567890
	// *readFile = "/home/mw/code/cubefs/blobstore/tools/blobnode/blobnode_test.conf"
	// *confFile = "/home/oppo/code/cubefs/blobstore/tools/blobnode/bench/bench_blobnode.conf"

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
	// keep writing data until the disk is full
	case opModePutFullDisk:
		mgr.putFullDisk(ctx)
	case opModeGetFullDisk:
		mgr.getFullDisk(ctx)
	case opModeDelFullDisk:
		mgr.delFullDisk(ctx)
	case opModeMixed:
		mgr.mixedOperations(ctx)
	default:
		panic(errors.New("invalid op mode"))
	}

	log.Info("main function wait...")
	for i := 0; i < cap(mgr.done); i++ {
		<-mgr.done
	}
	mgr.stat.Report()

	fmt.Printf("putCntTotal=%d, getCntTotal=%d, timeCost=%d ms\n", atomic.LoadUint64(&putCnt), atomic.LoadUint64(&getCnt), time.Since(now).Milliseconds())
	log.Infof("putCntTotal=%d, getCntTotal=%d, delCntTotal=%d, timeCost=%d ms\n",
		atomic.LoadUint64(&putCnt), atomic.LoadUint64(&getCnt), atomic.LoadUint64(&delCnt), time.Since(now).Milliseconds())
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

	if ds, err := strconv.Atoi(conf.DataSize); err == nil {
		fileSize[conf.DataSize] = ds
	}
	if _, ok := fileSize[strings.ToUpper(conf.DataSize)]; !ok {
		return errors.NewErrorf("not support data size %d", conf.DataSize)
	}
	if conf.MaxCnt == 0 { // fix it
		conf.MaxCnt = math.MaxInt64
	}
	if conf.MaxReadCnt == 0 { // || conf.MaxReadCnt < conf.MaxCnt {
		conf.MaxReadCnt = conf.MaxCnt
	}
	if conf.MaxRound == 0 {
		conf.MaxRound = 1
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

	if conf.Host == "" {
		conf.Host = getLocalHost()
	}

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
	// put, put until full
	if conf.Mode == opModePut || conf.Mode&opModePut != 0 {
		size := fileSize[strings.ToUpper(conf.DataSize)]
		f, err := os.Open(conf.SrcFile)
		if err != nil {
			panic(err)
		}

		dataBuff = make([]byte, size) // buff := make([]byte, size)
		_, err = f.Read(dataBuff)
		if err != nil {
			panic(err)
		}
	}
	log.Infof("read file, dataBuff len=%d (which size will be put)", len(dataBuff))
}

func getLocalHost() string {
	localIp := ""

	interfaces, err := net.Interfaces()
	if err != nil {
		log.Error("Error:", err)
		return ""
	}

	for _, iface := range interfaces {
		if iface.Name == "bond0" {
			addrs, err := iface.Addrs()
			if err != nil {
				log.Error("Error:", err)
				return ""
			}

			for _, addr := range addrs {
				if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					fmt.Println("bond0 IPv4 Address:", ipNet.IP.String())
					localIp = ipNet.IP.String()
					break
				}
			}
		}
	}

	return fmt.Sprintf("http://%s:8889", localIp)
}

// func newBlobnodeMgr(disks []*client.DiskInfoSimple, cid proto.ClusterID) *BlobnodeMgr {
func newBlobnodeMgr(ctx context.Context) *BlobnodeMgr {
	intervalNum := int(conf.MaxCnt) * conf.PerDisk
	if intervalNum > 100 {
		intervalNum = 100
	}

	mgr = &BlobnodeMgr{
		conf:          conf,
		hostDiskMap:   make(map[string][]*bnapi.DiskInfo),
		diskMap:       make([]*bnapi.DiskInfo, 0),
		diskChunkMap:  make(map[proto.DiskID][]*bnapi.ChunkInfo),
		chunkMap:      make(map[bnapi.ChunkId]*bnapi.ChunkInfo),
		blobnodeCli:   bnapi.New(&bnapi.Config{}),
		clusterMgrCli: cmapi.New(&conf.ClusterMgr),

		stat: statistic.NewTimeStatistic("bench", 20000*time.Microsecond, 300, conf.PerDisk),
	}
	// bidStart = conf.BidStart

	disks, err := mgr.clusterMgrCli.ListHostDisk(ctx, conf.Host)
	if err != nil {
		log.Fatalf("Fail to list cluster disk, err: %+v", err)
	}
	log.Debugf("cm all disks:%+v", disks)

	allDisk := make(map[proto.DiskID]*bnapi.DiskInfo)
	for i := range disks {
		if mgr.conf.ClusterID != disks[i].ClusterID {
			log.Errorf("the disk does not belong to this cluster: cluster_id[%d], disk[%+v]", mgr.conf.ClusterID, disks[i])
			continue
		}

		// there may be previously expired diskID
		for _, disk := range disks {
			allDisk[disk.DiskID] = disk
		}
		log.Debugf("disk info:%+v", *disks[i])
	}

	disks = mgr.removeRedundantDiskID(allDisk)
	mgr.diskMap = make([]*bnapi.DiskInfo, 0, len(disks))
	for i := range disks {
		mgr.addDisks(disks[i])
		mgr.addChunks(ctx, disks[i])
	}

	mgr.sortDisk()

	return mgr
}

func (mgr *BlobnodeMgr) removeRedundantDiskID(allDisks map[proto.DiskID]*bnapi.DiskInfo) []*bnapi.DiskInfo {
	uniq := make(map[string]proto.DiskID)
	for _, disk := range allDisks {
		id, exist := uniq[disk.Path]
		// this id is monotonically increasing, so we take the latest(maximum) diskID in the same path
		if !exist || id < disk.DiskID {
			if disk.Status != proto.DiskStatusNormal {
				continue
			}
			uniq[disk.Path] = disk.DiskID
		}
	}

	disks := make([]*bnapi.DiskInfo, 0, len(uniq))
	for _, id := range uniq {
		disks = append(disks, allDisks[id])
	}
	return disks
}

// func (mgr *BlobnodeMgr) addDisks(disk *client.DiskInfoSimple) {
func (mgr *BlobnodeMgr) addDisks(disk *cmapi.BlobNodeDiskInfo) {
	host := disk.Host
	if _, ok := mgr.hostDiskMap[host]; !ok {
		mgr.hostDiskMap[host] = []*bnapi.DiskInfo{}
	}
	mgr.hostDiskMap[host] = append(mgr.hostDiskMap[host], disk)

	// mgr.diskMap[disk.DiskID] = disk
	mgr.diskMap = append(mgr.diskMap, disk)
}

func (mgr *BlobnodeMgr) addChunks(ctx context.Context, disk *bnapi.DiskInfo) {
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
	// log.SetOutputLevel(0)
	mgr.alloc(ctx)
	os.Exit(1)
}

func (mgr *BlobnodeMgr) onlyRelease(ctx context.Context) {
	// log.SetOutputLevel(0)
	mgr.release(ctx)
	os.Exit(1)
}

func (mgr *BlobnodeMgr) putFullDisk(ctx context.Context) {
	// keep writing data until the disk is full
	if mgr.conf.BidStart == 0 {
		mgr.conf.BidStart = 1
	}
	log.Infof("start put... put until full disk:%d, bid start: %d", mgr.conf.DiskId, mgr.conf.BidStart)

	if len(mgr.conf.Vuids) != 0 {
		mgr.loopSerialDiskVuid(ctx, mgr.putSerial)
		return
	}

	// mgr.conf.Vuids = make(map[proto.DiskID][]proto.Vuid)
	for round := 0; round < mgr.conf.MaxRound; round++ {
		atomic.StoreUint64(&putCnt, 0)
		atomic.StoreUint64(&getCnt, 0)
		// alloc and replace new vuid for diskID
		mgr.alloc(ctx)
		// time.Sleep(time.Second)
		log.Infof("start put, round:%d, bid start: %d, vuids: %+v", round, mgr.conf.BidStart, mgr.conf.Vuids)
		mgr.put(ctx)

		for i := 0; i < cap(mgr.done); i++ {
			<-mgr.done
		}
		log.Infof("end put, round:%d", round)
	}
	close(mgr.done)
	log.Info("end, put until full disk, max round")
}

func (mgr *BlobnodeMgr) getFullDisk(ctx context.Context) {
	if len(mgr.conf.Vuids) == 0 {
		log.Errorf("getFullDisk, no vuid")
		return
	}

	mgr.loopSerialDiskVuid(ctx, mgr.getSerial)
	return
}

func (mgr *BlobnodeMgr) delFullDisk(ctx context.Context) {
	if len(mgr.conf.Vuids) == 0 {
		log.Errorf("delFullDisk, no vuid")
		return
	}

	mgr.loopSerialDiskVuid(ctx, mgr.delSerial)
	return
}

func (mgr *BlobnodeMgr) loopAllDisk(ctx context.Context, fn func(int, proto.Vuid, proto.DiskID)) {
	total := 0
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
		total += cnt
	}

	mgr.done = make(chan struct{}, total)
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

func (mgr *BlobnodeMgr) loopSpecificSerial(ctx context.Context, fn func(int, proto.Vuid, proto.DiskID)) {
	for dkId, chunks := range mgr.conf.Vuids {
		for idx, vuid := range chunks {
			fn(idx, vuid, dkId)
		}
	}
}

func (mgr *BlobnodeMgr) loopSerialDiskVuid(ctx context.Context, fn func(int, proto.Vuid, proto.DiskID, chan struct{})) {
	total := 0
	for dkId, chunks := range mgr.conf.Vuids {
		total += len(chunks)
		log.Infof("loopSerialDiskVuid, diskId:%d, chunks count:%d", dkId, len(chunks))
	}
	mgr.done = make(chan struct{}, total)

	for dkId, chunks := range mgr.conf.Vuids {
		threadCh := make(chan struct{}, mgr.conf.PerDisk)

		for idx, vuid := range chunks {
			threadCh <- struct{}{}
			log.Infof("start work, idx:%d, diskId:%d, vuid:%d", idx, dkId, vuid)
			go fn(idx, vuid, dkId, threadCh)
		}
	}
	log.Info("end to loopSerialDiskVuid")
}

// POST /shard/put/diskid/{diskid}/vuid/{vuid}/bid/{bid}/size/{size}?iotype={iotype}
func (mgr *BlobnodeMgr) put(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 {
		mgr.loopSpecificDiskVuid(ctx, mgr.putParallel) // mgr.singlePut)
		return
	}

	// all disk
	mgr.loopAllDisk(ctx, mgr.singlePut)
}

func (mgr *BlobnodeMgr) get(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 { // for get
		mgr.loopSpecificDiskVuid(ctx, mgr.getParallel) // mgr.singleGet)
		return
	}

	mgr.loopAllDisk(ctx, mgr.singleGet)
}

func (mgr *BlobnodeMgr) delete(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 {
		mgr.loopSpecificDiskVuid(ctx, mgr.delParallel)
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
		vuids := loc.dump(diskId)

		if mgr.conf.Vuids == nil {
			mgr.conf.Vuids = make(map[proto.DiskID][]proto.Vuid)
		}
		mgr.conf.Vuids[diskId] = vuids
		return
	}

	if len(mgr.conf.Vuids) > 0 {
		// mgr.loopSpecificDiskVuid(ctx, nil, mgr.singleAlloc)
		mgr.loopSpecificSerial(ctx, mgr.singleAlloc)
		return
	}

	mgr.loopAllDisk(ctx, mgr.singleAlloc)
}

func (mgr *BlobnodeMgr) release(ctx context.Context) {
	if len(mgr.conf.Vuids) > 0 {
		// mgr.loopSpecificDiskVuid(ctx, nil, mgr.singleRelease)
		mgr.loopSpecificSerial(ctx, mgr.singleRelease)
		return
	}
}

func (mgr *BlobnodeMgr) putParallel(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
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
			eCode := mgr.doPost(url, "Put")
			mgr.stat.Set(time.Since(start))

			switch eCode {
			case errcode.CodeOverload:
				time.Sleep(time.Millisecond * overloadSleepMs)
				continue
			case errcode.CodeChunkNoSpace:
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

func (mgr *BlobnodeMgr) getParallel(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
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
			eCode := mgr.doGet(urlStr, "Get")
			mgr.stat.Set(time.Now().Sub(start))

			switch eCode {
			case errcode.CodeOverload:
				time.Sleep(time.Millisecond * overloadSleepMs)
				continue
			case errcode.CodeBidNotFound:
				log.Warnf("bid not found, errCode:%d, last bid:%d", eCode, bid+off)
				return
			case http.StatusOK:
				atomic.AddUint64(&getCnt, 1)
				cnt += step
				if cnt >= mgr.conf.MaxReadCnt {
					return
				}

				if mgr.conf.Random {
					off = uint64((bidIdx + 1) * rand.Intn(int(mgr.conf.MaxCnt/step)))
				} else {
					off += step
					if off >= mgr.conf.MaxCnt {
						off = 0
					}
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

func (mgr *BlobnodeMgr) delParallel(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
	var wg sync.WaitGroup
	step := uint64(mgr.conf.PerVuid)

	judgeErrCode := func(eCode int) bool {
		switch eCode {
		case errcode.CodeOverload:
			time.Sleep(time.Millisecond * overloadSleepMs)
			return false // not success
		case http.StatusOK, errcode.CodeShardMarkDeleted:
			return true // true, ok
		default:
			panic(eCode)
		}
	}

	singleDel := func(bidIdx int, vuid proto.Vuid, diskId proto.DiskID) {
		bid, off := mgr.conf.BidStart+uint64(bidIdx), uint64(0)
		defer func() {
			if bidIdx == int(step) {
				log.Infof("diskID=%d, vuid=%d, bid start=%d, count=%d", diskId, vuid, mgr.conf.BidStart, off)
			}
			wg.Done()
		}()

		for off < mgr.conf.MaxCnt {
			urlStr := fmt.Sprintf("%v/shard/markdelete/diskid/%v/vuid/%v/bid/%v", mgr.conf.Host, diskId, vuid, bid+off)
			eCode := mgr.doPost(urlStr, "markDelete")
			if !judgeErrCode(eCode) {
				continue
			}

		RAW_DEL:
			urlStr = fmt.Sprintf("%v/shard/delete/diskid/%v/vuid/%v/bid/%v", mgr.conf.Host, diskId, vuid, bid+off)
			eCode = mgr.doPost(urlStr, "delete")
			if !judgeErrCode(eCode) {
				goto RAW_DEL
			}

			// after ok, next one
			off += step
			atomic.AddUint64(&delCnt, 1)
			time.Sleep(time.Millisecond * time.Duration(mgr.conf.Interval))
		}
	}

	for i := 0; i < mgr.conf.PerVuid; i++ {
		wg.Add(1)
		bidIdx := i
		go singleDel(bidIdx, vuid, diskId)
	}
	wg.Wait()
	mgr.done <- struct{}{}
}

func (mgr *BlobnodeMgr) putSerial(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID, concurrence chan struct{}) {
	mgr.putParallel(chunkIdx, vuid, diskId)
	<-concurrence
}

func (mgr *BlobnodeMgr) getSerial(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID, concurrence chan struct{}) {
	mgr.getParallel(chunkIdx, vuid, diskId)
	<-concurrence
}

func (mgr *BlobnodeMgr) delSerial(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID, concurrence chan struct{}) {
	mgr.delParallel(chunkIdx, vuid, diskId)
	<-concurrence
}

func (mgr *BlobnodeMgr) singlePut(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
	off := uint64(0)
	size := fileSize[strings.ToUpper(mgr.conf.DataSize)]
	file := getFile(vuid, diskId)
	defer func() {
		// log.Infof("diskID=%d, vuid=%d, bid start=%d, count=%d", diskId, vuid, mgr.conf.BidStart, off)
		file.WriteString(fmt.Sprintf("diskID=%d, vuid=%d, bid start=%d, count=%d, size=%d\n", diskId, vuid, mgr.conf.BidStart, off, size))
		mgr.done <- struct{}{}
	}()

	// bid already exist?
	bidLast := mgr.conf.BidStart + mgr.conf.MaxCnt - 1
	url := fmt.Sprintf("%s/shard/stat/diskid/%d/vuid/%d/bid/%d", mgr.conf.Host, diskId, vuid, bidLast)
	errCode := mgr.doGet(url, "Get")
	//if errCode != errcode.CodeBidNotFound {
	//	log.Warnf("bid already exist, errCode:%d, last bid:%d", errCode, bidLast)
	//	return
	//}

	for {
		bid := mgr.conf.BidStart + off // atomic.LoadUint64(&off)
		url = fmt.Sprintf("%v/shard/put/diskid/%v/vuid/%v/bid/%v/size/%v?iotype=%d",
			mgr.conf.Host, diskId, vuid, bid, size, bnapi.NormalIO)
		start := time.Now()
		errCode = mgr.doPost(url, "Put")
		mgr.stat.Set(time.Since(start))

		switch errCode {
		case errcode.CodeOverload:
			time.Sleep(time.Millisecond * overloadSleepMs)
			continue
		case errcode.CodeChunkNoSpace:
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
		eCode := mgr.doGet(urlStr, "Get")
		mgr.stat.Set(time.Now().Sub(start))

		switch eCode {
		case errcode.CodeOverload:
			time.Sleep(time.Millisecond * overloadSleepMs)
			continue
		case errcode.CodeBidNotFound:
			// panic(eCode)
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
	off := uint64(0)
	defer func() {
		log.Infof("diskID=%d, vuid=%d, bid start=%d, count=%d", diskId, vuid, mgr.conf.BidStart, off)
		mgr.done <- struct{}{}
	}()

	for ; off < mgr.conf.MaxCnt; off++ {
		bid := mgr.conf.BidStart + off
		urlStr := fmt.Sprintf("%v/shard/markdelete/diskid/%v/vuid/%v/bid/%v", mgr.conf.Host, diskId, vuid, bid)
		eCode := mgr.doPost(urlStr, "markDelete")
		urlStr = fmt.Sprintf("%v/shard/delete/diskid/%v/vuid/%v/bid/%v", mgr.conf.Host, diskId, vuid, bid)
		eCode = mgr.doPost(urlStr, "delete")

		if eCode != http.StatusOK {
			panic(eCode)
		}

		atomic.AddUint64(&delCnt, 1)
		time.Sleep(time.Millisecond * time.Duration(mgr.conf.Interval))
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
		// mgr.conf.ChunkSize
		urlStr = fmt.Sprintf("%v/chunk/create/diskid/%v/vuid/%v?chunksize=%v", mgr.conf.Host, diskId, vuid, _16GB)
		eCode = mgr.doPost(urlStr, "alloc")

		if eCode == errcode.CodeAlreadyExist {
			vuid++ // epoch+1
			continue
		}
		break
	}
	if eCode != http.StatusOK {
		panic(eCode)
	}

	urlStr = fmt.Sprintf("%v/chunk/stat/diskid/%v/vuid/%v", mgr.conf.Host, diskId, vuid)
	eCode = mgr.doGet(urlStr, "alloc")
	if eCode != http.StatusOK {
		panic(eCode)
	}
}

func (mgr *BlobnodeMgr) singleRelease(chunkIdx int, vuid proto.Vuid, diskId proto.DiskID) {
	urlStr := fmt.Sprintf("%v/chunk/release/diskid/%v/vuid/%v?force=%v", mgr.conf.Host, diskId, vuid, true)
	eCode := mgr.doPost(urlStr, "release")
	if eCode != http.StatusOK {
		panic(eCode)
	}

	urlStr = fmt.Sprintf("%v/chunk/stat/diskid/%v/vuid/%v", mgr.conf.Host, diskId, vuid)
	eCode = mgr.doGet(urlStr, "stat")
	if eCode != http.StatusOK && eCode != errcode.CodeVuidNotFound {
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

func (l *SingleDisk) dump(diskId proto.DiskID) []proto.Vuid {
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

	vuids := make([]proto.Vuid, len(l.Vuids))
	json.NewEncoder(file).Encode(l)
	fmt.Printf("dump log to file: %s, detail disk vuid info: ", fPath)
	for i := range l.Vuids {
		fmt.Printf("%d,", l.Vuids[i].Vuid)
		vuids[i] = l.Vuids[i].Vuid
	}
	fmt.Println("")

	return vuids
}

func genId() uint64 {
	// uid, _ := uuid.New().MarshalBinary()
	// return binary.LittleEndian.Uint64(uid[0:8])

	// uid := time.Now().UnixMilli() // +2
	uid := time.Now().Unix() // +5
	return uint64(uid*100000) + uint64(rand.Intn(10000))
}

const (
	strVal = "abcdefghijklmnopqrstuvwxyz_ABCDEFGHIJKLMNOPQRSTUVWXYZ-0123456789"
	// _4K    = 4096
	_256B = 256
	// _64B   = 64
)

func (mgr *BlobnodeMgr) doPost(url string, operation string) int {
	log.Debugf("do post once, %s", url)

	buff := make([]byte, 0, 1)
	if operation == "Put" {
		if mgr.conf.Random {
			buff = make([]byte, len(dataBuff))
			copy(buff, dataBuff)

			// mock random put data
			if len(dataBuff) >= _256B {
				rand.Seed(time.Now().UnixNano())
				for i := 0; i < len(dataBuff); i += _256B {
					buff[i] = strVal[rand.Intn(len(strVal))]
				}
			}
		} else {
			buff = dataBuff
		}
	}
	log.Debugf("do post, op: %s, buff len: %d\n", operation, len(buff))

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(buff))
	if err != nil {
		panic(err)
	}

	req.Header.Set("Content-Type", "application/json")
	// req.ContentLength = int64(len(buff))
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

	if resp.StatusCode != http.StatusOK { // || resp.StatusCode == errcode.CodeChunkNoSpace { // 627
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

func (mgr *BlobnodeMgr) doGet(url string, operation string) int {
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
	crc := rsp.Header.Get("Crc")
	if rsp.ContentLength > 0 && rsp.Body != nil {
		// io.LimitReader(rsp.Body, rsp.ContentLength).Read(buf) // not have limiter
		// io.CopyN(ioutil.Discard, rsp.Body, rsp.ContentLength)
		// buf := make([]byte, rsp.ContentLength)
		dst := bytes.NewBuffer([]byte{})
		rd := io.LimitReader(rsp.Body, rsp.ContentLength)
		// n, err := io.ReadAll(rsp.Body)
		// http get, operation: Get, url:http://ip:8889/shard/get/diskid/653/vuid/8127987705146507265/bid/47?iotype=0, response:1048576, dst:1048576, crc:1561641303
		// log.Infof("http get, operation: %s, url:%s, response:%d, dst:%d, crc:%s, data:%s", operation, url, rsp.ContentLength, len(buf), crc, buf[len(buf)-1])
		// log.Infof("get data, url:%s, len:%d, crc:%s, data:%s, n:%d, err:%+v", url, rsp.ContentLength, crc, dst.String(), n, err)
		if mgr.conf.Check {
			crc32 := crc32.NewIEEE()
			body := io.TeeReader(rd, crc32)
			n, err := io.CopyN(dst, body, rsp.ContentLength)
			crcSum := crc32.Sum32()
			crcNum, _ := strconv.Atoi(crc)
			log.Infof("get data, url:%s, len:%d, expectCrc:%d, crcSum32:%d, n:%d, err:%+v", url, rsp.ContentLength, crcNum, crcSum, n, err)
			if crcNum != int(crcSum) {
				log.Warnf("get data, crc not match, expect:%d, actual:%d, url:%s, len:%d, n:%d, err:%+v",
					crcNum, crcSum, url, rsp.ContentLength, n, err)
			}
		} else {
			n, err := io.CopyN(ioutil.Discard, rd, rsp.ContentLength)
			if err != nil {
				log.Warnf("get data error, url:%s, len:%d, n:%d, err:%+v", url, rsp.ContentLength, n, err)
			}
		}
	}
	//log.Debugf("do http get, crc=%s", crc) // !(EXTRA []string=[119067115])
	//for key, val := range rsp.Header {
	//	if key == "Crc" {
	//		log.Debugf("do http get, crc=%v", val) // !(EXTRA []string=[119067115])
	//	}
	//}

	// log.Debugf("do http get, url:%s, resp body: %s, resp:%+v", url, string(buf), rsp)
	log.Debugf("do http get, url:%s, crc=%s, resp:%+v", url, crc, rsp)
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
		diskMap:      make([]*bnapi.DiskInfo, 1),
		diskChunkMap: make(map[proto.DiskID][]*bnapi.ChunkInfo),
	}

	mgr.diskMap[0] = &bnapi.DiskInfo{
		DiskHeartBeatInfo: bnapi.DiskHeartBeatInfo{
			DiskID: 168,
		},
	}
	mgr.diskChunkMap[168] = make([]*bnapi.ChunkInfo, 2)
	mgr.diskChunkMap[168][0] = &bnapi.ChunkInfo{Vuid: 1234}
	mgr.diskChunkMap[168][1] = &bnapi.ChunkInfo{Vuid: 5678}
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

	file, err := os.OpenFile(fPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
				fmt.Printf("idx:%d, putCnt=%d, getCnt=%d, delCnt=%d \n", idx, atomic.LoadUint64(&putCnt), atomic.LoadUint64(&getCnt), atomic.LoadUint64(&delCnt))
			}
		}
	}()
}

// MixedOperationState tracks the state of chunks in mixed operations
type MixedOperationState struct {
	idx        int
	vuid       proto.Vuid
	diskID     proto.DiskID
	startBid   uint64
	writtenBid uint64
	isWritten  bool
}

// mixedOperations performs mixed read/write/delete operations
func (mgr *BlobnodeMgr) mixedOperations(ctx context.Context) {
	chunkCount := mgr.conf.MaxChunkCnt   // 分配的chunk数量
	concurrentWrites := mgr.conf.PerDisk // 并发写入chunk数量

	for round := 0; round < mgr.conf.MaxRound; round++ {
		atomic.StoreUint64(&putCnt, 0)
		atomic.StoreUint64(&getCnt, 0)
		atomic.StoreUint64(&delCnt, 0)

		// Step 1: Allocate chunks
		chunks := make([]*MixedOperationState, chunkCount)
		log.Info("Allocating chunks...")
		for i := 0; i < chunkCount; i++ {
			diskID := mgr.conf.DiskId // mgr.diskMap[0].DiskID // Use first disk for simplicity
			vuid := proto.Vuid(time.Now().UnixNano() + int64(i))
			mgr.singleAlloc(i, vuid, diskID)
			chunks[i] = &MixedOperationState{
				idx:        i,
				vuid:       vuid,
				diskID:     diskID,
				startBid:   1, // genId(),
				writtenBid: 0,
				isWritten:  false,
			}
		}
		log.Infof("chunk len=%d, chunk[0]=%v\n", len(chunks), *chunks[0])

		// Step 2: Start initial concurrent writes
		var wgWt, wgRd sync.WaitGroup
		writeChan := make(chan *MixedOperationState, chunkCount)
		readChan := make(chan *MixedOperationState, chunkCount)
		doneChan := make(chan struct{})
		mgr.done = make(chan struct{}, concurrentWrites)

		// Launch write workers
		for i := 0; i < concurrentWrites; i++ {
			wgWt.Add(1)
			go func() {
				defer wgWt.Done()
				for chunk := range writeChan {
					mgr.mixedWrite(ctx, chunk)
					chunk.isWritten = true
					readChan <- chunk // Make chunk available for reading
				}
			}()
		}

		// Launch read worker
		for i := 0; i < concurrentWrites; i++ {
			wgRd.Add(1)
			go func() {
				defer wgRd.Done()
				for chunk := range readChan {
					if chunk.isWritten {
						mgr.mixedRead(ctx, chunk)
					}
				}
			}()
		}

		// Feed initial chunks to writers
		for i := 0; i < chunkCount; i++ {
			writeChan <- chunks[i]
		}
		close(writeChan)

		// Wait for all operations to complete
		go func() {
			wgWt.Wait()
			close(readChan)
			wgRd.Wait()
			close(doneChan)
		}()

		// Wait for completion or context cancellation
		select {
		case <-ctx.Done():
			return
		case <-doneChan:
		}

		// Step 3: Delete all chunks
		log.Info("Deleting chunks...")
		for _, chunk := range chunks {
			mgr.mixedDelete(ctx, chunk)
		}

		log.Infof("Completed one round of mixed operations. Put: %d, Get: %d, Delete: %d",
			atomic.LoadUint64(&putCnt), atomic.LoadUint64(&getCnt), atomic.LoadUint64(&delCnt))
		// for i := 0; i < cap(mgr.done); i++ {
		//	<-mgr.done
		// }
		log.Infof("end round:%d", round)
	}
	close(mgr.done)
	log.Info("end, mix operation, max round")
}

func (mgr *BlobnodeMgr) mixedWrite(ctx context.Context, chunkInfo *MixedOperationState) {
	// threadCh := make(chan struct{}, mgr.conf.PerDisk)
	//
	// for idx, vuid := range chunks {
	//	threadCh <- struct{}{}
	//	log.Infof("start work, idx:%d, diskId:%d, vuid:%d", idx, dkId, vuid)
	//	go fn(idx, vuid, dkId, threadCh)
	// }
	log.Infof("start put, idx:%d, vuid:%d, diskID:%d", chunkInfo.idx, chunkInfo.vuid, chunkInfo.diskID)
	mgr.putParallel(chunkInfo.idx, chunkInfo.vuid, chunkInfo.diskID)
	<-mgr.done
}

func (mgr *BlobnodeMgr) mixedRead(ctx context.Context, chunkInfo *MixedOperationState) {
	log.Infof("start get, idx:%d, vuid:%d, diskID:%d", chunkInfo.idx, chunkInfo.vuid, chunkInfo.diskID)
	mgr.getParallel(chunkInfo.idx, chunkInfo.vuid, chunkInfo.diskID)
	<-mgr.done
}

func (mgr *BlobnodeMgr) mixedDelete(ctx context.Context, chunkInfo *MixedOperationState) {
	log.Infof("start del, idx:%d, vuid:%d, diskID:%d", chunkInfo.idx, chunkInfo.vuid, chunkInfo.diskID)
	mgr.delParallel(chunkInfo.idx, chunkInfo.vuid, chunkInfo.diskID)

	log.Infof("start release, idx:%d, vuid:%d, diskID:%d", chunkInfo.idx, chunkInfo.vuid, chunkInfo.diskID)
	mgr.singleRelease(chunkInfo.idx, chunkInfo.vuid, chunkInfo.diskID)

	<-mgr.done
}
