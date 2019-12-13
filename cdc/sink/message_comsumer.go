package sink

import (
	"context"
	"github.com/Shopify/sarama"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"go.uber.org/zap"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

type MessageConsumer struct {
	topic  string
	client sarama.ConsumerGroup
	sink   Sink

	cdcResolveTsMap     map[string][]*ResolveMsgWrapper
	partitionMessageMap map[int32][]*MessageWrapper

	lock       sync.Mutex
	metaGroup  *sync.WaitGroup
	cleanGroup *sync.WaitGroup
	cdcCount   int
}

type MessageWrapper struct {
	partition int32
	offset    int64
	message   *Message
}

type ResolveMsgWrapper struct {
	ResolveTs uint64
	partition int32
	offset    int64
}

func NewMessageConsumer(sink Sink, kafkaVersion, kafkaAddr, kafkaTopic string) (*MessageConsumer, error) {
	config, err := newSaramaConfig(kafkaVersion)
	if err != nil {
		return nil, errors.Trace(err)
	}

	config.Metadata.Retry.Max = 10000
	config.Metadata.Retry.Backoff = 500 * time.Millisecond

	config.Consumer.Offsets.Initial = sarama.OffsetOldest
	config.Consumer.Retry.Backoff = 500 * time.Millisecond

	consumer, err := sarama.NewConsumerGroup(strings.Split(kafkaAddr, ","), "", config)
	if err != nil {
		return nil, err
	}

	return &MessageConsumer{
		client: consumer,
		topic:  kafkaTopic,
		sink:   sink,
	}, nil

}

// Setup is run at the beginning of a new session, before ConsumeClaim.
func (consumer *MessageConsumer) Start(ctx context.Context) {
	go func() {
		for {
			if err := consumer.client.Consume(ctx, strings.Split(consumer.topic, ","), consumer); err != nil {
				log.Error("Error from consumer", zap.Error(err))
			}
			// check if context was cancelled, signaling that the consumer should stop
			if ctx.Err() != nil {
				return
			}
		}
	}()
}

func (consumer *MessageConsumer) Setup(session sarama.ConsumerGroupSession) error {
	return nil
}

func (consumer *MessageConsumer) Cleanup(sarama.ConsumerGroupSession) error {
	return nil
}

func decode(message *sarama.ConsumerMessage) *Message {
	return nil
}

func (consumer *MessageConsumer) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {

	for message := range claim.Messages() {
		msg := decode(message)
		switch msg.MsgType {
		case TxnType:
			consumer.processTxnMsg(message, msg)
		case ResolveTsType:
			consumer.processResolveRSMsg(message, msg)
		case MetaType: //cdc is added or deleted
			consumer.processMetaMsg(session, msg)()
		}
	}
	return nil
}

func (consumer *MessageConsumer) processTxnMsg(kafkaMessage *sarama.ConsumerMessage, msg *Message) {
	consumer.lock.Lock()
	defer consumer.lock.Unlock()

	wrapper := &MessageWrapper{message: msg, partition: kafkaMessage.Partition, offset: kafkaMessage.Offset}
	messages, _ := consumer.partitionMessageMap[wrapper.partition]
	messages = append(messages, wrapper)
	consumer.partitionMessageMap[wrapper.partition] = messages
}
func (consumer *MessageConsumer) processResolveRSMsg(kafkaMessage *sarama.ConsumerMessage, msg *Message) {
	consumer.lock.Lock()
	defer consumer.lock.Unlock()

	wrapper := &ResolveMsgWrapper{ResolveTs: msg.ResloveTs, partition: kafkaMessage.Partition, offset: kafkaMessage.Offset}
	messages, _ := consumer.cdcResolveTsMap[msg.CdcID]
	messages = append(messages, wrapper)
	consumer.cdcResolveTsMap[msg.CdcID] = messages

	//add to partition cache too
	wrapper2 := &MessageWrapper{message: msg, partition: kafkaMessage.Partition, offset: kafkaMessage.Offset}
	messages2, _ := consumer.partitionMessageMap[wrapper2.partition]
	messages2 = append(messages2, wrapper2)
	consumer.partitionMessageMap[wrapper.partition] = messages2
}

func (consumer *MessageConsumer) processMetaMsg(session sarama.ConsumerGroupSession, msg *Message) func() {
	consumer.lock.Lock()
	defer consumer.lock.Unlock()

	if consumer.metaGroup == nil {
		consumer.metaGroup = &sync.WaitGroup{}
		consumer.metaGroup.Add(msg.MetaCount - 1)
		consumer.cleanGroup.Add(1)
		return func() {
			defer consumer.cleanGroup.Done()

			consumer.metaGroup.Wait()
			consumer.tryPersistent(session)

			//after this time the cdc node count is changed
			consumer.cdcCount = len(msg.CdcList)
			existsMap := map[string]bool{}
			for _, cdcName := range msg.CdcList {
				existsMap[cdcName] = true
			}
			for cdcName, _ := range consumer.cdcResolveTsMap {
				if !existsMap[cdcName] {
					//cdc is deleted
					delete(consumer.cdcResolveTsMap, cdcName)
				}
			}
			consumer.metaGroup = nil
		}
	}
	return func() {
		consumer.metaGroup.Done()
		consumer.cleanGroup.Wait()
	}
}

func (consumer *MessageConsumer) tryPersistent(session sarama.ConsumerGroupSession) {
	consumer.lock.Lock()
	defer consumer.lock.Unlock()

	for {
		//check if we received all RS from all cdc node
		if consumer.cdcCount > 0 && consumer.cdcCount <= len(consumer.cdcResolveTsMap) {
			minRS, minRsCdcName, skip := consumer.findMinRs()
			if skip { //no enough rs data
				return
			}

			txnMap := consumer.getTxnMap(minRS)
			//empty rs interval
			if len(txnMap) <= 0 {
				//delete saved rs
				consumer.cdcResolveTsMap[minRsCdcName] = consumer.cdcResolveTsMap[minRsCdcName][1:]
				continue
			}
			offsetMap := consumer.calCommitOffset(minRS)

			//sort and save to MySQL
			list := consumer.saveMessage2Sink(txnMap, minRS)
			//commit kafka offset
			consumer.commitKafkaOffset(offsetMap, session)
			//delete saved rs
			consumer.cdcResolveTsMap[minRsCdcName] = consumer.cdcResolveTsMap[minRsCdcName][1:]
			//delete saved messages
			consumer.deleteSaveKafkaMessage(minRS, list[list.Len()-1].ts)
		}
	}
}

func (consumer *MessageConsumer) calCommitOffset(minRS uint64) map[int32]int64 {
	offsetMap := map[int32]int64{}
	for partition, messages := range consumer.partitionMessageMap {
		for _, msg := range messages {
			if msg.message.MsgType == ResolveTsType && msg.message.ResloveTs <= minRS ||
				msg.message.MsgType == TxnType && msg.message.Txn.Ts <= minRS {
				offsetMap[partition] = msg.offset
			}
		}
	}
	return offsetMap
}

func (consumer *MessageConsumer) getTxnMap(minRS uint64) map[uint64][]*Message {
	txnMap := map[uint64][]*Message{}
	for _, messages := range consumer.partitionMessageMap {
		for _, msg := range messages {
			if msg.message.MsgType == TxnType {
				if msg.message.Txn.Ts <= minRS {
					txnMessages := txnMap[msg.message.Txn.Ts]
					txnMessages = append(txnMessages, msg.message)
					txnMap[msg.message.Txn.Ts] = txnMessages
				}
			}
		}
	}
	return txnMap
}

func (consumer *MessageConsumer) findMinRs() (uint64, string, bool) {
	minRS := uint64(math.MaxUint64)
	minRsCdcName := ""
	for cdcName, messages := range consumer.cdcResolveTsMap {
		if len(messages) <= 0 { //has no rs, we can not calculate the min rs, skip
			return 0, "", true
		}
		if messages[0].ResolveTs < minRS {
			minRS = messages[0].ResolveTs
			minRsCdcName = cdcName
		}
	}
	return minRS, minRsCdcName, false
}

func (consumer *MessageConsumer) saveMessage2Sink(txnMap map[uint64][]*Message, minRS uint64) TxnSlice {
	list := TxnSlice{}
	for key, v := range txnMap {
		list = append(list, Txn{ts: key, msgs: v})
	}
	sort.Sort(list)
	for _, item := range list {
		//save to sink
		for _, txn := range item.msgs {
			//todo: error handle
			if err := consumer.sink.Emit(context.Background(), *txn.Txn); err != nil {
				log.Fatal("save to sink failed", zap.Error(err))
			}
		}
	}
	if err := consumer.sink.EmitResolvedTimestamp(context.Background(), minRS); err != nil {
		log.Fatal("save to sink failed", zap.Error(err))
	}
	return list
}

func (consumer *MessageConsumer) commitKafkaOffset(offsetMap map[int32]int64, session sarama.ConsumerGroupSession) {
	for partition, offset := range offsetMap {
		session.MarkOffset(consumer.topic, partition, offset, "")
	}
}

func (consumer *MessageConsumer) deleteSaveKafkaMessage(minRS uint64, maxSavedTs uint64) {
	for partition, list := range consumer.partitionMessageMap {
		n := 0
		for _, item := range list {
			if (item.message.MsgType == ResolveTsType && item.message.ResloveTs <= minRS) || item.message.Txn.Ts > maxSavedTs {
				list[n] = item
				n++
			}
		}
		consumer.partitionMessageMap[partition] = list[:n]
	}
}

type TxnSlice []Txn

type Txn struct {
	ts   uint64
	msgs []*Message
}

func (t TxnSlice) Len() int {
	return len(t)
}

func (t TxnSlice) Less(i int, j int) bool {
	return t[i].ts < t[j].ts
}

func (t TxnSlice) Swap(i int, j int) {
	t[i], t[j] = t[j], t[i]
}
