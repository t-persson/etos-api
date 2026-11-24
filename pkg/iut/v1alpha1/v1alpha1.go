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
package v1alpha1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"
	"time"

	eiffelevents "github.com/eiffel-community/eiffelevents-sdk-go"
	"github.com/eiffel-community/etos-api/internal/config"
	"github.com/eiffel-community/etos-api/internal/database"
	"github.com/eiffel-community/etos-api/pkg/application"
	packageurl "github.com/package-url/packageurl-go"

	"github.com/google/uuid"
	"github.com/julienschmidt/httprouter"
	"github.com/sirupsen/logrus"
)

type V1Alpha1Application struct {
	logger   *logrus.Entry
	cfg      config.IUTConfig
	database database.Opener
	wg       *sync.WaitGroup
}

type V1Alpha1Handler struct {
	logger   *logrus.Entry
	cfg      config.IUTConfig
	database database.Opener
	wg       *sync.WaitGroup
}

type Dataset struct{}

// RespondWithJSON writes a JSON response with a status code to the HTTP ResponseWriter.
func RespondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, _ := json.Marshal(payload)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(response)
}

// RespondWithError writes a JSON response with an error message and status code to the HTTP ResponseWriter.
func RespondWithError(w http.ResponseWriter, code int, message string) {
	RespondWithJSON(w, code, map[string]string{"error": message})
}

// Close does nothing atm. Present for interface coherence
func (a *V1Alpha1Application) Close() {
	a.wg.Wait()
}

// New returns a new V1Alpha1Application object/struct
func New(cfg config.IUTConfig, log *logrus.Entry, ctx context.Context, db database.Opener) application.Application {
	return &V1Alpha1Application{
		logger:   log,
		cfg:      cfg,
		database: db,
		wg:       &sync.WaitGroup{},
	}
}

// LoadRoutes loads all the v1alpha1 routes.
func (a V1Alpha1Application) LoadRoutes(router *httprouter.Router) {
	handler := &V1Alpha1Handler{a.logger, a.cfg, a.database, a.wg}
	router.GET("/iut/v1alpha1/selftest/ping", handler.Selftest)
	router.POST("/iut/start", handler.panicRecovery(handler.timeoutHandler(handler.Start)))
	router.GET("/iut/status", handler.panicRecovery(handler.timeoutHandler(handler.Status)))
	router.POST("/iut/stop", handler.panicRecovery(handler.timeoutHandler(handler.Stop)))
}

// Selftest is a handler to just return 204.
func (h V1Alpha1Handler) Selftest(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	RespondWithError(w, http.StatusNoContent, "")
}

type StartRequest struct {
	MinimumAmount     int                                                 `json:"minimum_amount"`
	MaximumAmount     int                                                 `json:"maximum_amount"`
	ArtifactIdentity  string                                              `json:"identity"`
	ArtifactID        string                                              `json:"artifact_id"`
	ArtifactCreated   eiffelevents.ArtifactCreatedV3                      `json:"artifact_created,omitempty"`
	ArtifactPublished eiffelevents.ArtifactPublishedV3                    `json:"artifact_published,omitempty"`
	TERCC             eiffelevents.TestExecutionRecipeCollectionCreatedV4 `json:"tercc,omitempty"`
	Context           uuid.UUID                                           `json:"context,omitempty"`
	Dataset           Dataset                                             `json:"dataset,omitempty"`
}

type StartResponse struct {
	Id uuid.UUID `json:"id"`
}

type StatusResponse struct {
	Id     uuid.UUID               `json:"id"`
	Status string                  `json:"status"`
	Iuts   []packageurl.PackageURL `json:"iuts"`
}

type StatusRequest struct {
	Id uuid.UUID `json:"id"`
}

// Start creates a number of IUTs and stores them in the ETCD database returning a checkout ID.
func (h V1Alpha1Handler) Start(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	identifier, err := uuid.Parse(r.Header.Get("X-Etos-Id"))
	logger := h.logger.WithField("identifier", identifier).WithContext(r.Context())
	if err != nil {
		RespondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	checkOutID := uuid.New()

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/json")

	var startReq StartRequest
	if err := json.NewDecoder(r.Body).Decode(&startReq); err != nil {
		logger.Errorf("Failed to decode request body: %s", r.Body)
		RespondWithError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer func() {
		if err := r.Body.Close(); err != nil {
			logger.Errorf("failed to close request body: %v", err)
		}
	}()
	purl, err := packageurl.FromString(startReq.ArtifactIdentity)
	if err != nil {
		logger.Errorf("Failed to create a purl struct from artifact identity: %s", startReq.ArtifactIdentity)
		RespondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	purls := make([]packageurl.PackageURL, startReq.MinimumAmount)
	for i := range purls {
		purls[i] = purl
	}
	iuts, err := json.Marshal(purls)
	if err != nil {
		logger.Errorf("Failed to marshal purls: %s", purls)
		RespondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	client := h.database.Open(r.Context(), identifier)
	_, err = client.Write([]byte(string(iuts)))
	if err != nil {
		logger.Errorf("Failed to write to database: %s", string(iuts))
		RespondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	startResp := StartResponse{Id: checkOutID}
	logger.Debugf("Start response: %s", startResp)
	w.WriteHeader(http.StatusOK)
	response, _ := json.Marshal(startResp)
	_, _ = w.Write(response)
}

// Status creates a simple DONE Status response with IUTs.
func (h V1Alpha1Handler) Status(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	identifier, err := uuid.Parse(r.Header.Get("X-Etos-Id"))
	if err != nil {
		RespondWithError(w, http.StatusInternalServerError, err.Error())
	}
	logger := h.logger.WithField("identifier", identifier).WithContext(r.Context())

	id, err := uuid.Parse(r.URL.Query().Get("id"))
	if err != nil {
		RespondWithError(w, http.StatusBadRequest, "invalid status id")
		return
	}
	client := h.database.Open(r.Context(), identifier)

	data, err := io.ReadAll(client)
	if err != nil {
		logger.Errorf("Failed to look up status request id: %s, %s", identifier, err.Error())
		RespondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	statusResp := StatusResponse{
		Id:     id,
		Status: "DONE",
	}
	if err = json.Unmarshal(data, &statusResp.Iuts); err != nil {
		logger.Errorf("Failed to unmarshal data: %s", data)
		RespondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	response, err := json.Marshal(statusResp)
	if err != nil {
		logger.Errorf("Failed to marshal status response: %s", statusResp)
		RespondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logger.Debugf("Status response: %s", statusResp)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response)
}

// Stop deletes the given IUTs from the database and returns an empty response.
func (h V1Alpha1Handler) Stop(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	identifier, err := uuid.Parse(r.Header.Get("X-Etos-Id"))
	if err != nil {
		RespondWithError(w, http.StatusInternalServerError, err.Error())
	}
	logger := h.logger.WithField("identifier", identifier).WithContext(r.Context())

	client := h.database.Open(r.Context(), identifier)
	deleter, canDelete := client.(database.Deleter)
	if !canDelete {
		logger.Warning("The database does not support delete. Writing nil.")
		_, err = client.Write(nil)
	} else {
		err = deleter.Delete()
	}

	if err != nil {
		logger.Errorf("Database delete failed: %s", err.Error())
		RespondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logger.Debugf("Stop request succeeded")
	w.WriteHeader(http.StatusNoContent)
}

// timeoutHandler will change the request context to a timeout context.
func (h V1Alpha1Handler) timeoutHandler(
	fn func(http.ResponseWriter, *http.Request, httprouter.Params),
) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		newRequest := r.WithContext(ctx)
		fn(w, newRequest, ps)
	}
}

// panicRecovery tracks panics from the service, logs them and returns an error response to the user.
func (h V1Alpha1Handler) panicRecovery(
	fn func(http.ResponseWriter, *http.Request, httprouter.Params),
) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		defer func() {
			if err := recover(); err != nil {
				buf := make([]byte, 2048)
				n := runtime.Stack(buf, false)
				buf = buf[:n]
				h.logger.WithField(
					"identifier", ps.ByName("identifier"),
				).WithContext(
					r.Context(),
				).Errorf("recovering from err %+v\n %s", err, buf)
				identifier := ps.ByName("identifier")
				RespondWithError(
					w,
					http.StatusInternalServerError,
					fmt.Sprintf("unknown error: contact server admin with id '%s'", identifier),
				)
			}
		}()
		fn(w, r, ps)
	}
}
