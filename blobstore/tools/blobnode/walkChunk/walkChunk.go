package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	bnapi "github.com/cubefs/cubefs/blobstore/api/blobnode"
	"github.com/cubefs/cubefs/blobstore/util/log"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	_4k             = 4 * 1024
	_pageSize       = 4 * 1024 // 4k
	chunkHeaderSize = 29
	shardHeaderSize = 32 // crc32 + magic + bid + vuid + size32 + padding
	shardFooterSize = 8  // magic + crc32
	crc32Len        = 4
	crc32BlockSize  = 64 * 1024
	timeFormat      = "2006-01-02 15:04:05"
)

var (
	chunkHeaderMagic = [4]byte{0x20, 0x21, 0x03, 0x18}
	shardHeaderMagic = [4]byte{0xab, 0xcd, 0xef, 0xcc}
	shardFooterMagic = [4]byte{0xcc, 0xef, 0xcd, 0xab}

	diskDir   = flag.String("disk", "/home/service/disks/data1", "disk dir")
	chunkName = flag.String("chunk", "", "chunk file name")
	vuid      = flag.Int64("vuid", 0, "vuid value[0:find all, xxx:specified vuid]")
	logLevel  = flag.Int("level", 1, "log level[0:debug,1:info,2:warn,3:err]")
)

func main() {
	flag.Parse()
	*diskDir = "/home/oppo/Documents/testChunk/"
	//*chunkName = "0000000000000001-17c4cb6d477ab32d"
	*vuid = 3
	log.SetOutputLevel(log.Level(*logLevel))

	if *chunkName != "" {
		readOneChunk()
		return
	}

	walkAllChunk()
}

func readOneChunk() {
	parseChunkNameStr(*chunkName)

	readSingleChunk(*chunkName)
}

func walkAllChunk() {
	files, err := os.ReadDir(*diskDir)
	if err != nil {
		panic(err)
	}

	chunkId := &bnapi.ChunkId{}
	findVuid := false
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		chunkId = parseChunkNameStr(file.Name())
		if *vuid == 0 { // read all file
			readSingleChunk(file.Name())
			continue
		}
		// vuidStr := fmt.Sprintf(chunkId.VolumeUnitId().ToString())
		if chunkId.VolumeUnitId().ToString() == strconv.FormatInt(*vuid, 10) { // find vuid
			readSingleChunk(file.Name())
			findVuid = true
			break
		}
	}

	if *vuid != 0 && !findVuid {
		fmt.Printf("cannot find vuid:%d \n", *vuid)
	}
}

func readSingleChunk(fileName string) {
	absFile := filepath.Join(*diskDir, fileName)
	fh, err := os.OpenFile(absFile, os.O_RDWR, 0o644)
	if err != nil {
		panic(err)
	}
	defer fh.Close()

	data := make([]byte, chunkHeaderSize)
	n, err := fh.Read(data)
	if err != nil {
		panic(err)
	}
	ch := parseChunkMeta(data, n)
	fmt.Printf("chunk header:%+v \n", ch)

	data = make([]byte, shardHeaderSize)
	off := int64(0 + _4k)
	for {
		n, err = fh.ReadAt(data, off)
		if err == io.EOF {
			return
		}
		if err != nil {
			panic(err)
		}
		sh := parseShardHeader(data, n)
		fmt.Printf("shard header:%+v, offset:%d \n", sh, off)
		off += getShardTotalSize(sh.size, crc32BlockSize)
		off = alignSize(off, _pageSize)
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
	if n != chunkHeaderSize {
		panic("ErrChunkHeaderFileSize")
	}

	return hdr
}

func parseShardHeader(buf []byte, n int) *ShardHeader {
	hdr := &ShardHeader{}
	if err := hdr.Unmarshal(buf); err != nil {
		panic(err)
	}
	if n != shardHeaderSize {
		panic("ErrChunkHeaderFileSize")
	}

	return hdr
}

func getShardTotalSize(payLoad, blockLen uint32) int64 {
	blockCnt := (payLoad + (blockLen - 1)) / blockLen
	return shardHeaderSize + int64(payLoad) + int64(crc32Len*blockCnt) + shardFooterSize
}

func alignSize(p int64, bound int64) (r int64) {
	r = (p + bound - 1) & (^(bound - 1))
	// r = (p + bound - 1) / bound * bound
	return r
}

type ChunkHeader struct {
	magic       [4]byte
	version     byte
	parentChunk bnapi.ChunkId
	createTime  int64
}

func (hdr *ChunkHeader) Unmarshal(data []byte) error {
	if len(data) != chunkHeaderSize {
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

type ShardHeader struct {
	crc   uint32
	magic [4]byte
	bid   uint64 // proto.Bid     // shard id
	vuid  uint64 // proto.Vuid // volume unit id
	size  uint32
}

func (hdr *ShardHeader) Unmarshal(data []byte) error {
	if len(data) != shardHeaderSize {
		panic("ErrShardHeaderBufSize")
	}

	magic := data[4:8]
	if !bytes.Equal(magic, shardHeaderMagic[:]) {
		panic("ErrShardDataMagic")
	}
	hdr.magic = shardHeaderMagic
	hdr.crc = binary.BigEndian.Uint32(data[:4])
	hdr.bid = binary.BigEndian.Uint64(data[4+4 : 4+4+8])
	hdr.vuid = binary.BigEndian.Uint64(data[16 : 16+8])
	hdr.size = binary.BigEndian.Uint32(data[24:28])

	return nil
}
