// Code generated by protoc-gen-gogo. DO NOT EDIT.
// source: shard.proto

package clustermgr

import (
	fmt "fmt"
	github_com_cubefs_cubefs_blobstore_common_proto "github.com/cubefs/cubefs/blobstore/common/proto"
	sharding "github.com/cubefs/cubefs/blobstore/common/sharding"
	_ "github.com/gogo/protobuf/gogoproto"
	proto "github.com/gogo/protobuf/proto"
	io "io"
	math "math"
	math_bits "math/bits"
)

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
// A compilation error at this line likely means your copy of the
// proto package needs to be updated.
const _ = proto.GoGoProtoPackageIsVersion3 // please upgrade the proto package

type Shard struct {
	ShardID              github_com_cubefs_cubefs_blobstore_common_proto.ShardID `protobuf:"varint,1,opt,name=shard_id,json=shardId,proto3,casttype=github.com/cubefs/cubefs/blobstore/common/proto.ShardID" json:"shard_id,omitempty"`
	AppliedIndex         uint64                                                  `protobuf:"varint,3,opt,name=applied_index,json=appliedIndex,proto3" json:"applied_index,omitempty"`
	LeaderIdx            uint32                                                  `protobuf:"varint,4,opt,name=leader_idx,json=leaderIdx,proto3" json:"leader_idx,omitempty"`
	Range                sharding.Range                                          `protobuf:"bytes,5,opt,name=range,proto3" json:"range"`
	Units                []ShardUnitInfo                                         `protobuf:"bytes,6,rep,name=units,proto3" json:"units"`
	Epoch                uint64                                                  `protobuf:"varint,7,opt,name=epoch,proto3" json:"epoch,omitempty"`
	XXX_NoUnkeyedLiteral struct{}                                                `json:"-"`
	XXX_unrecognized     []byte                                                  `json:"-"`
	XXX_sizecache        int32                                                   `json:"-"`
}

func (m *Shard) Reset()         { *m = Shard{} }
func (m *Shard) String() string { return proto.CompactTextString(m) }
func (*Shard) ProtoMessage()    {}
func (*Shard) Descriptor() ([]byte, []int) {
	return fileDescriptor_319ea41e44cdc364, []int{0}
}
func (m *Shard) XXX_Unmarshal(b []byte) error {
	return m.Unmarshal(b)
}
func (m *Shard) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	if deterministic {
		return xxx_messageInfo_Shard.Marshal(b, m, deterministic)
	} else {
		b = b[:cap(b)]
		n, err := m.MarshalToSizedBuffer(b)
		if err != nil {
			return nil, err
		}
		return b[:n], nil
	}
}
func (m *Shard) XXX_Merge(src proto.Message) {
	xxx_messageInfo_Shard.Merge(m, src)
}
func (m *Shard) XXX_Size() int {
	return m.Size()
}
func (m *Shard) XXX_DiscardUnknown() {
	xxx_messageInfo_Shard.DiscardUnknown(m)
}

var xxx_messageInfo_Shard proto.InternalMessageInfo

func (m *Shard) GetShardID() github_com_cubefs_cubefs_blobstore_common_proto.ShardID {
	if m != nil {
		return m.ShardID
	}
	return 0
}

func (m *Shard) GetAppliedIndex() uint64 {
	if m != nil {
		return m.AppliedIndex
	}
	return 0
}

func (m *Shard) GetLeaderIdx() uint32 {
	if m != nil {
		return m.LeaderIdx
	}
	return 0
}

func (m *Shard) GetRange() sharding.Range {
	if m != nil {
		return m.Range
	}
	return sharding.Range{}
}

func (m *Shard) GetUnits() []ShardUnitInfo {
	if m != nil {
		return m.Units
	}
	return nil
}

func (m *Shard) GetEpoch() uint64 {
	if m != nil {
		return m.Epoch
	}
	return 0
}

type ShardUnitInfo struct {
	Suid                 github_com_cubefs_cubefs_blobstore_common_proto.Suid   `protobuf:"varint,1,opt,name=suid,proto3,casttype=github.com/cubefs/cubefs/blobstore/common/proto.Suid" json:"suid,omitempty"`
	DiskID               github_com_cubefs_cubefs_blobstore_common_proto.DiskID `protobuf:"varint,2,opt,name=disk_id,json=diskId,proto3,casttype=github.com/cubefs/cubefs/blobstore/common/proto.DiskID" json:"disk_id,omitempty"`
	Learner              bool                                                   `protobuf:"varint,3,opt,name=learner,proto3" json:"learner,omitempty"`
	XXX_NoUnkeyedLiteral struct{}                                               `json:"-"`
	XXX_unrecognized     []byte                                                 `json:"-"`
	XXX_sizecache        int32                                                  `json:"-"`
}

func (m *ShardUnitInfo) Reset()         { *m = ShardUnitInfo{} }
func (m *ShardUnitInfo) String() string { return proto.CompactTextString(m) }
func (*ShardUnitInfo) ProtoMessage()    {}
func (*ShardUnitInfo) Descriptor() ([]byte, []int) {
	return fileDescriptor_319ea41e44cdc364, []int{1}
}
func (m *ShardUnitInfo) XXX_Unmarshal(b []byte) error {
	return m.Unmarshal(b)
}
func (m *ShardUnitInfo) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	if deterministic {
		return xxx_messageInfo_ShardUnitInfo.Marshal(b, m, deterministic)
	} else {
		b = b[:cap(b)]
		n, err := m.MarshalToSizedBuffer(b)
		if err != nil {
			return nil, err
		}
		return b[:n], nil
	}
}
func (m *ShardUnitInfo) XXX_Merge(src proto.Message) {
	xxx_messageInfo_ShardUnitInfo.Merge(m, src)
}
func (m *ShardUnitInfo) XXX_Size() int {
	return m.Size()
}
func (m *ShardUnitInfo) XXX_DiscardUnknown() {
	xxx_messageInfo_ShardUnitInfo.DiscardUnknown(m)
}

var xxx_messageInfo_ShardUnitInfo proto.InternalMessageInfo

func (m *ShardUnitInfo) GetSuid() github_com_cubefs_cubefs_blobstore_common_proto.Suid {
	if m != nil {
		return m.Suid
	}
	return 0
}

func (m *ShardUnitInfo) GetDiskID() github_com_cubefs_cubefs_blobstore_common_proto.DiskID {
	if m != nil {
		return m.DiskID
	}
	return 0
}

func (m *ShardUnitInfo) GetLearner() bool {
	if m != nil {
		return m.Learner
	}
	return false
}

type ShardReport struct {
	DiskID               github_com_cubefs_cubefs_blobstore_common_proto.DiskID `protobuf:"varint,1,opt,name=disk_id,json=diskId,proto3,casttype=github.com/cubefs/cubefs/blobstore/common/proto.DiskID" json:"disk_id,omitempty"`
	Shard                Shard                                                  `protobuf:"bytes,2,opt,name=shard,proto3" json:"shard"`
	XXX_NoUnkeyedLiteral struct{}                                               `json:"-"`
	XXX_unrecognized     []byte                                                 `json:"-"`
	XXX_sizecache        int32                                                  `json:"-"`
}

func (m *ShardReport) Reset()         { *m = ShardReport{} }
func (m *ShardReport) String() string { return proto.CompactTextString(m) }
func (*ShardReport) ProtoMessage()    {}
func (*ShardReport) Descriptor() ([]byte, []int) {
	return fileDescriptor_319ea41e44cdc364, []int{2}
}
func (m *ShardReport) XXX_Unmarshal(b []byte) error {
	return m.Unmarshal(b)
}
func (m *ShardReport) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	if deterministic {
		return xxx_messageInfo_ShardReport.Marshal(b, m, deterministic)
	} else {
		b = b[:cap(b)]
		n, err := m.MarshalToSizedBuffer(b)
		if err != nil {
			return nil, err
		}
		return b[:n], nil
	}
}
func (m *ShardReport) XXX_Merge(src proto.Message) {
	xxx_messageInfo_ShardReport.Merge(m, src)
}
func (m *ShardReport) XXX_Size() int {
	return m.Size()
}
func (m *ShardReport) XXX_DiscardUnknown() {
	xxx_messageInfo_ShardReport.DiscardUnknown(m)
}

var xxx_messageInfo_ShardReport proto.InternalMessageInfo

func (m *ShardReport) GetDiskID() github_com_cubefs_cubefs_blobstore_common_proto.DiskID {
	if m != nil {
		return m.DiskID
	}
	return 0
}

func (m *ShardReport) GetShard() Shard {
	if m != nil {
		return m.Shard
	}
	return Shard{}
}

type ShardTask struct {
	TaskType             github_com_cubefs_cubefs_blobstore_common_proto.ShardTaskType `protobuf:"varint,1,opt,name=task_type,json=taskType,proto3,casttype=github.com/cubefs/cubefs/blobstore/common/proto.ShardTaskType" json:"task_type,omitempty"`
	DiskID               github_com_cubefs_cubefs_blobstore_common_proto.DiskID        `protobuf:"varint,2,opt,name=disk_id,json=diskId,proto3,casttype=github.com/cubefs/cubefs/blobstore/common/proto.DiskID" json:"disk_id,omitempty"`
	Suid                 github_com_cubefs_cubefs_blobstore_common_proto.Suid          `protobuf:"varint,3,opt,name=suid,proto3,casttype=github.com/cubefs/cubefs/blobstore/common/proto.Suid" json:"suid,omitempty"`
	Epoch                uint64                                                        `protobuf:"varint,4,opt,name=epoch,proto3" json:"epoch,omitempty"`
	XXX_NoUnkeyedLiteral struct{}                                                      `json:"-"`
	XXX_unrecognized     []byte                                                        `json:"-"`
	XXX_sizecache        int32                                                         `json:"-"`
}

func (m *ShardTask) Reset()         { *m = ShardTask{} }
func (m *ShardTask) String() string { return proto.CompactTextString(m) }
func (*ShardTask) ProtoMessage()    {}
func (*ShardTask) Descriptor() ([]byte, []int) {
	return fileDescriptor_319ea41e44cdc364, []int{3}
}
func (m *ShardTask) XXX_Unmarshal(b []byte) error {
	return m.Unmarshal(b)
}
func (m *ShardTask) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	if deterministic {
		return xxx_messageInfo_ShardTask.Marshal(b, m, deterministic)
	} else {
		b = b[:cap(b)]
		n, err := m.MarshalToSizedBuffer(b)
		if err != nil {
			return nil, err
		}
		return b[:n], nil
	}
}
func (m *ShardTask) XXX_Merge(src proto.Message) {
	xxx_messageInfo_ShardTask.Merge(m, src)
}
func (m *ShardTask) XXX_Size() int {
	return m.Size()
}
func (m *ShardTask) XXX_DiscardUnknown() {
	xxx_messageInfo_ShardTask.DiscardUnknown(m)
}

var xxx_messageInfo_ShardTask proto.InternalMessageInfo

func (m *ShardTask) GetTaskType() github_com_cubefs_cubefs_blobstore_common_proto.ShardTaskType {
	if m != nil {
		return m.TaskType
	}
	return 0
}

func (m *ShardTask) GetDiskID() github_com_cubefs_cubefs_blobstore_common_proto.DiskID {
	if m != nil {
		return m.DiskID
	}
	return 0
}

func (m *ShardTask) GetSuid() github_com_cubefs_cubefs_blobstore_common_proto.Suid {
	if m != nil {
		return m.Suid
	}
	return 0
}

func (m *ShardTask) GetEpoch() uint64 {
	if m != nil {
		return m.Epoch
	}
	return 0
}

func init() {
	proto.RegisterType((*Shard)(nil), "cubefs.blobstore.api.clustermgr.Shard")
	proto.RegisterType((*ShardUnitInfo)(nil), "cubefs.blobstore.api.clustermgr.ShardUnitInfo")
	proto.RegisterType((*ShardReport)(nil), "cubefs.blobstore.api.clustermgr.ShardReport")
	proto.RegisterType((*ShardTask)(nil), "cubefs.blobstore.api.clustermgr.ShardTask")
}

func init() { proto.RegisterFile("shard.proto", fileDescriptor_319ea41e44cdc364) }

var fileDescriptor_319ea41e44cdc364 = []byte{
	// 515 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0xb4, 0x92, 0x4f, 0xab, 0xd3, 0x4c,
	0x14, 0xc6, 0xdf, 0xb4, 0x49, 0xff, 0x4c, 0xdf, 0x6e, 0x86, 0xbb, 0x08, 0x57, 0x6c, 0x4a, 0x05,
	0xed, 0x42, 0x26, 0x50, 0x45, 0x17, 0x22, 0x48, 0x2d, 0x42, 0x04, 0x37, 0x73, 0xeb, 0x46, 0x90,
	0x92, 0x76, 0xe6, 0xa6, 0x63, 0xd3, 0x4c, 0x98, 0x99, 0x40, 0xef, 0xb7, 0xf2, 0x33, 0xb8, 0xba,
	0x4b, 0x37, 0x6e, 0x83, 0xe4, 0x63, 0x74, 0x25, 0x39, 0x49, 0xae, 0x57, 0x44, 0xf4, 0x8a, 0x77,
	0x37, 0x73, 0xe6, 0x3c, 0xbf, 0x39, 0xe7, 0x3c, 0x07, 0x0d, 0xf4, 0x36, 0x54, 0x8c, 0xa4, 0x4a,
	0x1a, 0x89, 0xbd, 0x4d, 0xb6, 0xe6, 0xe7, 0x9a, 0xac, 0x63, 0xb9, 0xd6, 0x46, 0x2a, 0x4e, 0xc2,
	0x54, 0x90, 0x4d, 0x9c, 0x69, 0xc3, 0xd5, 0x3e, 0x52, 0xa7, 0x27, 0x91, 0x8c, 0x24, 0xe4, 0xfa,
	0xe5, 0xa9, 0x92, 0x9d, 0x3e, 0xac, 0x64, 0xfe, 0x95, 0xcc, 0xdf, 0xc8, 0xfd, 0x5e, 0x26, 0x3e,
	0xb0, 0x45, 0x12, 0xf9, 0x2a, 0x4c, 0x22, 0x5e, 0x65, 0x4f, 0xbe, 0xb4, 0x90, 0x73, 0x56, 0x3e,
	0xe0, 0x10, 0xf5, 0x20, 0x63, 0x25, 0x98, 0x6b, 0x8d, 0xad, 0xe9, 0x70, 0xfe, 0xaa, 0xc8, 0xbd,
	0x2e, 0x3c, 0x06, 0x8b, 0x63, 0xee, 0x3d, 0x8d, 0x84, 0xd9, 0x66, 0x6b, 0xb2, 0x91, 0x7b, 0xbf,
	0xfe, 0xe3, 0x57, 0x5f, 0x01, 0x9b, 0xd4, 0x52, 0xda, 0x05, 0x6e, 0xc0, 0xf0, 0x3d, 0x34, 0x0c,
	0xd3, 0x34, 0x16, 0x9c, 0xad, 0x44, 0xc2, 0xf8, 0xc1, 0x6d, 0x8f, 0xad, 0xa9, 0x4d, 0xff, 0xaf,
	0x83, 0x41, 0x19, 0xc3, 0x77, 0x11, 0x8a, 0x79, 0xc8, 0xb8, 0x5a, 0x09, 0x76, 0x70, 0xed, 0xb2,
	0x12, 0xda, 0xaf, 0x22, 0x01, 0x3b, 0xe0, 0x97, 0xc8, 0x81, 0xfa, 0x5d, 0x67, 0x6c, 0x4d, 0x07,
	0xb3, 0x07, 0xe4, 0xa7, 0x29, 0x55, 0x35, 0x90, 0xa6, 0x5d, 0x42, 0xcb, 0xf4, 0xb9, 0x7d, 0x99,
	0x7b, 0xff, 0xd1, 0x4a, 0x8b, 0x5f, 0x23, 0x27, 0x4b, 0x84, 0xd1, 0x6e, 0x67, 0xdc, 0x9e, 0x0e,
	0x66, 0x84, 0xfc, 0x66, 0xd4, 0x55, 0x2b, 0x6f, 0x13, 0x61, 0x82, 0xe4, 0x5c, 0x36, 0x2c, 0x40,
	0xe0, 0x13, 0xe4, 0xf0, 0x54, 0x6e, 0xb6, 0x6e, 0x17, 0x9a, 0xa9, 0x2e, 0x93, 0xdc, 0x42, 0xc3,
	0x1f, 0x44, 0x78, 0x89, 0x6c, 0x9d, 0xd5, 0xb3, 0xb5, 0xe7, 0x2f, 0x8a, 0xdc, 0xb3, 0xcf, 0x32,
	0xc1, 0x8e, 0xb9, 0xf7, 0xf8, 0xc6, 0x83, 0xcd, 0x04, 0xa3, 0x40, 0xc3, 0xef, 0x51, 0x97, 0x09,
	0xbd, 0x2b, 0x4d, 0x6b, 0x81, 0x69, 0x8b, 0x22, 0xf7, 0x3a, 0x0b, 0xa1, 0x77, 0xe0, 0xd9, 0x93,
	0x9b, 0xa2, 0x2b, 0x25, 0xed, 0x94, 0xd0, 0x80, 0x61, 0x17, 0x75, 0x63, 0x1e, 0xaa, 0x84, 0x2b,
	0xf0, 0xaa, 0x47, 0x9b, 0xeb, 0xe4, 0xa3, 0x85, 0x06, 0xd0, 0x20, 0xe5, 0xa9, 0x54, 0xe6, 0x7a,
	0x21, 0xd6, 0x2d, 0x14, 0x32, 0x47, 0x0e, 0x18, 0x0a, 0x5d, 0x0e, 0x66, 0xf7, 0xff, 0xcc, 0xb1,
	0xc6, 0x29, 0x90, 0x4e, 0x3e, 0xb5, 0x50, 0x1f, 0xc2, 0xcb, 0x50, 0xef, 0xf0, 0x07, 0xd4, 0x37,
	0xa1, 0xde, 0xad, 0xcc, 0x45, 0xca, 0xeb, 0x92, 0xdf, 0x14, 0xb9, 0xd7, 0x2b, 0x1f, 0x97, 0x17,
	0x29, 0x3f, 0xe6, 0xde, 0xf3, 0xbf, 0xda, 0xf8, 0x06, 0x40, 0x7b, 0xa6, 0x3e, 0xdd, 0xb6, 0x4b,
	0xcd, 0x6a, 0xb5, 0x81, 0xfd, 0xaf, 0x56, 0xeb, 0x6a, 0xb1, 0xed, 0x6b, 0x8b, 0x3d, 0xbf, 0x73,
	0x59, 0x8c, 0xac, 0xcf, 0xc5, 0xc8, 0xfa, 0x5a, 0x8c, 0xac, 0x77, 0x43, 0xe2, 0x3f, 0xfb, 0x3e,
	0xf4, 0x75, 0x07, 0x20, 0x8f, 0xbe, 0x05, 0x00, 0x00, 0xff, 0xff, 0x1e, 0x6e, 0xab, 0xf0, 0xc8,
	0x04, 0x00, 0x00,
}

func (m *Shard) Marshal() (dAtA []byte, err error) {
	size := m.Size()
	dAtA = make([]byte, size)
	n, err := m.MarshalToSizedBuffer(dAtA[:size])
	if err != nil {
		return nil, err
	}
	return dAtA[:n], nil
}

func (m *Shard) MarshalTo(dAtA []byte) (int, error) {
	size := m.Size()
	return m.MarshalToSizedBuffer(dAtA[:size])
}

func (m *Shard) MarshalToSizedBuffer(dAtA []byte) (int, error) {
	i := len(dAtA)
	_ = i
	var l int
	_ = l
	if m.XXX_unrecognized != nil {
		i -= len(m.XXX_unrecognized)
		copy(dAtA[i:], m.XXX_unrecognized)
	}
	if m.Epoch != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.Epoch))
		i--
		dAtA[i] = 0x38
	}
	if len(m.Units) > 0 {
		for iNdEx := len(m.Units) - 1; iNdEx >= 0; iNdEx-- {
			{
				size, err := m.Units[iNdEx].MarshalToSizedBuffer(dAtA[:i])
				if err != nil {
					return 0, err
				}
				i -= size
				i = encodeVarintShard(dAtA, i, uint64(size))
			}
			i--
			dAtA[i] = 0x32
		}
	}
	{
		size, err := m.Range.MarshalToSizedBuffer(dAtA[:i])
		if err != nil {
			return 0, err
		}
		i -= size
		i = encodeVarintShard(dAtA, i, uint64(size))
	}
	i--
	dAtA[i] = 0x2a
	if m.LeaderIdx != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.LeaderIdx))
		i--
		dAtA[i] = 0x20
	}
	if m.AppliedIndex != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.AppliedIndex))
		i--
		dAtA[i] = 0x18
	}
	if m.ShardID != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.ShardID))
		i--
		dAtA[i] = 0x8
	}
	return len(dAtA) - i, nil
}

func (m *ShardUnitInfo) Marshal() (dAtA []byte, err error) {
	size := m.Size()
	dAtA = make([]byte, size)
	n, err := m.MarshalToSizedBuffer(dAtA[:size])
	if err != nil {
		return nil, err
	}
	return dAtA[:n], nil
}

func (m *ShardUnitInfo) MarshalTo(dAtA []byte) (int, error) {
	size := m.Size()
	return m.MarshalToSizedBuffer(dAtA[:size])
}

func (m *ShardUnitInfo) MarshalToSizedBuffer(dAtA []byte) (int, error) {
	i := len(dAtA)
	_ = i
	var l int
	_ = l
	if m.XXX_unrecognized != nil {
		i -= len(m.XXX_unrecognized)
		copy(dAtA[i:], m.XXX_unrecognized)
	}
	if m.Learner {
		i--
		if m.Learner {
			dAtA[i] = 1
		} else {
			dAtA[i] = 0
		}
		i--
		dAtA[i] = 0x18
	}
	if m.DiskID != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.DiskID))
		i--
		dAtA[i] = 0x10
	}
	if m.Suid != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.Suid))
		i--
		dAtA[i] = 0x8
	}
	return len(dAtA) - i, nil
}

func (m *ShardReport) Marshal() (dAtA []byte, err error) {
	size := m.Size()
	dAtA = make([]byte, size)
	n, err := m.MarshalToSizedBuffer(dAtA[:size])
	if err != nil {
		return nil, err
	}
	return dAtA[:n], nil
}

func (m *ShardReport) MarshalTo(dAtA []byte) (int, error) {
	size := m.Size()
	return m.MarshalToSizedBuffer(dAtA[:size])
}

func (m *ShardReport) MarshalToSizedBuffer(dAtA []byte) (int, error) {
	i := len(dAtA)
	_ = i
	var l int
	_ = l
	if m.XXX_unrecognized != nil {
		i -= len(m.XXX_unrecognized)
		copy(dAtA[i:], m.XXX_unrecognized)
	}
	{
		size, err := m.Shard.MarshalToSizedBuffer(dAtA[:i])
		if err != nil {
			return 0, err
		}
		i -= size
		i = encodeVarintShard(dAtA, i, uint64(size))
	}
	i--
	dAtA[i] = 0x12
	if m.DiskID != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.DiskID))
		i--
		dAtA[i] = 0x8
	}
	return len(dAtA) - i, nil
}

func (m *ShardTask) Marshal() (dAtA []byte, err error) {
	size := m.Size()
	dAtA = make([]byte, size)
	n, err := m.MarshalToSizedBuffer(dAtA[:size])
	if err != nil {
		return nil, err
	}
	return dAtA[:n], nil
}

func (m *ShardTask) MarshalTo(dAtA []byte) (int, error) {
	size := m.Size()
	return m.MarshalToSizedBuffer(dAtA[:size])
}

func (m *ShardTask) MarshalToSizedBuffer(dAtA []byte) (int, error) {
	i := len(dAtA)
	_ = i
	var l int
	_ = l
	if m.XXX_unrecognized != nil {
		i -= len(m.XXX_unrecognized)
		copy(dAtA[i:], m.XXX_unrecognized)
	}
	if m.Epoch != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.Epoch))
		i--
		dAtA[i] = 0x20
	}
	if m.Suid != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.Suid))
		i--
		dAtA[i] = 0x18
	}
	if m.DiskID != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.DiskID))
		i--
		dAtA[i] = 0x10
	}
	if m.TaskType != 0 {
		i = encodeVarintShard(dAtA, i, uint64(m.TaskType))
		i--
		dAtA[i] = 0x8
	}
	return len(dAtA) - i, nil
}

func encodeVarintShard(dAtA []byte, offset int, v uint64) int {
	offset -= sovShard(v)
	base := offset
	for v >= 1<<7 {
		dAtA[offset] = uint8(v&0x7f | 0x80)
		v >>= 7
		offset++
	}
	dAtA[offset] = uint8(v)
	return base
}
func (m *Shard) Size() (n int) {
	if m == nil {
		return 0
	}
	var l int
	_ = l
	if m.ShardID != 0 {
		n += 1 + sovShard(uint64(m.ShardID))
	}
	if m.AppliedIndex != 0 {
		n += 1 + sovShard(uint64(m.AppliedIndex))
	}
	if m.LeaderIdx != 0 {
		n += 1 + sovShard(uint64(m.LeaderIdx))
	}
	l = m.Range.Size()
	n += 1 + l + sovShard(uint64(l))
	if len(m.Units) > 0 {
		for _, e := range m.Units {
			l = e.Size()
			n += 1 + l + sovShard(uint64(l))
		}
	}
	if m.Epoch != 0 {
		n += 1 + sovShard(uint64(m.Epoch))
	}
	if m.XXX_unrecognized != nil {
		n += len(m.XXX_unrecognized)
	}
	return n
}

func (m *ShardUnitInfo) Size() (n int) {
	if m == nil {
		return 0
	}
	var l int
	_ = l
	if m.Suid != 0 {
		n += 1 + sovShard(uint64(m.Suid))
	}
	if m.DiskID != 0 {
		n += 1 + sovShard(uint64(m.DiskID))
	}
	if m.Learner {
		n += 2
	}
	if m.XXX_unrecognized != nil {
		n += len(m.XXX_unrecognized)
	}
	return n
}

func (m *ShardReport) Size() (n int) {
	if m == nil {
		return 0
	}
	var l int
	_ = l
	if m.DiskID != 0 {
		n += 1 + sovShard(uint64(m.DiskID))
	}
	l = m.Shard.Size()
	n += 1 + l + sovShard(uint64(l))
	if m.XXX_unrecognized != nil {
		n += len(m.XXX_unrecognized)
	}
	return n
}

func (m *ShardTask) Size() (n int) {
	if m == nil {
		return 0
	}
	var l int
	_ = l
	if m.TaskType != 0 {
		n += 1 + sovShard(uint64(m.TaskType))
	}
	if m.DiskID != 0 {
		n += 1 + sovShard(uint64(m.DiskID))
	}
	if m.Suid != 0 {
		n += 1 + sovShard(uint64(m.Suid))
	}
	if m.Epoch != 0 {
		n += 1 + sovShard(uint64(m.Epoch))
	}
	if m.XXX_unrecognized != nil {
		n += len(m.XXX_unrecognized)
	}
	return n
}

func sovShard(x uint64) (n int) {
	return (math_bits.Len64(x|1) + 6) / 7
}
func sozShard(x uint64) (n int) {
	return sovShard(uint64((x << 1) ^ uint64((int64(x) >> 63))))
}
func (m *Shard) Unmarshal(dAtA []byte) error {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		preIndex := iNdEx
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return ErrIntOverflowShard
			}
			if iNdEx >= l {
				return io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= uint64(b&0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		fieldNum := int32(wire >> 3)
		wireType := int(wire & 0x7)
		if wireType == 4 {
			return fmt.Errorf("proto: Shard: wiretype end group for non-group")
		}
		if fieldNum <= 0 {
			return fmt.Errorf("proto: Shard: illegal tag %d (wire type %d)", fieldNum, wire)
		}
		switch fieldNum {
		case 1:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field ShardID", wireType)
			}
			m.ShardID = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.ShardID |= github_com_cubefs_cubefs_blobstore_common_proto.ShardID(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 3:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field AppliedIndex", wireType)
			}
			m.AppliedIndex = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.AppliedIndex |= uint64(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 4:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field LeaderIdx", wireType)
			}
			m.LeaderIdx = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.LeaderIdx |= uint32(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 5:
			if wireType != 2 {
				return fmt.Errorf("proto: wrong wireType = %d for field Range", wireType)
			}
			var msglen int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				msglen |= int(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			if msglen < 0 {
				return ErrInvalidLengthShard
			}
			postIndex := iNdEx + msglen
			if postIndex < 0 {
				return ErrInvalidLengthShard
			}
			if postIndex > l {
				return io.ErrUnexpectedEOF
			}
			if err := m.Range.Unmarshal(dAtA[iNdEx:postIndex]); err != nil {
				return err
			}
			iNdEx = postIndex
		case 6:
			if wireType != 2 {
				return fmt.Errorf("proto: wrong wireType = %d for field Units", wireType)
			}
			var msglen int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				msglen |= int(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			if msglen < 0 {
				return ErrInvalidLengthShard
			}
			postIndex := iNdEx + msglen
			if postIndex < 0 {
				return ErrInvalidLengthShard
			}
			if postIndex > l {
				return io.ErrUnexpectedEOF
			}
			m.Units = append(m.Units, ShardUnitInfo{})
			if err := m.Units[len(m.Units)-1].Unmarshal(dAtA[iNdEx:postIndex]); err != nil {
				return err
			}
			iNdEx = postIndex
		case 7:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field Epoch", wireType)
			}
			m.Epoch = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.Epoch |= uint64(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		default:
			iNdEx = preIndex
			skippy, err := skipShard(dAtA[iNdEx:])
			if err != nil {
				return err
			}
			if (skippy < 0) || (iNdEx+skippy) < 0 {
				return ErrInvalidLengthShard
			}
			if (iNdEx + skippy) > l {
				return io.ErrUnexpectedEOF
			}
			m.XXX_unrecognized = append(m.XXX_unrecognized, dAtA[iNdEx:iNdEx+skippy]...)
			iNdEx += skippy
		}
	}

	if iNdEx > l {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (m *ShardUnitInfo) Unmarshal(dAtA []byte) error {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		preIndex := iNdEx
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return ErrIntOverflowShard
			}
			if iNdEx >= l {
				return io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= uint64(b&0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		fieldNum := int32(wire >> 3)
		wireType := int(wire & 0x7)
		if wireType == 4 {
			return fmt.Errorf("proto: ShardUnitInfo: wiretype end group for non-group")
		}
		if fieldNum <= 0 {
			return fmt.Errorf("proto: ShardUnitInfo: illegal tag %d (wire type %d)", fieldNum, wire)
		}
		switch fieldNum {
		case 1:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field Suid", wireType)
			}
			m.Suid = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.Suid |= github_com_cubefs_cubefs_blobstore_common_proto.Suid(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 2:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field DiskID", wireType)
			}
			m.DiskID = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.DiskID |= github_com_cubefs_cubefs_blobstore_common_proto.DiskID(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 3:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field Learner", wireType)
			}
			var v int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				v |= int(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			m.Learner = bool(v != 0)
		default:
			iNdEx = preIndex
			skippy, err := skipShard(dAtA[iNdEx:])
			if err != nil {
				return err
			}
			if (skippy < 0) || (iNdEx+skippy) < 0 {
				return ErrInvalidLengthShard
			}
			if (iNdEx + skippy) > l {
				return io.ErrUnexpectedEOF
			}
			m.XXX_unrecognized = append(m.XXX_unrecognized, dAtA[iNdEx:iNdEx+skippy]...)
			iNdEx += skippy
		}
	}

	if iNdEx > l {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (m *ShardReport) Unmarshal(dAtA []byte) error {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		preIndex := iNdEx
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return ErrIntOverflowShard
			}
			if iNdEx >= l {
				return io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= uint64(b&0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		fieldNum := int32(wire >> 3)
		wireType := int(wire & 0x7)
		if wireType == 4 {
			return fmt.Errorf("proto: ShardReport: wiretype end group for non-group")
		}
		if fieldNum <= 0 {
			return fmt.Errorf("proto: ShardReport: illegal tag %d (wire type %d)", fieldNum, wire)
		}
		switch fieldNum {
		case 1:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field DiskID", wireType)
			}
			m.DiskID = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.DiskID |= github_com_cubefs_cubefs_blobstore_common_proto.DiskID(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 2:
			if wireType != 2 {
				return fmt.Errorf("proto: wrong wireType = %d for field Shard", wireType)
			}
			var msglen int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				msglen |= int(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			if msglen < 0 {
				return ErrInvalidLengthShard
			}
			postIndex := iNdEx + msglen
			if postIndex < 0 {
				return ErrInvalidLengthShard
			}
			if postIndex > l {
				return io.ErrUnexpectedEOF
			}
			if err := m.Shard.Unmarshal(dAtA[iNdEx:postIndex]); err != nil {
				return err
			}
			iNdEx = postIndex
		default:
			iNdEx = preIndex
			skippy, err := skipShard(dAtA[iNdEx:])
			if err != nil {
				return err
			}
			if (skippy < 0) || (iNdEx+skippy) < 0 {
				return ErrInvalidLengthShard
			}
			if (iNdEx + skippy) > l {
				return io.ErrUnexpectedEOF
			}
			m.XXX_unrecognized = append(m.XXX_unrecognized, dAtA[iNdEx:iNdEx+skippy]...)
			iNdEx += skippy
		}
	}

	if iNdEx > l {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (m *ShardTask) Unmarshal(dAtA []byte) error {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		preIndex := iNdEx
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return ErrIntOverflowShard
			}
			if iNdEx >= l {
				return io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= uint64(b&0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		fieldNum := int32(wire >> 3)
		wireType := int(wire & 0x7)
		if wireType == 4 {
			return fmt.Errorf("proto: ShardTask: wiretype end group for non-group")
		}
		if fieldNum <= 0 {
			return fmt.Errorf("proto: ShardTask: illegal tag %d (wire type %d)", fieldNum, wire)
		}
		switch fieldNum {
		case 1:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field TaskType", wireType)
			}
			m.TaskType = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.TaskType |= github_com_cubefs_cubefs_blobstore_common_proto.ShardTaskType(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 2:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field DiskID", wireType)
			}
			m.DiskID = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.DiskID |= github_com_cubefs_cubefs_blobstore_common_proto.DiskID(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 3:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field Suid", wireType)
			}
			m.Suid = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.Suid |= github_com_cubefs_cubefs_blobstore_common_proto.Suid(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 4:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field Epoch", wireType)
			}
			m.Epoch = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowShard
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.Epoch |= uint64(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		default:
			iNdEx = preIndex
			skippy, err := skipShard(dAtA[iNdEx:])
			if err != nil {
				return err
			}
			if (skippy < 0) || (iNdEx+skippy) < 0 {
				return ErrInvalidLengthShard
			}
			if (iNdEx + skippy) > l {
				return io.ErrUnexpectedEOF
			}
			m.XXX_unrecognized = append(m.XXX_unrecognized, dAtA[iNdEx:iNdEx+skippy]...)
			iNdEx += skippy
		}
	}

	if iNdEx > l {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func skipShard(dAtA []byte) (n int, err error) {
	l := len(dAtA)
	iNdEx := 0
	depth := 0
	for iNdEx < l {
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return 0, ErrIntOverflowShard
			}
			if iNdEx >= l {
				return 0, io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= (uint64(b) & 0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		wireType := int(wire & 0x7)
		switch wireType {
		case 0:
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return 0, ErrIntOverflowShard
				}
				if iNdEx >= l {
					return 0, io.ErrUnexpectedEOF
				}
				iNdEx++
				if dAtA[iNdEx-1] < 0x80 {
					break
				}
			}
		case 1:
			iNdEx += 8
		case 2:
			var length int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return 0, ErrIntOverflowShard
				}
				if iNdEx >= l {
					return 0, io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				length |= (int(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			if length < 0 {
				return 0, ErrInvalidLengthShard
			}
			iNdEx += length
		case 3:
			depth++
		case 4:
			if depth == 0 {
				return 0, ErrUnexpectedEndOfGroupShard
			}
			depth--
		case 5:
			iNdEx += 4
		default:
			return 0, fmt.Errorf("proto: illegal wireType %d", wireType)
		}
		if iNdEx < 0 {
			return 0, ErrInvalidLengthShard
		}
		if depth == 0 {
			return iNdEx, nil
		}
	}
	return 0, io.ErrUnexpectedEOF
}

var (
	ErrInvalidLengthShard        = fmt.Errorf("proto: negative length found during unmarshaling")
	ErrIntOverflowShard          = fmt.Errorf("proto: integer overflow")
	ErrUnexpectedEndOfGroupShard = fmt.Errorf("proto: unexpected end of group")
)