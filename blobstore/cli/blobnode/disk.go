// Copyright 2022 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package blobnode

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/desertbit/grumble"
	"github.com/tecbot/gorocksdb"

	"github.com/cubefs/cubefs/blobstore/api/blobnode"
	"github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/blobnode/core"
	"github.com/cubefs/cubefs/blobstore/cli/common"
	"github.com/cubefs/cubefs/blobstore/cli/common/fmt"
	"github.com/cubefs/cubefs/blobstore/cli/config"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/common/rpc/auth"
)

func addCmdDisk(cmd *grumble.Command) {
	diskCommand := &grumble.Command{
		Name:     "disk",
		Help:     "disk tools",
		LongHelp: "disk tools for blobnode",
	}
	cmd.AddCommand(diskCommand)

	diskCommand.AddCommand(&grumble.Command{
		Name: "all",
		Help: "show all register disks",
		Flags: func(f *grumble.Flags) {
			blobnodeFlags(f)
		},
		Run: func(c *grumble.Context) error {
			cli := blobnode.New(&blobnode.Config{})
			host := c.Flags.String("host")
			stat, err := cli.Stat(common.CmdContext(), host)
			if err != nil {
				return err
			}
			fmt.Println(common.Readable(stat))
			return nil
		},
	})

	diskCommand.AddCommand(&grumble.Command{
		Name: "stat",
		Help: "show disk info",
		Flags: func(f *grumble.Flags) {
			blobnodeFlags(f)
			f.UintL("diskid", 1, "disk id")
		},
		Run: func(c *grumble.Context) error {
			cli := blobnode.New(&blobnode.Config{})
			host := c.Flags.String("host")
			args := blobnode.DiskStatArgs{DiskID: proto.DiskID(c.Flags.Uint("diskid"))}
			stat, err := cli.DiskInfo(common.CmdContext(), host, &args)
			if err != nil {
				return err
			}
			fmt.Println(common.Readable(stat))
			return nil
		},
	})

	addCmdDiskDrop(diskCommand)
}

func addCmdDiskDrop(diskCommand *grumble.Command) {
	diskDropCommand := &grumble.Command{
		Name: "drop_stat",
		Help: "check dropped disk status",
		Flags: func(f *grumble.Flags) {
			f.StringL("disk_ids", "", "e.g. disk_ids=3,4,5")
			f.StringL("disk_dirs", "/home/service/var/data\\d+$", "this blobnode all disk dir")
			f.StringL("cm_hosts", "", "e.g. cm_hosts=http://ip1:9998,xxx")
			f.BoolL("db", false, "read local disk db, check chunk. default(false)")
			f.StringL("disk_node", "bond0", "get all disk_ids from cm (local blobnode ip/net_card)")
		},
		Run: dropStatCheck,
	}

	diskCommand.AddCommand(diskDropCommand)
}

func dropStatCheck(c *grumble.Context) error {
	diskIds, err := checkDiskDropConf(c)
	if err != nil {
		return err
	}

	vuidCmCnt := 0 // remain cm vuid count, the chunk which is not migration or cleanup
	vuidBnCnt := 0
	ctx := context.Background()
	readDb := c.Flags.Bool("db")

	cmCli := newCmClient(c)
	fmt.Printf("start time: " + time.Now().Format("2006-01-02 15:04:05") + "\n")

	if vuidCmCnt, err = getVuidFromCm(ctx, cmCli, diskIds); err != nil {
		return err
	}
	fmt.Printf("done get vuid from cm. diskCnt=%d, disk ids=%v\n", len(diskIds), diskIds)

	if vuidBnCnt, err = getVuidFromBn(ctx, cmCli, diskIds, readDb); err != nil {
		return err
	}
	fmt.Printf("done get vuid from bn. diskCnt=%d, disk ids=%v\n", len(diskIds), diskIds)

	fmt.Println("-----------------------------------------------------")
	fmt.Printf("remain vuid(chunk not migration or cleanup): cmVuid=%d, bnVuid=%d\n\n", vuidCmCnt, vuidBnCnt)
	return nil
}

func checkDiskDropConf(c *grumble.Context) ([]proto.DiskID, error) {
	cmHosts := strings.Split(c.Flags.String("cm_hosts"), ",")
	if len(cmHosts) == 0 {
		return nil, fmt.Errorf("invalid cm hosts")
	}

	allLocal := c.Flags.String("disk_node")
	if allLocal != "" {
		return parseAllLocalDiskIdsByCm(c)
	}

	allDiskDir := c.Flags.String("disk_dirs")
	if allDiskDir != "" {
		return parseAllDiskPath(allDiskDir)
	}

	diskIdsStr := c.Flags.String("disk_ids")
	return parseMultiDiskIds(diskIdsStr)
}

func newCmClient(c *grumble.Context) *clustermgr.Client {
	cmHosts := strings.Split(c.Flags.String("cm_hosts"), ",")

	secret := config.ClusterMgrSecret()
	cfg := &clustermgr.Config{}
	cfg.LbConfig.Hosts = cmHosts
	cfg.LbConfig.Config.Tc.Auth = auth.Config{EnableAuth: secret != "", Secret: secret}
	return clustermgr.New(cfg)
}

func parseAllDiskPath(patternPath string) (diskIds []proto.DiskID, err error) {
	pathRegex, err := regexp.Compile(patternPath)
	if err != nil {
		return nil, fmt.Errorf("failed to compile regex, err:%+v", err)
	}

	parentDir := filepath.Dir(patternPath)
	paths, err := os.ReadDir(parentDir)
	if err != nil || len(paths) == 0 {
		return nil, fmt.Errorf("invalid dir: diskPath:%s, rootDir:%s, err:%+v", patternPath, parentDir, err)
	}

	ctx := context.Background()

	for _, path := range paths {
		abs := filepath.Join(parentDir, path.Name())
		if path.IsDir() && pathRegex.MatchString(abs) {
			format, err := readFormatInfo(ctx, abs)
			if err != nil {
				return nil, fmt.Errorf("failed to read format info, err:%+v", err)
			}

			diskIds = append(diskIds, format.DiskID)
		}
	}

	if len(diskIds) == 0 {
		return nil, fmt.Errorf("error: empty, invalid disk ids")
	}
	return diskIds, nil
}

func parseMultiDiskIds(ids string) (diskIds []proto.DiskID, err error) {
	ret := strings.Split(strings.TrimSpace(ids), ",")
	if ret[0] == "" {
		return nil, fmt.Errorf("empty disk id")
	}

	for _, diskId := range ret {
		id, err := strconv.Atoi(strings.TrimSpace(diskId))
		if err != nil {
			return nil, fmt.Errorf("error: invalid disk id[%s]", diskId)
		}
		diskIds = append(diskIds, proto.DiskID(id))
	}
	if len(diskIds) == 0 {
		return nil, fmt.Errorf("error: empty, invalid disk ids")
	}

	return diskIds, nil
}

func parseAllLocalDiskIdsByCm(c *grumble.Context) (diskIds []proto.DiskID, err error) {
	ip, err := getLocalBlobnodeIp(c.Flags.String("disk_node"))
	if err != nil {
		return nil, err
	}

	const prefix = "http://"
	const postfix = ":8889"
	host := fmt.Sprintf("%s%s%s", prefix, ip, postfix)

	cmCli := newCmClient(c)
	ret, err := cmCli.ListDisk(context.Background(), &clustermgr.ListOptionArgs{Host: host})
	if err != nil {
		return nil, err
	}
	for _, d := range ret.Disks {
		diskIds = append(diskIds, d.DiskID)
	}
	if len(diskIds) == 0 {
		return nil, fmt.Errorf("error: empty, invalid disk ids")
	}
	return diskIds, nil
}

func readFormatInfo(ctx context.Context, diskRootPath string) (formatInfo *core.FormatInfo, err error) {
	_, err = ioutil.ReadDir(diskRootPath)
	if err != nil {
		fmt.Printf("read disk root path(%s) error\n", diskRootPath)
		return nil, err
	}

	formatInfo, err = core.ReadFormatInfo(ctx, diskRootPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("format file not exist. must be first register")
		}
		return nil, err
	}

	return formatInfo, err
}

func getVuidFromCm(ctx context.Context, cmCli *clustermgr.Client, diskIds []proto.DiskID) (int, error) {
	vuidCmCnt := 0
	for _, diskId := range diskIds {
		units, err := cmCli.ListVolumeUnit(ctx, &clustermgr.ListVolumeUnitArgs{DiskID: diskId})
		if err != nil {
			return 0, err
		}

		if len(units) != 0 {
			vuidCmCnt += len(units)
			fmt.Printf("diskID=%d\t\thost=%+v\n", units[0].DiskID, units[0].Host)
		}

		for _, info := range units {
			fmt.Printf("vuid=%d\n", info.Vuid)
		}
	}
	return vuidCmCnt, nil
}

type diskHost struct {
	DiskID proto.DiskID
	Host   string
	Path   string
}

func getVuidFromBn(ctx context.Context, cmCli *clustermgr.Client, diskIds []proto.DiskID, readDb bool) (int, error) {
	const dataDir = "data"
	vuidBnCnt := 0

	localIP, err := getLocalMachineIps()
	if err != nil {
		return 0, err
	}
	localDisk := make(map[proto.DiskID]diskHost)

	result := make(map[string][]diskHost)
	for _, diskId := range diskIds {
		ret, err := cmCli.DiskInfo(ctx, diskId)
		if err != nil {
			return 0, err
		}
		// ret := diskHost{Host: "https://192.168.117.132:1234", Path: "/home/mw/Documents/testChunk", DiskID: diskId}
		// {"host":"http://10.35.212.22:8889","path":"/home/service/disks/data8", "disk_id":10}
		result[ret.Host] = append(result[ret.Host], diskHost{
			DiskID: diskId,
			Host:   ret.Host,
			Path:   filepath.Join(ret.Path, dataDir),
		})
	}
	for host, info := range result {
		if isLocalMachineIp(host, localIP) {
			// fmt.Printf("host=%s, diskInfo=%+v \n", host, info)
			for _, diskInfo := range info {
				localDisk[diskInfo.DiskID] = diskInfo
			}

			if vuidBnCnt, err = walkAllDisk(ctx, cmCli, info, readDb); err != nil {
				return 0, err
			}
			continue
		}
		fmt.Printf("other host=%s, diskInfo=%+v \n", host, info)
	}

	return vuidBnCnt, nil
}

func getLocalMachineIps() (map[string]string, error) {
	localIP := make(map[string]string)

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		fmt.Printf("Failed to retrieve interface addresses: %v\n", err)
		return nil, err
	}

	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				// fmt.Println("Local IP address:", ipNet.IP.String())
				ipStr := ipNet.IP.String()
				localIP[ipStr] = ipStr
			}
		}
	}

	if len(localIP) < 1 {
		return nil, fmt.Errorf("no have valid local ip")
	}

	return localIP, nil
}

func isLocalMachineIp(host string, localIP map[string]string) bool {
	// "host":"http://10.35.212.22:8889"
	re := regexp.MustCompile(`\b(\d{1,3}\.){3}\d{1,3}\b`)
	ip := re.FindString(host)
	_, ok := localIP[ip]
	return ok
}

func getLocalBlobnodeIp(key string) (string, error) {
	re := regexp.MustCompile(`(\d{1,3}\.){3}\d{1,3}`)
	if re.MatchString(key) {
		return checkLocalIp(key)
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		fmt.Println("Error getting network interfaces:", err)
		return "", err
	}

	for _, iface := range interfaces {
		if iface.Name == key {
			addrs, err := iface.Addrs()
			if err != nil {
				fmt.Println("Error getting addresses for interface:", err)
				return "", err
			}

			for _, addr := range addrs {
				ipnet, ok := addr.(*net.IPNet)
				if !ok {
					continue
				}

				if ipnet.IP.To4() != nil {
					return ipnet.IP.String(), nil
				}
			}
		}
	}

	return "", fmt.Errorf("Unable to find the local IP address of bond0")
}

func checkLocalIp(ip string) (string, error) {
	localIps, err := getLocalMachineIps()
	if err != nil {
		return "", err
	}

	if _, ok := localIps[ip]; ok {
		return ip, nil
	}

	return "", fmt.Errorf("Unable to find the local IP address")
}

func walkAllDisk(ctx context.Context, cmCli *clustermgr.Client, disks []diskHost, readDb bool) (int, error) {
	vuidBnCnt := 0
	for _, dk := range disks {
		vuidCnt, err := walkSingleDisk(ctx, cmCli, dk, readDb)
		if err != nil {
			return 0, err
		}

		vuidBnCnt += vuidCnt
	}

	return vuidBnCnt, nil
}

func walkSingleDisk(ctx context.Context, cmCli *clustermgr.Client, dh diskHost, readDb bool) (int, error) {
	files, err := os.ReadDir(dh.Path)
	if err != nil {
		return 0, err
	}

	var db *gorocksdb.DB
	if len(files) != 0 {
		fmt.Printf("diskID=%d               info=%+v\n", dh.DiskID, dh)

		if readDb {
			dbPath := filepath.Join(dh.Path, "../meta/superblock")
			db, err = gorocksdb.OpenDbForReadOnly(gorocksdb.NewDefaultOptions(), dbPath, true)
			if err != nil {
				fmt.Printf("Err: Fail to load SuperBlock, err:%+v \n", err) // panic(err)
				return 0, err
			}
		}
	}
	defer func() {
		if db != nil {
			defer db.Close()
		}
	}()

	vuidCnt := 0
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		chunkId, err := parseChunkNameStr(file.Name()) // decode chunk
		if err != nil {
			fmt.Printf("---ERROR--- file:%s, error:%+v \n", file.Name(), err)
			continue
		}

		vuidCnt++
		vuid := chunkId.VolumeUnitId()
		newDisk := getNewDiskInfo(ctx, cmCli, vuid)
		vm := getChunkMeta(db, chunkId)
		fmt.Printf("  vuid=%d    status=%s    newDisk=%+v\n", vuid, chunkStatusString(vm.Status), newDisk)
	}
	return vuidCnt, nil
}

func getChunkMeta(db *gorocksdb.DB, chunkId blobnode.ChunkId) (vm core.VuidMeta) {
	if db == nil {
		return core.VuidMeta{}
	}

	// chunkId := parseChunkNameStr(*chunkName)
	ro := gorocksdb.NewDefaultReadOptions()
	ro.SetVerifyChecksums(true)
	key := []byte("chunks/" + chunkId.String())
	data, err := db.GetBytes(ro, key)
	if err != nil {
		fmt.Printf("Err: Fail to get chunk db, err:%+v \n", err) // panic(err)
		return core.VuidMeta{}
	}

	err = json.Unmarshal(data, &vm)
	if err != nil {
		fmt.Printf("Failed unmarshal, err:%+v \n", err)
		return core.VuidMeta{}
	}
	// fmt.Printf("chunk meta:%+v \n", vm)
	return vm
}

func parseChunkNameStr(name string) (chunkId blobnode.ChunkId, err error) {
	const chunkFileLen = 33 // 16+1+16

	if len(name) != chunkFileLen {
		return chunkId, fmt.Errorf("chunk file name length not match, file:%s", name)
	}

	if err = chunkId.Unmarshal([]byte(name)); err != nil {
		return chunkId, err
	}

	return chunkId, nil
}

// vuid -> vid -> volume info -> new disk
func getNewDiskInfo(ctx context.Context, cmCli *clustermgr.Client, vuid proto.Vuid) clustermgr.Unit {
	vid := vuid.Vid()

	volume, err := cmCli.GetVolumeInfo(ctx, &clustermgr.GetVolumeArgs{Vid: vid})
	if err != nil {
		fmt.Printf("Err: Fail to get volume, err:%+v \n", err)
		return clustermgr.Unit{} // panic(err)
	}

	idx := int(vuid.Index())
	if idx < 0 || idx >= len(volume.Units) {
		fmt.Printf("Err: invalid index:%d, volume:%+v err:%+v \n", idx, volume)
		return clustermgr.Unit{} // panic(err)
	}

	return volume.Units[idx]
}

func chunkStatusString(status blobnode.ChunkStatus) string {
	switch status {
	case blobnode.ChunkStatusNormal:
		return "normal"
	case blobnode.ChunkStatusReadOnly:
		return "readOnly"
	case blobnode.ChunkStatusRelease:
		return "release"
	default:
		return "unkown"
	}
}
