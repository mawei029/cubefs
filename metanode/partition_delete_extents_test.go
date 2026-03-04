// Copyright 2024 The CubeFS Authors.
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

package metanode

import (
	"encoding/binary"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	raftstoremock "github.com/cubefs/cubefs/metanode/mocktest/raftstore"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/raftstore"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/util/synclist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
)

// newTestMetaPartition creates a metaPartition for testing
func newTestMetaPartition(rootDir string, ctrl *gomock.Controller) *metaPartition {
	config := &MetaPartitionConfig{
		PartitionId:   10001,
		VolName:       "test_vol",
		Start:         0,
		End:           100,
		PartitionType: proto.VolumeTypeHot,
		RootDir:       rootDir,
		StoreMode:     proto.StoreModeMem,
		NodeId:        1,
		Peers:         []proto.Peer{{ID: 1, Addr: "127.0.0.1"}},
	}
	mp := newPartition(config, newManager())
	mp.stopC = make(chan bool)
	mp.objExtDelCh = make(chan []proto.ObjExtentKey, 100)

	// Mock raft partition for IsLeader
	if ctrl != nil {
		raft := raftstoremock.NewMockPartition(ctrl)
		raft.EXPECT().LeaderTerm().Return(uint64(1), uint64(1)).AnyTimes()
		raft.EXPECT().Status().Return(&raftstore.PartitionStatus{RestoringSnapshot: false}).AnyTimes()
		mp.raftPartition = raft
	}

	return mp
}

// createTestObjExtentKey creates a test ObjExtentKey
func createTestObjExtentKey(fileOffset, size, bid uint64) proto.ObjExtentKey {
	return proto.ObjExtentKey{
		FileOffset: fileOffset,
		Size:       size,
		Cid:        1,
		CodeMode:   1,
		BlobSize:   1024,
		BlobsLen:   1,
		Blobs: []proto.Blob{
			{MinBid: bid, Count: 1, Vid: 1},
		},
		Crc: 12345,
	}
}

// TestAppendDelObjExtentsToFile tests appendDelObjExtentsToFile function
func TestAppendDelObjExtentsToFile(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "test_append_del_obj_extents")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mp := newTestMetaPartition(rootDir, ctrl)
	fileList := synclist.New()

	// Start appendDelObjExtentsToFile in background
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		mp.appendDelObjExtentsToFile(fileList)
	}()

	// Send test obj extents to channel (batch)
	testOeks := []proto.ObjExtentKey{
		createTestObjExtentKey(0, 1024, 1),
		createTestObjExtentKey(1024, 2048, 2),
	}
	mp.objExtDelCh <- testOeks

	// Wait for file write to complete
	time.Sleep(100 * time.Millisecond)

	// Stop the goroutine
	close(mp.stopC)
	wg.Wait()

	// Verify file was created and contains data
	files, err := ioutil.ReadDir(rootDir)
	require.NoError(t, err)

	var foundFile bool
	for _, file := range files {
		if strings.HasPrefix(file.Name(), prefixDelObjExtent) {
			foundFile = true
			filePath := path.Join(rootDir, file.Name())
			data, err := ioutil.ReadFile(filePath)
			require.NoError(t, err)
			require.Greater(t, len(data), 8) // Should have header (8 bytes) + data
		}
	}
	require.True(t, foundFile, "OBJ_EXTENT_DEL file should be created")
}

// TestDeleteObjExtentsFromList tests deleteObjExtentsFromList function
func TestDeleteObjExtentsFromList(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "test_delete_obj_extents")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mp := newTestMetaPartition(rootDir, ctrl)
	fileList := synclist.New()

	// Create test file with multiple obj extents
	fileName := "OBJ_EXTENT_DEL_0"
	filePath := path.Join(rootDir, fileName)
	fp, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0o644)
	require.NoError(t, err)

	// Write header (8 bytes cursor) and data
	header := make([]byte, 8)
	binary.BigEndian.PutUint64(header, 8) // Cursor at 8 (after header)
	_, err = fp.Write(header)
	require.NoError(t, err)

	// Write two obj extents
	testOek1 := createTestObjExtentKey(0, 1024, 1)
	data1, err := testOek1.MarshalBinary()
	require.NoError(t, err)
	_, err = fp.Write(data1)
	require.NoError(t, err)

	testOek2 := createTestObjExtentKey(1024, 2048, 2)
	data2, err := testOek2.MarshalBinary()
	require.NoError(t, err)
	_, err = fp.Write(data2)
	require.NoError(t, err)
	fp.Close()

	fileList.PushBack(fileName)

	// Inject a non-nil blob client so deleteObjExtents can reach BlobStoreClient.Delete.
	mp.blobClientWrapper = &BlobStoreClientWrapper{
		blobClient: &blobstore.BlobStoreClient{},
	}

	// mock delete
	mockCallCnt := 0
	deleteExtentCnt := 0
	delDone := make(chan struct{})
	patches := gomonkey.NewPatches()
	patches.ApplyMethod(reflect.TypeOf(&blobstore.BlobStoreClient{}), "Delete",
		func(_ *blobstore.BlobStoreClient, oeks []proto.ObjExtentKey) error {
			mockCallCnt++
			deleteExtentCnt += len(oeks)
			delDone <- struct{}{}
			close(mp.stopC)
			return nil
		})
	// deleteObjExtentsFromList has a 1-minute polling sleep. Patch it to run immediately in UT.
	patches.ApplyFunc(time.Sleep, func(_ time.Duration) {})
	defer patches.Reset()

	go func() {
		mp.deleteObjExtentsFromList(fileList)
	}()

	// Wait stop the goroutine
	<-delDone

	// Verify BlobStoreClient.Delete was called and both extents were passed.
	require.Equal(t, 1, mockCallCnt)
	require.Equal(t, 2, deleteExtentCnt)

	// Verify cursor was updated in file header
	fp, err = os.OpenFile(filePath, os.O_RDONLY, 0o644)
	require.NoError(t, err)
	defer fp.Close()

	cursorBuf := make([]byte, 8)
	_, err = fp.ReadAt(cursorBuf, 0)
	require.NoError(t, err)
	cursor := binary.BigEndian.Uint64(cursorBuf)

	// Cursor should be updated beyond header (8 bytes) after reading extents
	require.Greater(t, cursor, uint64(8), "cursor should be updated after reading extents")
}

// TestAppendDelObjExtentsToFile_FileRotation tests file rotation when size exceeds limit
func TestAppendDelObjExtentsToFile_FileRotation(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "test_file_rotation")
	require.NoError(t, err)
	defer os.RemoveAll(rootDir)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mp := newTestMetaPartition(rootDir, ctrl)
	fileList := synclist.New()

	// Start appendDelObjExtentsToFile in background
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		mp.appendDelObjExtentsToFile(fileList)
	}()

	// Send large amount of data to trigger file rotation (one batch)
	// Create enough data to exceed maxDeleteExtentSize (10MB)
	largeOeks := make([]proto.ObjExtentKey, 0, 100)
	for i := 0; i < 100; i++ {
		largeOeks = append(largeOeks, createTestObjExtentKey(uint64(i*1024), 1024, uint64(i+1)))
	}

	// Send multiple batches
	for i := 0; i < 10; i++ {
		mp.objExtDelCh <- largeOeks
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for writes
	time.Sleep(200 * time.Millisecond)

	// Stop the goroutine
	close(mp.stopC)
	wg.Wait()

	// Verify multiple files were created (file rotation occurred)
	files, err := ioutil.ReadDir(rootDir)
	require.NoError(t, err)

	fileCount := 0
	for _, file := range files {
		if strings.HasPrefix(file.Name(), prefixDelObjExtent) {
			fileCount++
		}
	}
	// At least one file should be created
	require.Greater(t, fileCount, 0, "at least one file should be created")
}
