package edgeauth_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MetsaApp/platform-kit/edgeauth"
	"github.com/MetsaApp/platform-kit/edgeauth/internal/vectors"
)

// staticJWKS answers every request with the same JWKS, without a listener,
// so a fuzz run is not bound by sockets.
type staticJWKS []byte

func (s staticJWKS) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(s)),
	}, nil
}

// FuzzVerify feeds arbitrary tokens to a verifier that trusts the vectors'
// key. Whatever the input, Verify must not panic, must fail with ErrInvalid
// (the JWKS is always reachable here), and may accept a token only when its
// signed part (header and payload) is that of a valid vector: the signature
// binds them, so no mutation of either may pass.
func FuzzVerify(f *testing.F) {
	raw, err := os.ReadFile(vectorsFile)
	if err != nil {
		f.Fatal(err)
	}
	var file vectors.File
	if err = json.Unmarshal(raw, &file); err != nil {
		f.Fatal(err)
	}
	jwks, err := json.Marshal(file.JWKS)
	if err != nil {
		f.Fatal(err)
	}
	signed := map[string]bool{}
	for _, c := range file.Cases {
		f.Add(c.Token)
		if c.Want.OK && len(c.Audiences) == 1 && c.Audiences[0] == vectors.Audience {
			signed[signedPart(c.Token)] = true
		}
	}
	now := time.Unix(file.Now, 0)
	v, err := edgeauth.New(
		edgeauth.Config{JWKSURL: "http://jwks.invalid/jwks.json", Audiences: []string{vectors.Audience}},
		edgeauth.WithClock(func() time.Time { return now }),
		edgeauth.WithHTTPClient(&http.Client{Transport: staticJWKS(jwks)}),
	)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, token string) {
		id, verr := v.Verify(t.Context(), token)
		if verr != nil {
			if !errors.Is(verr, edgeauth.ErrInvalid) {
				t.Fatalf("Verify(%q): %v does not wrap ErrInvalid", token, verr)
			}
			return
		}
		if !signed[signedPart(token)] {
			t.Fatalf("Verify(%q) accepted a token whose signed part is not a valid vector's", token)
		}
		if id.Subject == "" {
			t.Fatalf("Verify(%q) accepted a token without sub", token)
		}
	})
}

// signedPart is a compact JWS without its signature.
func signedPart(token string) string {
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		return token
	}
	return token[:i]
}
