package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"syscall"
	//"time"

	"github.com/Shopify/sarama"
	"github.com/cubefs/cubefs/blobstore/common/config"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/common/trace"
	//"github.com/cubefs/cubefs/blobstore/scheduler/base"
	//"github.com/cubefs/cubefs/blobstore/scheduler/client"
	"github.com/cubefs/cubefs/blobstore/util/log"
)

var (
	conf     BlobDeleteConfig
	confFile = flag.String("f", "getKafka.conf", "config filename")
)

type BlobDeleteKafkaConfig struct {
	BrokerList  []string `json:"broker_list"`
	Topic       string   `json:"topic"`
	TopicNormal string   `json:"topic_normal"`
	TopicFailed string   `json:"topic_failed"`
	//FailMsgSenderTimeoutMs int64    `json:"fail_msg_sender_timeout_ms"`
	//CommitIntervalMs       int      `json:"commit_interval_ms"`
	MaxMessageBytes int `json:"max_message_bytes"`
}

type BlobDeleteConfig struct {
	ClusterID uint32                `json:"cluster_id"`
	LogLevel  log.Level             `json:"log_level"`
	Kafka     BlobDeleteKafkaConfig `json:"kafka"`
	//ClusterMgr cmapi.Config          `json:"cluster_mgr"`
}

func (cfg *BlobDeleteConfig) topics() []string {
	return []string{cfg.Kafka.TopicNormal, cfg.Kafka.TopicFailed}
}

type BlobDeleteMgr struct {
	cfg *BlobDeleteConfig
	//kafkaConsumerClient base.KafkaConsumer
	kafkaConsumerClient *KafkaCli
}

// NewBlobDeleteMgr returns blob delete manager
func NewBlobDeleteMgr(cfg *BlobDeleteConfig) (*BlobDeleteMgr, error) {
	mgr := &BlobDeleteMgr{
		cfg: cfg,
	}
	//clusterMgrCli := client.NewClusterMgrClient(&conf.ClusterMgr)
	//kafkaClient := base.NewKafkaConsumer(conf.Kafka.BrokerList, time.Duration(conf.Kafka.CommitIntervalMs)*time.Millisecond, clusterMgrCli)
	kafkaClient := NewKafkaClient("SCHEDULER", conf.Kafka.BrokerList, cfg.Kafka.MaxMessageBytes)
	mgr.kafkaConsumerClient = kafkaClient

	return mgr, nil
}

func (mgr *BlobDeleteMgr) startConsumer() error {
	//for _, topic := range mgr.cfg.topics() {
	mgr.kafkaConsumerClient.StartKafkaConsumer(mgr.cfg.Kafka.Topic, mgr.Consume)
	//_, err := mgr.kafkaConsumerClient.StartKafkaConsumer(mgr.cfg.Kafka.Topic, topic, mgr.Consume)
	//if err != nil {
	//	return err
	//}
	//mgr.consumers = append(mgr.consumers, consumer)
	//}
	return nil
}

//func (mgr *BlobDeleteMgr) Consume(msg *sarama.ConsumerMessage, consumerPause base.ConsumerPause) (consumed bool) {
func (mgr *BlobDeleteMgr) Consume(msg *sarama.ConsumerMessage) {
	var delMsg *proto.DeleteMsg
	err := json.Unmarshal(msg.Value, &delMsg)
	if err != nil {
		log.Errorf("json err[%+v]", err)
	}

	data, err := json.Marshal(delMsg)
	log.Infof("start json delete msg: %s", string(data))
	//log.Infof("start struct delete msg: [%+v]", delMsg)
	//return true
}

func main() {
	//path := "/home/oppo/code/cubefs/blobstore/tools/scheduler/msgKafka.log"
	//ReadLinesV2(path)
	//parseFile()

	flag.Parse()
	//*confFile = "/home/oppo/code/cubefs/blobstore/tools/scheduler/getKafka.conf"
	confBytes, err := ioutil.ReadFile(*confFile)
	if err != nil {
		log.Fatalf("read config file failed, filename: %s, err: %v", *confFile, err)
	}

	log.Infof("Config file %s:\n%s", *confFile, confBytes)
	if err = config.LoadData(&conf, confBytes); err != nil {
		log.Fatalf("load config failed, error: %+v", err)
	}
	log.SetOutputLevel(conf.LogLevel)
	log.Infof("Config: %+v", conf)

	mgr, err := NewBlobDeleteMgr(&conf)
	if err != nil {
		log.Fatalf("NewBlobDeleteMgr failed, error: %+v", err)
	}

	mgr.startConsumer()

	// wait for signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	sig := <-ch
	log.Infof("receive signal: %s, stop service...", sig.String())
	mgr.kafkaConsumerClient.Close()
	log.Info("stop...")
}

type KafkaCli struct {
	ModuleName      string
	Brokers         []string
	consumers       []*KafkaConsumer
	maxMessageBytes int
}

func NewKafkaClient(moduleName string, brokers []string, maxMessageBytes int) *KafkaCli {
	cli := &KafkaCli{
		ModuleName:      moduleName,
		Brokers:         brokers,
		maxMessageBytes: maxMessageBytes,
	}
	return cli
}

type KafkaConsumer struct {
	group  string
	client sarama.ConsumerGroup
	span   trace.Span
	cancel context.CancelFunc
}

func (cli *KafkaCli) StartKafkaConsumer(topic string, fn func(msg *sarama.ConsumerMessage)) {
	/**
	 * Construct a new Sarama configuration.
	 * The Kafka cluster version has to be defined before the consumer/producer is initialized.
	 */
	var VERSION = sarama.V2_1_0_0
	config := sarama.NewConfig()
	config.Version = VERSION
	config.Consumer.Offsets.Initial = sarama.OffsetOldest
	config.Consumer.Offsets.AutoCommit.Enable = false
	config.Consumer.Group.Rebalance.Retry.Max = 10

	/**
	 * Setup a new Sarama consumer group
	 */
	consumer := Consumer{
		ready:     make(chan bool),
		ConsumeFn: fn,
	}
	group := fmt.Sprintf("%s-%s", "SCHEDULER", topic)
	//group := fmt.Sprintf("%s-%s-%s", cli.ModuleName, groupName, topic)

	span, ctx := trace.StartSpanFromContext(context.Background(), group)
	ctx, cancel := context.WithCancel(ctx)

	client, err := sarama.NewConsumerGroup(cli.Brokers, group, config)
	if err != nil {
		span.Panicf("creating consumer group client failed: err[%+v]", err)
	}

	go func() {
		for {
			// `Consume` should be called inside an infinite loop, when a
			// server-side rebalance happens, the consumer session will need to be
			// recreated to get the new claims
			if err := client.Consume(ctx, []string{topic}, &consumer); err != nil {
				span.Errorf("consumer failed and try again: err[%+v]", err)
			}
			// check if context was cancelled, signaling that the consumer should stop
			if ctx.Err() != nil {
				return
			}
			consumer.ready = make(chan bool)
			span.Warnf("rebalance happens: topic[%s], consumer_group[%s]", topic, group)
		}
	}()
	groupConsumer := &KafkaConsumer{
		group:  group,
		client: client,
		cancel: cancel,
		span:   span,
	}
	cli.consumers = append(cli.consumers, groupConsumer)
	return
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
	ConsumeFn func(msg *sarama.ConsumerMessage)
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
	for {
		select {
		case message := <-claim.Messages():
			if message == nil {
				span.Warnf("no message for consume and continue")
				continue
			}
			log.Debugf("Message claimed: value[%s], timestamp[%v], topic[%s], partition[%d], offset[%d]", string(message.Value), message.Timestamp, message.Topic, message.Partition, message.Offset)
			consumer.ConsumeFn(message)
			session.MarkMessage(message, "")

		// Should return when `session.Context()` is done.
		// If not, will raise `ErrRebalanceInProgress` or `read tcp <ip>:<port>: i/o timeout` when kafka rebalance. see:
		// https://github.com/Shopify/sarama/issues/1192
		case <-session.Context().Done():
			return nil
		}
	}
}
