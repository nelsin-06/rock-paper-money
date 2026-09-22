package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	redis "github.com/redis/go-redis/v9"
)

var ErrInvalidRevisionHint = errors.New("invalid room revision hint")

type RevisionHint struct {
	RoomID   string `json:"room_id"`
	Revision uint64 `json:"revision"`
}

func RoomChannel(roomID string) string { return "room:" + roomID }

func EncodeRevisionHint(hint RevisionHint) ([]byte, error) {
	if hint.RoomID == "" || hint.Revision == 0 {
		return nil, ErrInvalidRevisionHint
	}
	return json.Marshal(hint)
}

func DecodeRevisionHint(channel string, payload []byte) (RevisionHint, error) {
	var hint RevisionHint
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&hint); err != nil || hint.RoomID == "" || hint.Revision == 0 || channel != RoomChannel(hint.RoomID) {
		return RevisionHint{}, ErrInvalidRevisionHint
	}
	return hint, nil
}

type SubscriptionEvent struct {
	Hint        *RevisionHint
	Reconnected bool
}

type RoomSubscription interface{ Close() error }

type RoomSubscriber interface {
	SubscribeRoom(context.Context, string, func(SubscriptionEvent)) (RoomSubscription, error)
}

type RedisTransport struct {
	client         redis.UniversalClient
	retryDelay     time.Duration
	healthInterval time.Duration
	observer       RedisObserver
}

type RedisObserver interface {
	RedisReconnect()
	RedisFailure()
}

func NewRedisTransport(client redis.UniversalClient, retryDelay time.Duration) *RedisTransport {
	if retryDelay <= 0 {
		retryDelay = time.Second
	}
	return &RedisTransport{client: client, retryDelay: retryDelay, healthInterval: time.Second}
}

func (r *RedisTransport) SetObserver(observer RedisObserver) { r.observer = observer }

func (r *RedisTransport) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

// CheckReady confirms both command connectivity and Pub/Sub subscription
// capability without retaining a room subscription.
func (r *RedisTransport) CheckReady(ctx context.Context) error {
	if err := r.Ping(ctx); err != nil {
		return err
	}
	probe := r.client.Subscribe(ctx, "health:realtime")
	defer probe.Close()
	_, err := probe.Receive(ctx)
	return err
}

func (r *RedisTransport) PublishRevision(ctx context.Context, roomID string, revision uint64) error {
	payload, err := EncodeRevisionHint(RevisionHint{RoomID: roomID, Revision: revision})
	if err != nil {
		return err
	}
	return r.client.Publish(ctx, RoomChannel(roomID), payload).Err()
}

func (r *RedisTransport) SubscribeRoom(ctx context.Context, roomID string, handler func(SubscriptionEvent)) (RoomSubscription, error) {
	if roomID == "" || handler == nil {
		return nil, ErrInvalidRevisionHint
	}
	subscriptionContext, cancel := context.WithCancel(context.Background())
	subscription := &redisRoomSubscription{cancel: cancel, done: make(chan struct{})}
	confirmed := make(chan error, 1)
	go r.runSubscription(subscriptionContext, subscription, roomID, handler, confirmed)
	select {
	case err := <-confirmed:
		if err != nil {
			cancel()
			<-subscription.done
			return nil, err
		}
		return subscription, nil
	case <-ctx.Done():
		cancel()
		<-subscription.done
		return nil, ctx.Err()
	}
}

func (r *RedisTransport) runSubscription(ctx context.Context, subscription *redisRoomSubscription, roomID string, handler func(SubscriptionEvent), confirmed chan<- error) {
	defer close(subscription.done)
	first := true
	for ctx.Err() == nil {
		pubsub := r.client.Subscribe(ctx, RoomChannel(roomID))
		if _, err := pubsub.Receive(ctx); err != nil {
			slog.Warn("redis subscription unavailable", "room", roomID, "error", err)
			if r.observer != nil {
				r.observer.RedisFailure()
			}
			_ = pubsub.Close()
			if first {
				confirmed <- err
				return
			}
			if !waitForRetry(ctx, r.retryDelay) {
				return
			}
			continue
		}
		if first {
			first = false
			confirmed <- nil
		} else {
			slog.Info("redis subscription restored", "room", roomID)
			if r.observer != nil {
				r.observer.RedisReconnect()
			}
			handler(SubscriptionEvent{Reconnected: true})
		}
		monitorContext, stopMonitor := context.WithCancel(ctx)
		failed := make(chan struct{})
		go r.monitorSubscription(monitorContext, pubsub, failed)
		for ctx.Err() == nil {
			message, err := pubsub.ReceiveMessage(ctx)
			if err != nil {
				break
			}
			hint, err := DecodeRevisionHint(message.Channel, []byte(message.Payload))
			if err == nil {
				handler(SubscriptionEvent{Hint: &hint})
			}
		}
		_ = pubsub.Close()
		stopMonitor()
		<-failed
		if !waitForRetry(ctx, r.retryDelay) {
			return
		}
	}
}

func (r *RedisTransport) monitorSubscription(ctx context.Context, pubsub *redis.PubSub, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(r.healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = pubsub.Close()
			return
		case <-ticker.C:
			pingContext, cancel := context.WithTimeout(ctx, r.healthInterval)
			err := r.client.Ping(pingContext).Err()
			cancel()
			if err != nil {
				_ = pubsub.Close()
				return
			}
		}
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type redisRoomSubscription struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (s *redisRoomSubscription) Close() error {
	s.once.Do(func() {
		s.cancel()
		<-s.done
	})
	return nil
}

type subscriptionFunc func() error

func (close subscriptionFunc) Close() error { return close() }
