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

package proto

type (
	ShardID uint32
	Suid    uint64
)

func (s Suid) ShardID() ShardID {
	return ShardID(s >> 32)
}

func (s Suid) Index() uint8 {
	return uint8(s & 0xff000000 >> 24)
}

func (s Suid) Epoch() uint32 {
	return uint32(s & 0xffffff)
}

func EncodeSuid(shardID ShardID, index uint8, epoch uint32) Suid {
	return Suid(uint64(shardID)<<32 + uint64(index)<<24 + uint64(epoch))
}

type SpaceStatus uint8

const (
	SpaceStatusInit = SpaceStatus(iota + 1)
	SpaceStatusNormal
)

type ShardUpdateType uint8

const (
	ShardUpdateTypeAddMember    ShardUpdateType = 0
	ShardUpdateTypeRemoveMember ShardUpdateType = 1
	ShardUpdateTypeSetNormal    ShardUpdateType = 2
)

type FieldType uint8

const (
	FieldTypeBool   = 1
	FieldTypeInt    = 2
	FieldTypeFloat  = 3
	FieldTypeString = 4
	FieldTypeBytes  = 5
)

type IndexOption uint8

const (
	IndexOptionNull     = 0
	IndexOptionIndexed  = 1
	IndexOptionFulltext = 2
	IndexOptionUnique   = 3
)

type FieldID uint32

type ShardTaskType uint8

const (
	ShardTaskTypeClearShard = 1
	ShardTaskTypeCheckpoint = 2
)