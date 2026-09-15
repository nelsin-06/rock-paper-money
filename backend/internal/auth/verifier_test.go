package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestJWTVerifierValidatesSupabaseClaimsAndSignature(t *testing.T) {
	key := newECKey(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	server := jwksServer(t, func() []jsonWebKey { return []jsonWebKey{ecJWK("current", &key.PublicKey)} })
	verifier := testJWTVerifier(t, server.URL, now)

	raw := signedToken(t, key, "current", jwt.RegisteredClaims{
		Issuer: server.URL + "/auth/v1", Subject: "11111111-1111-4111-8111-111111111111",
		Audience: jwt.ClaimStrings{"authenticated"}, ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
	})
	principal, err := verifier.Verify(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Subject != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("subject = %q", principal.Subject)
	}
}

func TestJWTVerifierAcceptsRS256SigningKeys(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	server := jwksServer(t, func() []jsonWebKey { return []jsonWebKey{rsaJWK("current", &key.PublicKey)} })
	verifier := testJWTVerifier(t, server.URL, now)
	claims := jwt.RegisteredClaims{
		Issuer: server.URL + "/auth/v1", Subject: "11111111-1111-4111-8111-111111111111",
		Audience: jwt.ClaimStrings{"authenticated"}, ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "current"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = verifier.Verify(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
}

func TestNewSupabaseJWTVerifierRequiresHTTPSOrigin(t *testing.T) {
	tests := []string{"", "http://project.supabase.co", "https://project.supabase.co/path", "https://project.supabase.co?token=secret"}
	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			if _, err := NewSupabaseJWTVerifier(value, "authenticated", nil); err == nil {
				t.Fatal("configuration error = nil")
			}
		})
	}
	verifier, err := NewSupabaseJWTVerifier("https://project.supabase.co/", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if verifier.audience != "authenticated" || verifier.issuer != "https://project.supabase.co/auth/v1" {
		t.Fatalf("verifier configuration = %#v", verifier)
	}
}

func TestJWTVerifierRejectsInvalidTokens(t *testing.T) {
	key := newECKey(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	server := jwksServer(t, func() []jsonWebKey { return []jsonWebKey{ecJWK("current", &key.PublicKey)} })
	verifier := testJWTVerifier(t, server.URL, now)
	valid := jwt.RegisteredClaims{
		Issuer: server.URL + "/auth/v1", Subject: "11111111-1111-4111-8111-111111111111",
		Audience: jwt.ClaimStrings{"authenticated"}, ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
	}
	tests := []struct {
		name   string
		claims jwt.RegisteredClaims
	}{
		{name: "expired", claims: withClaims(valid, func(c *jwt.RegisteredClaims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Second)) })},
		{name: "not active", claims: withClaims(valid, func(c *jwt.RegisteredClaims) { c.NotBefore = jwt.NewNumericDate(now.Add(time.Minute)) })},
		{name: "wrong issuer", claims: withClaims(valid, func(c *jwt.RegisteredClaims) { c.Issuer = "https://attacker.example/auth/v1" })},
		{name: "wrong audience", claims: withClaims(valid, func(c *jwt.RegisteredClaims) { c.Audience = jwt.ClaimStrings{"other"} })},
		{name: "invalid subject", claims: withClaims(valid, func(c *jwt.RegisteredClaims) { c.Subject = "not-a-uuid" })},
		{name: "missing expiration", claims: withClaims(valid, func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), signedToken(t, key, "current", tt.claims)); err != ErrInvalidToken {
				t.Fatalf("error = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestJWTVerifierRefreshesJWKSForRotatedKey(t *testing.T) {
	first := newECKey(t)
	second := newECKey(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	requests := 0
	server := jwksServer(t, func() []jsonWebKey {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if requests == 1 {
			return []jsonWebKey{ecJWK("first", &first.PublicKey)}
		}
		return []jsonWebKey{ecJWK("first", &first.PublicKey), ecJWK("second", &second.PublicKey)}
	})
	verifier := testJWTVerifier(t, server.URL, now)
	claims := jwt.RegisteredClaims{Issuer: server.URL + "/auth/v1", Subject: "11111111-1111-4111-8111-111111111111", Audience: jwt.ClaimStrings{"authenticated"}, ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}
	if _, err := verifier.Verify(context.Background(), signedToken(t, first, "first", claims)); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), signedToken(t, second, "second", claims)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 2 {
		t.Fatalf("JWKS requests = %d, want 2", requests)
	}
}

func testJWTVerifier(t *testing.T, projectURL string, now time.Time) *JWTVerifier {
	t.Helper()
	issuer := projectURL + "/auth/v1"
	return &JWTVerifier{issuer: issuer, audience: "authenticated", jwksURL: issuer + "/.well-known/jwks.json", client: http.DefaultClient, now: func() time.Time { return now }, cacheTTL: 10 * time.Minute}
}

func jwksServer(t *testing.T, keys func() []jsonWebKey) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/v1/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(jwksDocument{Keys: keys()})
	}))
	t.Cleanup(server.Close)
	return server
}

func newECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func ecJWK(keyID string, key *ecdsa.PublicKey) jsonWebKey {
	return jsonWebKey{KeyID: keyID, KeyType: "EC", Use: "sig", Algorithm: "ES256", Curve: "P-256", X: coordinate(key.X), Y: coordinate(key.Y)}
}

func rsaJWK(keyID string, key *rsa.PublicKey) jsonWebKey {
	return jsonWebKey{
		KeyID: keyID, KeyType: "RSA", Use: "sig", Algorithm: "RS256",
		Modulus:  base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		Exponent: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func coordinate(value *big.Int) string {
	return base64.RawURLEncoding.EncodeToString(value.FillBytes(make([]byte, 32)))
}

func signedToken(t *testing.T, key *ecdsa.PrivateKey, keyID string, claims jwt.RegisteredClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = keyID
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func withClaims(base jwt.RegisteredClaims, change func(*jwt.RegisteredClaims)) jwt.RegisteredClaims {
	change(&base)
	return base
}
