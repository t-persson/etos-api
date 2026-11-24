// Copyright Axis Communications AB.
//
// For a full list of individual contributors, please see the commit history.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package auth

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/eiffel-community/etos-api/internal/authorization/scope"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/julienschmidt/httprouter"
)

type Authorizer struct {
	publicKey  crypto.PublicKey
	signingKey crypto.PrivateKey
}

type scopeContextKey struct{}

// ScopeFromContext returns the scope set by the authorization middleware.
func ScopeFromContext(ctx context.Context) (scope.Scope, bool) {
	s, ok := ctx.Value(scopeContextKey{}).(scope.Scope)
	return s, ok
}

// respond writes an authorization error and reports a failed response write.
func respond(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	if _, err := fmt.Fprint(w, message); err != nil {
		log.Printf("failed to write authorization response: %v", err)
	}
}

var (
	ErrTokenInvalid = errors.New("invalid token")
	ErrTokenExpired = jwt.ErrTokenExpired
)

// NewAuthorizer loads private and public pem keys and creates a new authorizer.
// The private key can be set to an empty []byte but it would only be possible to
// verify tokens and not create new ones.
func NewAuthorizer(pub, priv []byte) (*Authorizer, error) {
	var private crypto.PrivateKey
	var err error
	// Private key is optional, only needed for signing.
	if len(priv) > 0 {
		private, err = jwt.ParseEdPrivateKeyFromPEM(priv)
		if err != nil {
			return nil, err
		}
	}
	public, err := jwt.ParseEdPublicKeyFromPEM(pub)
	if err != nil {
		return nil, err
	}
	return &Authorizer{public, private}, nil
}

// NewToken generates a new JWT for an identifier.
func (a Authorizer) NewToken(identifier string, tokenScope scope.Scope, expire time.Time) (string, error) {
	if a.signingKey == nil {
		return "", errors.New("a private key must be provided to the authorizer to create new tokens")
	}
	token := jwt.NewWithClaims(
		jwt.SigningMethodEdDSA,
		jwt.MapClaims{
			"scope": tokenScope.Format(), // Custom scope type, similar to oauth2. Describes what a subject can do with this token
			"sub":   identifier,          // Subject
			"aud":   "https://etos",      // Audience. The service that can be accessed with this token.
			"iss":   "https://etos",      // Issuer
			"iat":   time.Now().Unix(),   // Issued At
			"exp":   expire.Unix(),       // Expiration
		})
	return token.SignedString(a.signingKey)
}

// VerifyToken verifies that a token is properly signed with a specific signing key and has not expired.
func (a Authorizer) VerifyToken(tokenString string) (*jwt.Token, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return a.publicKey, nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, ErrTokenInvalid
	}
	return token, nil
}

// Middleware implements an httprouter middleware to use for verifying authorization header JWTs.
// Scope is added to the request context and can be accessed with ScopeFromContext.
func (a Authorizer) Middleware(
	permittedScope scope.Var,
	fn func(http.ResponseWriter, *http.Request, httprouter.Params),
) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		tokenString := r.Header.Get("Authorization")
		if tokenString == "" {
			respond(w, http.StatusUnauthorized, "Missing authorization header")
			return
		}
		tokenString = tokenString[len("Bearer "):]
		token, err := a.VerifyToken(tokenString)
		if err != nil {
			if errors.Is(err, ErrTokenExpired) {
				respond(w, http.StatusUnauthorized, "token has expired")
				return
			}
			respond(w, http.StatusUnauthorized, "invalid token")
			return
		}
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			respond(w, http.StatusUnauthorized, "no valid claims in token")
			return
		}
		tokenScope, ok := claims["scope"]
		if !ok {
			respond(w, http.StatusUnauthorized, "no valid scope in token")
			return
		}
		scopeString, ok := tokenScope.(string)
		if !ok {
			respond(w, http.StatusForbidden, "no valid scope in token")
			return
		}
		s := scope.Parse(scopeString)
		if !s.Has(permittedScope) {
			respond(w, http.StatusForbidden, "no permission to view this page")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), scopeContextKey{}, s))
		fn(w, r, ps)
	}
}
