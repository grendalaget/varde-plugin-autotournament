package plugin

import (
	"errors"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

const pluginID = "autotournament"

type Claims struct {
	GroupID string `json:"grp"`
	Role    string `json:"role"`
	jwt.RegisteredClaims
}

func verifyToken(secret []byte, authorization string) (Claims, error) {
	value, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || value == "" {
		return Claims{}, errors.New("missing bearer token")
	}
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(
		value,
		claims,
		func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, errors.New("unexpected signing method")
			}
			return secret, nil
		},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer("varde-control-plane"),
		jwt.WithAudience(pluginID),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)
	if err != nil {
		return Claims{}, errors.New("invalid bearer token")
	}
	if !token.Valid || claims.Subject == "" || claims.GroupID == "" {
		return Claims{}, errors.New("invalid bearer token")
	}
	return *claims, nil
}
