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
package provider

import (
	"context"
	"sync"

	"github.com/eiffel-community/etos-api/internal/config"
	"github.com/eiffel-community/etos-api/internal/database"
	"github.com/eiffel-community/etos-api/internal/executionspace/executor"
	"github.com/eiffel-community/etos-api/pkg/executionspace/executionspace"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

type Provider interface {
	New(database.Opener, config.ExecutionSpaceConfig) Provider
	Status(*logrus.Entry, context.Context, uuid.UUID) (*executionspace.ExecutionSpace, error)
	Checkout(*logrus.Entry, context.Context, ExecutorConfig)
	Checkin(*logrus.Entry, context.Context, []executionspace.ExecutorSpec) error
	Executor() executor.Executor
	SaveExecutor(context.Context, executionspace.ExecutorSpec) error
	Job(context.Context, uuid.UUID) (string, error)
	ExecutorSpec(context.Context, uuid.UUID) (*executionspace.ExecutorSpec, error)
	ExecutionSpace(context.Context, uuid.UUID) (*executionspace.ExecutionSpace, error)
	Done()
}

type ExecutorConfig struct {
	Amount         int
	TestRunner     string
	CheckoutID     uuid.UUID
	ETOSIdentifier string
	Environment    map[string]string
}

// providerCore partially implements the Provider interface. To use it it should
// be included into another struct that implements the rest of the interface.
type providerCore struct {
	db       database.Opener
	cfg      config.ExecutionSpaceConfig
	url      string
	active   *sync.WaitGroup
	executor executor.Executor
}

// Status fetches execution space status from a database
func (e providerCore) Status(logger *logrus.Entry, ctx context.Context, id uuid.UUID) (*executionspace.ExecutionSpace, error) {
	e.active.Add(1)
	defer e.active.Done()

	executionSpace, err := e.ExecutionSpace(ctx, id)
	if err != nil {
		return &executionspace.ExecutionSpace{
			ID:          id,
			Status:      executionspace.Failed,
			Description: err.Error(),
		}, err
	}

	for _, reference := range executionSpace.References {
		id, err := uuid.Parse(reference)
		if err != nil {
			return &executionspace.ExecutionSpace{
				ID:          id,
				Status:      executionspace.Failed,
				Description: err.Error(),
			}, err
		}

		executor, err := e.ExecutorSpec(ctx, id)
		if err != nil {
			return &executionspace.ExecutionSpace{
				ID:          id,
				Status:      executionspace.Failed,
				Description: err.Error(),
			}, err
		}
		executionSpace.Executors = append(executionSpace.Executors, *executor)
	}
	return executionSpace, nil
}

// Checkout checks out an execution space and stores it in a database
func (e providerCore) Checkout(logger *logrus.Entry, ctx context.Context, cfg ExecutorConfig) {
	e.active.Add(1)
	defer e.active.Done()

	executionSpace := executionspace.New(cfg.CheckoutID)
	client := e.db.Open(ctx, cfg.CheckoutID)
	if err := executionSpace.Save(client); err != nil {
		logger.Errorf("failed to write checkout pending status to RedisDB - %s", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(ctx, e.cfg.ExecutionSpaceWaitTimeout())
	defer cancel()

	for i := 0; i < cfg.Amount; i++ {
		executor := executionspace.NewExecutorSpec(e.url, cfg.ETOSIdentifier, cfg.TestRunner, cfg.Environment, ctx)
		executionSpace.Add(executor)
		if err := e.SaveExecutor(ctx, executor); err != nil {
			if failErr := executionSpace.Fail(client, err); failErr != nil {
				logger.Errorf("failed to write failure status to RedisDB - Reason: %s", failErr.Error())
			}
			return
		}
	}
	executionSpace.Status = executionspace.Done
	executionSpace.Description = "Execution spaces checked out successfully"

	if err := executionSpace.Save(client); err != nil {
		if failErr := executionSpace.Fail(client, err); failErr != nil {
			logger.Errorf("failed to write failure status to RedisDB - Reason: %s", failErr.Error())
		}
	}
	logger.WithField("user_log", true).Infof("Executor prepared for running tests")
}

// Checkin checks in an execution space by removing it from database
func (e providerCore) Checkin(logger *logrus.Entry, ctx context.Context, executors []executionspace.ExecutorSpec) error {
	e.active.Add(1)
	defer e.active.Done()
	for _, executor := range executors {
		client := e.db.Open(ctx, executor.ID)
		if err := executor.Delete(client); err != nil {
			return err
		}
	}
	return nil
}

// Executor returns the executor of this provider
func (e providerCore) Executor() executor.Executor {
	return e.executor
}

// SaveExecutor saves an executor specification into a database
func (e providerCore) SaveExecutor(ctx context.Context, executorSpec executionspace.ExecutorSpec) error {
	client := e.db.Open(ctx, executorSpec.ID)
	return executorSpec.Save(client)
}

// Job gets the Build ID of a test runner execution.
func (e providerCore) Job(ctx context.Context, id uuid.UUID) (string, error) {
	executorSpec, err := e.ExecutorSpec(ctx, id)
	if err != nil {
		return "", err
	}
	if executorSpec == nil {
		return "", nil
	}
	return executorSpec.BuildID, nil
}

// ExecutorSpec returns the specification of an executor stored in database
func (e providerCore) ExecutorSpec(ctx context.Context, id uuid.UUID) (*executionspace.ExecutorSpec, error) {
	client := e.db.Open(ctx, id)
	return executionspace.LoadExecutorSpec(client)
}

// ExecutionSPace returns the execution space stored in database
func (e providerCore) ExecutionSpace(ctx context.Context, id uuid.UUID) (*executionspace.ExecutionSpace, error) {
	client := e.db.Open(ctx, id)
	return executionspace.Load(client)
}

// Done waits for all jobs to be done
func (e providerCore) Done() {
	e.active.Wait()
}
