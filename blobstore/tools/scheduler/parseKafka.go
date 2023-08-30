package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type DeleteStage byte

const (
	DeleteStageInit DeleteStage = iota
	DeleteStageMarkDelete
	DeleteStageDelete
)

type BlobDeleteStage struct {
	Stages map[uint8]DeleteStage `json:"stages"`
}

type KafkaMsg struct {
	ClusterID     uint32          `json:"cluster_id"`
	Bid           uint64          `json:"bid"`
	Vid           uint32          `json:"vid"`
	Retry         int             `json:"retry"`
	Time          int64           `json:"time"`
	ReqId         string          `json:"req_id"`
	BlobDelStages BlobDeleteStage `json:"blob_del_stages"`
	blobStages    string
}

const (
	printStructMode = iota
	jsonMsgMode
)

func parseMsg(mode int, line string, rets map[uint64]KafkaMsg) error {
	st := KafkaMsg{}
	switch mode {
	case printStructMode:
		strs := strings.Split(line, "start delete msg: ")
		if len(strs) == 2 {
			line = strs[1][2 : len(strs[1])-3]
			strs = strings.Split(line, " ")
			if ecLenMatch(len(strs) - 6) { // EC 15+12 or 6+6
				cid, _ := strconv.Atoi(strings.Split(strs[0], ":")[1])
				bid, _ := strconv.ParseUint(strings.Split(strs[1], ":")[1], 10, 64)
				vid, _ := strconv.Atoi(strings.Split(strs[2], ":")[1])
				re, _ := strconv.Atoi(strings.Split(strs[3], ":")[1])
				tm, _ := strconv.ParseInt(strings.Split(strs[4], ":")[1], 10, 64)
				st.ClusterID = uint32(cid)
				st.Bid = bid
				st.Vid = uint32(vid)
				st.Retry = re
				st.Time = tm
				st.ReqId = strings.Split(strs[5], ":")[1]
				st.blobStages = strings.Split(line, "BlobDelStages:")[1]
				mp := strings.TrimPrefix(st.blobStages, "{Stages:map[")
				mp = strings.TrimRight(mp, "]}")
				strs = strings.Split(mp, " ")
				vTwo, vOne := false, false
				for _, v := range strs {
					delFlag := strings.Split(v, ":")[1]
					if vTwo && vOne {
						break
					}
					if delFlag == "2" {
						vTwo = true
						continue
					}
					if delFlag == "1" {
						vOne = true
						continue
					}
				}
				if vTwo && vOne {
					if st.Retry > *retry && rets[st.Bid].Retry < st.Retry {
						rets[st.Bid] = st
					}
				}
			}
		}
	case jsonMsgMode:
		strs := strings.Split(line, "start json delete msg: ")
		if len(strs) == 2 {
			//st = KafkaMsg{}
			line = strs[1][:len(strs[1])-1]
			err := json.Unmarshal([]byte(line), &st)
			if err != nil {
				return err
			}
			if ecLenMatch(len(st.BlobDelStages.Stages)) {
				vTwo, vOne := false, false
				for _, v := range st.BlobDelStages.Stages {
					if v == 2 {
						vTwo = true
					}
					if v == 1 {
						vOne = true
					}
				}
				if vTwo && vOne && st.Retry > *retry && rets[st.Bid].Retry < st.Retry {
					rets[st.Bid] = st
				}
			}
		} // end case
	}
	return nil
}

func ecLenMatch(len int) bool {
	for _, v := range conf1.EcLen {
		if len == v {
			return true
		}
	}
	return false
}

//func ReadLinesV2(path string) ([]string, error) {
func ReadLinesV2(path string) (rets map[uint64]KafkaMsg, err error) {
	//delMsg := KafkaMsg{ClusterID: 100, Bid: 222, BlobDelStages: BlobDeleteStage{Stages: map[uint8]DeleteStage{0: 2, 1: 2}}}
	//data, err := json.Marshal(delMsg)
	//log.Infof("start json delete msg: %s", string(data))
	//st1 := KafkaMsg{}
	//json.Unmarshal(data, &st1)

	rets = map[uint64]KafkaMsg{}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	//var lines []string
	reader := bufio.NewReader(file)
	//st := KafkaMsg{}
	cnt := 0
	for {
		cnt++
		// ReadString reads until the first occurrence of delim in the input,
		// returning a string containing the data up to and including the delimiter.
		line, err1 := reader.ReadString('\n')
		if err1 == io.EOF {
			//lines = append(lines, line)
			break
		}
		if err1 != nil {
			//return rets, err
			err = err1
			break
		}

		err1 = parseMsg(*parseMode, line, rets)
		if err1 != nil {
			err = err1
			break
		}
	} // end for

	var msgs []KafkaMsg
	for _, v := range rets {
		msgs = append(msgs, v)
	}
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].Time < msgs[j].Time
	})
	for i := range msgs {
		fmt.Printf("idx: %d, time: %s, val: %+v\n", i, time.Unix(msgs[i].Time, 0).Format(time.RFC3339), msgs[i])
	}

	fmt.Printf("err: %+v, parseCnt:%d, resultLen: %d\n", err, cnt, len(rets))
	return rets, nil
}

//4 line
var (
	msgFile   = flag.String("l", "msgKafka.log", "msg kafka File filename")
	parseMode = flag.Int("m", jsonMsgMode, "parse msg mode, default json mode")
	retry     = flag.Int("r", 0, "msg retry time")
	//eclen     = flag.Int("e", 27, "ec len")
)

// 根据日志解析出来没有两阶段删除的blobnode对应的BID
func main() {
	flag.Parse()
	// local debug
	//*confFile1 = "/home/oppo/code/cubefs/blobstore/tools/scheduler/parseKafka.conf"
	//*msgFile = "/home/oppo/code/cubefs/blobstore/tools/scheduler/msgKafka.log" // "/home/oppo/Documents/msgKafka1.log"
	initMgr()

	////local debug
	//var parseMode *int
	//
	//func parseFile() {
	//	var msgFile = flag.String("m", "msgKafka.log", "msg kafka File filename")
	//	*msgFile = "/home/oppo/code/cubefs/blobstore/tools/scheduler/msgKafka.log" // "/home/oppo/Documents/msgKafka1.log"
	//	n := jsonMsgMode
	//	parseMode = &n

	//log.SetOutputLevel(1)
	//path := "/home/oppo/code/cubefs/blobstore/tools/scheduler/msgKafka.log"

	fmt.Println("")
	rets, err := ReadLinesV2(*msgFile)
	fmt.Printf("err: %+v, len: %d\n", err, len(rets))
	//log.Infof("err: %+v, len: %d, rets: %+v", err, len(rets), rets)

	//initMgr()
	fmt.Println("")
	getAllHost(rets, *parseMode)
}
