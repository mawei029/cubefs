package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"
	//"github.com/link1st/go-stress-testing/model"
	//"github.com/link1st/go-stress-testing/server"
)

//"github.com/link1st/go-stress-testing/model"
//"github.com/link1st/go-stress-testing/server"

const (
	baseURL       = "http://ip:port/put" // 替换为你的 IP 和端口
	totalRequests = 1000                 // 总请求数
	dataSize      = 1024 * 1024          // 1MB 数据
)

var (
	diskIDs = []string{"disk1", "disk2", "disk3", "disk4"}
	vuidMap = make(map[int]struct{}) // 存储已使用的 vuid
	mu      sync.Mutex
)

func generateData(size int) []byte {
	data := make([]byte, size)
	rand.Read(data) // 随机生成数据
	return data
}

func main() {
	rand.Seed(time.Now().UnixNano())

	// 创建一个请求函数
	requestFunc := func() (*http.Request, error) {
		mu.Lock()
		// 随机选择 diskID
		diskID := diskIDs[rand.Intn(len(diskIDs))]

		// 查找唯一的 vuid
		var vuid int
		for {
			vuid = rand.Intn(1000) + 1 // 生成 1 到 1000 的 vuid
			if _, exists := vuidMap[vuid]; !exists {
				vuidMap[vuid] = struct{}{} // 标记为已使用
				break
			}
		}

		// 生成唯一的 bid
		bid := fmt.Sprintf("shard-%d", len(vuidMap)) // 使用当前 vuidMap 的大小作为 bid

		// 生成请求数据
		data := generateData(dataSize)
		req, err := http.NewRequest("PUT", fmt.Sprintf("%s/%s/%d/%s", baseURL, diskID, vuid, bid), bytes.NewReader(data))
		if err != nil {
			mu.Unlock()
			return nil, err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		mu.Unlock()
		return req, nil
	}

	// 创建并配置压力测试
	//st := stress.New(stress.Options{
	//	Request:  requestFunc,
	//	Workers:  10,               // 并发工作数
	//	Total:    totalRequests,    // 总请求数
	//	Duration: 30 * time.Second, // 测试持续时间（秒）
	//})
	//
	// 启动压力测试
	//if err := st.Run(); err != nil {
	//	fmt.Println("Error running stress test:", err)
	//}
	//
	//fmt.Println("Stress test completed.")

	//requestURL := ""
	//model.NewRequest(requestURL)
	//server.Dispose()
	requestFunc()
	//request, err := model.NewRequest("", "", 0, 0, false, "", nil, "", 0, false, false, false)
	//if err != nil {
	//	return
	//}
	//request.Print()
	//server.Dispose(context.Background(), 1, 2, request)
}
