package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"time"
)

// 测试目的，只控制写线程，不控制读的并发: 0. 写文件，大IO-8M，小IO-64k
// 1. 单个协程写，后台开一个写的协程消费者，一个装数据的队列，多个产生数据的生产者。不停的取出队列的数据，append写入到某个文件
// 2. 多个协程并发，append写入一个文件

// 1. 一次性往队列里扔，固定个数的data。 模拟瞬时大量请求，并查看谁先消费完
// 2. 慢慢的周期往队列里扔数据。 查看长尾的效应

const (
	// defaultIsSingleMode   = true             // 是否是单协程写
	defaultIsReservedData = false            // 预埋一批数据
	defaultThreadNum      = 1                // 消费者的并发个数
	defaultQueueSize      = 20               // 缓存队列的长度
	defaultLoopTime       = 30               // 持续写盘的时间
	defaultBatchCount     = 1                // 模拟客户端一次请求的总个数，固定IO大小
	defaultShowInterval   = 1                // 控制台输出的间隔
	defaultRecordDelay    = 2000             // 记录时延的数组长度
	defaultModel          = 1                // 测试模式模型,1:append write; 2:random write; 3:random read ; 4: random read and write
	defaultReadFile       = "./readFile.log" // "./demo/read8M.log"  read64K.log
	defaultWriteFile      = "./dstFile.log"
	defaultInterval       = "1us"
	defaultRecordCostFile = "costTimeInfo"
	defaultConfigFile     = "disk.conf"
)

const (
	modelAppendWrite = iota + 1
	modelRandomWrite
	modelRandomRdWt
	// modelRandomRead
)

var (
	rdFile         = flag.String("r", defaultReadFile, "Read from file")
	wtFile         = flag.String("w", defaultWriteFile, "Write to file")
	isReservedData = flag.Bool("rm", defaultIsReservedData, "is Reserved data Mode")
	//isSingerWrite  = flag.Bool("sm", defaultIsSingleMode, "is single write mode")
	consumerNum  = flag.Int("c", defaultThreadNum, "multi Consumer num")
	queueSize    = flag.Int("q", defaultQueueSize, "requst Queue max size")
	loopTime     = flag.Int("t", defaultLoopTime, "write disk elapsed Time")
	batchCount   = flag.Int("b", defaultBatchCount, "total request data count, Batch datas at once")
	showInterval = flag.Int("i", defaultShowInterval, "show Interval")
	delayArrLen  = flag.Int("d", defaultRecordDelay, "record Delay array length")
	model        = flag.Int("m", defaultModel, "test Model: 1:append write; 2:random write; 3:random read ; 4: random read and write")
	// consumeInterval := flag.String("ci", DefaultInterval, "consume interval")
	productInterval = flag.String("pi", defaultInterval, "Product Interval")
	confFile        = flag.String("f", defaultConfigFile, "config File")

	gTotalReqCnt   int // 单协程下统计写入请求的总个数
	gProductPeriod time.Duration
	gMaxDequeue    time.Duration // 单协程下，最大出队时间
	gConf          *DiskConfig
)

// 入队的每个节点
type NodeData struct {
	data string
	time time.Time
}

type DiskConfig struct {
	Model            int    `json:"model"`
	ReadCntWhenWrite int    `json:"read_cnt_when_write"`
	DirPrefix        string `json:"dir_prefix"`
	ReadPrefix       string `json:"read_prefix"`
}

// gflag: ./<exe> -r ./readFile.log -n 8 -t 200 -m=false
func parseFlag() (err error) {
	flag.Parse()
	fmt.Printf("parse flag: readFile=%s, writeFile=%s, consumerNum=%d, QmaxSiz=%d, eelapsedTime=%d, interval=%d, "+
		"batchDataAtOnce=%d, productInterval=%s, delayArrLen=%d, isReservedData=%v \n",
		*rdFile, *wtFile, *consumerNum, *queueSize, *queueSize, *showInterval, *batchCount, *productInterval, *delayArrLen, *isReservedData)

	gProductPeriod, err = time.ParseDuration(*productInterval)
	if err != nil {
		return err
	}
	return nil
}

func main() {
	fmt.Println("Tests the write performance of a single hard disk, exec=", os.Args[0])
	// init
	var err error
	if err = parseFlag(); err != nil {
		fmt.Println("ERROR... fail to parse flag.", err)
	}
	doneTest := make(chan bool)           // 结束测试
	finishRecord := make(chan bool)       // 单协程closed完毕
	cntCh := make(chan int, *consumerNum) // 多协程统计写请求总个数
	costCh := make(chan []time.Duration, *consumerNum)
	list := make(chan NodeData, *queueSize) // list := newListQueue()
	if *isReservedData && *batchCount > 1 {
		list = make(chan NodeData, *batchCount)
	}
	gConf, err = initConfig()
	if err != nil {
		return
	}

	tStart := time.Now()
	switch *model {
	case modelAppendWrite: // 1:append write;
		commonModel(list, doneTest, finishRecord, cntCh, costCh, modelAppendWrite)
	case modelRandomWrite: // 2:random write;
		randomWrite(list, doneTest, finishRecord, cntCh, costCh, modelRandomWrite)
	//case modelRandomRead: // 3:random read ;
	//	randomRead()
	case modelRandomRdWt: // 4: random read and write
		randomReadAndWrite(list, doneTest, finishRecord, cntCh, costCh, modelRandomRdWt)
	default:
		fmt.Println("error test model flag, exit.")
		return
	}

	// 主线程 周期打印
	wait := make(chan bool)
	go showInfoPeriod(list, doneTest, wait)
	<-wait
	showResult(finishRecord, cntCh, costCh)
	fmt.Println("all done... total cost time: ", time.Since(tStart))
	time.Sleep(time.Second)
}

func consumeOne(list chan NodeData, closed, finishRecord chan bool, fh *os.File, mode int) {
	costArr := make([]time.Duration, 0, *delayArrLen)
	costWtDoneArr := make([]time.Duration, 0, *delayArrLen)

	for {
		node, ok := <-list
		if !ok {
			goto FINISH
		}
		cost := time.Since(node.time)
		costArr = append(costArr, cost)
		if cost > gMaxDequeue {
			gMaxDequeue = cost
		}

		if mode == modelAppendWrite {
			writeAppend(fh, []byte(node.data))
		} else {
			fh1, err := getRandomFile(gConf.DirPrefix, os.O_WRONLY|os.O_CREATE|os.O_APPEND)
			if err != nil {
				panic(err)
			}
			writeAndClose(fh1, []byte(node.data))
		}
		cost = time.Since(node.time)
		costWtDoneArr = append(costWtDoneArr, cost)
		gTotalReqCnt++ // idx++

		select {
		case <-closed:
			goto FINISH
		default:
			break
		} // end select
	} // end for
FINISH:
	fmt.Printf("close consumer... gTotalReqCnt=%d, gMaxDequeue=%v, delayCostCount=%d \n", gTotalReqCnt, gMaxDequeue, len(costArr))
	// fmt.Printf("cost time deQueue... len=%d, costArr=%v \n", len(costArr), costArr)
	// fmt.Printf("cost time Write done... len=%d, costWtDoneArr=%v \n", len(costWtDoneArr), costWtDoneArr)
	recordCostTimeFile(defaultRecordCostFile+".one.log", costArr, costWtDoneArr)
	finishRecord <- true
	return
}

func consumeMulti(i int, list chan NodeData, closed chan bool, cntSum chan int, costSum chan []time.Duration, fh *os.File, mode int) {
	costArr := make([]time.Duration, 0, *delayArrLen)
	costWtDoneArr := make([]time.Duration, 0, *delayArrLen)
	idx := 0

	for {
		node, ok := <-list
		if !ok {
			goto FINISH
		}
		idx++
		cost := time.Since(node.time)
		if i == 0 && cost > gMaxDequeue {
			gMaxDequeue = cost
		}
		costArr = append(costArr, cost)

		if mode == modelAppendWrite {
			writeAppend(fh, []byte(node.data))
		} else {
			fh1, err := getRandomFile(gConf.DirPrefix, os.O_WRONLY|os.O_CREATE|os.O_APPEND)
			if err != nil {
				panic(err)
			}
			writeAndClose(fh1, []byte(node.data))
		}

		cost = time.Since(node.time)
		costWtDoneArr = append(costWtDoneArr, cost)

		select {
		case <-closed:
			goto FINISH
		default:
			break // break select
		}
	}
FINISH:
	fmt.Printf("close multi consumer...%d, write cost time record len=%d, \n", i, len(costWtDoneArr))
	cntSum <- idx
	costSum <- costWtDoneArr
	return
}

func productMulti(i int, list chan NodeData, closed chan bool) {
	datas, err := ioutil.ReadFile(*rdFile)
	if err != nil {
		fmt.Println("ERROR... product bad datas, read fail")
	}

	// 预埋数据，一次性固定生产一堆数据，模拟瞬时大量请求
	if *isReservedData {
		productConstCount(list, closed, datas)
	}

	productDataPeriod(i, list, closed, datas)
}

// 生产固定个数的请求
func productConstCount(list chan NodeData, closed chan bool, datas []byte) {
	fmt.Printf("I'm going to produce %d datas at once \n", *batchCount)

	ch := []byte("abcdefghijklmnopqrstuvwxyz")
	maxChar := len(ch)
	maxIdx := len(datas)
	rand.Seed(time.Now().UnixNano())

	for i := 0; i < *batchCount; i++ {
		datas[rand.Intn(maxIdx)] = ch[rand.Intn(maxChar)] // 随机修改一点数据，模拟不同的请求数据
		node := NodeData{
			data: string(datas),
			time: time.Now(),
		}
		list <- node // list.push(datas)
	}
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()

		for {
			if len(list) == 0 {
				close(closed)
				return
			}
			<-ticker.C
		}
	}()
	return
}

// 周期的生产数据
func productDataPeriod(i int, list chan NodeData, closed chan bool, datas []byte) {
	fmt.Printf("I'm going to produce %d datas per %v \n", *batchCount, gProductPeriod)
	tk := time.NewTicker(gProductPeriod)
	defer tk.Stop()

	ch := []byte("abcdefghijklmnopqrstuvwxyz")
	maxChar := len(ch)
	maxIdx := len(datas)
	rand.Seed(time.Now().UnixNano())

	// 先生产够一批，和多线程个数匹配的数据
	for i := 0; i < *consumerNum; i++ {
		node := NodeData{
			data: string(datas),
			time: time.Now(),
		}
		list <- node
	}

	for {
		select {
		case <-tk.C:
			datas[rand.Intn(maxIdx)] = ch[rand.Intn(maxChar)] // 随机修改一点数据，模拟不同的请求数据
			node := NodeData{
				data: string(datas),
				time: time.Now(),
			}
			for j := 0; j < *batchCount; j++ {
				list <- node // list.push(node)
			}
		case <-closed:
			close(list)
			fmt.Println("close product...", i)
			return
		}
	}
}

func recordCostTimeFile(path string, dequeTm, writeTm []time.Duration) {
	type costTime struct {
		DequeCostInt []time.Duration `json:"deque_int"`
		WriteCostInt []time.Duration `json:"write_int"`
		// DequeCostStr []string        `json:"deque_cost"`
		// WriteCostStr []string        `json:"write_cost"`
	}

	//dequeStr := make([]string, 0, len(dequeTm))
	//for _, v := range dequeTm {
	//	dequeStr = append(dequeStr, v.String())
	//}
	//writeStr := make([]string, 0, len(writeTm))
	//for _, v := range writeTm {
	//	writeStr = append(writeStr, v.String())
	//}

	cost := costTime{
		DequeCostInt: dequeTm,
		WriteCostInt: writeTm,
		// DequeCostStr: dequeStr,
		// WriteCostStr: writeStr,
	}
	datas, _ := json.MarshalIndent(cost, "", " ")
	err := ioutil.WriteFile(path, datas, 0o644)
	if err != nil {
		fmt.Println("ERROR... write file err:", err)
	}
}

func writeAppend(fh *os.File, data []byte) error {
	_, err := fh.Write(data)
	if err != nil {
		fmt.Println("ERROR... consumeOne: fail to write file.", err)
	}
	err = fh.Sync()
	if err != nil {
		fmt.Println("ERROR... consumeOne: fail to sync file.", err)
	}
	return err
}

func writeAndClose(fh *os.File, data []byte) error {
	err := writeAppend(fh, data)
	if err != nil {
		return err
	}
	return fh.Close()
}

func showInfoPeriod(list chan NodeData, doneTest chan bool, wait chan bool) {
	ticker := time.NewTicker(time.Second * time.Duration(*showInterval))
	defer ticker.Stop()
	defer func() {
		wait <- true
	}()
	t := 0

	for {
		select {
		case <-ticker.C:
			t += *showInterval
			fmt.Printf("%d s...  size: %d \n", t, len(list))
			if t >= *loopTime {
				goto FINAL
			}
		case <-doneTest:
			fmt.Printf("Finished consume const %d data\n", *batchCount)
			return
		}
	}

FINAL:
	fmt.Println("It's time to stop request, closing......")
	close(doneTest)
}

func showResult(finishRecord chan bool, cntCh chan int, costCh chan []time.Duration) {
	if *consumerNum > 1 {
		// waitTime := time.Now()
		fmt.Printf("main go: single consumer done... cnt=%d, gMaxDequeue=%v \n", gTotalReqCnt, gMaxDequeue)
		// fmt.Println("waiting consumer close...")
		<-finishRecord
		// fmt.Println("wait for closing consumer, cleanup cost time: ", time.Since(waitTime))
	} else {
		// multi thread consume
		cnt := 0
		for i := 0; i < *consumerNum; i++ {
			cnt += <-cntCh
		}
		fmt.Printf("multi consumer done... write cnt: %d , gMaxDequeue=%v \n", cnt, gMaxDequeue)

		cnt = 0
		costUn := make([][]time.Duration, *consumerNum)
		for i := 0; i < *consumerNum; i++ {
			// costEach := <-costCh
			// costTm = append(costTm, costEach...)
			costUn[i] = <-costCh
			cnt += len(costUn[i])
		}

		costTm := make([]time.Duration, cnt)
		for i := 0; i < cnt; {
			for j := 0; j < *consumerNum; j++ {
				if len(costUn[j]) > 0 {
					costTm[i] = costUn[j][0]
					costUn[j] = costUn[j][1:]
					i++
				}
			}
		}

		recordCostTimeFile(defaultRecordCostFile+".many.log", nil, costTm)
		fmt.Println("multi consumer record cost time, count=", len(costTm))
	}
}

func commonModel(list chan NodeData, doneTest, finishRecord chan bool, cntCh chan int, costCh chan []time.Duration, mode int) {
	os.Remove(*wtFile)
	fh, err := os.OpenFile(*wtFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Println("ERROR... open file err:", err.Error())
		return
	}
	defer fh.Close()
	_, err = os.Stat(*rdFile)
	if os.IsNotExist(err) {
		fmt.Println("ERROR... read file err:", err.Error())
		panic(err)
		return
	}

	// 改为单个生产者
	go productMulti(0, list, doneTest)

	if *consumerNum > 1 { // 单个消费者
		go consumeOne(list, doneTest, finishRecord, fh, mode)
	} else { // 测试多线程写
		for i := 0; i < *consumerNum; i++ {
			go consumeMulti(i, list, doneTest, cntCh, costCh, fh, mode)
		}
	}
}

//func appendWriteModel(list chan NodeData, doneTest, finishRecord chan bool, cntCh chan int, costCh chan []time.Duration, model int) {
//	commonModel(list, doneTest, finishRecord, cntCh, costCh, model)
//}

func initConfig() (*DiskConfig, error) {
	data, err := ioutil.ReadFile(*confFile)
	if nil != err {
		fmt.Println("Fail to read config file, err=", err)
		return nil, err
	}

	conf := DiskConfig{}
	err = json.Unmarshal(data, &conf)
	if err != nil {
		fmt.Println("Fail to parse config json, err=", err)
		return nil, err
	}

	return &conf, nil
}

var (
	// dirPrefix = "/home/oppo/code/cubefs/blobstore/cmd/diskWrite"
	// dirPrefix = "/home/service/var/data1/io_test"
	dirTop    = "test_dir_%d"
	dirMiddle = "vdb.1_%d.dir"
	dirDown   = "vdb_f000%d.file"
)

func randomWrite(list chan NodeData, doneTest, finishRecord chan bool, cntCh chan int, costCh chan []time.Duration, mode int) {
	dirPrefix := gConf.DirPrefix
	s, err := os.Stat(dirPrefix)
	if err != nil || !s.IsDir() {
		fmt.Printf("ERROR... dir=%s, err=%v \n", dirPrefix, err)
		panic(err)
	}

	commonModel(list, doneTest, finishRecord, cntCh, costCh, mode)
}

func randomRead() {
}

func randomReadAndWrite(list chan NodeData, doneTest, finishRecord chan bool, cntCh chan int, costCh chan []time.Duration, mode int) {
	dirPrefix := gConf.DirPrefix
	s, err := os.Stat(dirPrefix)
	if err != nil || !s.IsDir() {
		fmt.Printf("ERROR... dir=%s, err=%v \n", dirPrefix, err)
		panic(err)
	}
	if gConf.ReadCntWhenWrite <= 0 {
		panic("ERROR... read_cnt_when_write=%d, it must greater than 0")
	}

	for i := 0; i < gConf.ReadCntWhenWrite; i++ {
		go func() {
			tk := time.NewTicker(gProductPeriod)
			defer tk.Stop()

			for {
				fh, err := getRandomFile(gConf.ReadPrefix, os.O_RDONLY)
				if err != nil {
					panic(err)
				}
				//fh, _ = os.OpenFile("test_dir_1/vdb.1_5.dir/vdb_f0003.file", os.O_RDONLY, 0o644)

				// io.Copy(ioutil.Discard, fh)
				fi, err := fh.Stat()
				rdByte := 1024 * 1024 * 4
				buf := make([]byte, rdByte)
				rdByte, err = fh.ReadAt(buf, rand.Int63n(fi.Size()-int64(rdByte)))
				fh.Close()

				select {
				case <-doneTest:
					return
				case <-tk.C:
				default:
				}
			}
		}()
	}

	commonModel(list, doneTest, finishRecord, cntCh, costCh, mode)
}

func getRandomFile(dirPrefix string, flag int) (*os.File, error) {
	numTop, numMiddle, numDown := 2, 5, 9
	// rand.Seed(time.Now().UnixNano())
	fName := fmt.Sprintf(dirTop+"/"+dirMiddle+"/"+dirDown, rand.Intn(numTop)+1, rand.Intn(numMiddle)+1, rand.Intn(numDown)+1)
	fPath := fmt.Sprintf(dirPrefix + "/" + fName)
	// fmt.Println("test file fName=", fName)
	return os.OpenFile(fPath, flag, 0o644)
}
