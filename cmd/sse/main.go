// Copyright Axis Communications AB.
//
// For a full list of individual contributors, please see the commit history.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/eiffel-community/etos-api/internal/config"
	"github.com/eiffel-community/etos-api/internal/logging"
	"github.com/eiffel-community/etos-api/internal/metrics"
	"github.com/eiffel-community/etos-api/internal/server"
	"github.com/eiffel-community/etos-api/internal/stream"
	"github.com/eiffel-community/etos-api/pkg/application"
	v1 "github.com/eiffel-community/etos-api/pkg/sse/v1"
	v1alpha "github.com/eiffel-community/etos-api/pkg/sse/v1alpha"
	v2alpha "github.com/eiffel-community/etos-api/pkg/sse/v2alpha"
	"github.com/julienschmidt/httprouter"
	rabbitMQStream "github.com/rabbitmq/rabbitmq-stream-go-client/pkg/stream"
	"github.com/sirupsen/logrus"
	"github.com/snowzach/rotatefilehook"
	"go.elastic.co/ecslogrus"
)

// main sets up logging and starts up the sse webserver.
func main() {
	cfg := config.NewSSEConfig()
	ctx := context.Background()

	var hooks []logrus.Hook
	if fileHook := fileLogging(cfg); fileHook != nil {
		hooks = append(hooks, fileHook)
	}
	logger, err := logging.Setup(cfg.LogLevel(), hooks)
	if err != nil {
		logrus.Fatal(err.Error())
	}

	hostname, err := os.Hostname()
	if err != nil {
		logrus.Fatal(err.Error())
	}
	log := logger.WithFields(logrus.Fields{
		"hostname":    hostname,
		"application": "ETOS API SSE Server",
		"version":     vcsRevision(),
		"name":        "ETOS API",
	})

	log.Info("Loading SSE routes")
	sseMetrics := metrics.NewSSEMetrics(os.Getenv("ETOS_VERSION"))
	v1AlphaSSE := v1alpha.New(cfg, log, ctx)
	defer v1AlphaSSE.Close()
	v1SSE := v1.New(cfg, log, ctx)
	defer v1SSE.Close()

	var app *httprouter.Router

	// SSEv2
	var streamer stream.Streamer
	if cfg.RabbitMQURI() != "" {
		if cfg.RabbitMQStreamName() == "" {
			log.Fatal("ETOS_RABBITMQ_STREAM_NAME must be set for the SSE server to work.")
		}
		log.Infof("Starting up a RabbitMQStreamer with stream name: %s", cfg.RabbitMQStreamName())
		opts := rabbitMQStream.NewEnvironmentOptions().SetUri(cfg.RabbitMQURI())
		if strings.HasPrefix(cfg.RabbitMQURI(), "rabbitmq-stream+tls://") {
			var serverName string
			if u, err := url.Parse(cfg.RabbitMQURI()); err == nil {
				serverName = u.Hostname()
			}
			opts = opts.SetTLSConfig(&tls.Config{ServerName: serverName})
		}
		streamer, err = stream.NewRabbitMQStreamer(ctx, *opts, log, cfg.RabbitMQStreamName())
		if err != nil {
			log.Fatal(err.Error())
		}
		if err := streamer.CreateStream(ctx, log, cfg.RabbitMQStreamName()); err != nil {
			log.Fatal(err.Error())
		}
	} else {
		log.Warning("RabbitMQURI is not set, defaulting to FileStreamer")
		streamer, err = stream.NewFileStreamer(100*time.Millisecond, log)
	}
	if err != nil {
		log.Fatal(err.Error())
	}
	v2AlphaSSE := v2alpha.New(ctx, cfg, log, streamer, sseMetrics)
	defer v2AlphaSSE.Close()
	app = application.New(v1AlphaSSE, v1SSE, v2AlphaSSE)
	app.Handler(http.MethodGet, "/metrics", sseMetrics.Handler())

	srv := server.NewWebService(cfg, log, app)

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			log.Errorf("Webserver shutdown: %+v", err)
		}
	}()

	sig := <-done
	log.Infof("%s received", sig.String())

	ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
	defer cancel()

	if err := srv.Close(ctx); err != nil {
		log.Errorf("Webserver shutdown failed: %+v", err)
	}
	log.Info("Wait for shutdown to complete")
}

// fileLogging adds a hook into a slice of hooks, if the filepath configuration is set
func fileLogging(cfg config.Config) logrus.Hook {
	if filePath := cfg.LogFilePath(); filePath != "" {
		// TODO: Make these parameters configurable.
		// NewRotateFileHook cannot return an error which is why it's set to '_'.
		rotateFileHook, _ := rotatefilehook.NewRotateFileHook(rotatefilehook.RotateFileConfig{
			Filename:   filePath,
			MaxSize:    10, // megabytes
			MaxBackups: 3,
			MaxAge:     0, // days
			Level:      logrus.DebugLevel,
			Formatter: &ecslogrus.Formatter{
				DataKey: "labels",
			},
		})
		return rotateFileHook
	}
	return nil
}

// vcsRevision returns vcs revision from build info, if any. Otherwise '(unknown)'.
func vcsRevision() string {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	for _, val := range buildInfo.Settings {
		if val.Key == "vcs.revision" {
			return val.Value
		}
	}
	return "(unknown)"
}
