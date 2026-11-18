package edgeauth

import (
	"crypto/elliptic"
	"encoding/json"
	"os"
	"testing"
)

// FuzzParseJWKS feeds arbitrary bodies to the JWKS parser, the bytes guard's
// key endpoint (or anything answering in its place) sends. It must not
// panic, and every key it keeps must be a P-256 point on the curve under a
// non-empty kid: that is all Verify will check a signature against.
func FuzzParseJWKS(f *testing.F) {
	f.Add([]byte(`{"keys":[]}`))
	f.Add([]byte(`{"keys":[{"kty":"oct","k":"c2VjcmV0","kid":"h"}]}`))
	f.Add([]byte(`{"keys":[{"kty":"EC","crv":"P-384","x":"","y":"","kid":"p384"}]}`))
	f.Add([]byte(`{"keys":[{"kty":"RSA","n":"AQAB","e":"AQAB","kid":"r"}]}`))
	f.Add([]byte(`null`))
	if raw, err := os.ReadFile("testdata/vectors.json"); err == nil {
		var file struct {
			JWKS json.RawMessage `json:"jwks"`
		}
		if json.Unmarshal(raw, &file) == nil {
			f.Add([]byte(file.JWKS))
		}
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		keys, err := parseJWKS(body)
		if err != nil {
			return
		}
		for kid, pub := range keys {
			if kid == "" {
				t.Fatal("kept a key without a kid")
			}
			if pub == nil || pub.Curve != elliptic.P256() {
				t.Fatalf("kept key %q that is not P-256", kid)
			}
			if !pub.IsOnCurve(pub.X, pub.Y) { //nolint:staticcheck // the check this target exists for
				t.Fatalf("kept key %q that is not on the curve", kid)
			}
		}
	})
}
