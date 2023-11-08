package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Shopify/sarama"

	"github.com/cubefs/cubefs/blobstore/common/config"
	"github.com/cubefs/cubefs/blobstore/common/kafka"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/common/trace"
	"github.com/cubefs/cubefs/blobstore/scheduler/base"
	"github.com/cubefs/cubefs/blobstore/util/log"
)

const (
	defaultIntervalMs = 1000
	defaultBatch      = 100
)

var (
	conf           ToolConfig
	defaultVersion = sarama.V2_1_0_0
	maxBatch       = defaultBatch
	cIntervalMs    = defaultIntervalMs
	confPath       = flag.String("f", "dedupeKafka.conf", "config path: dedupe and migrate kafka msg")
)

type ToolConfig struct {
	ClusterID  uint32      `json:"cluster_id"`
	Batch      int         `json:"batch"`
	IntervalMs int         `json:"interval_ms"`
	MaxCount   int64       `json:"max_count"`
	LogLevel   log.Level   `json:"log_level"`
	OldKafka   KafkaConfig `json:"old_kafka"`
	NewKafka   KafkaConfig `json:"new_kafka"`
}

type KafkaConfig struct {
	BrokerList []string `json:"broker_list"`
	Topic      string   `json:"topic"`

	AutoCommit      bool   `json:"auto_commit"`
	TimeoutMs       int64  `json:"timeout_ms"`
	MaxMessageBytes int    `json:"max_message_bytes"`
	Version         string `json:"version"`
	kafkaVersion    sarama.KafkaVersion
}

func (cfg *ToolConfig) producerConfig() *kafka.ProducerCfg {
	return &kafka.ProducerCfg{
		BrokerList: cfg.NewKafka.BrokerList,
		Topic:      cfg.NewKafka.Topic,
		TimeoutMs:  cfg.NewKafka.TimeoutMs,
	}
}

func main() {
	flag.Parse()
	readConf()

	mgr, err := NewScKafkaMgr(&conf)
	if err != nil {
		log.Fatalf("Fail to new ScKafkaMgr, err: %+v", err)
	}

	go mgr.loopSendToNewKafka(context.Background())
	mgr.startConsumer()

	ch := make(chan int)
	<-ch
}

func readConf() {
	confBytes, err := ioutil.ReadFile(*confPath)
	if err != nil {
		log.Fatalf("Fail to read config file, filename: %s, err: %+v", *confPath, err)
	}

	if err = config.LoadData(&conf, confBytes); err != nil {
		log.Fatalf("Fail to load config, err: %+v", err)
	}

	parseVersion(&conf.OldKafka)
	parseVersion(&conf.NewKafka)
	log.SetOutputLevel(conf.LogLevel)

	if conf.IntervalMs <= 0 {
		conf.IntervalMs = defaultIntervalMs
	}
	if conf.Batch <= 0 {
		conf.Batch = defaultBatch
	}
	if conf.MaxCount <= 0 {
		conf.MaxCount = math.MaxInt64
	}

	maxBatch = conf.Batch
	cIntervalMs = conf.IntervalMs
	log.Infof("file: %s, config: %+v", *confPath, conf)
}

func parseVersion(kafkaConf *KafkaConfig) {
	if strings.TrimSpace(kafkaConf.Version) == "" {
		kafkaConf.kafkaVersion = defaultVersion
		return
	}

	var err error
	kafkaConf.kafkaVersion, err = sarama.ParseKafkaVersion(kafkaConf.Version)
	if err != nil {
		log.Fatalf("Fail to parse kafka version, err: %+v, version: %s", err, conf.OldKafka.Version)
	}
}

type ScKafkaMgr struct {
	cfg                 *ToolConfig
	kafkaConsumerClient MyKafkaConsumer
	newMsgSender        base.IProducer

	allMsgs  map[string]struct{}
	sendMsgs map[string]*proto.DeleteMsg
	allMsgLk sync.Mutex

	consumeCnt  int64
	sendCnt     int64
	repeatedCnt int64
	failCnt     int64
}

func NewScKafkaMgr(cfg *ToolConfig) (*ScKafkaMgr, error) {
	newMsgSender, err := base.NewMsgSender(cfg.producerConfig())
	if err != nil {
		return nil, err
	}

	mgr := newScKafkaMgr(cfg, newMsgSender, NewKafkaClient("SCHEDULER", conf.OldKafka.BrokerList, cfg.OldKafka.MaxMessageBytes))
	return mgr, nil
}

func newScKafkaMgr(cfg *ToolConfig, sender base.IProducer, consumer MyKafkaConsumer) *ScKafkaMgr {
	return &ScKafkaMgr{
		cfg:                 cfg,
		allMsgs:             make(map[string]struct{}),
		sendMsgs:            make(map[string]*proto.DeleteMsg),
		kafkaConsumerClient: consumer,
		newMsgSender:        sender,
	}
}

func (mgr *ScKafkaMgr) startConsumer() {
	_, err := mgr.kafkaConsumerClient.StartKafkaConsumer(mgr.cfg.OldKafka, mgr.Consume)
	if err != nil {
		log.Fatalf("Fail to start consume, err: %+v", err)
	}
}

func (mgr *ScKafkaMgr) Consume(msgs []*sarama.ConsumerMessage) {
	span, _ := trace.StartSpanFromContext(context.Background(), "consume")

	if mgr.needExit() {
		span.Warnf("Stopping... max consume count=%d, consumeCnt=%d", mgr.cfg.MaxCount, atomic.LoadInt64(&mgr.consumeCnt))
		time.Sleep(time.Second * 5)
		os.Exit(0)
	}

	delMsg := make([]*proto.DeleteMsg, len(msgs))
	for i := range msgs {
		err := json.Unmarshal(msgs[i].Value, &delMsg[i])
		if err != nil {
			log.Errorf("Fail to unmarshal json, err[%+v], msg.Value[%s], msg[%+v]", err, string(msgs[i].Value), msgs[i])
			return
		}
	}

	// span.Debugf("delMsg=%+v", delMsg)
	mgr.addToAllMsgs(delMsg)
}

func (mgr *ScKafkaMgr) addToAllMsgs(delMsg []*proto.DeleteMsg) {
	mgr.allMsgLk.Lock()
	defer mgr.allMsgLk.Unlock()

	key := ""
	for i := range delMsg {
		key = fmt.Sprintf("%d_%d_%d", delMsg[i].ClusterID, delMsg[i].Vid, delMsg[i].Bid)

		mgr.consumeCnt++
		_, ok := mgr.allMsgs[key]
		if ok {
			mgr.repeatedCnt++
			continue
		}

		if !ok {
			mgr.allMsgs[key] = struct{}{}
			mgr.sendMsgs[key] = delMsg[i]
			mgr.sendCnt++
		}
	}
}

func (mgr *ScKafkaMgr) loopSendToNewKafka(ctx context.Context) {
	tm := time.NewTimer(time.Millisecond * time.Duration(mgr.cfg.IntervalMs))
	defer tm.Stop()

	loop := 0
	for {
		select {
		case <-tm.C:
			mgr.sendToKafka(loop)
			tm.Reset(time.Millisecond * time.Duration(mgr.cfg.IntervalMs))
		case <-ctx.Done():
			return
		}
	}
}

func (mgr *ScKafkaMgr) getSendMsgs() (map[string]*proto.DeleteMsg, int64, int64, int64) {
	mgr.allMsgLk.Lock()
	defer mgr.allMsgLk.Unlock()

	ret := make(map[string]*proto.DeleteMsg, len(mgr.sendMsgs))
	for k, v := range mgr.sendMsgs {
		ret[k] = v
	}

	return ret, mgr.consumeCnt, mgr.repeatedCnt, mgr.sendCnt
}

func (mgr *ScKafkaMgr) cleanSendMsgs() {
	mgr.allMsgLk.Lock()
	defer mgr.allMsgLk.Unlock()

	mgr.sendMsgs = make(map[string]*proto.DeleteMsg)
}

func (mgr *ScKafkaMgr) sendToKafka(loop int) {
	span := trace.SpanFromContextSafe(context.Background())
	sendMsgs, consumeCnt, repeatedCnt, sendCnt := mgr.getSendMsgs()
	//span.Debugf("consume=%d, send=%d, repeated=%d, sendMsgs=%d", consumeCnt, sendCnt, repeatedCnt, len(sendMsgs))
	if len(sendMsgs) < mgr.cfg.Batch {
		return
	}

	var wg sync.WaitGroup
	for _, msg := range sendMsgs {
		wg.Add(1)
		go func(msg *proto.DeleteMsg) {
			defer wg.Done()

			b, err := json.Marshal(msg)
			if err != nil {
				span.Errorf("Fail to marshal kafka msg[%+v], err[%+v]", msg, err)
				atomic.AddInt64(&mgr.failCnt, 1)
				return
			}

			err = mgr.newMsgSender.SendMessage(b)
			if err != nil {
				atomic.AddInt64(&mgr.failCnt, 1)
				span.Errorf("Fail to send new kafka msg[%+v], err[%+v]", msg, err)
			}
		}(msg)
	}

	wg.Wait()
	loop++
	span.Infof("count \t %d: consume=%d, send=%d, repeated=%d, sendFail=%d", loop, consumeCnt, sendCnt, repeatedCnt, atomic.LoadInt64(&mgr.failCnt))
	mgr.cleanSendMsgs()
}

func (mgr *ScKafkaMgr) needExit() bool {
	if mgr.cfg.MaxCount != math.MaxInt64 && mgr.cfg.MaxCount != 0 && atomic.LoadInt64(&mgr.consumeCnt) > mgr.cfg.MaxCount {
		return true
	}

	return false
}

type MyKafkaConsumer interface {
	StartKafkaConsumer(kafkaConf KafkaConfig, fn func(msg []*sarama.ConsumerMessage)) (*GroupConsumer, error)
}

type KafkaCli struct {
	ModuleName      string
	Brokers         []string
	consumers       []*GroupConsumer
	maxMessageBytes int
}

func NewKafkaClient(moduleName string, brokers []string, maxMessageBytes int) MyKafkaConsumer {
	cli := &KafkaCli{
		ModuleName:      moduleName,
		Brokers:         brokers,
		maxMessageBytes: maxMessageBytes,
	}
	return cli
}

type GroupConsumer struct {
	group  string
	client sarama.ConsumerGroup
	span   trace.Span
	cancel context.CancelFunc
}

func (cli *KafkaCli) StartKafkaConsumer(kafkaConf KafkaConfig, fn func(msg []*sarama.ConsumerMessage)) (*GroupConsumer, error) {
	cfg := sarama.NewConfig()
	cfg.Version = kafkaConf.kafkaVersion
	cfg.Consumer.Offsets.Initial = sarama.OffsetOldest
	cfg.Consumer.Offsets.AutoCommit.Enable = kafkaConf.AutoCommit
	cfg.Consumer.Group.Rebalance.Retry.Max = 10

	consumer := Consumer{
		ready:     make(chan bool),
		ConsumeFn: fn,
	}
	group := fmt.Sprintf("%s-%s", "SCHEDULER", kafkaConf.Topic)
	span, ctx := trace.StartSpanFromContext(context.Background(), group)
	ctx, cancel := context.WithCancel(ctx)

	client, err := sarama.NewConsumerGroup(cli.Brokers, group, cfg)
	if err != nil {
		span.Panicf("creating consumer group client failed: err[%+v], group[%s]", err, group)
	}

	go func() {
		for {
			if err := client.Consume(ctx, []string{kafkaConf.Topic}, &consumer); err != nil {
				span.Errorf("consumer failed and try again: err[%+v]", err)
			}
			// check if context was cancelled, signaling that the consumer should stop
			if ctx.Err() != nil {
				return
			}
			consumer.ready = make(chan bool)
			span.Warnf("rebalance happens: topic[%s], consumer_group[%s]", kafkaConf.Topic, group)
		}
	}()
	groupConsumer := &GroupConsumer{
		group:  group,
		client: client,
		cancel: cancel,
		span:   span,
	}
	cli.consumers = append(cli.consumers, groupConsumer)
	return groupConsumer, nil
}

func (cli *KafkaCli) Close() {
	for _, c := range cli.consumers {
		c.span.Infof("start close kafka consumer: group[%s]", c.group)
		c.cancel()
		if err := c.client.Close(); err != nil {
			c.span.Errorf("close kafka consumer failed: err[%+v]", err)
			continue
		}
	}
}

// Consumer represents a Sarama consumer group consumer
type Consumer struct {
	ready     chan bool
	ConsumeFn func(msg []*sarama.ConsumerMessage)
}

// Setup is run at the beginning of a new session, before ConsumeClaim
func (consumer *Consumer) Setup(session sarama.ConsumerGroupSession) error {
	span := trace.SpanFromContextSafe(session.Context())
	span.Infof("consume topic and partition: [%+v]", session.Claims())
	// Mark the consumer as ready
	close(consumer.ready)
	return nil
}

// Cleanup is run at the end of a session, once all ConsumeClaim goroutines have exited
func (consumer *Consumer) Cleanup(sarama.ConsumerGroupSession) error {
	return nil
}

// ConsumeClaim must start a consumer loop of ConsumerGroupClaim's Messages().
func (consumer *Consumer) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	// NOTE:
	// Do not move the code below to a goroutine.
	// The `ConsumeClaim` itself is called within a goroutine, see:
	// https://github.com/Shopify/sarama/blob/main/consumer_group.go#L27-L29
	span := trace.SpanFromContextSafe(session.Context())
	msgs := make([]*sarama.ConsumerMessage, 0, maxBatch)
	tm := time.NewTimer(time.Millisecond * time.Duration(cIntervalMs))
	defer tm.Stop()

	for {
		select {
		case message := <-claim.Messages():
			if message == nil {
				span.Warnf("no message for consume and continue")
				continue
			}
			log.Debugf("Message claimed: value[%s], timestamp[%v], topic[%s], partition[%d], offset[%d]", string(message.Value), message.Timestamp, message.Topic, message.Partition, message.Offset)
			msgs = append(msgs, message)
			if len(msgs) < maxBatch {
				continue
			}

		case <-tm.C:
			if len(msgs) == 0 {
				continue
			}

		// Should return when `session.Context()` is done.
		// If not, will raise `ErrRebalanceInProgress` or `read tcp <ip>:<port>: i/o timeout` when kafka rebalance. see:
		// https://github.com/Shopify/sarama/issues/1192
		case <-session.Context().Done():
			return nil
		}

		consumer.ConsumeFn(msgs)
		session.MarkMessage(msgs[len(msgs)-1], "")
		// session.Commit()

		msgs = msgs[:0]
		tm.Reset(time.Millisecond * time.Duration(100))
	}
}
