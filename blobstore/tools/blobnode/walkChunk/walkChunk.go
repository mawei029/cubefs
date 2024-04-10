package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	bnapi "github.com/cubefs/cubefs/blobstore/api/blobnode"
	"github.com/cubefs/cubefs/blobstore/util/log"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	_4k        = 4 * 1024
	timeFormat = "2006-01-02 15:04:05"
)

var (
	chunkHeaderMagic = [4]byte{0x20, 0x21, 0x03, 0x18}
	diskDir          = flag.String("disk", "/home/service/disks/data1", "disk dir")
	chunkName        = flag.String("chunk", "", "chunk file name")
	vuid             = flag.Int64("vuid", 0, "vuid value")
	logLevel         = flag.Int("level", 1, "log level[0:debug,1:info,2:warn,3:err]")
)

func main() {
	flag.Parse()
	*diskDir = "/home/oppo/Documents/testChunk/"
	//*chunkName = "0000000000000001-17c4cb6d477ab32d"
	*vuid = 1
	log.SetOutputLevel(log.Level(*logLevel))

	if *chunkName != "" {
		readSingleChunk()
		return
	}

	walkAllChunk()
}

func readSingleChunk() {
	absFile := filepath.Join(*diskDir, *chunkName)
	fh, err := os.OpenFile(absFile, os.O_RDWR, 0o644)
	if err != nil {
		panic(err)
	}

	parseChunkNameStr(*chunkName)

	data := make([]byte, _4k)
	n, err := fh.Read(data)
	if err != nil {
		panic(err)
	}
	ch := parseChunkMeta(data, n)
	fmt.Printf("header:%+v \n", ch)
}

func walkAllChunk() {
	files, err := os.ReadDir(*diskDir)
	if err != nil {
		panic(err)
	}

	chunkId := &bnapi.ChunkId{}
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		chunkId = parseChunkNameStr(file.Name())
		fmt.Println(chunkId.VolumeUnitId().ToString())
		if chunkId.VolumeUnitId().ToString() == strconv.FormatInt(*vuid, 10) {
			break
		}
	}
}

func parseChunkNameStr(name string) *bnapi.ChunkId {
	chunkId := &bnapi.ChunkId{}

	if err := chunkId.Unmarshal([]byte(name)); err != nil {
		panic(err)
	}

	fmt.Printf("chunkStr=%s, vuid=%d, tm=%d, time=%s\n",
		chunkId, chunkId.VolumeUnitId(), chunkId.UnixTime(), time.Unix(0, int64(chunkId.UnixTime())).Format(timeFormat))
	return chunkId
}

func parseChunkMeta(buf []byte, n int) *ChunkHeader {
	hdr := &ChunkHeader{}
	if err := hdr.Unmarshal(buf); err != nil {
		panic(err)
	}
	if n != _4k {
		panic("ErrChunkHeaderFileSize")
	}

	return hdr
}

type ChunkHeader struct {
	magic       [4]byte
	version     byte
	parentChunk bnapi.ChunkId
	createTime  int64
}

func (hdr *ChunkHeader) Unmarshal(data []byte) error {
	if len(data) != _4k {
		panic("ErrChunkHeaderBufSize")
	}

	magic := data[0:4]
	if !bytes.Equal(magic, chunkHeaderMagic[:]) {
		panic("ErrChunkDataMagic")
	}
	hdr.magic = chunkHeaderMagic
	hdr.version = data[4:5][0]
	copy(hdr.parentChunk[:], data[5:5+16])
	hdr.createTime = int64(binary.BigEndian.Uint64(data[5+16 : 5+16+8]))

	return nil
}
