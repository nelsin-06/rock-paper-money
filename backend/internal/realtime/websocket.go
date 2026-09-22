package realtime

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"example.com/rock-paper-money/internal/auth"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
	"github.com/coder/websocket"
)

const (
	defaultMaxConnections     = 1024
	defaultReadLimit          = 1024
	defaultWriteTimeout       = 5 * time.Second
	defaultOperationTimeout   = 5 * time.Second
	defaultRevalidateInterval = 30 * time.Second
	defaultLeaseDuration      = 60 * time.Second
	defaultPingInterval       = 20 * time.Second
)

type SessionAuthenticator interface {
	Authenticate(context.Context, string) (auth.Session, error)
}

type SocketRepository interface {
	AuthorizeRoom(context.Context, string, string) (string, error)
	OpenConnection(context.Context, application.ConnectionLease) (application.PresenceTransition, error)
	RenewConnection(context.Context, string, time.Time, time.Time) error
	CloseConnection(context.Context, string, time.Time) (application.PresenceTransition, error)
}

type WebSocketConfig struct {
	MaxConnections     int
	ReadLimit          int64
	WriteTimeout       time.Duration
	OperationTimeout   time.Duration
	RevalidateInterval time.Duration
	LeaseDuration      time.Duration
	PingInterval       time.Duration
	LifecycleContext   context.Context
	Now                func() time.Time
	NewConnectionID    func() (string, error)
	Observer           SocketObserver
}

type SocketObserver interface {
	SocketOpened()
	SocketClosed(string)
	QueueEviction()
	AuthClose()
}

type WebSocketHandler struct {
	security   *auth.RequestSecurity
	sessions   SessionAuthenticator
	repository SocketRepository
	hub        *Hub
	config     WebSocketConfig
	registry   chan struct{}
	lifecycle  context.Context
	stop       context.CancelFunc
	activeMu   sync.Mutex
	stopping   bool
	active     sync.WaitGroup
	closeMu    sync.Mutex
	closeErrs  []error
}

func NewWebSocketHandler(security *auth.RequestSecurity, sessions SessionAuthenticator, repository SocketRepository, hub *Hub, config WebSocketConfig) *WebSocketHandler {
	if config.MaxConnections <= 0 {
		config.MaxConnections = defaultMaxConnections
	}
	if config.ReadLimit <= 0 {
		config.ReadLimit = defaultReadLimit
	}
	if config.WriteTimeout <= 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	if config.OperationTimeout <= 0 {
		config.OperationTimeout = defaultOperationTimeout
	}
	if config.RevalidateInterval <= 0 {
		config.RevalidateInterval = defaultRevalidateInterval
	}
	if config.LeaseDuration <= 0 {
		config.LeaseDuration = defaultLeaseDuration
	}
	if config.PingInterval <= 0 {
		config.PingInterval = defaultPingInterval
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.NewConnectionID == nil {
		config.NewConnectionID = randomConnectionID
	}
	if config.LifecycleContext == nil {
		config.LifecycleContext = context.Background()
	}
	lifecycle, stop := context.WithCancel(config.LifecycleContext)
	return &WebSocketHandler{security: security, sessions: sessions, repository: repository, hub: hub, config: config, registry: make(chan struct{}, config.MaxConnections), lifecycle: lifecycle, stop: stop}
}

func (h *WebSocketHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.security == nil || h.sessions == nil || h.repository == nil || h.hub == nil || h.security.ValidateOrigin(r) != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil || cookie.Value == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.config.OperationTimeout)
	session, err := h.sessions.Authenticate(ctx, cookie.Value)
	if err != nil {
		cancel()
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	roomID := strings.ToUpper(strings.TrimSpace(r.PathValue("code")))
	role, authorizeErr := h.repository.AuthorizeRoom(ctx, roomID, session.AccountID)
	if roomID == "" || authorizeErr != nil || role == "" {
		cancel()
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	cancel()
	select {
	case h.registry <- struct{}{}:
		defer func() { <-h.registry }()
	default:
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	if !h.beginConnection() {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	defer h.active.Done()
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(h.config.ReadLimit)
	closeSetupFailure := func() { _ = connection.Close(websocket.StatusInternalError, "connection setup failed") }

	queue := newSnapshotQueue(h.config.Observer)
	acquireContext, cancelAcquire := context.WithTimeout(h.lifecycle, h.config.OperationTimeout)
	release, err := h.hub.Acquire(acquireContext, roomID, queue.Offer)
	cancelAcquire()
	if err != nil {
		closeSetupFailure()
		return
	}
	defer release()

	connectionID, err := h.config.NewConnectionID()
	if err != nil {
		closeSetupFailure()
		return
	}
	now := h.config.Now().UTC()
	lease := application.ConnectionLease{ID: connectionID, RoomCode: roomID, AccountID: session.AccountID, SessionDigest: append([]byte(nil), session.Digest[:]...), ConnectedAt: now, LeaseExpiresAt: now.Add(h.config.LeaseDuration)}
	operationContext, cancelOperation := context.WithTimeout(h.lifecycle, h.config.OperationTimeout)
	_, err = h.repository.OpenConnection(operationContext, lease)
	cancelOperation()
	if err != nil {
		closeSetupFailure()
		return
	}
	defer h.closeLease(connectionID)
	refreshContext, cancelRefresh := context.WithTimeout(h.lifecycle, h.config.OperationTimeout)
	err = h.hub.Reconcile(refreshContext, roomID)
	cancelRefresh()
	if err != nil {
		closeSetupFailure()
		return
	}

	if h.config.Observer != nil {
		h.config.Observer.SocketOpened()
		defer h.config.Observer.SocketClosed("connection_ended")
	}
	h.serveConnection(h.lifecycle, connection, cookie.Value, session, role, roomID, connectionID, queue)
}

func (h *WebSocketHandler) beginConnection() bool {
	h.activeMu.Lock()
	defer h.activeMu.Unlock()
	if h.stopping {
		return false
	}
	h.active.Add(1)
	return true
}

// Shutdown stops accepted sockets and waits until their durable connection
// leases have been closed.
func (h *WebSocketHandler) Shutdown(ctx context.Context) error {
	h.activeMu.Lock()
	h.stopping = true
	h.stop()
	h.activeMu.Unlock()
	h.active.Wait()
	return errors.Join(ctx.Err(), h.closeErrors())
}

func (h *WebSocketHandler) serveConnection(parent context.Context, connection *websocket.Conn, token string, admitted auth.Session, role, roomID, connectionID string, queue *snapshotQueue) {
	ctx, cancel := context.WithCancel(parent)
	log := slog.With("room", roomID, "session_prefix", hex.EncodeToString(admitted.Digest[:4]), "connection", connectionID)
	log.Info("socket opened")
	defer log.Info("socket closed")
	readResult := make(chan error, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _, err := connection.Read(ctx)
		if err == nil {
			err = errMutationFrame
		}
		readResult <- err
	}()
	defer func() {
		cancel()
		connection.CloseNow()
		<-readDone
	}()
	revalidate := time.NewTicker(h.config.RevalidateInterval)
	defer revalidate.Stop()
	ping := time.NewTicker(h.config.PingInterval)
	defer ping.Stop()
	for {
		select {
		case <-parent.Done():
			log.Info("socket closing", "reason", "server_shutdown")
			_ = connection.Close(websocket.StatusNormalClosure, "server shutdown")
			return
		case err := <-readResult:
			if errors.Is(err, errMutationFrame) {
				log.Warn("socket closing", "reason", "mutation_frame")
				_ = connection.Close(websocket.StatusPolicyViolation, "WebSocket mutations are not allowed")
			}
			return
		case snapshot := <-queue.C():
			if !h.revalidate(parent, token, admitted, connectionID) {
				log.Warn("socket closing", "reason", "authorization_expired")
				if h.config.Observer != nil {
					h.config.Observer.AuthClose()
				}
				_ = connection.Close(websocket.StatusPolicyViolation, "authorization expired")
				return
			}
			payload, err := json.Marshal(snapshotFrame(snapshot, admitted.AccountID, role))
			if err != nil {
				log.Warn("socket closing", "reason", "write_backpressure")
				connection.CloseNow()
				return
			}
			writeContext, cancelWrite := context.WithTimeout(parent, h.config.WriteTimeout)
			err = connection.Write(writeContext, websocket.MessageText, payload)
			cancelWrite()
			if err != nil {
				connection.CloseNow()
				return
			}
		case <-revalidate.C:
			if !h.revalidate(parent, token, admitted, connectionID) {
				log.Warn("socket closing", "reason", "authorization_expired")
				if h.config.Observer != nil {
					h.config.Observer.AuthClose()
				}
				_ = connection.Close(websocket.StatusPolicyViolation, "authorization expired")
				return
			}
		case <-ping.C:
			pingContext, cancelPing := context.WithTimeout(parent, h.config.WriteTimeout)
			err := connection.Ping(pingContext)
			cancelPing()
			if err != nil {
				log.Warn("socket closing", "reason", "ping_failed")
				connection.CloseNow()
				return
			}
		}
	}
}

var errMutationFrame = errors.New("client data frames are not allowed")

func (h *WebSocketHandler) revalidate(parent context.Context, token string, admitted auth.Session, connectionID string) bool {
	ctx, cancel := context.WithTimeout(parent, h.config.OperationTimeout)
	defer cancel()
	current, err := h.sessions.Authenticate(ctx, token)
	if err != nil || current.AccountID != admitted.AccountID || subtle.ConstantTimeCompare(current.Digest[:], admitted.Digest[:]) != 1 {
		return false
	}
	now := h.config.Now().UTC()
	return h.repository.RenewConnection(ctx, connectionID, now, now.Add(h.config.LeaseDuration)) == nil
}

func (h *WebSocketHandler) closeLease(connectionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), h.config.OperationTimeout)
	defer cancel()
	if _, err := h.repository.CloseConnection(ctx, connectionID, h.config.Now().UTC()); err != nil {
		h.closeMu.Lock()
		h.closeErrs = append(h.closeErrs, fmt.Errorf("close connection %s: %w", connectionID, err))
		h.closeMu.Unlock()
	}
}

func (h *WebSocketHandler) closeErrors() error {
	h.closeMu.Lock()
	defer h.closeMu.Unlock()
	return errors.Join(h.closeErrs...)
}

type snapshotQueue struct {
	channel  chan application.Snapshot
	observer SocketObserver
}

func newSnapshotQueue(observers ...SocketObserver) *snapshotQueue {
	var observer SocketObserver
	if len(observers) > 0 {
		observer = observers[0]
	}
	return &snapshotQueue{channel: make(chan application.Snapshot, 1), observer: observer}
}
func (q *snapshotQueue) C() <-chan application.Snapshot { return q.channel }
func (q *snapshotQueue) Offer(snapshot application.Snapshot) {
	select {
	case q.channel <- snapshot:
		return
	default:
	}
	select {
	case <-q.channel:
		if q.observer != nil {
			q.observer.QueueEviction()
		}
	default:
	}
	select {
	case q.channel <- snapshot:
	default:
	}
}

type socketFrame struct {
	Type     string          `json:"type"`
	Revision uint64          `json:"revision"`
	Room     publicRoom      `json:"room"`
	Presence map[string]bool `json:"presence"`
	Player   socketPlayer    `json:"player"`
}
type socketPlayer struct {
	AccountID string `json:"account_id"`
	Role      string `json:"role"`
}
type publicRoom struct {
	Code     string         `json:"room_code"`
	Ready    bool           `json:"ready"`
	Resolved bool           `json:"resolved"`
	Round    uint64         `json:"round"`
	Closed   bool           `json:"closed"`
	Forfeit  bool           `json:"forfeit"`
	Players  []publicPlayer `json:"players"`
	Result   domain.Result  `json:"result,omitempty"`
	Moves    []publicMove   `json:"moves,omitempty"`
}
type publicPlayer struct {
	Role           string `json:"role"`
	Wins           int    `json:"wins"`
	Submitted      bool   `json:"submitted"`
	WantsNextRound bool   `json:"wants_next_round"`
}
type publicMove struct {
	Role string      `json:"role"`
	Move domain.Move `json:"move"`
}

func snapshotFrame(snapshot application.Snapshot, accountID, role string) socketFrame {
	state := snapshot.State
	room := publicRoom{Code: state.Code, Ready: state.Ready, Resolved: state.Resolved, Round: state.Round, Closed: state.Closed, Forfeit: state.Forfeit, Players: make([]publicPlayer, len(state.Players))}
	roles := make(map[string]string, len(state.Players))
	for index, player := range state.Players {
		role := "guest"
		if index == 0 {
			role = "host"
		}
		roles[player.ID] = role
		room.Players[index] = publicPlayer{Role: role, Wins: player.Wins, Submitted: player.Submitted, WantsNextRound: player.WantsNextRound}
	}
	if state.Resolved {
		room.Result = state.Result
		room.Moves = make([]publicMove, len(state.Moves))
		for index, move := range state.Moves {
			room.Moves[index] = publicMove{Role: roles[move.PlayerID], Move: move.Move}
		}
	}
	return socketFrame{Type: "snapshot", Revision: snapshot.Revision, Room: room, Presence: snapshot.Presence, Player: socketPlayer{AccountID: accountID, Role: role}}
}

func randomConnectionID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
