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

// 0. 写文件，大IO-8M，小IO-64k
// 1. 单个协程写，后台开一个写的协程消费者，一个装数据的队列，多个产生数据的生产者。不停的取出队列的数据，append写入到某个文件
// 2. 多个协程并发，append写入一个文件

// 1. 一次性往队列里扔，固定个数的data。 模拟瞬时大量请求，并查看谁先消费完
// 2. 慢慢的周期往队列里扔数据。 查看长尾的效应

var (
	READ_FILE        = "./readFile.log" // "./demo/read8M.log"  read64K.log
	WRITE_FILE       = "./dstFile.log"
	THREAD_NUM       = 2    // 生产者的并发个数
	QUEUE_SIZE       = 20   // 缓存队列的长度
	LOOP_TIME        = 30   // 持续写盘的时间
	INTERVAL         = 1    // 控制台输出的间隔
	IS_SINGLE        = true // 是否是单协程写
	DELAY_ARRAY      = 2000 // 记录时延的数组长度
	TOTAL_DATA_COUNT = 1    // 模拟客户端请求的总个数，固定IO大小
	COST_TIME_FILE   = "costTimeInfo"

	DefaultInterval = "1us"
	//CONSUME_INTERVAL time.Duration = 1000 // 消费间隔定时器，单位ns
	PRODUCT_INTERVAL time.Duration = 1000 // 生产者间隔定时器，单位ns

	gCnt        int           // 单协程下统计写入请求的总个数
	gMaxDequeue time.Duration // 单协程下，最大出队时间
)

// 入队的每个节点
type NodeData struct {
	data string
	time int64
}

// gflag: ./<exe> -r ./readFile.log -n 8 -t 200 -m=false
func parseFlag() (err error) {
	rdFile := flag.String("r", READ_FILE, "read from file")
	wtFile := flag.String("w", WRITE_FILE, "write to file")
	goNum := flag.Int("n", THREAD_NUM, "multi consumer num")
	queueSize := flag.Int("q", QUEUE_SIZE, "queue max size")
	totalDataCount := flag.Int("c", TOTAL_DATA_COUNT, "total request data count, many datas at once")
	loopTime := flag.Int("t", LOOP_TIME, "write disk elapsed time")
	interval := flag.Int("i", INTERVAL, "interval time")
	isSingerWrite := flag.Bool("m", IS_SINGLE, "is single write mode")
	//consumeInterval := flag.String("ci", DefaultInterval, "consume interval")
	productInterval := flag.String("pi", DefaultInterval, "product interval")
	delayArrLen := flag.Int("d", DELAY_ARRAY, "delay array length")
	flag.Parse()

	READ_FILE = *rdFile
	WRITE_FILE = *wtFile
	THREAD_NUM = *goNum
	QUEUE_SIZE = *queueSize
	LOOP_TIME = *loopTime
	INTERVAL = *interval
	IS_SINGLE = *isSingerWrite
	DELAY_ARRAY = *delayArrLen
	TOTAL_DATA_COUNT = *totalDataCount

	fmt.Printf("parse flag: readFile=%s, writeFile=%s, consumerNum=%d, QmaxSiz=%d, eelapsedTime=%d, interval=%d, "+
		"batchDataAtOnce=%d, productInterval=%s, delayArrLen=%d, isSingleMode=%v \n",
		READ_FILE, WRITE_FILE, THREAD_NUM, QUEUE_SIZE, LOOP_TIME, INTERVAL, TOTAL_DATA_COUNT, *productInterval, DELAY_ARRAY, IS_SINGLE)

	PRODUCT_INTERVAL, err = time.ParseDuration(*productInterval)
	if err != nil {
		return err
	}
	return nil
}

func main() {
	fmt.Println("Tests the write performance of a single hard disk")
	// init
	if err := parseFlag(); err != nil {
		fmt.Println("ERROR... fail to parse flag.", err)
	}
	doneTest := make(chan bool)         // 结束测试
	finishRecord := make(chan bool)     // 单协程closed完毕
	cntCh := make(chan int, THREAD_NUM) // 多协程统计写请求总个数
	costCh := make(chan []time.Duration, THREAD_NUM)
	list := make(chan NodeData, QUEUE_SIZE) // list := newListQueue()
	if TOTAL_DATA_COUNT > 1 {
		list = make(chan NodeData, TOTAL_DATA_COUNT)
	}

	os.Remove(WRITE_FILE)
	fh, err := os.OpenFile(WRITE_FILE, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		fmt.Println("ERROR... open file err:", err.Error())
	}
	defer fh.Close()

	// 改为单个生产者
	go productMulti(0, list, doneTest)
	tStart := time.Now()

	if IS_SINGLE { // 单个消费者
		go consumeOne(list, doneTest, finishRecord, fh)
	} else { // 测试多线程写
		for i := 0; i < THREAD_NUM; i++ {
			go consumeMulti(i, list, doneTest, cntCh, costCh, fh)
		}
	}

	// 主线程 周期打印
	wait := make(chan bool)
	go showInfoPeriod(list, doneTest, wait)
	<-wait
	showResult(finishRecord, cntCh, costCh)
	fmt.Println("all done... total cost time: ", time.Since(tStart))
	time.Sleep(time.Second)
}

func consumeOne(list chan NodeData, closed, finishRecord chan bool, fh *os.File) {
	costArr := make([]time.Duration, 0, DELAY_ARRAY)
	costWtDoneArr := make([]time.Duration, 0, DELAY_ARRAY)

	for {
		node := <-list
		tm := time.Unix(0, node.time)
		cost := time.Since(tm)
		costArr = append(costArr, cost)
		if cost > gMaxDequeue {
			gMaxDequeue = cost
		}

		appendWrite(fh, []byte(node.data))
		cost = time.Since(tm)
		costWtDoneArr = append(costWtDoneArr, cost)
		gCnt++ //idx++

		select {
		case <-closed:
			fmt.Printf("close consumer... gCnt=%d, gMaxDequeue=%v, delayCostCount=%d \n", gCnt, gMaxDequeue, len(costArr))
			//fmt.Printf("cost time deQueue... len=%d, costArr=%v \n", len(costArr), costArr)
			//fmt.Printf("cost time Write done... len=%d, costWtDoneArr=%v \n", len(costWtDoneArr), costWtDoneArr)
			recordCostTimeFile(COST_TIME_FILE+".one.log", costArr, costWtDoneArr)
			finishRecord <- true
			return

		default:
			break
		} // end select
	} // end for
}

func consumeMulti(i int, list chan NodeData, closed chan bool, cntSum chan int, costSum chan []time.Duration, fh *os.File) {
	costArr := make([]time.Duration, 0, DELAY_ARRAY)
	costWtDoneArr := make([]time.Duration, 0, DELAY_ARRAY)
	idx := 0

	for {
		node := <-list
		idx++
		tm := time.Unix(0, node.time)
		cost := time.Since(tm)
		if i == 0 && cost > gMaxDequeue {
			gMaxDequeue = cost
		}
		costArr = append(costArr, cost)

		_, err := fh.Write([]byte(node.data))
		if err != nil {
			fmt.Println("ERROR... consumeOne: fail to write file.", err)
		}
		err = fh.Sync()
		if err != nil {
			fmt.Println("ERROR... consumeOne: fail to sync file.", err)
		}
		cost = time.Since(tm)
		costWtDoneArr = append(costWtDoneArr, cost)

		select {
		case <-closed:
			fmt.Printf("close multi consumer...%d, write cost time record len=%d, \n", i, len(costWtDoneArr))
			cntSum <- idx
			costSum <- costWtDoneArr
			return

		default:
			break
		} // end select
	}
}

func productMulti(i int, list chan NodeData, closed chan bool) {
	datas, err := ioutil.ReadFile(READ_FILE)
	if err != nil {
		fmt.Println("ERROR... product bad datas, read fail")
		return
	}

	// 预埋数据，一次性固定生产一堆数据，模拟瞬时大量请求
	if TOTAL_DATA_COUNT > 1 {
		productConstCount(list, closed, datas)
		return
	}

	productDataPeriod(i, list, closed, datas)
}

// 生产固定个数的请求
func productConstCount(list chan NodeData, closed chan bool, datas []byte) {
	fmt.Printf("I'm going to produce %d datas at once \n", TOTAL_DATA_COUNT)

	ch := []byte("abcdefghijklmnopqrstuvwxyz")
	maxChar := len(ch)
	maxIdx := len(datas)
	rand.Seed(time.Now().UnixNano())

	for i := 0; i < TOTAL_DATA_COUNT; i++ {
		datas[rand.Intn(maxIdx)] = ch[rand.Intn(maxChar)] // 随机修改一点数据，模拟不同的请求数据
		node := NodeData{
			data: string(datas),
			time: time.Now().UnixNano(),
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
	fmt.Printf("I'm going to produce 1 datas per %v \n", PRODUCT_INTERVAL)
	ticker := time.NewTicker(PRODUCT_INTERVAL)
	defer ticker.Stop()

	ch := []byte("abcdefghijklmnopqrstuvwxyz")
	maxChar := len(ch)
	maxIdx := len(datas)
	rand.Seed(time.Now().UnixNano())

	// 先生产够一批，和多线程个数匹配的数据
	for i := 0; i < THREAD_NUM; i++ {
		node := NodeData{
			data: string(datas),
			time: time.Now().UnixNano(),
		}
		list <- node
	}

	for {
		select {
		case <-ticker.C:
			datas[rand.Intn(maxIdx)] = ch[rand.Intn(maxChar)] // 随机修改一点数据，模拟不同的请求数据
			node := NodeData{
				data: string(datas),
				time: time.Now().UnixNano(),
			}
			list <- node // list.push(node)
		case <-closed:
			fmt.Println("close product...", i)
			return
		}
	}
}

func recordCostTimeFile(path string, dequeTm, writeTm []time.Duration) {
	type costTime struct {
		DequeCostInt []time.Duration `json:"deque_int"`
		WriteCostInt []time.Duration `json:"write_int"`
		//DequeCostStr []string        `json:"deque_cost"`
		//WriteCostStr []string        `json:"write_cost"`
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
		//DequeCostStr: dequeStr,
		//WriteCostStr: writeStr,
	}
	datas, _ := json.MarshalIndent(cost, "", " ")
	err := ioutil.WriteFile(path, datas, 0644)
	if err != nil {
		fmt.Println("ERROR... write file err:", err)
	}
}

func appendWrite(fh *os.File, data []byte) error {
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

func showInfoPeriod(list chan NodeData, doneTest chan bool, wait chan bool) {
	ticker := time.NewTicker(time.Second * time.Duration(INTERVAL))
	defer ticker.Stop()
	defer func() {
		wait <- true
	}()
	t := 0

	for {
		select {
		case <-ticker.C:
			t += INTERVAL
			fmt.Printf("%d s...  size: %d \n", t, len(list))
			if t >= LOOP_TIME {
				goto FINAL
			}
		case <-doneTest:
			fmt.Printf("Finished consume const %d data\n", TOTAL_DATA_COUNT)
			return
		}
	}

FINAL:
	fmt.Println("It's time to stop request, closing......")
	close(doneTest)
}

func showResult(finishRecord chan bool, cntCh chan int, costCh chan []time.Duration) {
	if IS_SINGLE {
		//waitTime := time.Now()
		fmt.Printf("main go: single consumer done... cnt=%d, gMaxDequeue=%v \n", gCnt, gMaxDequeue)
		//fmt.Println("waiting consumer close...")
		<-finishRecord
		//fmt.Println("wait for closing consumer, cleanup cost time: ", time.Since(waitTime))
	} else {
		// multi thread consume
		cnt := 0
		for i := 0; i < THREAD_NUM; i++ {
			cnt += <-cntCh
		}
		fmt.Printf("multi consumer done... write cnt: %d , gMaxDequeue=%v \n", cnt, gMaxDequeue)

		cnt = 0
		costUn := make([][]time.Duration, THREAD_NUM)
		for i := 0; i < THREAD_NUM; i++ {
			//costEach := <-costCh
			//costTm = append(costTm, costEach...)
			costUn[i] = <-costCh
			cnt += len(costUn[i])
		}

		costTm := make([]time.Duration, cnt)
		for i := 0; i < cnt; {
			for j := 0; j < THREAD_NUM; j++ {
				if len(costUn[j]) > 0 {
					costTm[i] = costUn[j][0]
					costUn[j] = costUn[j][1:]
					i++
				}
			}
		}

		recordCostTimeFile(COST_TIME_FILE+".many.log", nil, costTm)
		fmt.Println("multi consumer record cost time, count=", len(costTm))
	}
}
