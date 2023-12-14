package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cubefs/cubefs/blobstore/api/blobnode"
	"github.com/cubefs/cubefs/blobstore/common/proto"
)

var (
	diskStr = flag.String("disks", "3339,3343,3348,3351,3354,3355,3356,3360,3365,3366,3367", "disk id array, like: 1001,1002,1003")
	host    = flag.String("host", "10.224.56.204:9998", "cluster mgr ip+port")
)

func main() {
	flag.Parse()
	diskArr := strings.Split(*diskStr, ",")

	if len(diskArr) == 0 {
		panic(errors.New("error disk id: " + *diskStr))
	}
	fmt.Printf("disk array=%+v,len=%d \n", diskArr, len(diskArr))

	for idx, disk := range diskArr {
		_, err := strconv.Atoi(disk)
		if err != nil {
			panic(err)
		}

		diskInfo := doHttpGet("/disk/info?disk_id=" + disk)

		if diskInfo != nil {
			diskInfo.Status = proto.DiskStatusNormal
			diskInfo.Readonly = true
			code := doHttpPost("/admin/disk/update", diskInfo)
			fmt.Printf("idx=%d, status code=%d \n", idx, code)
		}
	}
}

func doHttpGet(api string) *blobnode.DiskInfo {
	url := "http://" + *host + api
	fmt.Printf("do get once, %s \n", url)
	client := http.Client{}
	rsp, err := client.Get(url)
	if err != nil {
		panic(err)
	}
	defer rsp.Body.Close()

	if rsp.StatusCode != http.StatusOK {
		fmt.Printf("fail to http get, status code:%d, url:%s \n", rsp.StatusCode, url)
		return nil
	}

	result := blobnode.DiskInfo{}
	err = json.NewDecoder(rsp.Body).Decode(&result)
	if err != nil {
		panic(err)
	}

	fmt.Printf("ret[%+v] \n", result)
	return &result
}

func doHttpPost(api string, diskInfo *blobnode.DiskInfo) int {
	url := "http://" + *host + api
	fmt.Printf("do get once, %s, diskInfo=%v \n", url, *diskInfo)

	msg, err := json.Marshal(*diskInfo)
	if err != nil {
		panic(err)
	}

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

	return resp.StatusCode
}
