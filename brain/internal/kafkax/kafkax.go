// Package kafkax holds the Kafka settings shared by the brain and the gateway.
package kafkax

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

func Brokers(csv string) []string { return strings.Split(csv, ",") }

// NewWriter returns a producer that hashes the message key to pick a partition,
// so every message for one agent lands on the same partition, in order.
func NewWriter(brokers []string) *kafka.Writer {
	return &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Balancer:               &kafka.Hash{},
		BatchTimeout:           10 * time.Millisecond,
		RequiredAcks:           kafka.RequireOne,
		AllowAutoTopicCreation: false,
	}
}

// NewGroupReader joins a consumer group. New groups start at the latest offset,
// so a restarted service doesn't replay an hour of old events.
func NewGroupReader(brokers []string, topic, group string) *kafka.Reader {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:        brokers,
		Topic:          topic,
		GroupID:        group,
		StartOffset:    kafka.LastOffset,
		MinBytes:       1,
		MaxBytes:       1 << 20,
		MaxWait:        250 * time.Millisecond,
		CommitInterval: time.Second, // async commits, batched once per second
	})
}

// FollowLatest tails partition 0 of a single-partition topic from the newest
// message, without a consumer group. Used for world.snapshots, where only the
// latest value matters and nobody needs committed offsets.
func FollowLatest(ctx context.Context, brokers []string, topic string, fn func([]byte)) {
	for ctx.Err() == nil {
		r := kafka.NewReader(kafka.ReaderConfig{
			Brokers: brokers, Topic: topic, Partition: 0,
			MinBytes: 1, MaxBytes: 1 << 20, MaxWait: 250 * time.Millisecond,
		})
		if err := r.SetOffset(kafka.LastOffset); err == nil {
			for {
				m, err := r.ReadMessage(ctx)
				if err != nil {
					break
				}
				fn(m.Value)
			}
		}
		r.Close()
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second): // broker not ready yet: retry
		}
	}
}

// PublishJSON marshals v and writes it with the given key.
func PublishJSON(ctx context.Context, w *kafka.Writer, topic, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.WriteMessages(ctx, kafka.Message{Topic: topic, Key: []byte(key), Value: b})
}
