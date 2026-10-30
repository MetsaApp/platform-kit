package edgeauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// The token's fixed values.
const (
	// Header carries the identity JWT from guard to the app.
	Header = "x-bastion-identity"
	// Issuer is the iss of every identity JWT.
	Issuer = "bastion"
	// TokenType is the typ in the JOSE header of every identity JWT.
	TokenType = "bastion-id+jwt" //nolint:gosec // a media type, not a credential
	// DefaultJWKSURL is guard's in-cluster key endpoint.
	DefaultJWKSURL = "http://guard.guard-production.svc.cluster.local:8082/jwks.json"
)

// Leeway is the clock skew tolerated on exp, iat and nbf, each way.
const Leeway = 10 * time.Second

// defaultFetchTimeout bounds a JWKS fetch with the default HTTP client.
const defaultFetchTimeout = 5 * time.Second

var (
	// ErrInvalid means the token is present but not acceptable: malformed,
	// forged, expired, for another host or signed by an unknown key.
	// Middleware answers 401.
	ErrInvalid = errors.New("edgeauth: invalid identity token")
	// ErrUnavailable means the token could not be checked because guard's
	// JWKS could not be fetched. Middleware answers 503.
	ErrUnavailable = errors.New("edgeauth: identity keys unavailable")
	// ErrConfig means New was given an unusable Config.
	ErrConfig = errors.New("edgeauth: invalid config")
)

// Config is what New needs. Apps read it from the environment with
// ConfigFromEnv, or embed it in their own env-parsed config.
type Config struct {
	// JWKSURL is guard's JWKS. The default is the in-cluster endpoint; a
	// verifier outside the cluster uses
	// https://<host>/.well-known/bastion/jwks.json.
	JWKSURL string `env:"PLATFORM_KIT_EDGEAUTH_JWKS_URL" envDefault:"http://guard.guard-production.svc.cluster.local:8082/jwks.json"`
	// Audiences are the hosts this app serves (lower case, no port); a token
	// is accepted only for one of them. Required, comma-separated in the
	// environment.
	Audiences []string `env:"PLATFORM_KIT_EDGEAUTH_AUDIENCES"`
}

// ConfigFromEnv reads Config from PLATFORM_KIT_EDGEAUTH_JWKS_URL and
// PLATFORM_KIT_EDGEAUTH_AUDIENCES.
func ConfigFromEnv() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("load edgeauth config: %w", err)
	}
	return cfg, nil
}

// Option tunes New.
type Option func(*options)

type options struct {
	client *http.Client
	clock  func() time.Time
	logger *slog.Logger
}

// WithHTTPClient fetches the JWKS with client instead of a default client with
// a 5 s timeout.
func WithHTTPClient(client *http.Client) Option {
	return func(o *options) { o.client = client }
}

// WithClock replaces [time.Now] for the token's time checks and the JWKS cache,
// for tests.
func WithClock(clock func() time.Time) Option {
	return func(o *options) { o.clock = clock }
}

// WithLogger is where Middleware logs refused tokens. Without it nothing is
// logged.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// Verifier checks identity JWTs against guard's JWKS. It is safe for
// concurrent use; an app builds one at start and shares it.
type Verifier struct {
	audiences map[string]struct{}
	clock     func() time.Time
	logger    *slog.Logger
	keys      *keyCache
}

// New builds a Verifier. It never contacts guard: the JWKS is fetched when the
// first token arrives.
func New(cfg Config, opts ...Option) (*Verifier, error) {
	o := options{
		client: &http.Client{Timeout: defaultFetchTimeout},
		clock:  time.Now,
		logger: slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.JWKSURL == "" {
		return nil, fmt.Errorf("%w: JWKSURL is empty", ErrConfig)
	}
	audiences := make(map[string]struct{}, len(cfg.Audiences))
	for _, aud := range cfg.Audiences {
		aud = strings.ToLower(strings.TrimSpace(aud))
		if aud != "" {
			audiences[aud] = struct{}{}
		}
	}
	if len(audiences) == 0 {
		return nil, fmt.Errorf("%w: no audiences: set the hosts this app serves", ErrConfig)
	}
	return &Verifier{
		audiences: audiences,
		clock:     o.clock,
		logger:    o.logger,
		keys:      newKeyCache(cfg.JWKSURL, o.client, o.clock),
	}, nil
}

// claims is the identity JWT's payload as guard signs it.
type claims struct {
	Issuer        string           `json:"iss"`
	Audience      jwt.Audience     `json:"aud"`
	Subject       string           `json:"sub"`
	Email         string           `json:"email"`
	EmailVerified bool             `json:"email_verified"`
	Name          string           `json:"name"`
	Roles         []string         `json:"roles"`
	SessionID     string           `json:"sid"`
	AuthTime      *jwt.NumericDate `json:"auth_time"`
	ClientID      string           `json:"azp"`
	Source        string           `json:"src"`
	IssuedAt      *jwt.NumericDate `json:"iat"`
	Expiry        *jwt.NumericDate `json:"exp"`
	NotBefore     *jwt.NumericDate `json:"nbf"`
}

// Verify checks token and returns the identity it carries. The error wraps
// ErrInvalid or ErrUnavailable.
func (v *Verifier) Verify(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, fmt.Errorf("%w: empty", ErrInvalid)
	}
	tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(tok.Headers) != 1 {
		return Identity{}, fmt.Errorf("%w: want one signature", ErrInvalid)
	}
	header := tok.Headers[0]
	if typ, _ := header.ExtraHeaders[jose.HeaderType].(string); normalizeType(typ) != normalizeType(TokenType) {
		return Identity{}, fmt.Errorf("%w: typ is not %s", ErrInvalid, TokenType)
	}
	if header.KeyID == "" {
		return Identity{}, fmt.Errorf("%w: no kid", ErrInvalid)
	}
	key, err := v.keys.key(ctx, header.KeyID)
	if err != nil {
		return Identity{}, err
	}
	var c claims
	if err = tok.Claims(key, &c); err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err = v.check(&c); err != nil {
		return Identity{}, err
	}
	return identityOf(&c), nil
}

// check applies the claim rules to a payload whose signature verified.
func (v *Verifier) check(c *claims) error {
	if c.Issuer != Issuer {
		return fmt.Errorf("%w: iss is not %s", ErrInvalid, Issuer)
	}
	if !v.acceptsAudience(c.Audience) {
		return fmt.Errorf("%w: aud is not a host this app serves", ErrInvalid)
	}
	if c.Subject == "" {
		return fmt.Errorf("%w: no sub", ErrInvalid)
	}
	now := v.clock().Unix()
	leeway := int64(Leeway / time.Second)
	switch {
	case c.Expiry == nil:
		return fmt.Errorf("%w: no exp", ErrInvalid)
	case int64(*c.Expiry) <= now-leeway:
		return fmt.Errorf("%w: expired", ErrInvalid)
	case c.IssuedAt == nil:
		return fmt.Errorf("%w: no iat", ErrInvalid)
	case int64(*c.IssuedAt) > now+leeway:
		return fmt.Errorf("%w: issued in the future", ErrInvalid)
	case c.NotBefore != nil && int64(*c.NotBefore) > now+leeway:
		return fmt.Errorf("%w: not valid yet", ErrInvalid)
	}
	return nil
}

func (v *Verifier) acceptsAudience(aud jwt.Audience) bool {
	for _, a := range aud {
		if _, ok := v.audiences[a]; ok {
			return true
		}
	}
	return false
}

func identityOf(c *claims) Identity {
	id := Identity{
		Subject:       c.Subject,
		Email:         c.Email,
		EmailVerified: c.EmailVerified,
		Name:          c.Name,
		Roles:         append([]string{}, c.Roles...),
		SessionID:     c.SessionID,
		ClientID:      c.ClientID,
		Source:        c.Source,
	}
	if c.AuthTime != nil && *c.AuthTime > 0 {
		id.AuthTime = c.AuthTime.Time()
	}
	return id
}

// normalizeType compares typ the way RFC 7515 section 4.1.9 and the jose
// library behind the TypeScript verifier do: case-insensitively, with the
// "application/" prefix optional.
func normalizeType(typ string) string {
	typ = strings.ToLower(typ)
	if strings.Contains(typ, "/") {
		return typ
	}
	return "application/" + typ
}
