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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	auth "github.com/eiffel-community/etos-api/internal/authorization"
	"github.com/eiffel-community/etos-api/internal/authorization/scope"
	"github.com/eiffel-community/etos-api/internal/config"
	"github.com/eiffel-community/etos-api/pkg/application"
	"github.com/julienschmidt/httprouter"

	"github.com/sirupsen/logrus"
)

type Application struct {
	logger     *logrus.Entry
	cfg        config.KeyConfig
	ctx        context.Context
	cancel     context.CancelFunc
	authorizer *auth.Authorizer
}

type Handler struct {
	logger     *logrus.Entry
	cfg        config.KeyConfig
	ctx        context.Context
	authorizer *auth.Authorizer
}

// Close cancels the application context.
func (a *Application) Close() {
	a.cancel()
}

// New returns a new Application object/struct.
func New(ctx context.Context, cfg config.KeyConfig, log *logrus.Entry, authorizer *auth.Authorizer) application.Application {
	ctx, cancel := context.WithCancel(ctx)
	return &Application{
		logger:     log,
		cfg:        cfg,
		ctx:        ctx,
		cancel:     cancel,
		authorizer: authorizer,
	}
}

// LoadRoutes loads all the v2alpha routes.
func (a Application) LoadRoutes(router *httprouter.Router) {
	handler := &Handler{a.logger, a.cfg, a.ctx, a.authorizer}
	router.GET("/keys/v1alpha/selftest/ping", handler.Selftest)
	router.POST("/keys/v1alpha/generate", handler.CreateNew)
}

// Selftest is a handler to just return 204.
func (h Handler) Selftest(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNoContent)
}

// KeyResponse describes the response from the key server.
type KeyResponse struct {
	Token string `json:"token,omitempty"`
	Error string `json:"error,omitempty"`
}

// KeyRequest describes the request to the key server.
type KeyRequest struct {
	Scope    string `json:"scope"`
	Identity string `json:"identity"`
}

// CreateNew is a handler that can create new access tokens.
func (h Handler) CreateNew(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	identifier := ps.ByName("identifier")
	// Making it possible for us to correlate logs to a specific connection
	logger := h.logger.WithField("identifier", identifier)

	var keyRequest KeyRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&keyRequest); err != nil {
		logger.WithError(err).Warning("failed to decode json")
		w.WriteHeader(http.StatusBadRequest)
		response, _ := json.Marshal(KeyResponse{Error: "must provide one or several scopes to access"})
		_, _ = w.Write(response)
		return
	}
	logger.Info(keyRequest.Identity)
	requestedScope := scope.Parse(keyRequest.Scope)
	for _, s := range requestedScope {
		if !scope.AnonymousAccess.Has(s) {
			w.WriteHeader(http.StatusForbidden)
			response, _ := json.Marshal(KeyResponse{Error: fmt.Sprintf("not allowed to request the '%s' scope", s)})
			_, _ = w.Write(response)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	token, err := h.authorizer.NewToken(identifier, requestedScope, time.Now().Add(30*time.Minute))
	if err != nil {
		logger.WithError(err).Warning("failed to generate token")
		w.WriteHeader(http.StatusInternalServerError)
		response, _ := json.Marshal(KeyResponse{Error: "could not create a new token"})
		_, _ = w.Write(response)
		return
	}
	logger.Info("generated a new key")
	w.WriteHeader(http.StatusOK)
	response, _ := json.Marshal(KeyResponse{Token: token})
	_, _ = w.Write(response)
}
