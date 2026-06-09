package cmd

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/bits-and-blooms/bloom/v3"
)

func Test_uint64ToBytes(t *testing.T) {
	tests := []struct {
		input uint64
		want  []byte
	}{
		{1, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}},
		{123456789, []byte{0x00, 0x00, 0x00, 0x00, 0x07, 0x5b, 0xcd, 0x15}},
	}
	for _, tt := range tests {
		var buf [8]byte
		got := uint64ToBytes(&buf, tt.input)
		if !bytes.Equal(got, tt.want) {
			t.Errorf("uint64ToBytes(%d) = %v, want %v", tt.input, got, tt.want)
		}
		// Test round-trip conversion
		if binary.BigEndian.Uint64(got) != tt.input {
			t.Errorf("round-trip failed for %d: got %d", tt.input, binary.BigEndian.Uint64(got))
		}
	}
}

func TestBloomFilterRootInode(t *testing.T) {
	// Root inode (1) should be explicitly added to bloom filter
	estimatedInodeCount := uint(1000)
	bf := bloom.NewWithEstimates(estimatedInodeCount, 0.001)
	var buf [8]byte
	bf.Add(uint64ToBytes(&buf, 1))

	if !bf.Test(uint64ToBytes(&buf, 1)) {
		t.Error("root inode 1 should be in bloom filter after explicit add")
	}
}

func TestBloomFalsePositiveBehavior(t *testing.T) {
	// Create bloom filter with 0.001 FPR
	bf := bloom.NewWithEstimates(1000, 0.001)

	// Add some inodes
	inodes := []uint64{1, 2, 3, 4, 5}
	var buf [8]byte
	for _, ino := range inodes {
		bf.Add(uint64ToBytes(&buf, ino))
	}

	// Check that added inodes are detected as referenced
	for _, ino := range inodes {
		if !bf.Test(uint64ToBytes(&buf, ino)) {
			t.Errorf("inode %d should be detected as referenced in bloom filter", ino)
		}
	}

	// An inode not added should not be detected
	nonAdded := uint64(99999)
	if bf.Test(uint64ToBytes(&buf, nonAdded)) {
		// This is a false positive, possible but very unlikely for 0.001 FPR
		t.Logf("WARNING: inode %d was false positive (expected but rare)", nonAdded)
	}
}
