// Package memory is each agent's short-term working memory in Redis:
// a capped list of recent observations plus a small hash of current state.
//
//	agent:<id>:obs    LIST  newest first, trimmed to MaxObservations
//	agent:<id>:state  HASH  order, goal
//
// Inspect it live with: redis-cli LRANGE agent:miner:obs 0 -1
package memory

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

const MaxObservations = 15

type Store struct{ rdb *redis.Client }

func New(rdb *redis.Client) *Store { return &Store{rdb: rdb} }

func obsKey(agent string) string   { return "agent:" + agent + ":obs" }
func stateKey(agent string) string { return "agent:" + agent + ":state" }

// AddObservation pushes a line and trims the list in one round trip.
func (s *Store) AddObservation(ctx context.Context, agent string, tick int64, text string) error {
	pipe := s.rdb.TxPipeline()
	pipe.LPush(ctx, obsKey(agent), fmt.Sprintf("[t=%d] %s", tick, text))
	pipe.LTrim(ctx, obsKey(agent), 0, MaxObservations-1)
	_, err := pipe.Exec(ctx)
	return err
}

// Recent returns up to n observations, oldest first (the order a prompt should read them).
func (s *Store) Recent(ctx context.Context, agent string, n int) ([]string, error) {
	items, err := s.rdb.LRange(ctx, obsKey(agent), 0, int64(n-1)).Result()
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items, nil
}

const MaxChatTurns = 12

func chatKey(agent string) string { return "agent:" + agent + ":chat" }

// AddChat appends one line of the running conversation with the Owner.
func (s *Store) AddChat(ctx context.Context, agent, speaker, text string) error {
	pipe := s.rdb.TxPipeline()
	pipe.RPush(ctx, chatKey(agent), speaker+": "+text)
	pipe.LTrim(ctx, chatKey(agent), -MaxChatTurns, -1)
	_, err := pipe.Exec(ctx)
	return err
}

// Chat returns the conversation so far, oldest first.
func (s *Store) Chat(ctx context.Context, agent string) []string {
	items, err := s.rdb.LRange(ctx, chatKey(agent), 0, -1).Result()
	if err != nil {
		return nil
	}
	return items
}

func (s *Store) SetField(ctx context.Context, agent, field, value string) error {
	return s.rdb.HSet(ctx, stateKey(agent), field, value).Err()
}

func (s *Store) GetField(ctx context.Context, agent, field string) string {
	v, err := s.rdb.HGet(ctx, stateKey(agent), field).Result()
	if err != nil {
		return ""
	}
	return v
}

func (s *Store) ClearField(ctx context.Context, agent, field string) error {
	return s.rdb.HDel(ctx, stateKey(agent), field).Err()
}
