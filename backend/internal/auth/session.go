package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"time"
)

const (
	sessionTokenBytes    = 32
	SessionIdleLimit     = 24 * time.Hour
	SessionAbsoluteLimit = 30 * 24 * time.Hour
)

var ErrInvalidSession = errors.New("invalid session")

type Digest [sha256.Size]byte

type Session struct {
	Digest            Digest
	CSRFDigest        Digest
	AccountID         string
	CreatedAt         time.Time
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
	RevokedAt         *time.Time
}

type SessionCredentials struct {
	Token             string
	CSRFToken         string
	AbsoluteExpiresAt time.Time
}

type SessionStore interface {
	CreateSession(context.Context, Session) error
	UseSession(context.Context, Digest, time.Time, time.Time) (Session, error)
	RevokeSession(context.Context, Digest, time.Time) error
	DeleteExpiredSessions(context.Context, time.Time) (int64, error)
}

type SessionService struct {
	store  SessionStore
	random io.Reader
	now    func() time.Time
}

func NewSessionService(store SessionStore, random io.Reader, now func() time.Time) *SessionService {
	if random == nil {
		random = rand.Reader
	}
	if now == nil {
		now = time.Now
	}
	return &SessionService{store: store, random: random, now: now}
}

func (s *SessionService) Create(ctx context.Context, accountID string) (SessionCredentials, error) {
	token, err := s.newToken()
	if err != nil {
		return SessionCredentials{}, err
	}
	csrfToken, err := s.newToken()
	if err != nil {
		return SessionCredentials{}, err
	}
	now := s.now().UTC()
	session := Session{
		Digest:            digestToken(token),
		CSRFDigest:        digestToken(csrfToken),
		AccountID:         accountID,
		CreatedAt:         now,
		LastSeenAt:        now,
		IdleExpiresAt:     now.Add(SessionIdleLimit),
		AbsoluteExpiresAt: now.Add(SessionAbsoluteLimit),
	}
	if err := s.store.CreateSession(ctx, session); err != nil {
		return SessionCredentials{}, err
	}
	return SessionCredentials{Token: token, CSRFToken: csrfToken, AbsoluteExpiresAt: session.AbsoluteExpiresAt}, nil
}

func (s *SessionService) Authenticate(ctx context.Context, token string) (Session, error) {
	digest, err := sessionDigest(token)
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	now := s.now().UTC()
	return s.store.UseSession(ctx, digest, now, now.Add(SessionIdleLimit))
}

func (s *SessionService) Revoke(ctx context.Context, token string) error {
	digest, err := sessionDigest(token)
	if err != nil {
		return ErrInvalidSession
	}
	return s.store.RevokeSession(ctx, digest, s.now().UTC())
}

func (s *SessionService) Cleanup(ctx context.Context) (int64, error) {
	return s.store.DeleteExpiredSessions(ctx, s.now().UTC())
}

func (s *SessionService) ValidateCSRF(session Session, token string) bool {
	digest, err := sessionDigest(token)
	return err == nil && subtle.ConstantTimeCompare(digest[:], session.CSRFDigest[:]) == 1
}

func (s *SessionService) newToken() (string, error) {
	value := make([]byte, sessionTokenBytes)
	if _, err := io.ReadFull(s.random, value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func sessionDigest(token string) (Digest, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != sessionTokenBytes {
		return Digest{}, ErrInvalidSession
	}
	return digestToken(token), nil
}

func digestToken(token string) Digest {
	return sha256.Sum256([]byte(token))
}
