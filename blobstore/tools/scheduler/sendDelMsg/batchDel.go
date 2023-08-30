package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

const defaultBatchDelSize = 100

var (
	filePath    = flag.String("f", "8M.json", "location file path")
	delBatch    = flag.Int64("d", 100, "access send delete msg batch size")
	MaxCnt      = flag.Int64("max", math.MaxInt64, "max handle location count")
	total, fail = int64(0), int64(0)
)

// 批量调用access接口发送删除消息到kafka
func main() {
	flag.Parse()
	if *delBatch <= 0 {
		*delBatch = defaultBatchDelSize
	}

	// wait for signal
	ch := make(chan os.Signal, 1)
	go func() {
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		sig := <-ch
		fmt.Printf("receive signal: %s, stop service... \n", sig.String())
		fmt.Printf("stop, totalCnt=%d, failCnt=%d \n", total, fail)
		os.Exit(0)
	}()

	//*filePath = "/home/oppo/code/cubefs/blobstore/tools/scheduler/getKafka.conf"
	defer func() {
		fmt.Printf("done, totalCnt=%d, failCnt=%d \n", total, fail)
	}()
	err := readFile(*filePath)
	if err != nil {
		panic(err)
	}
}

type DeleteArgs struct {
	Locations []Location `json:"locations"`
}

type OneLine struct {
	Location Location `json:"location"`
}

type Location struct {
	_         [0]byte
	ClusterID uint32      `json:"cluster_id"`
	CodeMode  uint8       `json:"code_mode"`
	Size      uint64      `json:"size"`
	BlobSize  uint32      `json:"blob_size"`
	Crc       uint32      `json:"crc"`
	Blobs     []SliceInfo `json:"blobs"`
}

type SliceInfo struct {
	_      [0]byte
	MinBid uint64 `json:"min_bid"`
	Vid    uint32 `json:"vid"`
	Count  uint32 `json:"count"`
}

func readFile(path string) (err error) {
	rets := DeleteArgs{
		Locations: make([]Location, 0),
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for {
		if total >= *MaxCnt {
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

		// line = `{"location":{"cluster_id":11,"code_mode":2,"size":8388608,"blob_size":8388608,"crc":2988698822,"blobs":[{"min_bid":66101023,"vid":3223,"count":1}]},"offset":0,"read_size":8388608}`
		// line = `{"location":{"cluster_id":11,"code_mode":2,"size":4,"blob_size":8388608,"crc":2963547763,"blobs":[{"min_bid":65922993,"vid":4649,"count":1}]},"hashsum":{"4":"gdyb21LQTcIANtvYMT7QVQ=="}}`
		err1 = parseMsg(line, &rets)
		if err1 != nil {
			err = err1
			break
		}

		if total%*delBatch == 0 {
			fail += sendDelMsg(&rets)
			rets.Locations = rets.Locations[:0]
		}
	} // end for

	//fmt.Printf("done, totalCnt=%d, failCnt=%d \n", total, fail)
	return nil
}

func parseMsg(line string, rets *DeleteArgs) error {
	msg := OneLine{}
	err := json.Unmarshal([]byte(line), &msg)
	if err != nil {
		return err
	}

	//fmt.Printf("%+v \n", msg)
	rets.Locations = append(rets.Locations, msg.Location)
	return nil
}

// 批量调用access接口发送删除消息到kafka
func sendDelMsg(rets *DeleteArgs) int64 {
	// curl -XPOST --header 'Content-Type: application/json' 127.0.0.1:9500/delete -d '{"locations":[{"cluster_id":11,"code_mode":2,"size":8,"blob_size":8388608,"crc":2724760903,"blobs":[{"min_bid":66000305,"vid":3240,"count":1}]}]}'
	url := "http://127.0.0.1:9500/delete"
	data, err := json.Marshal(rets)
	if err != nil {
		panic(err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
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

	fmt.Printf("totalCnt=%d, response Status: %s \n", total, resp.Status)
	if resp.StatusCode != http.StatusOK {
		// fmt.Println("response Status:", resp.Status)
		return *delBatch
	}
	return 0
}
