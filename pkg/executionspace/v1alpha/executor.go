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
	"net/http"
	"time"

	"github.com/eiffel-community/eiffelevents-sdk-go"
	"github.com/eiffel-community/etos-api/internal/config"
	"github.com/eiffel-community/etos-api/internal/eventrepository"
	"github.com/eiffel-community/etos-api/internal/executionspace/executor"
	"github.com/eiffel-community/etos-api/pkg/executionspace/executionspace"
	"github.com/google/uuid"
	"github.com/julienschmidt/httprouter"
	"github.com/sethvargo/go-retry"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type startRequest struct {
	ID uuid.UUID `json:"id"`
}

// Start starts up a testrunner job and waits for it to start completely
func (h ProviderServiceHandler) ExecutorStart(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	h.wg.Add(1)
	defer h.wg.Done()

	identifier := r.Header.Get("X-Etos-Id")
	// This context is used until we can retrieve the timeout we shall be using from the executorSpec.
	ctx, cancelRequest := context.WithCancel(r.Context())
	defer cancelRequest()
	logger := h.logger.WithField("identifier", identifier).WithContext(ctx)

	_, span := h.getOtelTracer().Start(h.getOtelContext(ctx, r), "start_executor", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()

	executorName := h.provider.Executor().Name()
	request := startRequest{}
	defer func() {
		if err := r.Body.Close(); err != nil {
			logger.Errorf("failed to close request body: %v", err)
		}
	}()
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		msg := fmt.Errorf("there was an error when preparing the %s execution space", executorName)
		logger.WithField("user_log", true).Error(msg)
		h.recordOtelException(span, msg)
		RespondWithError(w, http.StatusBadRequest, "could not read ID from post body")
		return
	}

	executor, err := h.provider.ExecutorSpec(ctx, request.ID)
	if err != nil {
		msg := fmt.Errorf("timed out when reading the %s execution space configuration from database", executorName)
		if ctx.Err() != nil {
			logger.WithField("user_log", true).Error(msg)
			h.recordOtelException(span, msg)
			RespondWithError(w, http.StatusRequestTimeout, msg.Error())
			return
		}
		RespondWithError(w, http.StatusBadRequest, msg.Error())
		logger.WithField("user_log", true).Error(msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second*time.Duration(executor.Request.Timeout))
	defer cancel()

	id, err := h.provider.Executor().Start(ctx, logger, executor)
	if err != nil {
		if ctx.Err() != nil {
			msg := fmt.Errorf("timed out when trying to start the test execution job")
			RespondWithError(w, http.StatusRequestTimeout, msg.Error())
			logger.WithField("user_log", true).Error(msg)
			h.recordOtelException(span, msg)
			return
		}
		msg := fmt.Errorf("error trying to start the test execution job: %s", err.Error())
		RespondWithError(w, http.StatusInternalServerError, msg.Error())
		logger.WithField("user_log", true).Error(msg)
		h.recordOtelException(span, msg)
		return
	}

	buildID, buildURL, err := h.provider.Executor().Wait(ctx, logger, id, executor)
	if err != nil {
		if cancelErr := h.provider.Executor().Cancel(context.Background(), logger, id); cancelErr != nil {
			msg := fmt.Errorf("cancel failed: %s", cancelErr.Error())
			logger.Error(msg)
			h.recordOtelException(span, msg)
		}
		if ctx.Err() != nil {
			msg := fmt.Errorf("timed out when waiting for the test execution job to start - Error: %s", err.Error())
			RespondWithError(w, http.StatusRequestTimeout, msg.Error())
			logger.WithField("user_log", true).Error(msg)
			h.recordOtelException(span, msg)
			return
		}
		msg := fmt.Errorf("error when waiting for the test execution job to start - Error: %s", err.Error())
		RespondWithError(w, http.StatusInternalServerError, msg.Error())
		logger.WithField("user_log", true).Error(msg)
		h.recordOtelException(span, msg)
		return
	}
	executor.BuildID = buildID

	if err := h.provider.SaveExecutor(ctx, *executor); err != nil {
		logger.Error(err.Error())
		if cancelErr := h.provider.Executor().Stop(context.Background(), logger, buildID); cancelErr != nil {
			msg := fmt.Errorf("cancel failed: %s", cancelErr.Error())
			logger.Error(msg)
			h.recordOtelException(span, msg)
		}
		if ctx.Err() != nil {
			msg := fmt.Errorf("timed out when saving the test execution configuration")
			RespondWithError(w, http.StatusRequestTimeout, msg.Error())
			logger.WithField("user_log", true).Error(msg)
			h.recordOtelException(span, msg)
			return
		}
		msg := fmt.Errorf("error when saving the test execution configuration")
		RespondWithError(w, http.StatusInternalServerError, msg.Error())
		logger.WithField("user_log", true).Error(msg)
		h.recordOtelException(span, msg)
		return
	}

	subSuiteState := state{ExecutorSpec: executor}
	if err = subSuiteState.waitStart(ctx, h.cfg, logger, h.provider.Executor()); err != nil {
		if cancelErr := h.provider.Executor().Stop(context.Background(), logger, buildID); cancelErr != nil {
			msg := fmt.Errorf("cancel failed: %s", cancelErr.Error())
			logger.Error(msg)
		}
		if ctx.Err() != nil {
			msg := fmt.Errorf("timed out when waiting for the test execution job to initialize - Error: %s", err.Error())
			RespondWithError(w, http.StatusRequestTimeout, msg.Error())
			logger.WithField("user_log", true).Error(msg)
			h.recordOtelException(span, msg)
			return
		}
		msg := fmt.Errorf("error when waiting for the test execution job to initialize - Error: %s", err.Error())
		RespondWithError(w, http.StatusBadRequest, msg.Error())
		logger.WithField("user_log", true).Error(msg)
		h.recordOtelException(span, msg)
		return
	}
	span.SetAttributes(attribute.String("etos.execution_space.build_id", buildID))
	span.SetAttributes(attribute.String("etos.execution_space.build_url", buildURL))
	logger.WithField("user_log", true).Info("Executor has started successfully")
	if buildURL != "" {
		logger.WithField("user_log", true).Info("Executor build URL: ", buildURL)
	}
	w.WriteHeader(http.StatusNoContent)
	_, _ = w.Write([]byte(""))
}

type state struct {
	ExecutorSpec *executionspace.ExecutorSpec
	environment  *eiffelevents.EnvironmentDefinedV3
	mainSuite    *eiffelevents.TestSuiteStartedV3
}

// getSubSuite gets a sub suite from event repository
func (s *state) getSubSuite(ctx context.Context, cfg config.ExecutionSpaceConfig) (*eiffelevents.TestSuiteStartedV3, error) {
	if s.environment == nil {
		event, err := eventrepository.EnvironmentDefined(ctx, cfg.EiffelGoerURL(), s.ExecutorSpec.Instructions.Environment["ENVIRONMENT_ID"])
		if err != nil {
			return nil, err
		}
		s.environment = event
	}
	if s.environment != nil && s.mainSuite == nil {
		event, err := eventrepository.MainSuiteStarted(ctx, cfg.EiffelGoerURL(), s.environment.Links.FindFirst("CONTEXT"))
		if err != nil {
			return nil, err
		}
		s.mainSuite = event
	}
	if s.mainSuite != nil && s.environment != nil {
		event, err := eventrepository.TestSuiteStarted(ctx, cfg.EiffelGoerURL(), s.mainSuite.Meta.ID, s.environment.Data.Name)
		if err != nil {
			return nil, err
		}
		return event, err
	}
	return nil, errors.New("sub suite not yet available")
}

// waitStart waits for a job to start completely
func (s *state) waitStart(ctx context.Context, cfg config.ExecutionSpaceConfig, logger *logrus.Entry, executor executor.Executor) error {
	var event *eiffelevents.TestSuiteStartedV3
	var err error
	if err = retry.Constant(ctx, 5*time.Second, func(ctx context.Context) error {
		alive, err := executor.Alive(ctx, logger, s.ExecutorSpec.BuildID)
		if err != nil {
			logger.Errorf("Retrying - %s", err.Error())
			// TODO: Verify that this is retryable
			return retry.RetryableError(err)
		}
		if !alive {
			return errors.New("test runner did not start properly")
		}
		event, err = s.getSubSuite(ctx, cfg)
		if err != nil {
			logger.Errorf("Retrying - %s", err.Error())
			// TODO: Verify that this is always retryable
			return retry.RetryableError(err)
		}
		if event == nil {
			return retry.RetryableError(errors.New("not yet started"))
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}
