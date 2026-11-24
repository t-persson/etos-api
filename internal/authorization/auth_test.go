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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eiffel-community/etos-api/internal/authorization/scope"
	"github.com/eiffel-community/etos-api/test"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/julienschmidt/httprouter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewToken tests that it is possible to sign a new token with the authorizer.
func TestNewToken(t *testing.T) {
	priv, pub, err := test.NewKeys()
	assert.NoError(t, err)
	authorizer, err := NewAuthorizer(pub, priv)
	assert.NoError(t, err)
	publicKey, err := jwt.ParseEdPublicKeyFromPEM(pub)
	require.NoError(t, err)

	tests := []struct {
		name  string
		scope scope.Scope
	}{
		{name: "TestNewAnonymousToken", scope: scope.AnonymousAccess},
		{name: "TestNewAdminToken", scope: scope.AdminAccess},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			tokenString, err := authorizer.NewToken(testCase.name, testCase.scope, time.Now().Add(time.Second*5))
			assert.NoError(t, err)
			token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
				return publicKey, nil
			})
			require.NoError(t, err)
			subject, err := token.Claims.GetSubject()
			assert.NoError(t, err)
			assert.Equal(t, subject, testCase.name)
			claims, ok := token.Claims.(jwt.MapClaims)
			assert.True(t, ok)
			tokenScope, ok := claims["scope"]
			assert.True(t, ok)
			scopeString, ok := tokenScope.(string)
			assert.True(t, ok)
			s := scope.Parse(scopeString)
			assert.Equal(t, s, testCase.scope)
		})
	}
}

// TestVerifyToken tests that the authorizer can verify a signed token with a public key.
func TestVerifyToken(t *testing.T) {
	priv, pub, err := test.NewKeys()
	assert.NoError(t, err)
	authorizer, err := NewAuthorizer(pub, priv)
	assert.NoError(t, err)
	tokenString, err := authorizer.NewToken("TestVerifyToken", scope.Scope{}, time.Now().Add(time.Second*1))
	assert.NoError(t, err)
	_, err = authorizer.VerifyToken(tokenString)
	assert.NoError(t, err)
	time.Sleep(2 * time.Second)
	_, err = authorizer.VerifyToken(tokenString)
	// Should have expired after 2 seconds
	assert.Error(t, err)
}

// TestMiddleware tests that the authorizer middleware blocks unauthorized requests to an endpoint and lets authorized requests through.
func TestMiddleware(t *testing.T) {
	priv, pub, err := test.NewKeys()
	assert.NoError(t, err)
	authorizer, err := NewAuthorizer(pub, priv)
	assert.NoError(t, err)

	tests := []struct {
		name           string
		scope          scope.Scope
		permittedScope scope.Var
		header         bool
		expire         time.Time
		expected       int
	}{
		{name: "TestMiddlewareNoHeader", header: false, expected: http.StatusUnauthorized, expire: time.Now().Add(5 * time.Second)},
		{name: "TestMiddlewareExpiredToken", header: true, expected: http.StatusUnauthorized, expire: time.Now()},
		{name: "TestMiddlewareNoScope", header: true, scope: scope.Scope{}, permittedScope: "not-this-one", expected: http.StatusForbidden, expire: time.Now().Add(5 * time.Second)},
		{name: "TestMiddlewareWrongScope", header: true, scope: scope.Scope{scope.Var("a-scope")}, permittedScope: "not-this-one", expected: http.StatusForbidden, expire: time.Now().Add(5 * time.Second)},
		{name: "TestMiddlewarePassThrough", header: true, scope: scope.Scope{scope.Var("a-scope")}, permittedScope: "a-scope", expected: http.StatusOK, expire: time.Now().Add(5 * time.Second)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var gotThrough bool
			endpoint := func(_ http.ResponseWriter, r *http.Request, _ httprouter.Params) {
				gotThrough = true
				actual, ok := ScopeFromContext(r.Context())
				assert.True(t, ok)
				assert.Equal(t, testCase.scope, actual)
			}
			token, err := authorizer.NewToken(testCase.name, testCase.scope, testCase.expire)
			assert.NoError(t, err)

			responseRecorder := httptest.NewRecorder()
			ps := httprouter.Params{}
			request := httptest.NewRequest("GET", "/my/test", nil)
			if testCase.header {
				request.Header.Add("Authorization", fmt.Sprintf("Bearer %s", token))
			}
			authorizer.Middleware(testCase.permittedScope, endpoint)(responseRecorder, request, ps)
			assert.Equal(t, responseRecorder.Code, testCase.expected, responseRecorder.Body.String())
			if testCase.expected >= 400 {
				assert.False(t, gotThrough)
			} else {
				assert.True(t, gotThrough)
			}
		})
	}
}
