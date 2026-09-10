package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/segmentio/kafka-go"
)

// A KafkaSource reads authorizations from one topic as part of a consumer
// group. Offsets are committed explicitly rather than on an interval, because
// the ordering contract in this package's doc comment requires the commit to
// happen after the produce, and an automatic commit cannot promise that.
type KafkaSource struct {
	reader *kafka.Reader
}

// KafkaConfig is the broker and topic layout.
type KafkaConfig struct {
	Brokers  []string
	Group    string
	InTopic  string
	Approved string
	Review   string
}

func OpenKafkaSource(config KafkaConfig) *KafkaSource {
	return &KafkaSource{reader: kafka.NewReader(kafka.ReaderConfig{
		Brokers: config.Brokers,
		GroupID: config.Group,
		Topic:   config.InTopic,
		// The pipeline commits; the reader must not do it behind our back.
		CommitInterval: 0,
		// A new group starts at the beginning of the topic rather than at the
		// end, so a pipeline started after its producer still sees the backlog
		// instead of silently skipping it.
		StartOffset: kafka.FirstOffset,
	})}
}

func (s *KafkaSource) Read(ctx context.Context) (Message, error) {
	// FetchMessage rather than ReadMessage: ReadMessage commits as it reads,
	// which would break the produce-then-commit ordering.
	raw, err := s.reader.FetchMessage(ctx)
	if err != nil {
		return Message{}, fmt.Errorf("reading from kafka: %w", err)
	}
	msg := Message{Raw: raw.Value, offset: raw}
	// A parse failure is not an error here. The pipeline decides what to do
	// with a message it cannot read, and it needs the message to do it.
	_ = json.Unmarshal(raw.Value, &msg.Authorization)
	return msg, nil
}

func (s *KafkaSource) Commit(ctx context.Context, msg Message) error {
	raw, ok := msg.offset.(kafka.Message)
	if !ok {
		return fmt.Errorf("committing: message did not come from this source")
	}
	if err := s.reader.CommitMessages(ctx, raw); err != nil {
		return fmt.Errorf("committing offset %d: %w", raw.Offset, err)
	}
	return nil
}

func (s *KafkaSource) Close() error { return s.reader.Close() }

// A KafkaSink writes routed authorizations. One writer per topic, each with
// RequiredAcks set to all: a routed decision that only reached one broker is a
// decision that can vanish, and the whole point of the produce-then-commit
// ordering is that it cannot.
type KafkaSink struct {
	brokers []string

	mu      sync.Mutex
	writers map[string]*kafka.Writer
}

func OpenKafkaSink(brokers []string) *KafkaSink {
	return &KafkaSink{brokers: brokers, writers: map[string]*kafka.Writer{}}
}

func (s *KafkaSink) Write(ctx context.Context, topic, key string, value []byte) error {
	writer := s.writerFor(topic)
	if err := writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(key), // the card, so one card's payments keep their order
		Value: value,
	}); err != nil {
		return fmt.Errorf("writing to %s: %w", topic, err)
	}
	return nil
}

func (s *KafkaSink) writerFor(topic string) *kafka.Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if writer, ok := s.writers[topic]; ok {
		return writer
	}
	writer := &kafka.Writer{
		Addr:                   kafka.TCP(s.brokers...),
		Topic:                  topic,
		Balancer:               &kafka.Hash{}, // key -> partition, so order holds per card
		RequiredAcks:           kafka.RequireAll,
		AllowAutoTopicCreation: true,
	}
	s.writers[topic] = writer
	return writer
}

func (s *KafkaSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for topic, writer := range s.writers {
		if err := writer.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("closing writer for %s: %w", topic, err)
		}
	}
	return firstErr
}

// EnsureTopics creates the topics if they are absent.
//
// Relying on auto-creation instead loses the first produce to an unknown-topic
// error, because creation is asynchronous and the metadata has not propagated
// by the time the write lands. It also leaves the partition count to the
// broker's default. Creating them explicitly makes both the existence and the
// layout a decision rather than an accident.
//
// One partition per topic, deliberately: the sink keys on the card so one
// card's payments keep their order, and with a single partition that ordering
// is total rather than per-key. A real deployment raises this and relies on the
// key; the guarantee the pipeline needs is the same either way.
func EnsureTopics(brokers []string, topics ...string) error {
	if len(brokers) == 0 {
		return fmt.Errorf("no brokers configured")
	}
	conn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		return fmt.Errorf("dialling %s: %w", brokers[0], err)
	}
	defer conn.Close()

	controller, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("finding the controller: %w", err)
	}
	controllerConn, err := kafka.Dial("tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		return fmt.Errorf("dialling the controller: %w", err)
	}
	defer controllerConn.Close()

	configs := make([]kafka.TopicConfig, 0, len(topics))
	for _, topic := range topics {
		configs = append(configs, kafka.TopicConfig{
			Topic:             topic,
			NumPartitions:     1,
			ReplicationFactor: 1,
		})
	}
	// Creating a topic that already exists is not an error worth surfacing.
	if err := controllerConn.CreateTopics(configs...); err != nil {
		return fmt.Errorf("creating topics %v: %w", topics, err)
	}
	return nil
}
