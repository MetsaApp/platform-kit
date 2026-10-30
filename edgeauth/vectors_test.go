package edgeauth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MetsaApp/platform-kit/edgeauth"
	"github.com/MetsaApp/platform-kit/edgeauth/internal/vectors"
)

const vectorsFile = "testdata/vectors.json"

func TestVectorsAreCurrent(t *testing.T) {
	t.Parallel()
	want, err := vectors.Generate()
	require.NoError(t, err)
	got, err := os.ReadFile(vectorsFile)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got), "testdata/vectors.json is stale: run go generate ./edgeauth")
}

func TestVectors(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(vectorsFile)
	require.NoError(t, err)
	var file vectors.File
	require.NoError(t, json.Unmarshal(raw, &file))
	jwks, err := json.Marshal(file.JWKS)
	require.NoError(t, err)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwks)
	}))
	t.Cleanup(up.Close)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	now := time.Unix(file.Now, 0)
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			url := up.URL
			if c.JWKSUnreachable {
				url = down.URL
			}
			v, nerr := edgeauth.New(
				edgeauth.Config{JWKSURL: url, Audiences: c.Audiences},
				edgeauth.WithClock(func() time.Time { return now }),
			)
			require.NoError(t, nerr)
			id, verr := v.Verify(t.Context(), c.Token)
			switch c.Want.Error {
			case vectors.Invalid:
				require.ErrorIs(t, verr, edgeauth.ErrInvalid)
				return
			case vectors.Unavailable:
				require.ErrorIs(t, verr, edgeauth.ErrUnavailable)
				return
			}
			require.NoError(t, verr)
			assert.Equal(t, wantIdentity(c.Want), id)
		})
	}
}

func wantIdentity(w vectors.Want) edgeauth.Identity {
	id := edgeauth.Identity{
		Subject:       w.Subject,
		Email:         w.Email,
		EmailVerified: w.EmailVerified,
		Name:          w.Name,
		Roles:         append([]string{}, w.Roles...),
		SessionID:     w.SessionID,
		ClientID:      w.ClientID,
		Source:        w.Source,
	}
	if w.AuthTime != 0 {
		id.AuthTime = time.Unix(w.AuthTime, 0)
	}
	return id
}
