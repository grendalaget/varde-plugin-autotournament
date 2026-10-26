package plugin

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestVerifyToken(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	now := time.Now()
	for _, test := range []struct {
		name     string
		method   jwt.SigningMethod
		audience string
		expires  time.Time
		wantErr  bool
	}{
		{name: "valid", method: jwt.SigningMethodHS256, audience: pluginID, expires: now.Add(time.Minute)},
		{name: "wrong algorithm", method: jwt.SigningMethodHS384, audience: pluginID, expires: now.Add(time.Minute), wantErr: true},
		{name: "wrong audience", method: jwt.SigningMethodHS256, audience: "another-plugin", expires: now.Add(time.Minute), wantErr: true},
		{name: "expired", method: jwt.SigningMethodHS256, audience: pluginID, expires: now.Add(-time.Minute), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := jwt.MapClaims{
				"iss":  "varde-control-plane",
				"aud":  test.audience,
				"sub":  "user-1",
				"grp":  "group-1",
				"role": "member",
				"iat":  now.Unix(),
				"exp":  test.expires.Unix(),
				"jti":  "nonce",
			}
			token, err := jwt.NewWithClaims(test.method, claims).SignedString(secret)
			if err != nil {
				t.Fatal(err)
			}
			got, err := verifyToken(secret, "Bearer "+token)
			if test.wantErr {
				if err == nil {
					t.Fatalf("verifyToken() = %#v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.GroupID != "group-1" || got.Subject != "user-1" {
				t.Fatalf("claims = %#v", got)
			}
		})
	}
}
