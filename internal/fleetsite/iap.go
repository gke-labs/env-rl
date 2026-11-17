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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"google.golang.org/api/idtoken"
)

// IAPHeader carries the assertion Identity-Aware Proxy signs for each request.
const IAPHeader = "X-Goog-Iap-Jwt-Assertion"

// iapIssuer is the iss claim of every IAP assertion.
const iapIssuer = "https://cloud.google.com/iap"

// iapAudience matches the audiences IAP signs for: a Cloud Run service, a
// backend service, or an App Engine app.
var iapAudience = regexp.MustCompile(`^/projects/[0-9]+/(locations/[a-z0-9-]+/services/[a-z0-9-]+|global/backendServices/[0-9]+|apps/[a-z0-9-]+)$`)

// clockSkew is the tolerance on an assertion's iat.
const clockSkew = time.Minute

// maxAssertionBytes bounds the header before it is parsed.
const maxAssertionBytes = 8 << 10

// IAPVerifier checks IAP's signed assertion and returns the user's email.
type IAPVerifier struct {
	validator *idtoken.Validator
	audience  string
	now       func() time.Time
}

// NewIAPVerifier returns a verifier for audience, the resource IAP protects,
// for Cloud Run /projects/<number>/locations/<region>/services/<service>.
// opts configure the HTTP client that fetches IAP's public keys.
func NewIAPVerifier(ctx context.Context, audience string, opts ...idtoken.ClientOption) (*IAPVerifier, error) {
	if !iapAudience.MatchString(audience) {
		return nil, fmt.Errorf("IAP audience %q is not /projects/<number>/locations/<region>/services/<service> (or a backend service or App Engine audience)", audience)
	}
	v, err := idtoken.NewValidator(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &IAPVerifier{validator: v, audience: audience, now: time.Now}, nil
}

// Verify checks the assertion: ES256 only, signed by an IAP key, for the
// audience, unexpired, issued by IAP, issued in the past, with an email.
func (v *IAPVerifier) Verify(ctx context.Context, assertion string) (string, error) {
	if assertion == "" {
		return "", errors.New("no IAP assertion")
	}
	if len(assertion) > maxAssertionBytes {
		return "", errors.New("the IAP assertion is too long")
	}
	// The algorithm is fixed before the library sees the token: idtoken
	// also accepts RS256 ID tokens that any Google service account can mint
	// for any audience, and it slices an ES256 signature without checking
	// its length.
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return "", errors.New("the IAP assertion is not a JWT")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", errors.New("the IAP assertion header is not base64url")
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(header, &h); err != nil || h.Alg != "ES256" {
		return "", errors.New("the IAP assertion is not signed with ES256")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return "", errors.New("the IAP assertion signature is not an ES256 signature")
	}

	payload, err := v.validator.Validate(ctx, assertion, v.audience)
	if err != nil {
		return "", fmt.Errorf("the IAP assertion is not valid: %w", err)
	}
	if payload.Issuer != iapIssuer {
		return "", fmt.Errorf("the IAP assertion has issuer %q, not %s", payload.Issuer, iapIssuer)
	}
	if payload.IssuedAt == 0 {
		return "", errors.New("the IAP assertion has no iat")
	}
	if time.Unix(payload.IssuedAt, 0).After(v.now().Add(clockSkew)) {
		return "", errors.New("the IAP assertion is issued in the future")
	}
	email, _ := payload.Claims["email"].(string)
	if email == "" {
		return "", errors.New("the IAP assertion has no email")
	}
	return email, nil
}
