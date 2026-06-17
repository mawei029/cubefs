package proto

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/cubefs/cubefs/blobstore/api/access"
	blog "github.com/cubefs/cubefs/blobstore/util/log"
	"github.com/cubefs/cubefs/util/config"
	"github.com/cubefs/cubefs/util/log"
)

// EbsClientConfig controls blobstore access client settings for cfs-client fuse mount.
type EbsClientConfig struct {
	ConnMode           *uint8   `json:"conn_mode,omitempty"`
	ConsulAddress      string   `json:"consul_address,omitempty"`
	MaxSizePutOnce     *int64   `json:"max_size_put_once,omitempty"`
	LogLevel           *int     `json:"log_level,omitempty"`
	ServiceIntervalS   *int     `json:"service_interval_s,omitempty"`
	FailRetryIntervalS *int     `json:"fail_retry_interval_s,omitempty"`
	MaxFailsPeriodS    *int     `json:"max_fails_period_s,omitempty"`
	HostTryTimes       *int     `json:"host_try_times,omitempty"`
	BodyBaseTimeoutMs  *int64   `json:"body_base_timeout_ms,omitempty"`
	BodyBandwidthMBPs  *float64 `json:"body_bandwidth_mb_ps,omitempty"`
	MaxHostRetry       *int     `json:"max_host_retry,omitempty"`
	MaxPartRetry       *int     `json:"max_part_retry,omitempty"`
	PartConcurrence    *int     `json:"part_concurrence,omitempty"`
}

// DefaultEbsClientConfig returns fuse mount defaults when ebs_config is absent or partial.
func DefaultEbsClientConfig() EbsClientConfig {
	connMode := uint8(access.NoLimitConnMode) // no limit conn mode.
	logLevel := int(log.GetBlobLogLevel())    // warn log level. Default is 2.
	bodyBandwidthMBPs := 2.0                  // if access.NoLimitConnMode, it's invalid. 2MB/s. body Minimum Speed: timeout = ContentLength/BodyBandwidthMBPs + BodyBaseTimeoutMs. Default is 10MBps.
	bodyBaseTimeoutMs := int64(6000)          // if access.NoLimitConnMode, it's invalid. 6 seconds. base timeout for read body. Default is 30000ms.
	serviceIntervalS := 60                    // 1 minute. interval seconds for discovering service hosts, at least 5 seconds and default is 5 minutes. Default is 300s.
	maxSizePutOnce := int64(8388608)          // 8MB. default is 256MB.
	maxPartRetry := 3                         // 3 times, putat retry
	partConcurrence := 0                      // putat concurrency, 0 means no limit
	maxHostRetry := 6                         // 0 means all hosts. max retry hosts of access, default all hosts. Default is 10.
	failRetryIntervalS := -1                  // -1 means remove failed hosts will not work. Failure retry interval, default value is 300s, if FailRetryIntervalS < 0, remove failed hosts will not work. Default is 300s.
	maxFailsPeriodS := 5                      //  if FailRetryIntervalS <0. it's invalid. 5 seconds. Within MaxFailsPeriodS, if the number of failures is greater than or equal to MaxFails, the host is considered disconnected. Default is 10s.
	hostTryTimes := 0                         //  if FailRetryIntervalS <0. it's invalid. 0 means no retry. Number of host failure retries. Default is 3.
	return EbsClientConfig{
		ConnMode:           &connMode,
		LogLevel:           &logLevel,
		BodyBandwidthMBPs:  &bodyBandwidthMBPs,
		BodyBaseTimeoutMs:  &bodyBaseTimeoutMs,
		ServiceIntervalS:   &serviceIntervalS,
		MaxSizePutOnce:     &maxSizePutOnce,
		MaxPartRetry:       &maxPartRetry,
		PartConcurrence:    &partConcurrence,
		MaxHostRetry:       &maxHostRetry,
		FailRetryIntervalS: &failRetryIntervalS,
		MaxFailsPeriodS:    &maxFailsPeriodS,
		HostTryTimes:       &hostTryTimes,
	}
}

// ParseEbsClientConfig loads ebs_config from client json; missing key uses DefaultEbsClientConfig.
func ParseEbsClientConfig(cfg *config.Config) (EbsClientConfig, error) {
	ec := DefaultEbsClientConfig()
	if cfg == nil || !cfg.HasKey("ebs_config") {
		return ec, nil
	}
	raw, err := json.Marshal(cfg.GetValue("ebs_config"))
	if err != nil {
		return EbsClientConfig{}, err
	}
	var patch EbsClientConfig
	if err := json.Unmarshal(raw, &patch); err != nil {
		return EbsClientConfig{}, err
	}
	mergeEbsClientConfig(&ec, patch)
	return ec, nil
}

// ParseEbsClientJson parses a JSON object string and merges it onto DefaultEbsClientConfig.
func ParseEbsClientJson(jsonStr string) (EbsClientConfig, error) {
	ec := DefaultEbsClientConfig()

	jsonStr = strings.TrimSpace(jsonStr)
	if jsonStr == "" {
		return ec, nil
	}

	var patch EbsClientConfig
	if err := json.Unmarshal([]byte(jsonStr), &patch); err != nil {
		return EbsClientConfig{}, err
	}

	mergeEbsClientConfig(&ec, patch)
	return ec, nil
}

func mergeEbsClientConfig(base *EbsClientConfig, patch EbsClientConfig) {
	if patch.ConnMode != nil {
		base.ConnMode = patch.ConnMode
	}
	if patch.LogLevel != nil {
		base.LogLevel = patch.LogLevel
	}
	if patch.BodyBandwidthMBPs != nil {
		base.BodyBandwidthMBPs = patch.BodyBandwidthMBPs
	}
	if patch.BodyBaseTimeoutMs != nil {
		base.BodyBaseTimeoutMs = patch.BodyBaseTimeoutMs
	}
	if patch.ConsulAddress != "" {
		base.ConsulAddress = patch.ConsulAddress
	}
	if patch.ServiceIntervalS != nil {
		base.ServiceIntervalS = patch.ServiceIntervalS
	}
	if patch.MaxSizePutOnce != nil {
		base.MaxSizePutOnce = patch.MaxSizePutOnce
	}
	if patch.MaxPartRetry != nil {
		base.MaxPartRetry = patch.MaxPartRetry
	}
	if patch.PartConcurrence != nil {
		base.PartConcurrence = patch.PartConcurrence
	}
	if patch.MaxHostRetry != nil {
		base.MaxHostRetry = patch.MaxHostRetry
	}
	if patch.FailRetryIntervalS != nil {
		base.FailRetryIntervalS = patch.FailRetryIntervalS
	}
	if patch.MaxFailsPeriodS != nil {
		base.MaxFailsPeriodS = patch.MaxFailsPeriodS
	}
	if patch.HostTryTimes != nil {
		base.HostTryTimes = patch.HostTryTimes
	}
}

// ToAccessConfig builds access.Config for blobstore.NewEbsClient.
// consul_address in ebs_config overrides poolECAddr when non-empty.
func (c EbsClientConfig) ToAccessConfig(poolECAddr, logPath string) (access.Config, error) {
	consulAddr := poolECAddr
	if c.ConsulAddress != "" {
		consulAddr = c.ConsulAddress
	}
	if consulAddr == "" {
		return access.Config{}, fmt.Errorf("consul address empty: set pool ecAddr or ebs_config.consul_address")
	}

	ac := access.Config{
		ConnMode: access.NoLimitConnMode,
		Consul: access.ConsulConfig{
			Address: consulAddr,
		},
		Logger: &access.Logger{
			Filename: path.Join(logPath, "client/ebs.log"),
		},
	}
	if c.ConnMode != nil {
		ac.ConnMode = access.RPCConnectMode(*c.ConnMode)
	}
	if c.LogLevel != nil {
		ac.LogLevel = blog.Level(*c.LogLevel)
	}
	if c.BodyBandwidthMBPs != nil {
		ac.BodyBandwidthMBPs = *c.BodyBandwidthMBPs
	}
	if c.BodyBaseTimeoutMs != nil {
		ac.BodyBaseTimeoutMs = *c.BodyBaseTimeoutMs
	}
	if c.ServiceIntervalS != nil {
		ac.ServiceIntervalS = *c.ServiceIntervalS
	}
	if c.MaxSizePutOnce != nil {
		ac.MaxSizePutOnce = *c.MaxSizePutOnce
	}
	if c.MaxPartRetry != nil {
		ac.MaxPartRetry = *c.MaxPartRetry
	}
	if c.PartConcurrence != nil {
		ac.PartConcurrence = *c.PartConcurrence
	}
	if c.MaxHostRetry != nil {
		ac.MaxHostRetry = *c.MaxHostRetry
	}
	if c.FailRetryIntervalS != nil {
		ac.FailRetryIntervalS = *c.FailRetryIntervalS
	}
	if c.MaxFailsPeriodS != nil {
		ac.MaxFailsPeriodS = *c.MaxFailsPeriodS
	}
	if c.HostTryTimes != nil {
		ac.HostTryTimes = *c.HostTryTimes
	}
	return ac, nil
}
