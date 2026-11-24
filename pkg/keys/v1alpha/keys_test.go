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
package keys

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	auth "github.com/eiffel-community/etos-api/internal/authorization"
	"github.com/eiffel-community/etos-api/internal/authorization/scope"
	"github.com/eiffel-community/etos-api/internal/config"
	"github.com/eiffel-community/etos-api/test"
	"github.com/julienschmidt/httprouter"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

type testcfg struct {
	config.Config
	private []byte
	public  []byte
}

func (c testcfg) PrivateKey() ([]byte, error) {
	return c.private, nil
}

// TestCreateNew tests that it is possible to create a new token via the keys API.
func TestCreateNew(t *testing.T) {
	log := logrus.WithFields(logrus.Fields{})
	priv, pub, err := test.NewKeys()
	assert.NoError(t, err)
	cfg := testcfg{private: priv, public: pub}
	authorizer, err := auth.NewAuthorizer(pub, priv)
	assert.NoError(t, err)
	handler := Handler{log, cfg, context.Background(), authorizer}

	responseRecorder := httptest.NewRecorder()
	ps := httprouter.Params{httprouter.Param{Key: "identifier", Value: "Hello"}}
	requestData := []byte(fmt.Sprintf(`{"scope":"%s","identity":"TestCreateNew"}`, scope.StreamSSE))
	request := httptest.NewRequest("POST", "/v1alpha/generate", bytes.NewReader(requestData))
	handler.CreateNew(responseRecorder, request, ps)
	assert.Equal(t, responseRecorder.Code, http.StatusOK, responseRecorder.Body.String())

	var keyResponse KeyResponse
	decoder := json.NewDecoder(responseRecorder.Body)
	err = decoder.Decode(&keyResponse)
	assert.NoError(t, err)
	assert.Empty(t, keyResponse.Error)
	_, err = authorizer.VerifyToken(keyResponse.Token)
	assert.NoError(t, err)
}

// TestCreateNewWrongScope tests that it is not possible to create a new token if the scope is outside of anonymous access.
func TestCreateNewWrongScope(t *testing.T) {
	log := logrus.WithFields(logrus.Fields{})
	priv, pub, err := test.NewKeys()
	assert.NoError(t, err)
	cfg := testcfg{private: priv, public: pub}
	authorizer, err := auth.NewAuthorizer(pub, priv)
	assert.NoError(t, err)
	handler := Handler{log, cfg, context.Background(), authorizer}

	responseRecorder := httptest.NewRecorder()
	ps := httprouter.Params{httprouter.Param{Key: "identifier", Value: "Hello"}}
	requestData := []byte(fmt.Sprintf(`{"scope":"%s","identity":"TestCreateNew"}`, scope.DefineSSE))
	request := httptest.NewRequest("POST", "/v1alpha/generate", bytes.NewReader(requestData))
	handler.CreateNew(responseRecorder, request, ps)
	assert.Equal(t, responseRecorder.Code, http.StatusForbidden, responseRecorder.Body.String())

	var keyResponse KeyResponse
	decoder := json.NewDecoder(responseRecorder.Body)
	err = decoder.Decode(&keyResponse)
	assert.NoError(t, err)
	assert.NotEmpty(t, keyResponse.Error)
	assert.Empty(t, keyResponse.Token)
}
