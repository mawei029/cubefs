package main

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
)

func TestName(t *testing.T) {
	reader := strings.NewReader("Go语言")
	reader.Seek(2, 0)
	r, _, _ := reader.ReadRune()
	t.Logf("%c\n", r) // 语
	str := "Go语言"
	t.Log([]byte(str)) // [71 111 232 175 173 232 168 128]

	testDemoRandomWrite()
}

func testDemoRandomWrite() {
	numTop, numMiddle, numDown := 2, 5, 9
	rand.Seed(time.Now().UnixNano())

	datas, err := ioutil.ReadFile(*rdFile)
	if err != nil {
		panic(err)
	}

	fPath := ""
	for i := 0; i < 10; i++ {
		fName := fmt.Sprintf(dirTop+"/"+dirMiddle+"/"+dirDown, rand.Intn(numTop)+1, rand.Intn(numMiddle)+1, rand.Intn(numDown)+1)
		//fPath = fmt.Sprintf(dirPrefix + "/" + fName)
		fPath = "test_dir_1/vdb.1_5.dir/vdb_f0003.file"
		fmt.Println("test file fName=", fName)

		//fh, err := os.OpenFile(fPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		fh, err := os.OpenFile(fPath, os.O_WRONLY, 0o644)
		if err != nil {
			panic(err)
		}

		fi, err := fh.Stat()

		size := fi.Size()
		if size != 0 {
			//fh.Seek(rand.Int63n(size), os.SEEK_SET)
			off := rand.Int63n(size)
			off = size/2 + int64(i*1024)
			fh.Seek(off, os.SEEK_SET)
		}
		n, err := fh.Write(datas)
		fh.Close()
		fmt.Printf("random write success, n=%d, file=%s, err=%v \n", n, fName, err)
	}
	fmt.Println("random write all file done")
}

func testRead() { // 创建句柄
	fi, err := os.Open("a.txt")
	if err != nil {
		panic(err)
	}

	// 创建 Reader
	r := bufio.NewReader(fi)

	// 每次读取 1024 个字节
	buf := make([]byte, 1024)
	for {
		// func (b *Reader) Read(p []byte) (n int, err error) {}
		n, err := r.Read(buf)
		if err != nil && err != io.EOF {
			panic(err)
		}

		if n == 0 {
			break
		}
		fmt.Println(string(buf[:n]))
	}
}
