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

func TestParseMountOptionHDDAccCache(t *testing.T) {
	savedOptions := append([]proto.MountOption(nil), GlobalMountOptions...)
	t.Cleanup(func() {
		GlobalMountOptions = savedOptions
	})

	cfg := config.NewConfig()
	cfg.SetString("mountPoint", t.TempDir())
	cfg.SetString("volName", "testvol")
	cfg.SetString("owner", "test-owner")
	cfg.SetString("masterAddr", "127.0.0.1:17010")
	cfg.SetNewVal("HDDAccCache", "private-topo")

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	opt, err := parseMountOption(cfg)
	require.NoError(t, err)
	require.Equal(t, "private-topo", opt.HDDAccCache)
}

func TestParseMountOptionEbsBufferCacheLimit(t *testing.T) {
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
	require.Equal(t, int64(512), opt.EbsBufferCacheLimit)

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	cfg := newConfig()
	cfg.SetNewVal("ebsBufferCacheLimit", "256")
	opt, err = parseMountOption(cfg)
	require.NoError(t, err)
	require.Equal(t, int64(256), opt.EbsBufferCacheLimit)

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	cfg = newConfig()
	cfg.SetNewVal("ebsBufferCacheLimit", "-1")
	_, err = parseMountOption(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "EbsBufferCacheLimit")
}

func TestParseMountOptionEnableEbsSdk(t *testing.T) {
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
	require.False(t, opt.EnableEbsSdk)

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	cfg := newConfig()
	cfg.SetNewVal("enableEbsSdk", "true")
	opt, err = parseMountOption(cfg)
	require.NoError(t, err)
	require.True(t, opt.EnableEbsSdk)
}

func TestParseMountOptionEbsConfig(t *testing.T) {
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
	opt, err := parseMountOption(cfg)
	require.NoError(t, err)
	require.Same(t, cfg, opt.Config)
	require.Nil(t, opt.EbsConfig.LogLevel) // follow --logLevel unless ebs_config.log_level set
	require.Equal(t, -1, *opt.EbsConfig.FailRetryIntervalS)

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	cfg = newConfig()
	cfg.SetNewVal("ebs_config", map[string]interface{}{
		"consul_address":        "10.0.0.2:8500",
		"fail_retry_interval_s": 30,
	})
	opt, err = parseMountOption(cfg)
	require.NoError(t, err)
	require.Equal(t, "10.0.0.2:8500", opt.EbsConfig.ConsulAddress)
	require.NotNil(t, opt.EbsConfig.FailRetryIntervalS)
	require.Equal(t, 30, *opt.EbsConfig.FailRetryIntervalS)
}

func TestParseMountOptionEbsConfigInvalid(t *testing.T) {
	savedOptions := append([]proto.MountOption(nil), GlobalMountOptions...)
	t.Cleanup(func() {
		GlobalMountOptions = savedOptions
	})

	cfg := config.NewConfig()
	cfg.SetString("mountPoint", t.TempDir())
	cfg.SetString("volName", "testvol")
	cfg.SetString("owner", "test-owner")
	cfg.SetString("masterAddr", "127.0.0.1:17010")
	cfg.SetNewVal("ebs_config", map[string]interface{}{
		"conn_mode": "invalid",
	})

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	_, err := parseMountOption(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ParseEbsClientConfig failed")
}

func TestParseMountOptionEbsConfigCLIOverride(t *testing.T) {
	savedOptions := append([]proto.MountOption(nil), GlobalMountOptions...)
	t.Cleanup(func() {
		GlobalMountOptions = savedOptions
	})

	cfg := config.NewConfig()
	cfg.SetString("mountPoint", t.TempDir())
	cfg.SetString("volName", "testvol")
	cfg.SetString("owner", "test-owner")
	cfg.SetString("masterAddr", "127.0.0.1:17010")
	cfg.SetNewVal("ebs_config", map[string]interface{}{
		"host_try_times": 10,
	})
	cfg.SetString("ebsConfig", `{"host_try_times":40,"fail_retry_interval_s":30}`)

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	opt, err := parseMountOption(cfg)
	require.NoError(t, err)
	require.NotNil(t, opt.EbsConfig.HostTryTimes)
	require.Equal(t, 40, *opt.EbsConfig.HostTryTimes)
	require.NotNil(t, opt.EbsConfig.FailRetryIntervalS)
	require.Equal(t, 30, *opt.EbsConfig.FailRetryIntervalS)
}

func TestParseMountOptionEbsConfigCLIInvalidJSON(t *testing.T) {
	savedOptions := append([]proto.MountOption(nil), GlobalMountOptions...)
	t.Cleanup(func() {
		GlobalMountOptions = savedOptions
	})

	cfg := config.NewConfig()
	cfg.SetString("mountPoint", t.TempDir())
	cfg.SetString("volName", "testvol")
	cfg.SetString("owner", "test-owner")
	cfg.SetString("masterAddr", "127.0.0.1:17010")
	cfg.SetString("ebsConfig", `{invalid`)

	GlobalMountOptions = append([]proto.MountOption(nil), savedOptions...)
	_, err := parseMountOption(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ParseEbsClientJson failed")
}
