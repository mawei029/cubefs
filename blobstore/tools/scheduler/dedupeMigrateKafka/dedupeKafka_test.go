package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/blobstore/common/proto"
)

//go:generate mockgen -destination=./base_mock_test.go -package=main -mock_names KafkaConsumer=MockKafkaConsumer,GroupConsumer=MockGroupConsumer,IProducer=MockProducer github.com/cubefs/cubefs/blobstore/scheduler/base KafkaConsumer,GroupConsumer,IProducer

var (
	any     = gomock.Any()
	errMock = errors.New("fake error")
)

func TestDedupeKafkaConsume(t *testing.T) {
	//conf = ToolConfig{
	//	Batch:    3,
	//	Interval: 1,
	//	NewKafka: KafkaConfig{
	//		BrokerList: []string{"192.168.0.12:9095"},
	//		Topic:      "test_del",
	//		TimeoutMs:  100,
	//	},
	//}
	//
	//mgr, err := NewScKafkaMgr(&conf)
	//require.Nil(t, err)

	mgr := &ScKafkaMgr{
		cfg:      &ToolConfig{Batch: 3},
		allMsgs:  make(map[string]struct{}),
		sendMsgs: make(map[string]*proto.DeleteMsg),
	}

	delMsg := proto.DeleteMsg{
		ClusterID: 1,
		Vid:       10,
		Bid:       100,
	}
	kafkaMsgBt, err := json.Marshal(delMsg)
	require.Nil(t, err)

	msg := &sarama.ConsumerMessage{Value: kafkaMsgBt}
	mgr.Consume(msg)

	bid := delMsg.Bid
	for i := 0; i < 100; i++ {
		delMsg.Bid = bid + proto.BlobID(i)
		kafkaMsgBt, _ = json.Marshal(delMsg)
		msg = &sarama.ConsumerMessage{Value: kafkaMsgBt}
		mgr.Consume(msg)
	}

	require.Equal(t, int64(101), mgr.consumeCnt)
	require.Equal(t, int64(1), mgr.repeatedCnt)
	require.Equal(t, int64(100), mgr.sendCnt)
	require.Equal(t, 100, len(mgr.allMsgs))
	require.Equal(t, 100, len(mgr.sendMsgs))

	ctr := gomock.NewController(t)
	producer := NewMockProducer(ctr)
	producer.EXPECT().SendMessage(any).Times(len(mgr.sendMsgs)).Return(nil)
	mgr.newMsgSender = producer

	mgr.cfg.Interval = 1
	ctx, cancle := context.WithTimeout(context.Background(), time.Second*time.Duration(mgr.cfg.Interval*2))
	go mgr.loopSendToNewKafka(ctx)

	time.Sleep(time.Second * time.Duration(mgr.cfg.Interval*2))
	cancle()
	require.Equal(t, 0, len(mgr.sendMsgs))
}
