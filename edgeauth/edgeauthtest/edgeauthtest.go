// Package edgeauthtest mints identity JWTs for an app's tests, the way guard
// signs them, with a throwaway key served from an in-process JWKS:
//
//	iss := edgeauthtest.NewIssuer(t)
//	v, err := edgeauth.New(iss.Config("live.metsa.app"))
//	...
//	req := httptest.NewRequest(http.MethodGet, "https://live.metsa.app/admin", nil)
//	iss.Sign(req, edgeauth.Identity{Subject: "u1", Roles: []string{"admin"}})
//
// MintWith edits the claims before signing, for the tokens an app must refuse
// (expired, for another host).
package edgeauthtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/MetsaApp/platform-kit/edgeauth"
)

// KeyID is the kid of every Issuer's key.
const KeyID = "edgeauthtest"

// Lifetime is how long a minted token is valid, as guard's are.
const Lifetime = 60 * time.Second

// Claims is a token's payload before signing, for MintWith to edit.
type Claims struct {
	Issuer        string   `json:"iss"`
	Audience      string   `json:"aud"`
	Subject       string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Name          string   `json:"name"`
	Roles         []string `json:"roles"`
	SessionID     string   `json:"sid"`
	AuthTime      int64    `json:"auth_time,omitempty"`
	ClientID      string   `json:"azp"`
	Source        string   `json:"src"`
	IssuedAt      int64    `json:"iat"`
	Expiry        int64    `json:"exp"`
}

// Issuer signs identity JWTs with an in-memory P-256 key and serves its JWKS
// on a test server that is closed when the test ends.
type Issuer struct {
	t      testing.TB
	signer jose.Signer
	server *httptest.Server
}

// NewIssuer starts an Issuer for the test.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("edgeauthtest: generate key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: key, KeyID: KeyID}},
		(&jose.SignerOptions{}).WithType(edgeauth.TokenType),
	)
	if err != nil {
		t.Fatalf("edgeauthtest: signer: %v", err)
	}
	jwks, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &key.PublicKey, KeyID: KeyID, Algorithm: string(jose.ES256), Use: "sig",
	}}})
	if err != nil {
		t.Fatalf("edgeauthtest: marshal JWKS: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	}))
	t.Cleanup(server.Close)
	return &Issuer{t: t, signer: signer, server: server}
}

// JWKSURL is where the Issuer serves its public key.
func (i *Issuer) JWKSURL() string {
	return i.server.URL + "/jwks.json"
}

// Config is a verifier config that trusts this Issuer for the hosts aud.
func (i *Issuer) Config(aud ...string) edgeauth.Config {
	return edgeauth.Config{JWKSURL: i.JWKSURL(), Audiences: aud}
}

// Mint signs an identity JWT for id with audience aud, issued now and valid
// for Lifetime. An empty Source becomes edgeauth.SourceSession.
func (i *Issuer) Mint(id edgeauth.Identity, aud string) string {
	i.t.Helper()
	return i.MintWith(id, aud, nil)
}

// MintWith is Mint with edit applied to the claims before signing.
func (i *Issuer) MintWith(id edgeauth.Identity, aud string, edit func(*Claims)) string {
	i.t.Helper()
	now := time.Now()
	c := Claims{
		Issuer:        edgeauth.Issuer,
		Audience:      aud,
		Subject:       id.Subject,
		Email:         id.Email,
		EmailVerified: id.EmailVerified,
		Name:          id.Name,
		Roles:         append([]string{}, id.Roles...),
		SessionID:     id.SessionID,
		ClientID:      id.ClientID,
		Source:        id.Source,
		IssuedAt:      now.Unix(),
		Expiry:        now.Add(Lifetime).Unix(),
	}
	if c.Source == "" {
		c.Source = edgeauth.SourceSession
	}
	if !id.AuthTime.IsZero() {
		c.AuthTime = id.AuthTime.Unix()
	}
	if edit != nil {
		edit(&c)
	}
	token, err := jwt.Signed(i.signer).Claims(c).Serialize()
	if err != nil {
		i.t.Fatalf("edgeauthtest: sign: %v", err)
	}
	return token
}

// Sign sets r's identity header to a token for id, with the audience guard
// would use: r's host, lower case, without the port.
func (i *Issuer) Sign(r *http.Request, id edgeauth.Identity) {
	i.t.Helper()
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	r.Header.Set(edgeauth.Header, i.Mint(id, strings.ToLower(host)))
}
