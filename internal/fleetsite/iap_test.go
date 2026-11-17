// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fleetsite

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"
)

const testAudience = "/projects/123456789/locations/us-central1/services/ate-fleet-site"

// jwkTransport answers every request, the fetch of IAP's keys, with keys.
type jwkTransport struct{ keys []byte }

func (t jwkTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(t.keys)),
		Request:    r,
	}, nil
}

type iapSigner struct {
	key *ecdsa.PrivateKey
	kid string
}

func newIAPSigner(t *testing.T, kid string) iapSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return iapSigner{key: key, kid: kid}
}

func (s iapSigner) jwk() map[string]string {
	pad := func(b []byte) string {
		out := make([]byte, 32)
		copy(out[32-len(b):], b)
		return base64.RawURLEncoding.EncodeToString(out)
	}
	return map[string]string{"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": s.kid,
		"x": pad(s.key.PublicKey.X.Bytes()), "y": pad(s.key.PublicKey.Y.Bytes())}
}

// sign returns an ES256 JWT with header alg and the claims.
func (s iapSigner) sign(t *testing.T, alg string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signed := enc(map[string]string{"alg": alg, "kid": s.kid, "typ": "JWT"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signed))
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newTestVerifier(t *testing.T, signers ...iapSigner) *IAPVerifier {
	t.Helper()
	var keys []map[string]string
	for _, s := range signers {
		keys = append(keys, s.jwk())
	}
	body, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewIAPVerifier(context.Background(), testAudience,
		option.WithHTTPClient(&http.Client{Transport: jwkTransport{keys: body}}))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func validClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":   iapIssuer,
		"aud":   testAudience,
		"iat":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(9 * time.Minute).Unix(),
		"sub":   "accounts.google.com:1234",
		"email": "member@example.com",
	}
}

// FS-7: the site takes the user's email from a valid IAP assertion only.
func TestIAPVerifierAcceptsAValidAssertion(t *testing.T) {
	signer := newIAPSigner(t, "k1")
	v := newTestVerifier(t, signer)
	email, err := v.Verify(context.Background(), signer.sign(t, "ES256", validClaims()))
	if err != nil {
		t.Fatal(err)
	}
	if email != "member@example.com" {
		t.Errorf("email = %q", email)
	}
}

// FS-7: every other assertion is denied, and none panics.
func TestIAPVerifierRejects(t *testing.T) {
	signer := newIAPSigner(t, "k1")
	other := newIAPSigner(t, "k1") // same kid, a key IAP does not publish
	v := newTestVerifier(t, signer)

	with := func(k string, val any) map[string]any {
		c := validClaims()
		if val == nil {
			delete(c, k)
		} else {
			c[k] = val
		}
		return c
	}
	valid := signer.sign(t, "ES256", validClaims())
	parts := strings.Split(valid, ".")
	for _, tc := range []struct {
		name, assertion string
	}{
		{"empty", ""},
		{"not a JWT", "abc"},
		{"too long", strings.Repeat("a", maxAssertionBytes+1)},
		{"other audience", signer.sign(t, "ES256", with("aud", "/projects/1/locations/us-central1/services/other"))},
		{"expired", signer.sign(t, "ES256", with("exp", time.Now().Add(-time.Second).Unix()))},
		{"other issuer", signer.sign(t, "ES256", with("iss", "https://accounts.google.com"))},
		{"issued in the future", signer.sign(t, "ES256", with("iat", time.Now().Add(time.Hour).Unix()))},
		{"no email", signer.sign(t, "ES256", with("email", nil))},
		{"no iat", signer.sign(t, "ES256", with("iat", nil))},
		{"email not a string", signer.sign(t, "ES256", with("email", 7))},
		{"other key", other.sign(t, "ES256", validClaims())},
		{"RS256 header", signer.sign(t, "RS256", validClaims())},
		{"none", signer.sign(t, "none", validClaims())},
		{"short signature", parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte("short"))},
		{"no signature", parts[0] + "." + parts[1] + "."},
		{"payload swapped", parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://cloud.google.com/iap","aud":"`+testAudience+`","exp":9999999999,"email":"evil@example.com"}`)) + "." + parts[2]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if email, err := v.Verify(context.Background(), tc.assertion); err == nil {
				t.Errorf("accepted as %q", email)
			}
		})
	}
}

// The algorithm and the length are refused by the site's own checks, before
// the library sees the token.
func TestIAPVerifierOwnChecks(t *testing.T) {
	signer := newIAPSigner(t, "k1")
	v := newTestVerifier(t, signer)
	for _, tc := range []struct{ assertion, want string }{
		{signer.sign(t, "RS256", validClaims()), "not signed with ES256"},
		{signer.sign(t, "HS256", validClaims()), "not signed with ES256"},
		{strings.Repeat("a", maxAssertionBytes+1), "too long"},
	} {
		if _, err := v.Verify(context.Background(), tc.assertion); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
}

func TestNewIAPVerifierChecksTheAudience(t *testing.T) {
	for _, aud := range []string{"", "ate-fleet-site", "/projects/my-project/locations/us-central1/services/s", "https://site.example.com"} {
		if _, err := NewIAPVerifier(context.Background(), aud); err == nil {
			t.Errorf("audience %q accepted", aud)
		}
	}
	for _, aud := range []string{testAudience, "/projects/42/global/backendServices/987", "/projects/42/apps/my-app"} {
		if _, err := NewIAPVerifier(context.Background(), aud); err != nil {
			t.Errorf("audience %q: %v", aud, err)
		}
	}
}
