package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"time"

	"github.com/Shopify/sarama"

	"github.com/cubefs/cubefs/blobstore/common/config"
)

var (
	conf         ToolConfig
	kafkaVersion = sarama.V2_1_0_0
	confPath     = flag.String("f", "kafka_tool.conf", "kafka config path")
)

type ToolConfig struct {
	ClusterID uint32      `json:"cluster_id"`
	Kafka     KafkaConfig `json:"kafka"`
	//ClusterMgr cmapi.Config          `json:"cluster_mgr"`
}

type KafkaConfig struct {
	MaxMessageBytes int      `json:"max_message_bytes"`
	BrokerList      []string `json:"broker_list"`
	Topic           []string `json:"topic"`
	//TopicNormal string   `json:"topic_normal"`
	//TopicFailed string   `json:"topic_failed"`
	//FailMsgSenderTimeoutMs int64    `json:"fail_msg_sender_timeout_ms"`
	//CommitIntervalMs       int      `json:"commit_interval_ms"`
}

func defaultKafkaCfg() *sarama.Config {
	cfg := sarama.NewConfig()
	cfg.Version = kafkaVersion
	cfg.Consumer.Return.Errors = true
	cfg.Producer.Return.Successes = true
	cfg.Producer.RequiredAcks = sarama.WaitForAll
	cfg.Producer.Compression = sarama.CompressionSnappy
	return cfg
}

func main() {
	conf = ToolConfig{
		ClusterID: 1,
		Kafka: KafkaConfig{
			BrokerList: []string{"11"},
			Topic:      []string{"22"},
		},
	}
	data, err := json.Marshal(&conf)
	fmt.Println(err, string(data))

	data2 := []byte(`
{
	"cluster_id": 1,
	"kafka": {
		"broker_list": [      
			"10.84.28.170:9095",      
			"10.84.28.171:9095",      
			"10.84.28.172:9095"
		],
	# "shard_repair",
	# "shard_repair_prior",
	# "shard_repair_failed",
		"topic": [
			"blob_delete",        
			"blob_delete_failed"    
		]
	}	
}
`)
	//# "shard_repair",
	//# "shard_repair_prior",
	//# "shard_repair_failed",
	json.Unmarshal(data2, &conf)
	fmt.Println(conf)

	flag.Parse()
	confBytes, err := ioutil.ReadFile(*confPath)
	if err != nil {
		//panic(err)
		log.Fatalf("read config file failed, filename: %s, err: %v", *confPath, err)
	}

	log.Printf("Config file %s:\n%s", *confPath, confBytes)
	//if err = LoadData(&conf, confBytes); err != nil {
	if err = config.LoadData(&conf, confBytes); err != nil {
		log.Fatalf("load config failed, error: %+v", err)
	}
	log.Printf("Config: %+v", conf)

	consumer, err := sarama.NewConsumer(conf.Kafka.BrokerList, defaultKafkaCfg())
	if err != nil {
		panic(err)
	}
	client, err := sarama.NewClient(conf.Kafka.BrokerList, nil)
	if err != nil {
		panic(err)
	}

	for _, topic := range conf.Kafka.Topic {
		partitions, err := consumer.Partitions(topic)
		if err != nil {
			panic(err)
		}

		for _, pid := range partitions {
			oldestOffset, err1 := client.GetOffset(topic, pid, sarama.OffsetOldest)
			consumeOffset, err2 := client.GetOffset(topic, pid, time.Now().UnixMilli())
			newestOffset, err3 := client.GetOffset(topic, pid, sarama.OffsetNewest)
			log.Printf("topic=%s, partition=%d, offset=[%d, %d, %d], errs=[%+v, %+v, %+v] \n",
				topic, pid, oldestOffset, consumeOffset, newestOffset, err1, err2, err3)
		}
	}
}