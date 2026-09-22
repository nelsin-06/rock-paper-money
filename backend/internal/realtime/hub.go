package realtime

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"example.com/rock-paper-money/internal/room/application"
)

type SnapshotLoader interface {
	LoadRoom(context.Context, string) (application.Snapshot, error)
}

type SnapshotConsumer func(application.Snapshot)

type Hub struct {
	subscriber RoomSubscriber
	loader     SnapshotLoader
	mu         sync.Mutex
	rooms      map[string]*localRoom
	nextID     uint64
	observer   HubObserver
}

type HubObserver interface {
	SnapshotDelivered(time.Duration)
	RevisionGap()
}

type localRoom struct {
	subscription RoomSubscription
	cancel       context.CancelFunc
	consumers    map[uint64]SnapshotConsumer
	revision     uint64
	snapshot     application.Snapshot
	reconcileMu  sync.Mutex
}

func NewHub(subscriber RoomSubscriber, loader SnapshotLoader) *Hub {
	return &Hub{subscriber: subscriber, loader: loader, rooms: make(map[string]*localRoom)}
}

func (h *Hub) SetObserver(observer HubObserver) { h.observer = observer }

func (h *Hub) Acquire(ctx context.Context, roomID string, consumer SnapshotConsumer) (func(), error) {
	h.mu.Lock()
	if room := h.rooms[roomID]; room != nil {
		h.nextID++
		consumerID := h.nextID
		room.consumers[consumerID] = consumer
		snapshot := room.snapshot
		h.mu.Unlock()
		if snapshot.Revision > 0 {
			consumer(snapshot)
		}
		return h.release(roomID, consumerID), nil
	}

	roomContext, cancel := context.WithCancel(context.Background())
	room := &localRoom{consumers: make(map[uint64]SnapshotConsumer), cancel: cancel}
	type subscribeResult struct {
		subscription RoomSubscription
		err          error
	}
	result := make(chan subscribeResult, 1)
	go func() {
		subscription, err := h.subscriber.SubscribeRoom(roomContext, roomID, func(event SubscriptionEvent) {
			h.handleEvent(roomContext, roomID, event)
		})
		result <- subscribeResult{subscription: subscription, err: err}
	}()
	var subscription RoomSubscription
	var err error
	select {
	case subscribed := <-result:
		subscription, err = subscribed.subscription, subscribed.err
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		cancel()
		h.mu.Unlock()
		return nil, err
	}
	room.subscription = subscription
	h.nextID++
	consumerID := h.nextID
	room.consumers[consumerID] = consumer
	h.rooms[roomID] = room
	h.mu.Unlock()

	if err = h.reconcile(ctx, roomID, 0, true); err != nil {
		h.release(roomID, consumerID)()
		return nil, err
	}
	return h.release(roomID, consumerID), nil
}

// Reconcile reloads a room after admission changes such as a durable presence
// lease. Consumers only observe revisions newer than their current snapshot.
func (h *Hub) Reconcile(ctx context.Context, roomID string) error {
	return h.reconcile(ctx, roomID, 0, true)
}

func (h *Hub) handleEvent(ctx context.Context, roomID string, event SubscriptionEvent) {
	if event.Hint != nil && event.Hint.RoomID != roomID {
		return
	}
	minimum := uint64(0)
	force := event.Reconnected
	if event.Hint != nil {
		minimum = event.Hint.Revision
	}
	_ = h.reconcile(ctx, roomID, minimum, force)
}

func (h *Hub) reconcile(ctx context.Context, roomID string, minimum uint64, force bool) error {
	started := time.Now()
	h.mu.Lock()
	room := h.rooms[roomID]
	if room == nil || (!force && minimum <= room.revision) {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	room.reconcileMu.Lock()
	defer room.reconcileMu.Unlock()
	h.mu.Lock()
	room = h.rooms[roomID]
	if room == nil || (!force && minimum <= room.revision) {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	snapshot, err := h.loader.LoadRoom(ctx, roomID)
	if err != nil {
		return err
	}
	h.mu.Lock()
	room = h.rooms[roomID]
	if room == nil || snapshot.Revision <= room.revision {
		h.mu.Unlock()
		return nil
	}
	previousRevision := room.revision
	room.revision = snapshot.Revision
	if h.observer != nil {
		if minimum > 0 && previousRevision > 0 && minimum > previousRevision+1 {
			h.observer.RevisionGap()
			slog.Warn("room revision gap reconciled", "room", roomID, "previous_revision", previousRevision, "hint_revision", minimum, "snapshot_revision", snapshot.Revision)
		}
		h.observer.SnapshotDelivered(time.Since(started))
	}
	room.snapshot = snapshot
	consumers := make([]SnapshotConsumer, 0, len(room.consumers))
	for _, consumer := range room.consumers {
		consumers = append(consumers, consumer)
	}
	h.mu.Unlock()
	for _, consumer := range consumers {
		consumer(snapshot)
	}
	return nil
}

func (h *Hub) release(roomID string, consumerID uint64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			room := h.rooms[roomID]
			if room == nil {
				h.mu.Unlock()
				return
			}
			delete(room.consumers, consumerID)
			if len(room.consumers) > 0 {
				h.mu.Unlock()
				return
			}
			delete(h.rooms, roomID)
			subscription := room.subscription
			cancel := room.cancel
			h.mu.Unlock()
			cancel()
			_ = subscription.Close()
		})
	}
}
