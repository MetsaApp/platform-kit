package edgeauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// The JWKS cache's timings, shared with the TypeScript verifier. Guard
// publishes a key at least 10 minutes before signing with it, twice the
// cache lifetime.
const (
	// cacheTTL is how long a fetched JWKS is used without refetching.
	cacheTTL = 5 * time.Minute
	// refetchInterval is the least time between fetches triggered by a kid
	// that is not in a fresh cache, so a flood of forged kids costs guard
	// one request per interval.
	refetchInterval = 30 * time.Second
	// failureBackoff is how long a failed fetch is remembered before an
	// expired or empty cache tries again, so requests during an outage do
	// not each wait on guard.
	failureBackoff = 5 * time.Second
	// maxJWKSBytes bounds the JWKS response.
	maxJWKSBytes = 1 << 20
)

// keyCache holds guard's public keys by kid. Fetches are lazy and
// single-flight: one request fetches, the others wait for it.
type keyCache struct {
	url    string
	client *http.Client
	clock  func() time.Time

	mu        sync.Mutex
	keys      map[string]*ecdsa.PublicKey // nil until the first successful fetch
	fetchedAt time.Time                   // of the last successful fetch
	attemptAt time.Time                   // when the last fetch ended, successful or not
	lastErr   error                       // of the last fetch; nil when it succeeded
	inflight  chan struct{}               // closed when the running fetch ends
}

func newKeyCache(url string, client *http.Client, clock func() time.Time) *keyCache {
	return &keyCache{url: url, client: client, clock: clock}
}

// key returns the public key for kid, fetching the JWKS when the cache is
// expired, or when kid is unknown and the last fetch is old enough.
func (c *keyCache) key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	for {
		c.mu.Lock()
		if c.inflight != nil {
			wait := c.inflight
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, fmt.Errorf("%w: waiting for the JWKS: %w", ErrUnavailable, ctx.Err())
			}
		}
		key, done, err := c.lookup(kid)
		if done {
			c.mu.Unlock()
			return key, err
		}
		c.inflight = make(chan struct{})
		c.mu.Unlock()

		keys, err := c.fetch(ctx)

		c.mu.Lock()
		// The attempt is timed at its end, so a slow fetch still counts
		// against the refetch interval and the next pass decides from it.
		c.attemptAt, c.lastErr = c.clock(), err
		if err == nil {
			c.keys, c.fetchedAt = keys, c.attemptAt
		}
		close(c.inflight)
		c.inflight = nil
		c.mu.Unlock()
	}
}

// lookup decides from the cache alone, with c.mu held. done is false when the
// JWKS must be fetched first.
func (c *keyCache) lookup(kid string) (*ecdsa.PublicKey, bool, error) {
	now := c.clock()
	key, known := c.keys[kid]
	fresh := c.keys != nil && now.Sub(c.fetchedAt) < cacheTTL
	switch {
	case fresh && known:
		return key, true, nil
	case fresh:
		if now.Sub(c.attemptAt) >= refetchInterval {
			return nil, false, nil
		}
		if c.lastErr != nil {
			return nil, true, fmt.Errorf("%w: %w", ErrUnavailable, c.lastErr)
		}
		return nil, true, fmt.Errorf("%w: unknown kid", ErrInvalid)
	case c.lastErr != nil && now.Sub(c.attemptAt) < failureBackoff:
		// The cache is expired or empty and guard just failed to answer.
		// A key it published before is still good: rotation keeps an old
		// key published until no token signed by it is live.
		if known {
			return key, true, nil
		}
		return nil, true, fmt.Errorf("%w: %w", ErrUnavailable, c.lastErr)
	default:
		return nil, false, nil
	}
}

// fetch gets and parses the JWKS. It is detached from the caller's
// cancellation, because other requests wait on the same fetch.
func (c *keyCache) fetch(ctx context.Context) (map[string]*ecdsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("build JWKS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch JWKS: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return nil, fmt.Errorf("read JWKS: %w", err)
	}
	return parseJWKS(body)
}

// parseJWKS keeps the P-256 signing keys of a JWKS, by kid. Keys of another
// type, curve, use or alg are skipped, as are keys without a kid.
func parseJWKS(body []byte) (map[string]*ecdsa.PublicKey, error) {
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}
	keys := make(map[string]*ecdsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		pub, ok := k.Key.(*ecdsa.PublicKey)
		switch {
		case !ok, pub.Curve != elliptic.P256(), k.KeyID == "":
			continue
		case k.Use != "" && k.Use != "sig":
			continue
		case k.Algorithm != "" && k.Algorithm != string(jose.ES256):
			continue
		}
		keys[k.KeyID] = pub
	}
	return keys, nil
}
