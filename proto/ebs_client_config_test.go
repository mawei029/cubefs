package proto

import (
	"testing"

	"github.com/cubefs/cubefs/blobstore/api/access"
	blog "github.com/cubefs/cubefs/blobstore/util/log"
	"github.com/cubefs/cubefs/util/config"
	"github.com/stretchr/testify/require"
)

func TestDefaultEbsClientConfig(t *testing.T) {
	ec := DefaultEbsClientConfig()
	require.NotNil(t, ec.ConnMode)
	require.Equal(t, uint8(access.NoLimitConnMode), *ec.ConnMode)
	require.Nil(t, ec.LogLevel) // follow fuse --logLevel unless ebsConfig sets log_level
	require.NotNil(t, ec.BodyBandwidthMBPs)
	require.Equal(t, 2.0, *ec.BodyBandwidthMBPs)
	require.NotNil(t, ec.BodyBaseTimeoutMs)
	require.Equal(t, int64(6000), *ec.BodyBaseTimeoutMs)
	require.NotNil(t, ec.ServiceIntervalS)
	require.Equal(t, 60, *ec.ServiceIntervalS)
	require.NotNil(t, ec.FailRetryIntervalS)
	require.Equal(t, -1, *ec.FailRetryIntervalS)
	require.NotNil(t, ec.MaxFailsPeriodS)
	require.Equal(t, 5, *ec.MaxFailsPeriodS)
	require.NotNil(t, ec.HostTryTimes)
	require.Equal(t, 0, *ec.HostTryTimes)
	require.NotNil(t, ec.MaxSizePutOnce)
	require.Equal(t, int64(8388608), *ec.MaxSizePutOnce)
	require.NotNil(t, ec.MaxPartRetry)
	require.Equal(t, 3, *ec.MaxPartRetry)
	require.NotNil(t, ec.PartConcurrence)
	require.Equal(t, 0, *ec.PartConcurrence)
	require.NotNil(t, ec.MaxHostRetry)
	require.Equal(t, 6, *ec.MaxHostRetry)
}

func TestParseEbsClientConfigNilUsesDefaults(t *testing.T) {
	ec, err := ParseEbsClientConfig(nil)
	require.NoError(t, err)
	def := DefaultEbsClientConfig()
	require.Equal(t, *def.ConnMode, *ec.ConnMode)
	require.Equal(t, *def.ServiceIntervalS, *ec.ServiceIntervalS)
}

func TestParseEbsClientConfigMissingKeyUsesDefaults(t *testing.T) {
	cfg := config.LoadConfigString(`{"volName":"v"}`)
	ec, err := ParseEbsClientConfig(cfg)
	require.NoError(t, err)
	def := DefaultEbsClientConfig()
	require.Nil(t, ec.LogLevel)
	require.Equal(t, *def.FailRetryIntervalS, *ec.FailRetryIntervalS)
	require.Equal(t, *def.MaxSizePutOnce, *ec.MaxSizePutOnce)
}

func TestParseEbsClientConfigPartialOverride(t *testing.T) {
	cfg := config.LoadConfigString(`{
		"ebsConfig": {
			"host_try_times": 40,
			"fail_retry_interval_s": 60
		}
	}`)
	ec, err := ParseEbsClientConfig(cfg)
	require.NoError(t, err)
	require.NotNil(t, ec.HostTryTimes)
	require.Equal(t, 40, *ec.HostTryTimes)
	require.NotNil(t, ec.FailRetryIntervalS)
	require.Equal(t, 60, *ec.FailRetryIntervalS)
	require.NotNil(t, ec.MaxHostRetry)
	require.Equal(t, 6, *ec.MaxHostRetry)
}

func TestParseEbsClientConfigFullOverride(t *testing.T) {
	cfg := config.LoadConfigString(`{
		"ebsConfig": {
			"conn_mode": 0,
			"log_level": 1,
			"body_bandwidth_mb_ps": 5.5,
			"body_base_timeout_ms": 8000,
			"consul_address": "10.1.1.1:8500",
			"service_interval_s": 120,
			"max_size_put_once": 67108864,
			"max_part_retry": 5,
			"part_concurrence": 2,
			"max_host_retry": 8,
			"fail_retry_interval_s": 30,
			"max_fails_period_s": 15,
			"host_try_times": 3
		}
	}`)
	ec, err := ParseEbsClientConfig(cfg)
	require.NoError(t, err)
	require.NotNil(t, ec.ConnMode)
	require.Equal(t, uint8(0), *ec.ConnMode)
	require.NotNil(t, ec.LogLevel)
	require.Equal(t, 1, *ec.LogLevel)
	require.NotNil(t, ec.BodyBandwidthMBPs)
	require.Equal(t, 5.5, *ec.BodyBandwidthMBPs)
	require.NotNil(t, ec.BodyBaseTimeoutMs)
	require.Equal(t, int64(8000), *ec.BodyBaseTimeoutMs)
	require.Equal(t, "10.1.1.1:8500", ec.ConsulAddress)
	require.NotNil(t, ec.ServiceIntervalS)
	require.Equal(t, 120, *ec.ServiceIntervalS)
	require.NotNil(t, ec.MaxSizePutOnce)
	require.Equal(t, int64(67108864), *ec.MaxSizePutOnce)
	require.NotNil(t, ec.MaxPartRetry)
	require.Equal(t, 5, *ec.MaxPartRetry)
	require.NotNil(t, ec.PartConcurrence)
	require.Equal(t, 2, *ec.PartConcurrence)
	require.NotNil(t, ec.MaxHostRetry)
	require.Equal(t, 8, *ec.MaxHostRetry)
	require.NotNil(t, ec.FailRetryIntervalS)
	require.Equal(t, 30, *ec.FailRetryIntervalS)
	require.NotNil(t, ec.MaxFailsPeriodS)
	require.Equal(t, 15, *ec.MaxFailsPeriodS)
	require.NotNil(t, ec.HostTryTimes)
	require.Equal(t, 3, *ec.HostTryTimes)
}

func TestParseEbsClientConfigInvalidValue(t *testing.T) {
	cfg := config.LoadConfigString(`{"ebsConfig":{"conn_mode":"bad"}}`)
	_, err := ParseEbsClientConfig(cfg)
	require.Error(t, err)
}

func TestEbsClientConfigToAccessConfigConsulPriority(t *testing.T) {
	ec := DefaultEbsClientConfig()
	ec.ConsulAddress = "10.0.0.1:8500"

	ac, err := ec.ToAccessConfig("127.0.0.1:1", "/tmp/log")
	require.NoError(t, err)
	require.Equal(t, "10.0.0.1:8500", ac.Consul.Address)
	require.Equal(t, access.NoLimitConnMode, ac.ConnMode)
	require.Equal(t, blog.Lwarn, ac.LogLevel)
	require.Equal(t, int64(6000), ac.BodyBaseTimeoutMs)
	require.Equal(t, 60, ac.ServiceIntervalS)
	require.Equal(t, int64(8388608), ac.MaxSizePutOnce)
	require.Equal(t, 3, ac.MaxPartRetry)
	require.Equal(t, 0, ac.PartConcurrence)
	require.Equal(t, 6, ac.MaxHostRetry)
	require.Equal(t, -1, ac.FailRetryIntervalS)
	require.Equal(t, 5, ac.MaxFailsPeriodS)
	require.Equal(t, 0, ac.HostTryTimes)
	require.Equal(t, "/tmp/log/client/ebs.log", ac.Logger.Filename)
}

func TestEbsClientConfigToAccessConfigUsesPoolECAddr(t *testing.T) {
	ec := DefaultEbsClientConfig()
	ac, err := ec.ToAccessConfig("127.0.0.2:8500", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "127.0.0.2:8500", ac.Consul.Address)
}

func TestEbsClientConfigToAccessConfigCustomFields(t *testing.T) {
	cfg := config.LoadConfigString(`{
		"ebsConfig": {
			"conn_mode": 0,
			"log_level": 1,
			"body_bandwidth_mb_ps": 5.5,
			"body_base_timeout_ms": 8000,
			"service_interval_s": 120,
			"max_size_put_once": 67108864,
			"max_part_retry": 5,
			"part_concurrence": 2,
			"max_host_retry": 8,
			"fail_retry_interval_s": 30,
			"max_fails_period_s": 15,
			"host_try_times": 3
		}
	}`)
	ec, err := ParseEbsClientConfig(cfg)
	require.NoError(t, err)

	ac, err := ec.ToAccessConfig("127.0.0.3:8500", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, access.DefaultConnMode, ac.ConnMode)
	require.Equal(t, blog.Level(1), ac.LogLevel)
	require.Equal(t, 5.5, ac.BodyBandwidthMBPs)
	require.Equal(t, int64(8000), ac.BodyBaseTimeoutMs)
	require.Equal(t, 120, ac.ServiceIntervalS)
	require.Equal(t, int64(67108864), ac.MaxSizePutOnce)
	require.Equal(t, 5, ac.MaxPartRetry)
	require.Equal(t, 2, ac.PartConcurrence)
	require.Equal(t, 8, ac.MaxHostRetry)
	require.Equal(t, 30, ac.FailRetryIntervalS)
	require.Equal(t, 15, ac.MaxFailsPeriodS)
	require.Equal(t, 3, ac.HostTryTimes)
}

func TestEbsClientConfigToAccessConfigRequiresConsul(t *testing.T) {
	ec := DefaultEbsClientConfig()
	_, err := ec.ToAccessConfig("", "/tmp/log")
	require.Error(t, err)
}

func TestParseEbsClientJsonPartialOverride(t *testing.T) {
	ec, err := ParseEbsClientJson(`{"host_try_times":40,"fail_retry_interval_s":60}`)
	require.NoError(t, err)
	require.NotNil(t, ec.HostTryTimes)
	require.Equal(t, 40, *ec.HostTryTimes)
	require.NotNil(t, ec.FailRetryIntervalS)
	require.Equal(t, 60, *ec.FailRetryIntervalS)
	require.NotNil(t, ec.MaxSizePutOnce)
	require.Equal(t, int64(8388608), *ec.MaxSizePutOnce)

	ec, err = ParseEbsClientJson("")
	require.NoError(t, err)
	def := DefaultEbsClientConfig()
	require.Equal(t, *def.MaxSizePutOnce, *ec.MaxSizePutOnce)
}
