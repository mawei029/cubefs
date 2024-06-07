package blobnode

import (
	"regexp"
	"testing"

	"github.com/cubefs/cubefs/blobstore/cli/common/fmt"
)

func isLocalMachineIp(host string, localHost string) bool {
	// "host":"http://192.168.117.132:8889"
	re := regexp.MustCompile(`\b(\d{1,3}\.){3}\d{1,3}\b`)
	cmIp := re.FindString(host)
	localIp := re.FindString(localHost)
	return cmIp == localIp
}

func TestCliDisk(t *testing.T) {
	nodeIP := "http://10.35.212.22:8889"
	re := regexp.MustCompile(`\b(\d{1,3}\.){3}\d{1,3}:\d{1,6}\b`)
	bl := re.MatchString(nodeIP)
	fmt.Println(bl)

	nodeIP = "10.35.212.22:8889"
	bl = re.MatchString(nodeIP)
	fmt.Println(bl)

	nodeIP = "10.35.212.22"
	bl = re.MatchString(nodeIP)
	fmt.Println(bl)

	host := "http://10.35.212.22:8889"
	localIp := "10.35.212.22"
	fmt.Println(isLocalMachineIp(host, localIp))
}
