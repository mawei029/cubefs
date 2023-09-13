package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

var (
	filePath    = flag.String("src", "4M.json", "bid `location file path")
	host        = flag.String("host", "127.0.0.1:9500", "accessSvr ip+port")
	concurrency = flag.Int("c", 1, "access concurrency")
	getBatch    = flag.Int64("batch", 10, "access send get msg batch count")
	maxCnt      = flag.Int64("max", math.MaxInt64, "max handle bid location count")
	printSec    = flag.Int("printS", 0, "print status interval sec")           // loop print interval
	sendMill    = flag.Int("sendMs", 240, "interval ms between each send msg") // interval ms between each send msg
	failSec     = flag.Int("failS", 2, "interval second at fail")              // interval second at fail

	total, fail = int64(0), int64(0)
	msgCh       chan string
)

func main() {
	// 1. flag parse
	flag.Parse()
	if err := invalidArgs(); err != nil {
		panic(err)
	}

	// 2. wait for signal
	//ch := make(chan os.Signal, 1)
	//go func() {
	//	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	//	sig := <-ch
	//	fmt.Printf("stop service... signal=%s, total=%d, fail=%d \n", sig.String(), total, fail)
	//	os.Exit(0)
	//}()

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
	err := handleFile(*filePath)
	if err != nil {
		panic(err)
	}

}

func invalidArgs() error {
	if *getBatch <= 0 {
		return errors.New("invalid args: get batch count")
	}

	if *filePath == "" {
		return errors.New("invalid args: src file path")
	}

	if *maxCnt <= 0 {
		return errors.New("invalid args: max count")
	}

	if *host == "" {
		return errors.New("invalid args: access host(ip+port)")
	}

	return nil
}

func handleFile(path string) (err error) {
	file, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	msgCh = make(chan string, *getBatch)
	go sendMsgConcurrency()

	// read, parse file
	reader := bufio.NewReader(file)
	//msgArr := make([]string, 0, *getBatch)
	for {
		if total >= *maxCnt {
			return
		}

		total++
		line, err1 := reader.ReadString('\n')
		if err1 == io.EOF {
			total--
			break
		}
		if err1 != nil {
			err = err1
			break
		}

		// send get msg
		//msgArr = append(msgArr, line)
		//if total%*getBatch == 0 {
		//	sendGetMsg(msgArr)
		//	msgArr = msgArr[:0]
		//}
		msgCh <- line
	}

	return nil
}

func sendMsgConcurrency() {
	url := "http://" + *host + "/get" // curl -XPOST -d ''$line'' -H "Content-Type: application/json"  http://127.0.0.1:9500/get

	doOneMsg := func(msg string, failNum int64) {
		req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte(msg)))
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

		time.Sleep(time.Millisecond * time.Duration(*sendMill))

		if resp.StatusCode != http.StatusOK {
			failNum++
			atomic.AddInt64(&fail, 1)

			// slow down
			if failNum > 1 {
				failNum = 0
				time.Sleep(time.Second * time.Duration(*failSec))
			}
		}
	}

	for i := 0; i < *concurrency; i++ {
		go func() {
			failLoc := int64(0)

			for msg := range msgCh {
				doOneMsg(msg, failLoc)
			}
		}()
	}
}

// curl -XPOST -d ''$line'' -H "Content-Type: application/json"  http://127.0.0.1:9500/get  > /dev/null 2>&1
func sendGetMsg(msgs []string) {
	for _, v := range msgs {
		msgCh <- v
	}

	fmt.Printf("total=%d, fail=%d \n", total, fail)
}
