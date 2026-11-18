package edgeauth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MetsaApp/platform-kit/edgeauth"
)

// guardKey is one signing entry of guard's keys file (guard
// internal/auth/keys): a PKCS#8 PEM P-256 private key under a kid.
type guardKey struct {
	kid    string
	pem    []byte
	active bool
}

func newGuardKey(t *testing.T, kid string, active bool) guardKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)
	return guardKey{kid: kid, pem: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), active: active}
}

func (g guardKey) private(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode(g.pem)
	require.NotNil(t, block)
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err)
	ec, ok := k.(*ecdsa.PrivateKey)
	require.True(t, ok)
	return ec
}

// guardKeys builds the JWKS and the signer the way guard's keys.Parse does:
// every entry published (alg ES256, use sig), the active one signs with typ
// bastion-id+jwt and its kid.
func guardKeys(t *testing.T, entries ...guardKey) ([]byte, jose.Signer) {
	t.Helper()
	var set jose.JSONWebKeySet
	var signer jose.Signer
	for _, e := range entries {
		key := e.private(t)
		set.Keys = append(set.Keys, jose.JSONWebKey{
			Key: &key.PublicKey, KeyID: e.kid, Algorithm: string(jose.ES256), Use: "sig",
		})
		if !e.active {
			continue
		}
		opts := (&jose.SignerOptions{}).WithType("bastion-id+jwt")
		var err error
		signer, err = jose.NewSigner(
			jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: key, KeyID: e.kid}}, opts,
		)
		require.NoError(t, err)
	}
	jwks, err := json.Marshal(set)
	require.NoError(t, err)
	return jwks, signer
}

// guardClaims is guard's identity.claims (internal/auth/identity), field for
// field: auth_time is always present, even when it is the zero time.
type guardClaims struct {
	Issuer        string   `json:"iss"`
	Audience      string   `json:"aud"`
	Subject       string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Name          string   `json:"name"`
	Roles         []string `json:"roles"`
	SessionID     string   `json:"sid"`
	AuthTime      int64    `json:"auth_time"`
	ClientID      string   `json:"azp"`
	Source        string   `json:"src"`
	IssuedAt      int64    `json:"iat"`
	Expiry        int64    `json:"exp"`
}

// guardMint is guard's identity.Mint: [json.Marshal] of the claims, signed as
// a compact JWS by the active key.
func guardMint(t *testing.T, signer jose.Signer, c guardClaims) string {
	t.Helper()
	payload, err := json.Marshal(c)
	require.NoError(t, err)
	obj, err := signer.Sign(payload)
	require.NoError(t, err)
	tok, err := obj.CompactSerialize()
	require.NoError(t, err)
	return tok
}

func serveJWKS(t *testing.T, jwks []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/jwks.json"
}

// TestGuardIssuedToken verifies tokens made exactly the way guard's branch
// cor-1074-1 makes them (keys file, JWKS, header and claims), including a
// bearer with a zero auth_time and a token signed by a key that is published
// but no longer active, as during a rotation.
func TestGuardIssuedToken(t *testing.T) {
	t.Parallel()
	old, current := newGuardKey(t, "2026-11-a", false), newGuardKey(t, "2026-11-b", true)
	jwks, signer := guardKeys(t, old, current)
	_, oldSigner := guardKeys(t, guardKey{kid: old.kid, pem: old.pem, active: true})

	now := time.Unix(1_800_000_100, 0)
	v, err := edgeauth.New(
		edgeauth.Config{JWKSURL: serveJWKS(t, jwks), Audiences: []string{"live.metsa.app"}},
		edgeauth.WithClock(func() time.Time { return now }),
	)
	require.NoError(t, err)

	session := guardClaims{
		Issuer: "bastion", Audience: "live.metsa.app", Subject: "312345678901234567",
		Email: "ada@example.com", EmailVerified: true, Name: "Ada Lovelace",
		Roles: []string{"admin", "member"}, SessionID: "0123456789abcdef", AuthTime: 1_800_000_000,
		ClientID: "394513759751636980", Source: "session",
		IssuedAt: now.Unix(), Expiry: now.Add(60 * time.Second).Unix(),
	}
	id, err := v.Verify(context.Background(), guardMint(t, signer, session))
	require.NoError(t, err)
	assert.Equal(t, edgeauth.Identity{
		Subject: "312345678901234567", Email: "ada@example.com", EmailVerified: true, Name: "Ada Lovelace",
		Roles: []string{"admin", "member"}, SessionID: "0123456789abcdef", AuthTime: time.Unix(1_800_000_000, 0),
		ClientID: "394513759751636980", Source: edgeauth.SourceSession,
	}, id)

	// A bearer: no session id, no email, no roles, and guard's zero
	// AuthTime, whose Unix() is negative.
	bearer := session
	bearer.SessionID, bearer.Email, bearer.Name, bearer.Roles = "", "", "", []string{}
	bearer.AuthTime, bearer.Source = time.Time{}.Unix(), "bearer"
	id, err = v.Verify(context.Background(), guardMint(t, signer, bearer))
	require.NoError(t, err)
	assert.True(t, id.AuthTime.IsZero())
	assert.Equal(t, edgeauth.SourceBearer, id.Source)
	assert.Equal(t, []string{}, id.Roles)

	// The previous key is still published after the flip.
	_, err = v.Verify(context.Background(), guardMint(t, oldSigner, session))
	require.NoError(t, err)

	// Another host's token is refused.
	other := session
	other.Audience = "dash.metsa.app"
	_, err = v.Verify(context.Background(), guardMint(t, signer, other))
	require.ErrorIs(t, err, edgeauth.ErrInvalid)
}
