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

package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	kvstore "github.com/cubefs/cubefs/blobstore/common/kvstorev2"
	cproto "github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/common/raft"
	"github.com/cubefs/cubefs/blobstore/common/rpc2"
	"github.com/cubefs/cubefs/blobstore/shardnode/proto"
	"github.com/stretchr/testify/require"
)

func TestServerShardSM_Item(t *testing.T) {
	mockShard, shardClean := newMockShard(t)
	defer shardClean()
	oldProtoItem := &proto.Item{
		ID: []byte{1},
		Fields: []proto.Field{
			{ID: 0, Value: []byte("string")},
			{ID: 1, Value: []byte{1}},
		},
	}
	oldkv, err := InitKV(oldProtoItem.ID, &io.LimitedReader{R: rpc2.Codec2Reader(oldProtoItem), N: int64(oldProtoItem.Size())})
	require.NoError(t, err)

	newProtoItem := &proto.Item{
		ID: []byte{1},
		Fields: []proto.Field{
			{ID: 0, Value: []byte("string")},
			{ID: 1, Value: []byte{2}},
		},
	}
	newProtoItemBytes, err := newProtoItem.Marshal()
	require.NoError(t, err)

	// Insert
	err = mockShard.shardSM.applyInsertRaw(ctx, oldkv.Marshal())
	require.Nil(t, err)
	checkItemEqual(t, mockShard, oldProtoItem.ID, oldProtoItem)
	err = mockShard.shardSM.applyInsertRaw(ctx, oldkv.Marshal())
	require.Nil(t, err)
	checkItemEqual(t, mockShard, oldProtoItem.ID, oldProtoItem)
	// Update
	require.Error(t, mockShard.shardSM.applyUpdateItem(ctx, []byte("a")))
	notFoundItem := proto.Item{ID: []byte{10}}
	notFoundItemBytes, _ := notFoundItem.Marshal()
	err = mockShard.shardSM.applyUpdateItem(ctx, notFoundItemBytes)
	require.Nil(t, err)

	err = mockShard.shardSM.applyUpdateItem(ctx, newProtoItemBytes)
	require.Nil(t, err)
	checkItemEqual(t, mockShard, newProtoItem.ID, newProtoItem)
	// Delete
	err = mockShard.shardSM.applyDeleteRaw(ctx, newProtoItem.ID)
	require.Nil(t, err)
	_, err = mockShard.shard.GetItem(ctx, OpHeader{
		ShardKeys: [][]byte{newProtoItem.ID},
	}, newProtoItem.ID)
	require.ErrorIs(t, err, kvstore.ErrNotFound)
	err = mockShard.shardSM.applyDeleteRaw(ctx, newProtoItem.ID)
	require.Nil(t, err)
	_, err = mockShard.shard.GetItem(ctx, OpHeader{
		ShardKeys: [][]byte{newProtoItem.ID},
	}, newProtoItem.ID)
	require.ErrorIs(t, err, kvstore.ErrNotFound)

	// List
	n := 4
	items := make([]*proto.Item, n)
	for i := 0; i < n; i++ {
		s := fmt.Sprint(i)
		protoItem := &proto.Item{
			ID: []byte(s),
			Fields: []proto.Field{
				{ID: 0, Value: []byte("string")},
				{ID: 1, Value: []byte(s)},
			},
		}
		kv, err := InitKV(protoItem.ID, &io.LimitedReader{R: rpc2.Codec2Reader(protoItem), N: int64(protoItem.Size())})
		require.NoError(t, err)
		err = mockShard.shardSM.applyInsertRaw(ctx, kv.Marshal())
		require.Nil(t, err)
		items[i] = protoItem
	}
	rets, marker, err := mockShard.shard.ListItem(ctx, OpHeader{
		ShardKeys: [][]byte{items[0].ID},
	}, nil, items[0].ID, uint64(n-1))
	require.Nil(t, err)
	require.Equal(t, items[n-1].ID, marker)

	for i := 0; i < n-1; i++ {
		require.Equal(t, items[i].ID, rets[i].ID)
	}

	_, marker, err = mockShard.shard.ListItem(ctx, OpHeader{
		ShardKeys: [][]byte{items[0].ID},
	}, nil, items[0].ID, uint64(n))
	require.Nil(t, err)
	require.Nil(t, marker)
}

func TestServerShardSM_Raw(t *testing.T) {
	mockShard, shardClean := newMockShard(t)
	defer shardClean()

	key := []byte("key")
	value := []byte("value")
	newValue := []byte("new-value")

	// Insert
	kv, err := InitKV(key, &io.LimitedReader{R: bytes.NewReader(value), N: int64(len(value))})
	require.Nil(t, err)

	err = mockShard.shardSM.applyInsertRaw(ctx, kv.Marshal())
	require.Nil(t, err)

	// Get
	vg, err := mockShard.shard.Get(ctx, OpHeader{ShardKeys: [][]byte{key}}, key)
	require.Nil(t, err)
	require.Equal(t, value, vg.Value())
	vg.Close()

	kv2, err := InitKV(key, &io.LimitedReader{R: bytes.NewReader(newValue), N: int64(len(newValue))})
	require.Nil(t, err)

	// Update
	err = mockShard.shardSM.applyUpdateRaw(ctx, kv2.Marshal())
	require.Nil(t, err)

	vg1, err := mockShard.shard.Get(ctx, OpHeader{ShardKeys: [][]byte{key}}, key)
	require.Nil(t, err)
	require.Equal(t, newValue, vg1.Value())
	vg1.Close()

	// Delete
	err = mockShard.shardSM.applyDeleteRaw(ctx, key)
	require.Nil(t, err)
	_, err = mockShard.shard.Get(ctx, OpHeader{ShardKeys: [][]byte{key}}, key)
	require.ErrorIs(t, err, kvstore.ErrNotFound)

	// List
	n := 4
	keys := make([][]byte, n)
	values := make([][]byte, n)
	for i := 0; i < n; i++ {
		s := fmt.Sprint(i)
		keys[i] = []byte(s)
		values[i] = []byte(s)

		kv, err = InitKV(keys[i], &io.LimitedReader{R: bytes.NewReader(values[i]), N: int64(len(values[i]))})
		require.Nil(t, err)

		err = mockShard.shardSM.applyInsertRaw(ctx, kv.Marshal())
		require.Nil(t, err)
	}

	rets := make([][]byte, 0)
	rangeFunc := func(value []byte) error {
		buf := make([]byte, len(value))
		copy(buf, value)
		rets = append(rets, buf)
		return nil
	}

	marker, err := mockShard.shard.List(ctx, OpHeader{
		ShardKeys: keys,
	}, nil, keys[0], uint64(n-1), rangeFunc)
	require.Nil(t, err)
	require.Equal(t, keys[n-1], marker)

	for i := 0; i < n-1; i++ {
		require.Equal(t, values[i], rets[i])
	}

	marker, err = mockShard.shard.List(ctx, OpHeader{
		ShardKeys: keys,
	}, nil, keys[0], uint64(n), func(i []byte) error {
		return nil
	})
	require.Nil(t, err)
	require.Nil(t, marker)
}

func TestServerShardSM_Apply(t *testing.T) {
	mockShard, shardClean := newMockShard(t)
	defer shardClean()

	i1 := &proto.Item{
		ID: []byte{1},
		Fields: []proto.Field{
			{ID: 1, Value: []byte("string")},
			{ID: 2, Value: []byte{1}},
		},
	}
	i2 := &proto.Item{
		ID: []byte{2},
		Fields: []proto.Field{
			{ID: 1, Value: []byte("string")},
			{ID: 2, Value: []byte{1}},
		},
	}
	i3 := &proto.Item{
		ID: []byte{1},
		Fields: []proto.Field{
			{ID: 1, Value: []byte("string1")},
			{ID: 2, Value: []byte{2}},
		},
	}

	ib1, _ := InitKV(i1.ID, &io.LimitedReader{R: rpc2.Codec2Reader(i1), N: int64(i1.Size())})
	ib2, _ := InitKV(i2.ID, &io.LimitedReader{R: rpc2.Codec2Reader(i2), N: int64(i2.Size())})
	ib3, _ := i3.Marshal()

	db := i1.ID

	pds := []raft.ProposalData{
		{Op: RaftOpInsertRaw, Data: ib1.Marshal()},
		{Op: RaftOpInsertRaw, Data: ib2.Marshal()},
		{Op: RaftOpUpdateItem, Data: ib3},
		{Op: RaftOpDeleteRaw, Data: db},
	}
	ret, err := mockShard.shardSM.Apply(ctx, pds, 1)
	require.Nil(t, err)
	require.Nil(t, ret[0])
	require.Nil(t, ret[1])
	require.Nil(t, ret[2])
	require.Nil(t, ret[3])

	require.Panics(t, func() {
		_, _ = mockShard.shardSM.Apply(ctx, []raft.ProposalData{{
			Op: 999,
		}}, 1)
	})
}

func TestServer_BlobList(t *testing.T) {
	ctx := context.Background()
	mockShard, shardClean := newMockShard(t)
	defer shardClean()

	err := mockShard.shard.SaveShardInfo(ctx, false, true)
	require.Nil(t, err)

	blobs := make([]cproto.Blob, 0)
	n := 4
	for i := 0; i < n; i++ {
		b := cproto.Blob{
			Name: []byte(fmt.Sprintf("blob%d", i)),
		}
		kv, _ := InitKV(b.Name, &io.LimitedReader{R: rpc2.Codec2Reader(&b), N: int64(b.Size())})
		mockShard.shardSM.applyInsertRaw(ctx, kv.Marshal())
		blobs = append(blobs, b)
	}

	retBlobs := make([]cproto.Blob, 0)
	rangeFunc := func(data []byte) error {
		b := cproto.Blob{}
		if err := b.Unmarshal(data); err != nil {
			return err
		}
		retBlobs = append(retBlobs, b)
		return nil
	}

	// without prefix and marker
	mkr, err := mockShard.shard.List(ctx, OpHeader{}, nil, nil, uint64(n+1), rangeFunc)
	require.Nil(t, err)
	require.Equal(t, n, len(retBlobs))
	for i := 0; i < n; i++ {
		require.Equal(t, blobs[i].Name, retBlobs[i].Name)
	}
	require.Nil(t, mkr)

	retBlobs = retBlobs[:0]
	mkr, err = mockShard.shard.List(ctx, OpHeader{}, nil, nil, uint64(n-1), rangeFunc)
	require.Nil(t, err)
	require.Equal(t, blobs[n-1].Name, mkr)

	// with prefix
	retBlobs = retBlobs[:0]
	mkr, err = mockShard.shard.List(ctx, OpHeader{}, []byte("blob"), nil, uint64(n), rangeFunc)
	require.Nil(t, err)
	require.Equal(t, n, len(retBlobs))
	for i := 0; i < n; i++ {
		require.Equal(t, blobs[i].Name, retBlobs[i].Name)
	}
	require.Nil(t, mkr)

	// with marker
	retBlobs = retBlobs[:0]
	mkr, err = mockShard.shard.List(ctx, OpHeader{}, nil, []byte("blob3"), uint64(1), rangeFunc)
	require.Nil(t, err)
	require.Nil(t, mkr)
	require.Equal(t, 1, len(retBlobs))
	require.Equal(t, blobs[n-1].Name, retBlobs[0].Name)
}

func TestServer_Snapshot(t *testing.T) {
	mockShard, shardClean := newMockShard(t)
	defer shardClean()

	ss := mockShard.shardSM.Snapshot()

	members := make([]raft.Member, 3)
	for i := range members {
		mctx := proto.ShardMemberCtx{
			Suid: cproto.EncodeSuid(1, uint8(i), 1),
		}
		rawCtx, _ := mctx.Marshal()

		members[i] = raft.Member{
			NodeID:  1,
			Host:    "127.0.0.1",
			Type:    raft.MemberChangeType_AddMember,
			Learner: false,
			Context: rawCtx,
		}
	}

	err := mockShard.shardSM.ApplySnapshot(raft.RaftSnapshotHeader{Members: members}, ss)
	require.Nil(t, err)
}

func checkItemEqual(t *testing.T, shard *mockShard, id []byte, item *proto.Item) {
	ret, err := shard.shard.GetItem(ctx, OpHeader{
		ShardKeys: [][]byte{id},
	}, id)
	if err != nil {
		require.ErrorIs(t, err, kvstore.ErrNotFound)
		return
	}

	itm := proto.Item{
		ID:     ret.ID,
		Fields: protoFieldsToInternalFields(ret.Fields),
	}
	require.Equal(t, *item, itm)
}