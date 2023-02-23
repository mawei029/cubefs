package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"sync/atomic"
	"time"
)

// 0. 写文件，大IO-8M，小IO-64k
// 1. 单个协程写，后台开一个写的协程消费者，一个装数据的队列，多个产生数据的生产者。不停的取出队列的数据，append写入到某个文件
// 2. 多个协程并发，append写入一个文件

var (
	READ_FILE      = "./readFile.log" // "./demo/read8M.log"  read64K.log
	WRITE_FILE     = "./dstFile.log"
	THREAD_NUM     = 8    // 生产者的并发个数
	QUEUE_SIZE     = 200  // 缓存队列的长度
	LOOP_TIME      = 30   // 持续写盘的时间
	INTERVAL       = 1    // 控制台输出的间隔
	IS_SINGLE      = true // 是否是单协程写
	DELAY_ARRAY    = 2000 // 记录时延的数组长度
	COST_TIME_FILE = "costTimeInfo"

	DefaultInterval                = "1us"
	CONSUME_INTERVAL time.Duration = 1000 // 消费间隔定时器，单位ns
	PRODUCT_INTERVAL time.Duration = 1000 // 生产者间隔定时器，单位ns

	gCnt        int           = 0 // 单协程下统计写入请求的总个数
	gIsStop     int32         = 0 // 单协程下，是否停止，原子变量
	gMaxDequeue time.Duration     // 单协程下，最大出队时间
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
	goNum := flag.Int("n", THREAD_NUM, "multi product num")
	queueSize := flag.Int("q", QUEUE_SIZE, "queue max size")
	loopTime := flag.Int("t", LOOP_TIME, "write disk elapsed time")
	interval := flag.Int("i", INTERVAL, "interval time")
	isSingerWrite := flag.Bool("m", IS_SINGLE, "is single write mode")
	consumeInterval := flag.String("ci", DefaultInterval, "consume interval")
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

	fmt.Printf("parse flag: readFile=%s, writeFile=%s, productNum=%d, QmaxSiz=%d, eelapsedTime=%d, interval=%d, "+
		"consumeInterval=%s, productInterval=%s, delayArrLen=%d, isSingleMode=%v \n",
		READ_FILE, WRITE_FILE, THREAD_NUM, QUEUE_SIZE, LOOP_TIME, INTERVAL, *consumeInterval, *productInterval, DELAY_ARRAY, IS_SINGLE)

	CONSUME_INTERVAL, err = time.ParseDuration(*consumeInterval)
	if err != nil {
		return err
	}
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
	singleDone := make(chan bool)       // 单协程closed完毕
	cntCh := make(chan int, THREAD_NUM) // 多协程统计写请求总个数
	costCh := make(chan []time.Duration, THREAD_NUM)
	list := newListQueue()

	os.Remove(WRITE_FILE)
	fh, err := os.OpenFile(WRITE_FILE, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		fmt.Println("ERROR... open file err:", err.Error())
	}
	defer fh.Close()

	if IS_SINGLE {
		// 多个生产者
		for i := 0; i < THREAD_NUM; i++ {
			go productMulti(i, list, doneTest)
		}
		// 单个消费者
		go consumeOne(list, doneTest, singleDone, fh)
	} else {
		// 测试多线程写
		for i := 0; i < THREAD_NUM; i++ {
			go consumeMulti(i, doneTest, cntCh, costCh, fh)
		}
	}

	// 主线程 周期打印
	ticker := time.NewTicker(time.Second * time.Duration(INTERVAL))
	defer ticker.Stop()
	t := 0
	for {
		select {
		case <-ticker.C:
			list.lock.RLock()
			size := list.getSize()
			list.lock.RUnlock()
			t += INTERVAL
			fmt.Printf("%d s...  size: %d \n", t, size)
			if t >= LOOP_TIME {
				goto FINAL
			}
		}
	}

FINAL:
	fmt.Println("stop request, closing......")
	close(doneTest)
	atomic.StoreInt32(&gIsStop, 1)

	if IS_SINGLE {
		waitTime := time.Now()
		fmt.Printf("main go: single consumer done... cnt=%d, gMaxDequeue=%v \n", gCnt, gMaxDequeue)
		fmt.Println("waiting consumer close...")
		<-singleDone
		fmt.Println("wait for closing consumer, cleanup cost time: ", time.Since(waitTime))
	} else {
		// multi thread consume
		cnt := 0
		for i := 0; i < THREAD_NUM; i++ {
			cnt += <-cntCh
		}
		fmt.Printf("multi consumer done... write cnt: %d \n", cnt)

		costTm := make([]time.Duration, 0, cnt)
		for i := 0; i < THREAD_NUM; i++ {
			costEach := <-costCh
			costTm = append(costTm, costEach...)
		}
		recordCostTimeFile(COST_TIME_FILE+".many.log", nil, costTm)
		fmt.Println("multi consumer record cost time, count=", len(costTm))
	}
	time.Sleep(time.Second)
}

func consumeOne(list *MyQueue, closed, finish chan bool, fh *os.File) {
	ticker := time.NewTicker(CONSUME_INTERVAL)
	defer ticker.Stop()

	size := 0
	costArr := make([]time.Duration, 0, DELAY_ARRAY)
	costWtDoneArr := make([]time.Duration, 0, DELAY_ARRAY)
	for {
		select {
		case <-closed:
			fmt.Printf("close consumer... cnt=%d, gMaxDequeue=%v, timeCount=%d \n", gCnt, gMaxDequeue, len(costArr))
			//fmt.Printf("cost time deQueue... len=%d, costArr=%v \n", len(costArr), costArr)
			//fmt.Printf("cost time Write done... len=%d, costWtDoneArr=%v \n", len(costWtDoneArr), costWtDoneArr)
			recordCostTimeFile(COST_TIME_FILE+".one.log", costArr, costWtDoneArr)
			finish <- true
			return

		case <-ticker.C:
			list.lock.RLock()
			temp := list.head
			size = list.getSize()
			list.lock.RUnlock()
			if size != 0 {
				fmt.Println("consume... queue size:", size)
			}

			notEmpty := false
			for temp != nil && atomic.LoadInt32(&gIsStop) == 0 {
				notEmpty = true
				node, ok := list.pop().(NodeData)
				if !ok {
					fmt.Println("ERROR... consumeOne: fail to pop, bad datas")
				}
				//fmt.Println("consume... queue size:", size)
				tm := time.Unix(0, node.time)
				cost := time.Since(tm)
				costArr = append(costArr, cost)
				if cost > gMaxDequeue {
					gMaxDequeue = cost
				}
				//err := ioutil.WriteFile(WRITE_FILE, []byte(node.data), 0644)
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

				gCnt++ //idx++
				list.lock.RLock()
				temp = temp.next
				size = list.getSize()
				list.lock.RUnlock()
			} // end for list
			if notEmpty {
				fmt.Printf("consume once..., size=%d, gMaxDequeue=%v \n", gCnt, gMaxDequeue)
			}
		} // end select
	} // end for
}

func productMulti(i int, list *MyQueue, closed chan bool) {
	ticker := time.NewTicker(PRODUCT_INTERVAL)
	defer ticker.Stop()

	ch := []byte("abcdefghijklmnopqrstuvwxyz")
	idx := 0
	datas, err := ioutil.ReadFile(READ_FILE)
	if err != nil {
		fmt.Println("ERROR... product bad datas, read fail")
		return
	}
	rand.Seed(time.Now().UnixNano())
	maxIdx := len(datas)
	for {
		select {
		case <-ticker.C:
			list.lock.RLock()
			size := list.getSize()
			list.lock.RUnlock()
			if size >= QUEUE_SIZE { // oom
				time.Sleep(time.Millisecond * 3)
				continue
			}
			dataIdx := rand.Intn(maxIdx)
			datas[dataIdx] = ch[idx%26] // 随机修改一点数据，模拟不同的请求数据
			idx++
			node := NodeData{
				data: string(datas),
				time: time.Now().UnixNano(),
			}
			list.push(node)
			//return
		case <-closed:
			fmt.Println("close product...", i)
			return
		}
	}
}

func consumeMulti(i int, closed chan bool, cnt chan int, cost chan []time.Duration, fh *os.File) {
	ticker := time.NewTicker(CONSUME_INTERVAL)
	defer ticker.Stop()

	ch := []byte("abcdefghijklmnopqrstuvwxyz")
	idx := 0
	datas, err := ioutil.ReadFile(READ_FILE)
	if err != nil {
		fmt.Println("ERROR... product bad datas, read fail")
		return
	}
	rand.Seed(time.Now().UnixNano())
	maxIdx := len(datas)
	costWtDoneArr := make([]time.Duration, 0, DELAY_ARRAY)

	for {
		select {
		case <-closed:
			fmt.Printf("close multi consumer...%d, write cost time record len=%d, \n", i, len(costWtDoneArr))
			//fmt.Printf("close multi consumer...%d, costWtDoneArr=%v \n", i, costWtDoneArr)
			//recordCostTimeFile([]time.Duration{}, costWtDoneArr)
			cnt <- idx
			cost <- costWtDoneArr
			return

		case <-ticker.C:
			dataIdx := rand.Intn(maxIdx)
			datas[dataIdx] = ch[idx%26] // 模拟不同的请求数据
			idx++
			tm := time.Now()
			//err := ioutil.WriteFile(WRITE_FILE+strconv.Itoa(i), datas, 0644)
			_, err := fh.Write([]byte(datas))
			if err != nil {
				fmt.Println("ERROR... consumeMulti: fail to write file.", err)
			}
			err = fh.Sync()
			if err != nil {
				fmt.Println("ERROR... consumeMulti: fail to sync file.", err)
			}
			cost := time.Since(tm)
			costWtDoneArr = append(costWtDoneArr, cost)
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
