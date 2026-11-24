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
package providerservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"

	"github.com/eiffel-community/eiffelevents-sdk-go"
	"github.com/eiffel-community/etos-api/internal/config"
	"github.com/eiffel-community/etos-api/internal/executionspace/provider"
	"github.com/eiffel-community/etos-api/pkg/application"
	httperrors "github.com/eiffel-community/etos-api/pkg/executionspace/errors"
	"github.com/eiffel-community/etos-api/pkg/executionspace/executionspace"
	"github.com/google/uuid"
	"github.com/julienschmidt/httprouter"
	"github.com/package-url/packageurl-go"
	"github.com/sirupsen/logrus"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	service_version  string
	otel_sdk_version string
)

type ProviderServiceApplication struct {
	logger   *logrus.Entry
	cfg      config.ExecutionSpaceConfig
	provider provider.Provider
	wg       *sync.WaitGroup
}

type ProviderServiceHandler struct {
	logger   *logrus.Entry
	cfg      config.ExecutionSpaceConfig
	provider provider.Provider
	wg       *sync.WaitGroup
}

type StartRequest struct {
	MinimumAmount     int                                                 `json:"minimum_amount"`
	MaximumAmount     int                                                 `json:"maximum_amount"`
	TestRunner        string                                              `json:"test_runner"`
	Environment       map[string]string                                   `json:"environment"`
	ArtifactIdentity  string                                              `json:"identity"`
	ArtifactID        string                                              `json:"artifact_id"`
	ArtifactCreated   eiffelevents.ArtifactCreatedV3                      `json:"artifact_created,omitempty"`
	ArtifactPublished eiffelevents.ArtifactPublishedV3                    `json:"artifact_published,omitempty"`
	TERCC             eiffelevents.TestExecutionRecipeCollectionCreatedV4 `json:"tercc,omitempty"`
	Dataset           Dataset                                             `json:"dataset,omitempty"`
	Context           uuid.UUID                                           `json:"context,omitempty"`
}

type Dataset struct {
	ETRBranch string `json:"ETR_BRANCH"`
	ETRRepo   string `json:"ETR_REPO"`
	DEV       bool   `json:"DEV"`
}

type StartResponse struct {
	ID uuid.UUID `json:"id"`
}

type StatusRequest struct {
	ID uuid.UUID `json:"id"`
}

// initTracer initializes the OpenTelemetry instrumentation for trace collection
func (a *ProviderServiceApplication) initTracer() {
	_, endpointSet := os.LookupEnv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if !endpointSet {
		a.logger.Infof("No OpenTelemetry collector is set. OpenTelemetry traces will not be available.")
		return
	}
	collector := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	a.logger.Infof("Using OpenTelemetry collector: %s", collector)

	// Create OTLP exporter to export traces
	exporter, err := otlptrace.New(context.Background(), otlptracegrpc.NewClient(
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithEndpoint(collector),
	))
	if err != nil {
		log.Fatal(err)
	}

	// Create a resource with service name attribute
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String("execution-space-provider"),
			semconv.ServiceNamespaceKey.String(os.Getenv("OTEL_SERVICE_NAMESPACE")),
			semconv.ServiceVersionKey.String(service_version),
			semconv.TelemetrySDKLanguageGo.Key.String("go"),
			semconv.TelemetrySDKNameKey.String("opentelemetry"),
			semconv.TelemetrySDKVersionKey.String(otel_sdk_version),
		),
	)
	if err != nil {
		log.Fatal(err)
	}

	// Create a TraceProvider with the exporter and resource
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	// Set the global TracerProvider
	otel.SetTracerProvider(tp)

	// Set the global propagator to TraceContext (W3C Trace Context)
	otel.SetTextMapPropagator(propagation.TraceContext{})
}

// Close waits for all active jobs to finish
func (a *ProviderServiceApplication) Close() {
	a.provider.Done()
	a.wg.Wait()
}

// New returns a new ProviderServiceApplication object/struct
func New(cfg config.ExecutionSpaceConfig, log *logrus.Entry, provider provider.Provider, ctx context.Context) application.Application {
	return &ProviderServiceApplication{
		logger:   log,
		cfg:      cfg,
		provider: provider,
		wg:       &sync.WaitGroup{},
	}
}

// LoadRoutes loads all the v1alpha1 routes.
func (a ProviderServiceApplication) LoadRoutes(router *httprouter.Router) {
	handler := &ProviderServiceHandler{a.logger, a.cfg, a.provider, a.wg}
	router.GET("/executionspace/v1alpha/selftest/ping", handler.Selftest)
	router.POST("/executionspace/start", handler.panicRecovery(handler.timeoutHandler(handler.Start)))
	router.GET("/executionspace/status", handler.panicRecovery(handler.timeoutHandler(handler.Status)))
	router.POST("/executionspace/stop", handler.panicRecovery(handler.timeoutHandler(handler.Stop)))

	router.POST(fmt.Sprintf("/executionspace/v1alpha/executor/%s", a.provider.Executor().Name()), handler.panicRecovery(handler.timeoutHandler(handler.ExecutorStart)))
	a.initTracer()
}

// getOtelTracer returns the current OpenTelemetry tracer
func (h ProviderServiceHandler) getOtelTracer() trace.Tracer {
	return otel.Tracer("execution-space-provider")
}

// getOtelContext returns OpenTelemetry context from the given HTTP request object
func (h ProviderServiceHandler) getOtelContext(ctx context.Context, r *http.Request) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))
}

// recordOtelException records an error to the given span
func (h ProviderServiceHandler) recordOtelException(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// Selftest is a handler to just return 204.
func (h ProviderServiceHandler) Selftest(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	RespondWithError(w, http.StatusNoContent, "")
}

// Start handles the start request and checks out execution spaces
func (h ProviderServiceHandler) Start(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	ctx := context.Background()
	identifier := r.Header.Get("X-Etos-Id")
	logger := h.logger.WithField("identifier", identifier).WithContext(ctx)
	checkoutId := uuid.New()

	ctx = h.getOtelContext(ctx, r)
	_, span := h.getOtelTracer().Start(ctx, "start", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()

	startReq, err := h.verifyStartInput(r)
	if err != nil {
		msg := fmt.Errorf("start input could not be verified: %s", err.Error())
		logger.Error(msg)
		h.recordOtelException(span, msg)
		sendError(w, msg)
		return
	}
	if startReq.MaximumAmount == 0 {
		startReq.MaximumAmount = startReq.MinimumAmount
	}
	if startReq.Dataset.ETRBranch != "" {
		startReq.Environment["ETR_BRANCH"] = startReq.Dataset.ETRBranch
	}
	if startReq.Dataset.ETRRepo != "" {
		startReq.Environment["ETR_REPOSITORY"] = startReq.Dataset.ETRRepo
	}
	if startReq.Dataset.DEV {
		startReq.Environment["DEV"] = "true"
	}

	go h.provider.Checkout(logger, ctx, provider.ExecutorConfig{
		Amount:         startReq.MaximumAmount,
		TestRunner:     startReq.TestRunner,
		Environment:    startReq.Environment,
		ETOSIdentifier: identifier,
		CheckoutID:     checkoutId,
	})
	span.SetAttributes(attribute.Int("etos.execution_space_provider.checkout.maximum_amount", startReq.MaximumAmount))
	span.SetAttributes(attribute.String("etos.execution_space_provider.checkout.test_runner", startReq.TestRunner))
	span.SetAttributes(attribute.String("etos.execution_space_provider.checkout.environment", fmt.Sprintf("%v", startReq.Environment)))
	span.SetAttributes(attribute.String("etos.execution_space_provider.checkout.id", checkoutId.String()))

	RespondWithJSON(w, http.StatusOK, StartResponse{ID: checkoutId})
}

// Status handles the status request, gets and returns the execution space checkout status
func (h ProviderServiceHandler) Status(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	identifier := r.Header.Get("X-Etos-Id")
	logger := h.logger.WithField("identifier", identifier).WithContext(r.Context())

	ctx, span := h.getOtelTracer().Start(h.getOtelContext(context.Background(), r), "status", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()

	id, err := uuid.Parse(r.URL.Query().Get("id"))
	if err != nil {
		msg := fmt.Errorf("error parsing id parameter in status request - Reason: %s", err.Error())
		logger.Error(msg)
		h.recordOtelException(span, msg)
		sendError(w, httperrors.NewHTTPError(msg, http.StatusBadRequest))
		return
	}

	executionSpace, err := h.provider.Status(logger, ctx, id)
	if err != nil {
		msg := fmt.Errorf("failed to retrieve execution space status (id=%s) - Reason: %s", id, err.Error())
		logger.Error(msg.Error())
		h.recordOtelException(span, msg)
		RespondWithJSON(w, http.StatusInternalServerError, executionSpace)
		return
	}

	for _, executorSpec := range executionSpace.Executors {
		span.SetAttributes(
			attribute.String("etos.execution_space_provider.status.executorspec", fmt.Sprintf("%v", executorSpec)),
		)
	}
	RespondWithJSON(w, http.StatusOK, executionSpace)
}

// Stop handles the stop request, stops the execution space executors and checks in all the provided execution spaces
func (h ProviderServiceHandler) Stop(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	h.wg.Add(1)
	defer h.wg.Done()
	identifier := r.Header.Get("X-Etos-Id")
	logger := h.logger.WithField("identifier", identifier).WithContext(r.Context())

	ctx := h.getOtelContext(context.Background(), r)
	_, span := h.getOtelTracer().Start(ctx, "stop", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()

	executors, err := executionspace.LoadExecutorSpecs(r.Body)
	if err != nil {
		msg := fmt.Errorf("failed to load executor spec: %s. Unable to decode post body: %v", err.Error(), err)
		logger.Error(msg)
		h.recordOtelException(span, msg)
		sendError(w, httperrors.NewHTTPError(msg, http.StatusBadRequest))
		return
	}
	defer func() {
		if err := r.Body.Close(); err != nil {
			logger.Errorf("failed to close request body: %v", err)
		}
	}()

	err = nil

	for _, executorSpec := range executors {
		id, jobInitErr := h.provider.Job(r.Context(), executorSpec.ID)
		if jobInitErr != nil {
			if errors.Is(jobInitErr, io.EOF) {
				// Already been checked in
				continue
			}
			err = errors.Join(err, jobInitErr)
			continue
		}
		// If the executorSpec does not exist in the database, we should not
		// try to stop the job (because we cannot find the job without the ID)
		if id == "" {
			continue
		}
		success := true
		if stopErr := h.provider.Executor().Stop(r.Context(), logger, id); stopErr != nil {
			success = false
			err = errors.Join(err, stopErr)
			msg := fmt.Errorf("failed to stop executor %v - Reason: %s", id, err.Error())
			logger.Error(msg)
			h.recordOtelException(span, msg)
		}
		span.SetAttributes(attribute.Bool(fmt.Sprintf("etos.execution_space_provider.stop.%v", id), success))
	}
	if err != nil {
		msg := fmt.Errorf("some of the executors could not be stopped - Reason: %s", err.Error())
		logger.Error(msg)
		h.recordOtelException(span, msg)
		RespondWithJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := h.provider.Checkin(logger, r.Context(), executors); err != nil {
		msg := fmt.Errorf("failed to check in executors: %v - Reason: %s", executors, err)
		logger.Error(msg)
		h.recordOtelException(span, msg)
		RespondWithJSON(w, http.StatusInternalServerError, msg)
		return
	}
	RespondWithJSON(w, http.StatusNoContent, "")
}

// sendError sends an error HTTP response depending on which error has been returned.
func sendError(w http.ResponseWriter, err error) {
	httpError, ok := err.(*httperrors.HTTPError)
	if !ok {
		RespondWithError(w, http.StatusInternalServerError, fmt.Sprintf("unknown error %+v", err))
	} else {
		RespondWithError(w, httpError.Code, httpError.Message)
	}
}

// verifyStartInput verify input (json body) from a start request
func (h ProviderServiceHandler) verifyStartInput(r *http.Request) (StartRequest, error) {
	request := StartRequest{}
	defer func() {
		if err := r.Body.Close(); err != nil {
			h.logger.Errorf("failed to close request body: %v", err)
		}
	}()
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, httperrors.NewHTTPError(
			fmt.Errorf("unable to decode post body - Reason: %s", err.Error()),
			http.StatusBadRequest,
		)
	}
	_, purlErr := packageurl.FromString(request.ArtifactIdentity)
	if purlErr != nil {
		return request, httperrors.NewHTTPError(purlErr, http.StatusBadRequest)
	}

	return request, nil
}

// timeoutHandler will change the request context to a timeout context.
func (h ProviderServiceHandler) timeoutHandler(
	fn func(http.ResponseWriter, *http.Request, httprouter.Params),
) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout())
		defer cancel()
		newRequest := r.WithContext(ctx)
		fn(w, newRequest, ps)
	}
}

// panicRecovery tracks panics from the service, logs them and returns an error response to the user.
func (h ProviderServiceHandler) panicRecovery(
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
