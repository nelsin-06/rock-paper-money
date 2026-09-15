package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken = errors.New("invalid access token")
	uuidPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
)

type Principal struct {
	Subject string
}

type Verifier interface {
	Verify(context.Context, string) (Principal, error)
}

type JWTVerifier struct {
	issuer   string
	audience string
	jwksURL  string
	client   *http.Client
	now      func() time.Time
	cacheTTL time.Duration

	mu        sync.Mutex
	keys      map[string]signingKey
	expiresAt time.Time
}

type signingKey struct {
	algorithm string
	key       any
}

type jwksDocument struct {
	Keys []jsonWebKey `json:"keys"`
}

type jsonWebKey struct {
	KeyID     string `json:"kid"`
	KeyType   string `json:"kty"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	Curve     string `json:"crv"`
	X         string `json:"x"`
	Y         string `json:"y"`
	Modulus   string `json:"n"`
	Exponent  string `json:"e"`
}

func NewSupabaseJWTVerifier(projectURL, audience string, client *http.Client) (*JWTVerifier, error) {
	projectURL = strings.TrimRight(strings.TrimSpace(projectURL), "/")
	parsed, err := url.Parse(projectURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, errors.New("SUPABASE_URL must be an HTTPS origin")
	}
	if audience == "" {
		audience = "authenticated"
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	issuer := projectURL + "/auth/v1"
	return &JWTVerifier{
		issuer: issuer, audience: audience, jwksURL: issuer + "/.well-known/jwks.json",
		client: client, now: time.Now, cacheTTL: 10 * time.Minute,
	}, nil
}

func (v *JWTVerifier) Verify(ctx context.Context, raw string) (Principal, error) {
	claims := &jwt.RegisteredClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		keyID, ok := token.Header["kid"].(string)
		if !ok || keyID == "" {
			return nil, ErrInvalidToken
		}
		key, err := v.signingKey(ctx, keyID)
		if err != nil || key.algorithm != token.Method.Alg() {
			return nil, ErrInvalidToken
		}
		return key.key, nil
	}, jwt.WithValidMethods([]string{"ES256", "RS256"}), jwt.WithIssuer(v.issuer), jwt.WithAudience(v.audience), jwt.WithExpirationRequired(), jwt.WithTimeFunc(v.now))
	if err != nil || !token.Valid || !uuidPattern.MatchString(claims.Subject) {
		return Principal{}, ErrInvalidToken
	}
	return Principal{Subject: strings.ToLower(claims.Subject)}, nil
}

func (v *JWTVerifier) signingKey(ctx context.Context, keyID string) (signingKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.now().Before(v.expiresAt) {
		if key, ok := v.keys[keyID]; ok {
			return key, nil
		}
	}
	if err := v.refresh(ctx); err != nil {
		return signingKey{}, err
	}
	key, ok := v.keys[keyID]
	if !ok {
		return signingKey{}, ErrInvalidToken
	}
	return key, nil
}

func (v *JWTVerifier) refresh(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS request returned status %d", response.StatusCode)
	}
	var document jwksDocument
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	keys := make(map[string]signingKey, len(document.Keys))
	for _, value := range document.Keys {
		key, err := parseJWK(value)
		if err == nil && value.KeyID != "" && (value.Use == "" || value.Use == "sig") {
			keys[value.KeyID] = key
		}
	}
	if len(keys) == 0 {
		return errors.New("JWKS contains no supported signing keys")
	}
	v.keys = keys
	v.expiresAt = v.now().Add(v.cacheTTL)
	return nil
}

func parseJWK(value jsonWebKey) (signingKey, error) {
	switch {
	case value.KeyType == "EC" && value.Curve == "P-256" && (value.Algorithm == "" || value.Algorithm == "ES256"):
		x, err := decodeBigInt(value.X)
		if err != nil {
			return signingKey{}, err
		}
		y, err := decodeBigInt(value.Y)
		if err != nil || !elliptic.P256().IsOnCurve(x, y) {
			return signingKey{}, errors.New("invalid EC key")
		}
		return signingKey{algorithm: "ES256", key: &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}}, nil
	case value.KeyType == "RSA" && (value.Algorithm == "" || value.Algorithm == "RS256"):
		n, err := decodeBigInt(value.Modulus)
		if err != nil {
			return signingKey{}, err
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(value.Exponent)
		if err != nil || len(eBytes) == 0 || len(eBytes) > 4 {
			return signingKey{}, errors.New("invalid RSA exponent")
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 | int(b)
		}
		if e < 3 || n.Sign() <= 0 {
			return signingKey{}, errors.New("invalid RSA key")
		}
		return signingKey{algorithm: "RS256", key: &rsa.PublicKey{N: n, E: e}}, nil
	default:
		return signingKey{}, errors.New("unsupported signing key")
	}
}

func decodeBigInt(encoded string) (*big.Int, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 {
		return nil, errors.New("invalid key coordinate")
	}
	return new(big.Int).SetBytes(decoded), nil
}
