// Copyright 2020 The CubeFS Authors.
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

package cmd

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/cubefs/cubefs/metanode"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/spf13/cobra"
)

const (
	InodeCheckOpt int = 1 << iota
	DentryCheckOpt

	orphanBloomPhase1Concurrency = 3
)

var (
	mpCheckLog      *os.File
	checkHTTPClient = &http.Client{Timeout: 30 * time.Minute}
)

type MpMap struct {
	Imap map[uint64]*metanode.Inode
	Dmap map[string]*metanode.Dentry
}

// Concurrency limit related variables and functions (referenced from gc.go)
var (
	orphanCheckHostLimit = make(map[string]chan struct{})
	orphanCheckCntLimit  = 3 // Maximum 3 concurrent requests per host
	orphanCheckHostLk    = sync.RWMutex{}
)

func setOrphanCheckHostCntLimit(cnt int) {
	orphanCheckHostLk.Lock()
	defer orphanCheckHostLk.Unlock()
	// Clear old channels
	for k := range orphanCheckHostLimit {
		delete(orphanCheckHostLimit, k)
	}
	orphanCheckCntLimit = cnt
}

func getOrphanCheckToken(host string) {
	orphanCheckHostLk.Lock()
	ch, ok := orphanCheckHostLimit[host]
	if !ok {
		ch = make(chan struct{}, orphanCheckCntLimit)
		orphanCheckHostLimit[host] = ch
	}
	orphanCheckHostLk.Unlock()

	ch <- struct{}{}
}

func releaseOrphanCheckToken(host string) {
	orphanCheckHostLk.RLock()
	defer orphanCheckHostLk.RUnlock()

	ch, ok := orphanCheckHostLimit[host]
	if !ok {
		log.Printf("Warning: channel not found for host %s, cannot release token", host)
		return
	}

	select {
	case <-ch:
		return
	default:
		log.Printf("Warning: no token in channel for host %s", host)
	}
}

func newCheckCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "check",
		Short: "check and verify specified volume",
		Args:  cobra.MinimumNArgs(0),
	}

	c.AddCommand(
		newCheckInodeCmd(),
		newCheckDentryCmd(),
		newCheckBothCmd(),
		newCheckMpCmd(),
		newCheckOrphanBloomCmd(),
	)

	return c
}

func newCheckInodeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "inode",
		Short: "check and verify inode",
		Run: func(cmd *cobra.Command, args []string) {
			if err := Check(InodeCheckOpt); err != nil {
				fmt.Println(err)
			}
		},
	}

	return c
}

func newCheckDentryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "dentry",
		Short: "check and verify dentry",
		Run: func(cmd *cobra.Command, args []string) {
			if err := Check(DentryCheckOpt); err != nil {
				fmt.Println(err)
			}
		},
	}

	return c
}

func newCheckBothCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "both",
		Short: "check and verify both inode and dentry",
		Run: func(cmd *cobra.Command, args []string) {
			if err := Check(InodeCheckOpt | DentryCheckOpt); err != nil {
				fmt.Println(err)
			}
		},
	}

	return c
}

func newCheckMpCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "mp",
		Short: "check inode and dentry consistency of mp",
		Run: func(cmd *cobra.Command, args []string) {
			if err := CheckMP(); err != nil {
				fmt.Println(err)
			}
		},
	}

	return c
}

func Check(chkopt int) (err error) {
	var remote bool

	if InodesFile == "" || DensFile == "" {
		remote = true
	}

	if VolName == "" || (remote && (MasterAddr == "")) {
		err = fmt.Errorf("Lack of mandatory args: master(%v) vol(%v)", MasterAddr, VolName)
		return
	}

	/*
	 * Record all the inodes and dentries retrieved from metanode
	 */
	var (
		ifile *os.File
		dfile *os.File
	)

	dirPath := fmt.Sprintf("_export_%s", VolName)
	if err = os.MkdirAll(dirPath, 0o666); err != nil {
		return
	}

	if remote {
		if ifile, err = os.Create(fmt.Sprintf("%s/%s", dirPath, inodeDumpFileName)); err != nil {
			return
		}
		defer ifile.Close()
		if dfile, err = os.Create(fmt.Sprintf("%s/%s", dirPath, dentryDumpFileName)); err != nil {
			return
		}
		defer dfile.Close()
		if err = importRawDataFromRemote(ifile, dfile, chkopt); err != nil {
			return
		}
		// go back to the beginning of the files
		ifile.Seek(0, 0)
		dfile.Seek(0, 0)
	} else {
		if ifile, err = os.Open(InodesFile); err != nil {
			return
		}
		defer ifile.Close()
		if dfile, err = os.Open(DensFile); err != nil {
			return
		}
		defer dfile.Close()
	}

	/*
	 * Perform analysis
	 */
	imap, dlist, err := analyze(ifile, dfile)
	if err != nil {
		return
	}

	if chkopt&InodeCheckOpt != 0 {
		if err = dumpObsoleteInode(imap, fmt.Sprintf("%s/%s", dirPath, obsoleteInodeDumpFileName)); err != nil {
			return
		}
	}
	if chkopt&DentryCheckOpt != 0 {
		if err = dumpObsoleteDentry(dlist, fmt.Sprintf("%s/%s", dirPath, obsoleteDentryDumpFileName)); err != nil {
			return
		}
	}
	return
}

func CheckMP() (err error) {
	var dirPath string

	if (MpId == 0 && VolName == "") || MasterAddr == "" {
		err = fmt.Errorf("Lack of mandatory args: master(%v) vol(%v)", MasterAddr, VolName)
		return
	}
	if VolName != "" {
		dirPath = fmt.Sprintf("_export_%s", VolName)
	} else {
		dirPath = fmt.Sprintf("_export_mp_%d", MpId)
	}
	if err = os.MkdirAll(dirPath, 0o666); err != nil {
		return
	}

	if mpCheckLog, err = os.Create(fmt.Sprintf("%s/%s", dirPath, "mpCheck.log")); err != nil {
		return
	}
	defer mpCheckLog.Close()

	mc := master.NewMasterClient([]string{MasterAddr}, false)
	upGradeCompatibleSettings, err := mc.AdminAPI().GetUpgradeCompatibleSettings()
	if err != nil {
		log.Fatalf("CheckMP: Get UpGradeCompatibleSettings failed err(%v)", err)
	}
	if !upGradeCompatibleSettings.DataMediaTypeVaild {
		log.Fatalf("CheckMp: %v DataMediaType is not valid", upGradeCompatibleSettings)
	}
	storageClass := upGradeCompatibleSettings.LegacyDataMediaType
	metanode.SetLegacyType(storageClass)

	if MpId != 0 {
		var mp *proto.MetaPartitionInfo
		mp, err = getMetaPartitionById(MasterAddr, MpId)
		if err != nil {
			return
		}
		startTime := time.Now()
		mpCheckLog.WriteString(fmt.Sprintf("StartTime: %v\n", startTime))
		err = importAndAnalyzePartitionData(MpId, mp.Hosts, dirPath)
		if err != nil {
			return
		}
		mpCheckLog.WriteString(fmt.Sprintf("EndTime: %v\n", time.Now()))
		mpCheckLog.WriteString(fmt.Sprintf("CostTime: %v\n", time.Since(startTime)))
		return
	}

	mps, err := getMetaPartitions(MasterAddr, VolName)
	if err != nil {
		return
	}

	startTime := time.Now()
	mpCheckLog.WriteString(fmt.Sprintf("StartTime: %v\n", startTime))
	for _, mp := range mps {
		err = importAndAnalyzePartitionData(mp.PartitionID, mp.Members, dirPath)
		if err != nil {
			return
		}
	}
	mpCheckLog.WriteString(fmt.Sprintf("EndTime: %v\n", time.Now()))
	mpCheckLog.WriteString(fmt.Sprintf("CostTime: %v\n", time.Since(startTime)))

	return
}

func importRawDataFromRemote(ifile, dfile *os.File, opt int) error {
	/*
	 * Get all the meta partitions info
	 */
	mps, err := getMetaPartitions(MasterAddr, VolName)
	if err != nil {
		return err
	}

	/*
	 * Note that if we are about to clean obsolete inodes,
	 * we should get all inodes before geting all dentries.
	 */
	if opt&InodeCheckOpt != 0 {
		for _, mp := range mps {
			cmdline := fmt.Sprintf("http://%s:%s/getAllInodes?pid=%d", strings.Split(mp.LeaderAddr, ":")[0], MetaPort, mp.PartitionID)
			if err := exportToFile(ifile, cmdline); err != nil {
				return err
			}
		}

		for _, mp := range mps {
			cmdline := fmt.Sprintf("http://%s:%s/getAllDentry?pid=%d", strings.Split(mp.LeaderAddr, ":")[0], MetaPort, mp.PartitionID)
			if err = exportToFile(dfile, cmdline); err != nil {
				return err
			}
		}
	} else if opt&DentryCheckOpt != 0 {
		for _, mp := range mps {
			cmdline := fmt.Sprintf("http://%s:%s/getAllDentry?pid=%d", strings.Split(mp.LeaderAddr, ":")[0], MetaPort, mp.PartitionID)
			if err = exportToFile(dfile, cmdline); err != nil {
				return err
			}
		}

		for _, mp := range mps {
			cmdline := fmt.Sprintf("http://%s:%s/getAllInodes?pid=%d", strings.Split(mp.LeaderAddr, ":")[0], MetaPort, mp.PartitionID)
			if err := exportToFile(ifile, cmdline); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("Invalid opt: %v", opt)
	}
	return nil
}

func importAndAnalyzePartitionData(mpId uint64, addrs []string, dirPath string) error {
	var (
		mpMap    = make(map[string]MpMap)
		applieds = make(map[string]uint64)
		wg       sync.WaitGroup
		mu       sync.Mutex
		err      error
	)

	if _, err = mpCheckLog.WriteString(fmt.Sprintf("analyze mp %v start\n", mpId)); err != nil {
		return err
	}

	// addrs := mp.Members
	for _, addr := range addrs {
		resp, err := checkHTTPClient.Get(fmt.Sprintf("http://%s:%s/getRaftStatus?id=%d", strings.Split(addr, ":")[0], MetaPort, mpId))
		if err != nil {
			return fmt.Errorf("Get request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return fmt.Errorf("Invalid status code: %v", resp.StatusCode)
		}

		var raftStatus struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				Applied uint64 `json:"applied"`
			} `json:"data"`
		}
		if err = json.NewDecoder(resp.Body).Decode(&raftStatus); err != nil {
			return fmt.Errorf("Decode raft status failed: %v", err)
		}
		applieds[addr] = raftStatus.Data.Applied
	}

	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			imap := make(map[uint64]*metanode.Inode)
			dmap := make(map[string]*metanode.Dentry)
			if err = getInodes(mpId, imap, addr); err != nil {
				return
			}
			if err = getDentries(mpId, dmap, addr); err != nil {
				return
			}
			mu.Lock()
			mpMap[addr] = MpMap{Imap: imap, Dmap: dmap}
			mu.Unlock()
		}(addr)
	}
	wg.Wait()

	for i, addr1 := range addrs {
		for j := i + 1; j < len(addrs); j++ {
			addr2 := addrs[j]
			if !isCheckApplyId {
				analyzeInode(mpMap[addr1].Imap, mpMap[addr2].Imap, addr1, addr2)
				continue
			}
			if applieds[addr1] == applieds[addr2] {
				analyzeInode(mpMap[addr1].Imap, mpMap[addr2].Imap, addr1, addr2)
			} else {
				mpCheckLog.WriteString(fmt.Sprintf("mp %v in %v and %v have different applyId\n", mpId, addr1, addr2))
			}
		}
	}

	for _, addr := range addrs {
		mpCheckLog.WriteString(fmt.Sprintf("mp %v in %v have %v inodes and %v dentries\n", mpId, addr, len(mpMap[addr].Imap), len(mpMap[addr].Dmap)))
	}

	for i, addr1 := range addrs {
		for j := i + 1; j < len(addrs); j++ {
			addr2 := addrs[j]
			if !isCheckApplyId {
				analyzeDentry(mpMap[addr1].Dmap, mpMap[addr2].Dmap, addr1, addr2)
				continue
			}
			if applieds[addr1] == applieds[addr2] {
				analyzeDentry(mpMap[addr1].Dmap, mpMap[addr2].Dmap, addr1, addr2)
			}
		}
	}

	if _, err = mpCheckLog.WriteString(fmt.Sprintf("analyze mp %v end\n", mpId)); err != nil {
		return err
	}

	return nil
}

func getInodes(mpId uint64, imap map[uint64]*metanode.Inode, addr string) (err error) {
	resp, err := checkHTTPClient.Get(fmt.Sprintf("http://%s:%s/getInodeSnapshot?pid=%d", strings.Split(addr, ":")[0], MetaPort, mpId))
	if err != nil {
		return fmt.Errorf("Get request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("Invalid status code: %v", resp.StatusCode)
	}

	defer func() {
		mpCheckLog.WriteString(fmt.Sprintf("getInodes from %v have %v inodes, err %v\n", addr, len(imap), err))
	}()

	data, err := io.ReadAll(bufio.NewReaderSize(resp.Body, 4*1024*1024))
	if err != nil {
		err = errors.NewErrorf("[loadInode] ReadAll: %s", err.Error())
		return
	}

	offset := 0
	total := len(data)
	for offset < total {
		if total-offset < 4 {
			err = errors.NewErrorf("[loadInode] ReadHeader: truncated header, remain=%d", total-offset)
			return
		}

		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if length < 0 || total-offset < length {
			err = errors.NewErrorf("[loadInode] ReadBody: invalid body length=%d, remain=%d", length, total-offset)
			return
		}

		inoBuf := data[offset : offset+length]
		offset += length

		inode := &metanode.Inode{}
		if err = inode.Unmarshal(inoBuf); err != nil {
			err = errors.NewErrorf("[loadInode] Unmarshal: %s", err.Error())
			return
		}
		imap[inode.Inode] = inode
	}
	return
}

func getDentries(mpId uint64, dmap map[string]*metanode.Dentry, addr string) (err error) {
	resp, err := checkHTTPClient.Get(fmt.Sprintf("http://%s:%s/getDentrySnapshot?pid=%d", strings.Split(addr, ":")[0], MetaPort, mpId))
	if err != nil {
		return fmt.Errorf("Get request failed: %v %v", resp, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("Invalid status code: %v", resp.StatusCode)
	}

	reader := bufio.NewReaderSize(resp.Body, 4*1024*1024)
	dentryBuf := make([]byte, 4)
	for {
		dentryBuf = dentryBuf[:4]
		// First Read 4byte header length
		_, err = io.ReadFull(reader, dentryBuf)
		if err != nil {
			if err == io.EOF {
				err = nil
				return
			}
			err = errors.NewErrorf("[loadDentry] ReadHeader: %s", err.Error())
			return
		}

		length := binary.BigEndian.Uint32(dentryBuf)

		// next read body
		if uint32(cap(dentryBuf)) >= length {
			dentryBuf = dentryBuf[:length]
		} else {
			dentryBuf = make([]byte, length)
		}
		_, err = io.ReadFull(reader, dentryBuf)
		if err != nil {
			err = errors.NewErrorf("[loadDentry]: ReadBody: %s", err.Error())
			return
		}
		den := &metanode.Dentry{}
		if err = den.Unmarshal(dentryBuf); err != nil {
			err = errors.NewErrorf("[loadDentry] Unmarshal: %s", err.Error())
			return
		}
		key := fmt.Sprintf("%d/%d/%s", den.ParentId, den.Inode, den.Name)
		dmap[key] = den
	}
}

func compareInodes(i1 *metanode.Inode, i2 *metanode.Inode) *bytes.Buffer {
	var buffer bytes.Buffer

	if i1.Inode != i2.Inode {
		buffer.WriteString(fmt.Sprintf("Inode: %v != %v ", i1.Inode, i2.Inode))
	}
	if i1.Type != i2.Type {
		buffer.WriteString(fmt.Sprintf("Type: %v != %v ", i1.Type, i2.Type))
	}
	if i1.Uid != i2.Uid {
		buffer.WriteString(fmt.Sprintf("Uid: %v != %v ", i1.Uid, i2.Uid))
	}
	if i1.Gid != i2.Gid {
		buffer.WriteString(fmt.Sprintf("Gid: %v != %v ", i1.Gid, i2.Gid))
	}
	if i1.Size != i2.Size {
		buffer.WriteString(fmt.Sprintf("Size: %v != %v ", i1.Size, i2.Size))
	}
	if i1.Generation != i2.Generation {
		buffer.WriteString(fmt.Sprintf("Generation: %v != %v ", i1.Generation, i2.Generation))
	}
	if i1.CreateTime != i2.CreateTime {
		buffer.WriteString(fmt.Sprintf("CreateTime: %v != %v ", i1.CreateTime, i2.CreateTime))
	}
	// if i1.AccessTime != i2.AccessTime {
	// 	buffer.WriteString(fmt.Sprintf("AccessTime: %v != %v ", i1.AccessTime, i2.AccessTime))
	// }
	// if i1.ModifyTime != i2.ModifyTime {
	// 	buffer.WriteString(fmt.Sprintf("ModifyTime: %v != %v ", i1.ModifyTime, i2.ModifyTime))
	// }
	if !bytes.Equal(i1.LinkTarget, i2.LinkTarget) {
		buffer.WriteString(fmt.Sprintf("LinkTarget: %v != %v ", i1.LinkTarget, i2.LinkTarget))
	}
	if i1.NLink != i2.NLink {
		buffer.WriteString(fmt.Sprintf("NLink: %v != %v ", i1.NLink, i2.NLink))
	}
	if i1.Flag != i2.Flag {
		buffer.WriteString(fmt.Sprintf("Flag: %v != %v ", i1.Flag, i2.Flag))
	}
	// if i1.Reserved != i2.Reserved {
	// 	buffer.WriteString(fmt.Sprintf("Reserved: %v != %v ", i1.Reserved, i2.Reserved))
	// }

	if i1.StorageClass != i2.StorageClass {
		buffer.WriteString(fmt.Sprintf("StorageClass: %v != %v ", i1.StorageClass, i2.StorageClass))
	} else {
		if i1.HybridCloudExtents.GetSortedEks() != nil && i2.HybridCloudExtents.GetSortedEks() == nil ||
			i1.HybridCloudExtents.GetSortedEks() == nil && i2.HybridCloudExtents.GetSortedEks() != nil {
			buffer.WriteString(fmt.Sprintf("HybridCloudExtents [%v] != [%v] ", i1.HybridCloudExtents.GetSortedEks(), i2.HybridCloudExtents.GetSortedEks()))
		} else if i1.HybridCloudExtents.GetSortedEks() != nil && i2.HybridCloudExtents.GetSortedEks() != nil {
			if proto.IsStorageClassReplica(i1.StorageClass) {
				ext1 := i1.HybridCloudExtents.GetSortedEks().(*metanode.SortedExtents)
				ext2 := i2.HybridCloudExtents.GetSortedEks().(*metanode.SortedExtents)
				if !ext1.Equals(ext2) {
					buffer.WriteString(fmt.Sprintf("HybridCloudExtents [%v] != [%v] ", ext1, ext2))
				}
			} else {
				ext1 := i1.HybridCloudExtents.GetSortedEks().(*metanode.SortedObjExtents)
				ext2 := i2.HybridCloudExtents.GetSortedEks().(*metanode.SortedObjExtents)
				if !ext1.Equals(ext2) {
					buffer.WriteString(fmt.Sprintf("HybridCloudExtents [%v] != [%v] ", ext1, ext2))
				}
			}
		}
	}

	if i1.HybridCloudExtentsMigration != nil && i2.HybridCloudExtentsMigration == nil ||
		i1.HybridCloudExtentsMigration == nil && i2.HybridCloudExtentsMigration != nil {
		buffer.WriteString(fmt.Sprintf("HybridCloudExtentsMigration [%v] != [%v] ", i1.HybridCloudExtentsMigration, i2.HybridCloudExtentsMigration))
	} else if i1.HybridCloudExtentsMigration != nil && i2.HybridCloudExtentsMigration != nil {
		if i1.HybridCloudExtentsMigration.GetStorageClass() != i2.HybridCloudExtentsMigration.GetStorageClass() ||
			i1.HybridCloudExtentsMigration.GetExpiredTime() != i2.HybridCloudExtentsMigration.GetExpiredTime() {
			buffer.WriteString(fmt.Sprintf("HybridCloudExtentsMigration [%v] != [%v] ", i1.HybridCloudExtentsMigration, i2.HybridCloudExtentsMigration))
		} else {
			if i1.HybridCloudExtentsMigration.GetSortedEks() != nil && i2.HybridCloudExtentsMigration.GetSortedEks() == nil ||
				i1.HybridCloudExtentsMigration.GetSortedEks() == nil && i2.HybridCloudExtentsMigration.GetSortedEks() != nil {
				buffer.WriteString(fmt.Sprintf("HybridCloudExtentsMigration [%v] != [%v] ", i1.HybridCloudExtentsMigration, i2.HybridCloudExtentsMigration))
			} else if i1.HybridCloudExtentsMigration.GetSortedEks() != nil && i2.HybridCloudExtentsMigration.GetSortedEks() != nil {
				if proto.IsStorageClassReplica(i1.HybridCloudExtentsMigration.GetStorageClass()) {
					ext1 := i1.HybridCloudExtentsMigration.GetSortedEks().(*metanode.SortedExtents)
					ext2 := i2.HybridCloudExtentsMigration.GetSortedEks().(*metanode.SortedExtents)
					if !ext1.Equals(ext2) {
						buffer.WriteString(fmt.Sprintf("HybridCloudExtentsMigration [%v] != [%v] ", i1.HybridCloudExtentsMigration, i2.HybridCloudExtentsMigration))
					}
				} else {
					ext1 := i1.HybridCloudExtentsMigration.GetSortedEks().(*metanode.SortedObjExtents)
					ext2 := i2.HybridCloudExtentsMigration.GetSortedEks().(*metanode.SortedObjExtents)
					if !ext1.Equals(ext2) {
						buffer.WriteString(fmt.Sprintf("HybridCloudExtentsMigration [%v] != [%v] ", i1.HybridCloudExtentsMigration, i2.HybridCloudExtentsMigration))
					}
				}
			}
		}
	}

	if i1.ClientID != i2.ClientID {
		buffer.WriteString(fmt.Sprintf("ClientID: %v != %v ", i1.ClientID, i2.ClientID))
	}

	// if i1.LeaseExpireTime != i2.LeaseExpireTime {
	// 	buffer.WriteString(fmt.Sprintf("LeaseExpireTime : %v != %v ", i1.LeaseExpireTime, i2.LeaseExpireTime))
	// }

	return &buffer
}

// func compareInodes(v1, v2 *metanode.Inode) *bytes.Buffer {
// 	var buffer bytes.Buffer

// 	v1Val := reflect.ValueOf(v1).Elem()
// 	v2Val := reflect.ValueOf(v2).Elem()

// 	t := v1Val.Type()
// 	for i := 0; i < t.NumField(); i++ {
// 		field1 := v1Val.Field(i)
// 		field2 := v2Val.Field(i)
// 		if field1.CanInterface() && field2.CanInterface() {
// 			if !field1.Type().Comparable() {
// 				continue
// 			}
// 			if !reflect.DeepEqual(field1.Interface(), field2.Interface()) {
// 				fieldName := t.Field(i).Name
// 				buffer.WriteString(fmt.Sprintf("%s: %v != %v", fieldName, field1.Interface(), field2.Interface()))
// 			}
// 		}
// 	}
// 	return &buffer
// }

func compareDentries(v1, v2 *metanode.Dentry) *bytes.Buffer {
	var buffer bytes.Buffer

	v1Val := reflect.ValueOf(v1).Elem()
	v2Val := reflect.ValueOf(v2).Elem()

	t := v1Val.Type()
	for i := 0; i < t.NumField(); i++ {
		field1 := v1Val.Field(i)
		field2 := v2Val.Field(i)
		if field1.CanInterface() && field2.CanInterface() {
			if !field1.Type().Comparable() {
				continue
			}
			if !reflect.DeepEqual(field1.Interface(), field2.Interface()) {
				fieldName := t.Field(i).Name
				buffer.WriteString(fmt.Sprintf("%s: %v != %v", fieldName, field1.Interface(), field2.Interface()))
			}
		}
	}
	return &buffer
}

func analyzeInode(imap1, imap2 map[uint64]*metanode.Inode, addr1, addr2 string) error {
	arr1 := make([]uint64, 0)
	arr2 := make([]uint64, 0)

	for k := range imap1 {
		arr1 = append(arr1, k)
	}
	for k := range imap2 {
		arr2 = append(arr2, k)
	}

	sort.Slice(arr1, func(i, j int) bool {
		return arr1[i] < arr1[j]
	})
	sort.Slice(arr2, func(i, j int) bool {
		return arr2[i] < arr2[j]
	})

	for _, k := range arr1 {
		v1 := imap1[k]
		v2, ok2 := imap2[k]
		if !ok2 {
			if _, err := mpCheckLog.WriteString(fmt.Sprintf("Inode %v Exists in %v but not exist in %v \n", k, addr1, addr2)); err != nil {
				return err
			}
			continue
		}
		differences := compareInodes(v1, v2)
		if differences.Len() > 0 {
			if _, err := mpCheckLog.WriteString(fmt.Sprintf("Inode %v and %v Exists in both %v and %v but has different fields:  ", v1, v2, addr1, addr2)); err != nil {
				return err
			}
			if _, err := mpCheckLog.WriteString(differences.String()); err != nil {
				return err
			}
			if _, err := mpCheckLog.WriteString("\n"); err != nil {
				return err
			}
		}
	}

	for _, k := range arr2 {
		_, ok1 := imap1[k]
		if !ok1 {
			if _, err := mpCheckLog.WriteString(fmt.Sprintf("Inode %v Exists in %v but not exist in %v \n", k, addr2, addr1)); err != nil {
				return err
			}
		}
	}
	return nil
}

func analyzeDentry(dmap1, dmap2 map[string]*metanode.Dentry, addr1, addr2 string) error {
	arr1 := make([]string, 0)
	arr2 := make([]string, 0)
	for k := range dmap1 {
		arr1 = append(arr1, k)
	}
	for k := range dmap2 {
		arr2 = append(arr2, k)
	}
	sort.Slice(arr1, func(i, j int) bool {
		return arr1[i] < arr1[j]
	})
	sort.Slice(arr2, func(i, j int) bool {
		return arr2[i] < arr2[j]
	})
	for _, k := range arr1 {
		v1 := dmap1[k]
		v2, ok2 := dmap2[k]
		if !ok2 {
			if _, err := mpCheckLog.WriteString(fmt.Sprintf("Dentry %v Exists in %v but not exist in %v \n", k, addr1, addr2)); err != nil {
				return err
			}
			continue
		}
		differences := compareDentries(v1, v2)
		if differences.Len() > 0 {
			if _, err := mpCheckLog.WriteString(fmt.Sprintf("Dentry %v Exists in both %v and %v but has different fields: ", k, addr1, addr2)); err != nil {
				return err
			}
			if _, err := mpCheckLog.WriteString(differences.String()); err != nil {
				return err
			}
			if _, err := mpCheckLog.WriteString("\n"); err != nil {
				return err
			}
		}
	}

	for k := range dmap2 {
		if _, ok1 := dmap1[k]; !ok1 {
			if _, err := mpCheckLog.WriteString(fmt.Sprintf("Dentry %v Exists in %v but not exist in %v \n", k, addr2, addr1)); err != nil {
				return err
			}
		}
	}
	return nil
}

func analyze(ifile, dfile *os.File) (imap map[uint64]*Inode, dlist []*Dentry, err error) {
	imap = make(map[uint64]*Inode)
	dlist = make([]*Dentry, 0)

	/*
	 * Walk through all the inodes to establish inode index
	 */
	dec := json.NewDecoder(ifile)
	for dec.More() {
		inode := &Inode{Dens: make([]*Dentry, 0)}
		if err = dec.Decode(inode); err != nil {
			fmt.Printf("Unmarshal inode failed: %v", err)
			return
		}
		imap[inode.Inode] = inode
	}

	/*
	 * Walk through all the dentries to establish inode relations.
	 */
	dec = json.NewDecoder(dfile)
	for dec.More() {
		body := &struct {
			Code int32     `json:"code"`
			Msg  string    `json:"msg"`
			Data []*Dentry `json:"data"`
		}{}

		if err = dec.Decode(body); err != nil {
			err = fmt.Errorf("Decode failed: %v", err)
			return
		}

		for _, den := range body.Data {
			inode, ok := imap[den.ParentId]
			if !ok {
				dlist = append(dlist, den)
			} else {
				inode.Dens = append(inode.Dens, den)
			}
		}
	}

	root, ok := imap[1]
	if !ok {
		err = fmt.Errorf("No root inode")
		return
	}

	/*
	 * Iterate all the path, and mark reachable inode and dentry.
	 */
	followPath(imap, root)
	return
}

func followPath(imap map[uint64]*Inode, inode *Inode) {
	inode.Valid = true
	// there is no down path for file inode
	if inode.Type == 0 || len(inode.Dens) == 0 {
		return
	}

	for _, den := range inode.Dens {
		childInode, ok := imap[den.Inode]
		if !ok {
			continue
		}
		den.Valid = true
		followPath(imap, childInode)
	}
}

func dumpObsoleteInode(imap map[uint64]*Inode, name string) error {
	var (
		obsoleteTotalCount uint64
		totalCount         uint64
		safeCleanCount     uint64
	)

	fp, err := os.Create(name)
	if err != nil {
		return err
	}
	defer fp.Close()

	for _, inode := range imap {
		if !inode.Valid {
			if _, err = fp.WriteString(inode.String() + "\n"); err != nil {
				return err
			}
			obsoleteTotalCount++
			if inode.NLink == 0 {
				safeCleanCount++
			}
		}
		totalCount++
	}

	fmt.Printf("Total Count: %v\nObselete Total Count: %v\nNLink Zero Total Count: %v\n", totalCount, obsoleteTotalCount, safeCleanCount)
	return nil
}

func dumpObsoleteDentry(dlist []*Dentry, name string) error {
	/*
	 * Note: if we get all the inodes raw data first, then obsolete
	 * dentries are not trustable.
	 */
	fp, err := os.Create(name)
	if err != nil {
		return err
	}
	defer fp.Close()

	for _, den := range dlist {
		if _, err = fp.WriteString(den.String() + "\n"); err != nil {
			return err
		}
	}
	return nil
}

func getMetaPartitions(addr, name string) ([]*proto.MetaPartitionView, error) {
	resp, err := checkHTTPClient.Get(fmt.Sprintf("http://%s%s?name=%s", addr, proto.ClientMetaPartitions, name))
	if err != nil {
		return nil, fmt.Errorf("Get meta partitions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Invalid status code: %v", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Get meta partitions read all body failed: %v", err)
	}

	var mps []*proto.MetaPartitionView
	if err = proto.UnmarshalHTTPReply(data, &mps); err != nil {
		return nil, fmt.Errorf("Unmarshal meta partitions view failed: %v", err)
	}
	return mps, nil
}

func getMetaPartitionById(addr string, id uint64) (*proto.MetaPartitionInfo, error) {
	resp, err := checkHTTPClient.Get(fmt.Sprintf("http://%s%s?id=%d", addr, proto.ClientMetaPartition, id))
	if err != nil {
		return nil, fmt.Errorf("Get meta partitions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Invalid status code: %v", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Get meta partitions read all body failed: %v", err)
	}

	var mp *proto.MetaPartitionInfo
	if err = proto.UnmarshalHTTPReply(data, &mp); err != nil {
		return nil, fmt.Errorf("Unmarshal meta partitions view failed: %v", err)
	}
	return mp, nil
}

func exportToFile(fp *os.File, cmdline string) error {
	resp, err := http.Get(cmdline)
	if err != nil {
		return fmt.Errorf("Get request failed: %v %v", cmdline, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("Invalid status code: %v", resp.StatusCode)
	}

	if _, err = io.Copy(fp, resp.Body); err != nil {
		return fmt.Errorf("io Copy failed: %v", err)
	}
	_, err = fp.WriteString("\n")
	return err
}

// newCheckOrphanBloomCmd creates a command to find inodes not referenced by any dentry.
func newCheckOrphanBloomCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "orphan-bloom",
		Short: "approximately find inodes not referenced by dentries using bloom filter",
		Long: `Approximately find inodes not referenced by any dentry using bloom filter.

This command is memory efficient but approximate. Bloom filter false positives
can cause real orphan inodes to be missed from the result. Do not use
inode.dump.obsolete.bloom as the only input for automatic inode deletion.
Missing some cleanup candidates is an accepted trade-off for lower memory usage
and faster initial assessment.

This command checks dentry references only. It does not verify whether the
dentry path is reachable from root inode 1, so the result is not equivalent to
"check inode" root-reachability analysis.`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := CheckOrphanInodesWithBloom(); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
		},
	}
	return c
}

// CheckOrphanInodesWithBloom uses a bloom filter to stream inodes not referenced by any dentry.
// It does not validate whether a dentry is reachable from the root inode.
func CheckOrphanInodesWithBloom() (err error) {
	if VolName == "" || MasterAddr == "" {
		return fmt.Errorf("missing required parameters: master(%v) vol(%v)", MasterAddr, VolName)
	}

	// Get all meta partitions
	mps, err := getMetaPartitions(MasterAddr, VolName)
	if err != nil {
		return fmt.Errorf("failed to get meta partitions: %v", err)
	}

	if len(mps) == 0 {
		return fmt.Errorf("no meta partitions found")
	}

	// Calculate total inode count from InodeCount of each partition (for bloom filter initialization)
	// More accurate than fixed "10 million per partition" estimate, more reasonable bloom filter memory usage
	var totalInodeEstimate uint64
	for _, mp := range mps {
		totalInodeEstimate += mp.InodeCount
	}
	estimatedInodeCount := uint(totalInodeEstimate)
	if estimatedInodeCount < 1000000 {
		estimatedInodeCount = 1000000 // Minimum 1 million
	}

	m, k := bloom.EstimateParameters(estimatedInodeCount, 0.001)
	// Create bloom filter with false positive rate of 0.001 (0.1%)
	// Bloom filter size is automatically calculated based on element count and false positive rate
	bloomFilter := bloom.New(m, k)
	estFPR := bloom.EstimateFalsePositiveRate(m, k, estimatedInodeCount)
	estimatedMissUpperBound := uint64(float64(estimatedInodeCount) * estFPR)
	fmt.Printf("Bloom filter: capacity=%d, m=%d, k=%d, estimated FPR=%.4f\n", estimatedInodeCount, m, k, estFPR)
	fmt.Printf("Bloom false positives may miss orphan inodes; estimated missed-orphan upper bound is about %d when capacity is accurate\n", estimatedMissUpperBound)
	fmt.Printf("WARNING: result is approximate, not equivalent to check inode, and must not be the only basis for automatic deletion\n")

	fmt.Printf("Starting orphan inode check (using bloom filter, total inodes: %d)\n", estimatedInodeCount)
	fmt.Printf("Meta Partition count: %d\n", len(mps))

	// Set concurrency limit per host (referenced from gc.go, limit to 3)
	setOrphanCheckHostCntLimit(3)

	// Phase 1: Stream all dentries and add their child inode IDs to the bloom filter.
	fmt.Println("Phase 1: Scanning dentries, building referenced inode set...")
	dentryCount := uint64(0)
	referencedInodeCount := uint64(0)

	var mu sync.Mutex
	var wg sync.WaitGroup
	errChan := make(chan error, len(mps))
	phase1Limit := make(chan struct{}, orphanBloomPhase1Concurrency)

	for _, mp := range mps {
		mp := mp
		wg.Add(1)
		go func() {
			defer wg.Done()
			phase1Limit <- struct{}{}
			defer func() {
				<-phase1Limit
			}()
			count, refCount, localBloom, e := streamDentriesToBloom(mp, m, k)
			if e != nil {
				errChan <- fmt.Errorf("failed to process dentries for partition %d: %v", mp.PartitionID, e)
				return
			}
			mu.Lock()
			e = bloomFilter.Merge(localBloom)
			mu.Unlock()
			if e != nil {
				errChan <- fmt.Errorf("failed to merge bloom filter for partition %d: %v", mp.PartitionID, e)
				return
			}
			atomic.AddUint64(&dentryCount, count)
			atomic.AddUint64(&referencedInodeCount, refCount)
		}()
	}

	wg.Wait()
	close(errChan)

	// Check for errors
	for e := range errChan {
		if err == nil {
			err = e
		} else {
			err = fmt.Errorf("%v; %v", err, e)
		}
	}
	if err != nil {
		return err
	}

	// Root inode (ID=1) may not appear as a dentry child, so add it explicitly.
	var rootInodeBuf [8]byte
	bloomFilter.Add(uint64ToBytes(&rootInodeBuf, 1))
	referencedInodeCount++
	if referencedInodeCount > uint64(estimatedInodeCount) {
		fmt.Printf("WARNING: referenced inode count %d exceeds bloom capacity estimate %d; actual FPR may be higher and more orphan inodes may be missed\n", referencedInodeCount, estimatedInodeCount)
	}

	fmt.Printf("Phase 1 completed: processed %d dentries, found %d referenced inodes\n", dentryCount, referencedInodeCount)

	// Phase 2: Stream all inodes and check for orphans
	fmt.Println("Phase 2: Scanning inodes, finding orphans...")
	orphanCount := uint64(0)
	totalInodeCount := uint64(0)
	orphanCountNLinkZero := uint64(0)    // Count of orphan inodes with NLink==0
	orphanCountNLinkNonZero := uint64(0) // Count of orphan inodes with NLink!=0
	totalSizeNLinkZero := uint64(0)      // Total size of orphan inodes with NLink==0
	totalSizeNLinkNonZero := uint64(0)   // Total size of orphan inodes with NLink!=0

	// Statistics for orphan inodes with access time older than one month (for priority deletion)
	oneMonthAgo := time.Now().AddDate(0, -1, 0).Unix() // Timestamp of one month ago
	orphanCountNLinkZeroOldAccess := uint64(0)         // Count of orphan inodes with NLink==0 and access time older than one month
	orphanCountNLinkNonZeroOldAccess := uint64(0)      // Count of orphan inodes with NLink!=0 and access time older than one month
	totalSizeNLinkZeroOldAccess := uint64(0)           // Total size of orphan inodes with NLink==0 and access time older than one month
	totalSizeNLinkNonZeroOldAccess := uint64(0)        // Total size of orphan inodes with NLink!=0 and access time older than one month

	// Use a distinct output file because this check is reference-based, not root-reachability-based.
	dirPath := fmt.Sprintf("_export_%s", VolName)
	outputFile := fmt.Sprintf("%s/%s", dirPath, obsoleteInodeDumpBloomFileName)
	if err = os.MkdirAll(dirPath, 0o666); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}

	outFile, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}

	writer := bufio.NewWriter(outFile)
	outputClosed := false
	defer func() {
		if !outputClosed {
			_ = writer.Flush()
			_ = outFile.Close()
		}
	}()

	// Stream process inodes for each partition
	// Note: Sequential processing is used here instead of concurrent due to shared writer
	// If concurrency is needed, create temporary files for each partition and merge at the end
	for _, mp := range mps {
		count, orphan, cntZero, cntNonZero, szZero, szNonZero, cntZeroOld, cntNonZeroOld, szZeroOld, szNonZeroOld, e := streamInodesCheckOrphan(mp, bloomFilter, writer, oneMonthAgo)
		if e != nil {
			if err == nil {
				err = fmt.Errorf("failed to process inodes for partition %d: %v", mp.PartitionID, e)
			} else {
				err = fmt.Errorf("%v; failed to process inodes for partition %d: %v", err, mp.PartitionID, e)
			}
			continue
		}
		atomic.AddUint64(&totalInodeCount, count)
		atomic.AddUint64(&orphanCount, orphan)
		atomic.AddUint64(&orphanCountNLinkZero, cntZero)
		atomic.AddUint64(&orphanCountNLinkNonZero, cntNonZero)
		atomic.AddUint64(&totalSizeNLinkZero, szZero)
		atomic.AddUint64(&totalSizeNLinkNonZero, szNonZero)
		atomic.AddUint64(&orphanCountNLinkZeroOldAccess, cntZeroOld)
		atomic.AddUint64(&orphanCountNLinkNonZeroOldAccess, cntNonZeroOld)
		atomic.AddUint64(&totalSizeNLinkZeroOldAccess, szZeroOld)
		atomic.AddUint64(&totalSizeNLinkNonZeroOldAccess, szNonZeroOld)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Phase 2 failed, result file is incomplete and invalid: %s\n", outputFile)
		if flushErr := writer.Flush(); flushErr != nil {
			err = fmt.Errorf("%v; failed to flush incomplete result file: %v", err, flushErr)
		}
		if closeErr := outFile.Close(); closeErr != nil {
			err = fmt.Errorf("%v; failed to close incomplete result file: %v", err, closeErr)
		}
		outputClosed = true
		if removeErr := os.Remove(outputFile); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("%v; incomplete result file %s is invalid and failed to remove: %v", err, outputFile, removeErr)
		}
		fmt.Fprintf(os.Stderr, "Removed incomplete result file: %s\n", outputFile)
		return err
	}

	fmt.Printf("\nCheck completed!\n")
	fmt.Printf("Total inode count: %d\n", totalInodeCount)
	fmt.Printf("Orphan inode count: %d\n", orphanCount)
	fmt.Printf("Orphan inodes with NLink==0: %d, Total size: %d bytes\n", orphanCountNLinkZero, totalSizeNLinkZero)
	fmt.Printf("Orphan inodes with NLink!=0: %d, Total size: %d bytes\n", orphanCountNLinkNonZero, totalSizeNLinkNonZero)
	fmt.Printf("\n[Orphan inodes with access time older than one month (priority for deletion)]\n")
	fmt.Printf("Orphan inodes with NLink==0 and access time older than one month: %d, Total size: %d bytes\n", orphanCountNLinkZeroOldAccess, totalSizeNLinkZeroOldAccess)
	fmt.Printf("Orphan inodes with NLink!=0 and access time older than one month: %d, Total size: %d bytes\n", orphanCountNLinkNonZeroOldAccess, totalSizeNLinkNonZeroOldAccess)
	fmt.Printf("Results saved to: %s\n", outputFile)

	return nil
}

// streamDentriesToBloom streams dentries and adds child inode IDs to a local bloom filter.
func streamDentriesToBloom(mp *proto.MetaPartitionView, bloomM, bloomK uint) (dentryCount, referencedInodeCount uint64, bloomFilter *bloom.BloomFilter, err error) {
	host := strings.Split(mp.LeaderAddr, ":")[0]
	// Get token to limit concurrency (referenced from gc.go)
	getOrphanCheckToken(host)
	defer releaseOrphanCheckToken(host)

	url := fmt.Sprintf("http://%s:%s/getDentrySnapshot?pid=%d", host, MetaPort, mp.PartitionID)
	resp, err := checkHTTPClient.Get(url)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("failed to get dentry snapshot: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, 0, nil, fmt.Errorf("invalid status code: %v", resp.StatusCode)
	}

	reader := bufio.NewReaderSize(resp.Body, 4*1024*1024)
	dentryBuf := make([]byte, 4)
	var inodeIDBuf [8]byte
	bloomFilter = bloom.New(bloomM, bloomK)
	seenInodes := make(map[uint64]bool) // Used for deduplication to avoid adding duplicate inodes to bloom filter

	for {
		dentryBuf = dentryBuf[:4]
		// Read 4-byte length header
		_, err = io.ReadFull(reader, dentryBuf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, 0, nil, fmt.Errorf("failed to read dentry length: %v", err)
		}

		length := binary.BigEndian.Uint32(dentryBuf)

		// Read dentry data
		if uint32(cap(dentryBuf)) >= length {
			dentryBuf = dentryBuf[:length]
		} else {
			dentryBuf = make([]byte, length)
		}
		_, err = io.ReadFull(reader, dentryBuf)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("failed to read dentry data: %v", err)
		}

		den := &Dentry{}
		if err = decodeDentry(dentryBuf, den); err != nil {
			return 0, 0, nil, fmt.Errorf("failed to decode dentry: %v", err)
		}

		dentryCount++

		// Add the dentry child inode ID to bloom filter (deduplication).
		if !seenInodes[den.Inode] {
			bloomFilter.Add(uint64ToBytes(&inodeIDBuf, den.Inode))
			seenInodes[den.Inode] = true
			referencedInodeCount++
		}
	}

	return dentryCount, referencedInodeCount, bloomFilter, nil
}

// streamInodesCheckOrphan streams inodes and writes those not referenced by any dentry.
func streamInodesCheckOrphan(mp *proto.MetaPartitionView, bloomFilter *bloom.BloomFilter, writer *bufio.Writer, oneMonthAgo int64) (totalCount, orphanCount, orphanCountNLinkZero, orphanCountNLinkNonZero, sizeNLinkZero, sizeNLinkNonZero, orphanCountNLinkZeroOldAccess, orphanCountNLinkNonZeroOldAccess, sizeNLinkZeroOldAccess, sizeNLinkNonZeroOldAccess uint64, err error) {
	host := strings.Split(mp.LeaderAddr, ":")[0]
	// Get token to limit concurrency (referenced from gc.go)
	getOrphanCheckToken(host)
	defer releaseOrphanCheckToken(host)

	url := fmt.Sprintf("http://%s:%s/getInodeSnapshot?pid=%d", host, MetaPort, mp.PartitionID)
	resp, err := checkHTTPClient.Get(url)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, fmt.Errorf("failed to get inode snapshot: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, fmt.Errorf("invalid status code: %v", resp.StatusCode)
	}

	reader := bufio.NewReaderSize(resp.Body, 4*1024*1024)
	inoBuf := make([]byte, 4)
	var inodeIDBuf [8]byte

	for {
		inoBuf = inoBuf[:4]
		// Read 4-byte length header
		_, err = io.ReadFull(reader, inoBuf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, fmt.Errorf("failed to read inode length: %v", err)
		}

		length := binary.BigEndian.Uint32(inoBuf)

		// Read inode data
		if uint32(cap(inoBuf)) >= length {
			inoBuf = inoBuf[:length]
		} else {
			inoBuf = make([]byte, length)
		}
		_, err = io.ReadFull(reader, inoBuf)
		if err != nil {
			return 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, fmt.Errorf("failed to read inode data: %v", err)
		}

		inode := &Inode{Dens: make([]*Dentry, 0)}
		if err = decodeInode(inoBuf, inode); err != nil {
			return 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, fmt.Errorf("failed to decode inode: %v", err)
		}

		totalCount++

		// Root inode (ID=1) is always reachable, skip
		if inode.Inode == 1 {
			continue
		}

		// Check if inode is in bloom filter (i.e., if it is referenced).
		isReferenced := bloomFilter.Test(uint64ToBytes(&inodeIDBuf, inode.Inode))

		if !isReferenced {
			// This is an orphan inode, count and accumulate size by NLink
			isOldAccess := inode.AccessTime > 0 && inode.AccessTime < oneMonthAgo // Access time is older than one month

			if inode.NLink == 0 {
				orphanCountNLinkZero++
				sizeNLinkZero += inode.Size
				if isOldAccess {
					orphanCountNLinkZeroOldAccess++
					sizeNLinkZeroOldAccess += inode.Size
				}
			} else {
				orphanCountNLinkNonZero++
				sizeNLinkNonZero += inode.Size
				if isOldAccess {
					orphanCountNLinkNonZeroOldAccess++
					sizeNLinkNonZeroOldAccess += inode.Size
				}
			}
			orphanCount++
			line := inode.String() + "\n"
			if _, err = writer.WriteString(line); err != nil {
				return 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, fmt.Errorf("failed to write to file: %v", err)
			}
		}
	}

	return totalCount, orphanCount, orphanCountNLinkZero, orphanCountNLinkNonZero, sizeNLinkZero, sizeNLinkNonZero, orphanCountNLinkZeroOldAccess, orphanCountNLinkNonZeroOldAccess, sizeNLinkZeroOldAccess, sizeNLinkNonZeroOldAccess, nil
}

// uint64ToBytes writes v into buf and returns buf as bytes for bloom hashing.
func uint64ToBytes(buf *[8]byte, v uint64) []byte {
	binary.BigEndian.PutUint64(buf[:], v)
	return buf[:]
}
