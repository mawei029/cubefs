package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/blobstore/util/log"
)

var (
	host        = flag.String("host", "127.0.0.1:8889", "blobnode service ip+port")
	dataSize    = flag.String("d", "4B", "data size")
	readFile    = flag.String("s", "src.data", "src, read data from src file path")
	concurrency = flag.Int("c", 1, "access concurrency")
	maxCnt      = flag.Int64("max", math.MaxInt64, "max handle bid location count")
	diskId      = flag.Int64("diskid", 1, "disk id")
	vuid        = flag.Int64("vuid", 1, "vuid")
	bidStart    = flag.Int64("bid", 1, "bid start")
	printSec    = flag.Int("printS", 0, "print status interval sec")           // loop print interval
	sendUs      = flag.Int("sendUs", 200, "interval us between each send msg") // interval ms between each send msg
	level       = flag.Int("log", 1, "log level")
	ioType      = flag.Int("iotype", 1, "io type")

	total, fail, size = int64(0), int64(0), int(4)
	dataBuff          []byte
	fileSize          = map[string]int{
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

func main() {
	// 1. flag parse
	log.SetOutputLevel(log.Level(*level)) //log.SetOutputLevel(log.Ldebug)
	flag.Parse()
	if err := invalidArgs(); err != nil {
		panic(err)
	}
	initData()

	//loop print
	go func() {
		if *printSec <= 0 {
			return
		}
		tk := time.NewTicker(time.Second * time.Duration(*printSec))
		for {
			select {
			case <-tk.C:
				fmt.Printf("total=%d, fail=%d \n", atomic.LoadInt64(&total), atomic.LoadInt64(&fail))
			}
		}
	}()

	// 3. parse src file, and send get msg
	defer func() {
		fmt.Printf("done, total=%d, fail=%d \n", total, fail)
	}()
	// *filePath = "/home/oppo/code/cubefs/blobstore/tools/batchGet/4M.json"  // debug
	sendMsgConcurrency()
	ch := make(chan int)
	<-ch
}

func invalidArgs() error {
	if *maxCnt <= 0 {
		return errors.New("invalid args: max count")
	}

	if *host == "" {
		return errors.New("invalid args: access host(ip+port)")
	}

	return nil
}

func initData() {
	size = fileSize[strings.ToUpper(*dataSize)]
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

func sendMsgConcurrency() {
	// curl -XPOST 127.0.0.1:8889/shard/put/diskid/49/vuid/6557240344503648259/bid/169534829508100/size/4 -d '1234' -H "Content-type: application/json"
	bidOff := int64(0)

	doOneMsg := func() {
		bid := atomic.AddInt64(&bidOff, 1) + *bidStart
		url := fmt.Sprintf("http://%s/shard/put/diskid/%d/vuid/%d/bid/%d/size/%d?iotype=%d", *host, *diskId, *vuid, bid, size, *ioType)
		log.Debugf("url=%s", url)

		req, err := http.NewRequest("POST", url, bytes.NewBuffer(dataBuff))
		if err != nil {
			panic(err)
		}

		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			panic(err)
		}
		defer resp.Body.Close()

		atomic.AddInt64(&total, 1)
		time.Sleep(time.Microsecond * time.Duration(*sendUs))

		if resp.StatusCode != http.StatusOK {
			atomic.AddInt64(&fail, 1)
			// slow down
		}
	}

	doOneConcurrency := func() {
		for i := int64(0); i < *maxCnt; i++ {
			doOneMsg()
		}
	}

	for i := 0; i < *concurrency; i++ {
		go doOneConcurrency()
	}
}
