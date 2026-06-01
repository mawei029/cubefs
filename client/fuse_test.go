package main

import (
	"os"
	"syscall"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/config"
	"github.com/stretchr/testify/require"
)

func TestRegisterInterceptedSignal(t *testing.T) {
	mnt := "test-mount"
	receivedExitSignal := false
	sigRegister := make(chan interface{}, 1)
	registerInterceptedSignal(mnt, func(sig bool) bool {
		receivedExitSignal = sig
		sigRegister <- true
		return true
	})

	err := syscall.Kill(os.Getpid(), syscall.SIGBUS)
	if err != nil {
		t.Errorf("Failed to send SIGINT signal: %v", err)
	}
	<-sigRegister

	require.NoError(t, err)
	require.Equal(t, receivedExitSignal, true)
}

func TestParseMountOptionUpdateInodeMetaOnOverwrite(t *testing.T) {
	savedOptions := append([]proto.MountOption(nil), GlobalMountOptions...)
	t.Cleanup(func() {
		GlobalMountOptions = savedOptions
	})

	newConfig := func() *config.Config {
		cfg := config.NewConfig()
		cfg.SetString("mountPoint", t.TempDir())
		cfg.SetString("volName", "testvol")
		cfg.SetString("owner", "test-owner")
		cfg.SetString("masterAddr", "127.0.0.1:17010")
		return cfg
	}

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	opt, err := parseMountOption(newConfig())
	require.NoError(t, err)
	require.True(t, opt.UpdateInodeMetaOnOverwrite)

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	cfg := newConfig()
	cfg.SetNewVal("updateInodeMetaOnOverwrite", false)
	opt, err = parseMountOption(cfg)
	require.NoError(t, err)
	require.False(t, opt.UpdateInodeMetaOnOverwrite)
}

func TestParseMountOptionAheadReadBlockSize(t *testing.T) {
	savedOptions := append([]proto.MountOption(nil), GlobalMountOptions...)
	t.Cleanup(func() {
		GlobalMountOptions = savedOptions
	})

	newConfig := func() *config.Config {
		cfg := config.NewConfig()
		cfg.SetString("mountPoint", t.TempDir())
		cfg.SetString("volName", "testvol")
		cfg.SetString("owner", "test-owner")
		cfg.SetString("masterAddr", "127.0.0.1:17010")
		return cfg
	}

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	cfg := newConfig()
	cfg.SetNewVal("aheadReadEnable", true)
	cfg.SetNewVal("aheadReadBlockSizeMB", "8")
	opt, err := parseMountOption(cfg)
	require.NoError(t, err)
	require.True(t, opt.AheadReadEnable)
	require.Equal(t, int64(8)*util.MB, opt.AheadReadBlockSize)

	// Without explicit configuration the block size must default to 2MB.
	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	cfg = newConfig()
	cfg.SetNewVal("aheadReadEnable", true)
	opt, err = parseMountOption(cfg)
	require.NoError(t, err)
	require.Equal(t, int64(util.DefaultAheadReadBlockSize), opt.AheadReadBlockSize)
}
