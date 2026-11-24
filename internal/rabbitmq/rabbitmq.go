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
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/sethvargo/go-retry"
	"github.com/sirupsen/logrus"
)

// PublisherConfig defines the configuration to use when publishing to an
// exchange.
type PublisherConfig struct {
	URL          string `yaml:"url"`
	ExchangeName string `yaml:"exchange_name"`
}

// Publisher maintains a persistent AMQP connection that can be used to
// publish messages synchronously.
//
// A publisher can safely be called concurrently from multiple goroutines,
// but the broker communication will be serialized with no more than one
// outstanding message, i.e. you should publish concurrently when it's
// convenient and not because you want to increase the rate of publishing.
// Use multiple Publishers in the latter case.
type Publisher struct {
	config         PublisherConfig
	conn           *amqp.Connection
	channel        *amqp.Channel
	chanClosures   chan *amqp.Error
	connClosures   chan *amqp.Error
	confirmations  chan amqp.Confirmation
	hasOutstanding bool       // Is there an in-flight message that hasn't been (n)acked?
	connMu         sync.Mutex // Prevent overlapping connection setup/teardown
	publishMu      sync.Mutex // Prevent overlapping publishing
}

func NewPublisher(config PublisherConfig) *Publisher {
	return &Publisher{
		config: config,
	}
}

// Close closes any current connection and any channel open within it.
// This will interrupt any ongoing publishing, but only temporarily as
// it'll retry. To permanently interrupt ongoing publishing and force
// a return to the caller, cancel the context passed to Publish.
func (p *Publisher) Close() {
	p.connMu.Lock()
	if p.conn != nil {
		// Closing the connection also closes p.channel and notification channels.
		if err := p.conn.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
			log.Printf("failed to close RabbitMQ connection: %v", err)
		}
	}
	p.connMu.Unlock()
}

// Publish attempts to publish a single message. It'll block until a connection
// is available, sends the message, and then waits for the message to be
// acknowledged by the broker. All kinds of errors except context expirations
// are retried indefinitely with a backoff.
//
// Connections are created lazily upon the first call to this method and will
// be kept alive. Any error will cause the connection to be torn down and
// reestablished upon the next attempt.
func (p *Publisher) Publish(ctx context.Context, logger *logrus.Entry, topic string, message amqp.Publishing) error {
	backoff := retry.WithCappedDuration(1*time.Minute, retry.NewExponential(1*time.Second))
	return retry.Do(ctx, backoff, func(ctx context.Context) error {
		if err := p.tryPublish(ctx, logger, topic, message); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logger.Errorf("Could not publish message, will retry: %s", err)
			return retry.RetryableError(err)
		}
		return nil
	})
}

// awaitConfirmation waits for and returns the confirmation (positive or
// negative) for a single message. An error is returned if the context
// expires or the connection or channel closes. Must only be called while
// the p.publishMu mutex is held.
func (p *Publisher) awaitConfirmation(ctx context.Context) (amqp.Confirmation, error) {
	select {
	case err := <-p.connClosures:
		return amqp.Confirmation{}, fmt.Errorf("connection closed: %w", err)
	case err := <-p.chanClosures:
		return amqp.Confirmation{}, fmt.Errorf("channel closed: %w", err)
	case c := <-p.confirmations:
		p.hasOutstanding = false
		return c, nil
	case <-ctx.Done():
		return amqp.Confirmation{}, ctx.Err()
	}
}

func (p *Publisher) ensureConnection(logger *logrus.Entry) error {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	if p.conn == nil || p.channel == nil || p.conn.IsClosed() {
		if p.conn != nil {
			if err := p.conn.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
				logger.Warnf("failed to close RabbitMQ connection: %v", err)
			}
		}
		amqpURL, err := url.Parse(p.config.URL)
		if err != nil {
			return fmt.Errorf("invalid AMQP URL: %w", err)
		}
		logger.Infof("Opening AMQP connection to %s", amqpURL.Redacted())
		if p.conn, err = amqp.Dial(amqpURL.String()); err != nil {
			return fmt.Errorf("error making AMQP connection: %w", err)
		}
		p.connClosures = p.conn.NotifyClose(make(chan *amqp.Error, 1))

		if p.channel, err = p.conn.Channel(); err != nil {
			return fmt.Errorf("error creating channel: %w", err)
		}
		if err = p.channel.Confirm(false); err != nil {
			// Force closure of possibly healthy connection to make sure
			// we get to set up confirms in the next ensureConnection call.
			if closeErr := p.conn.Close(); closeErr != nil && !errors.Is(closeErr, amqp.ErrClosed) {
				logger.Warnf("failed to close RabbitMQ connection: %v", closeErr)
			}
			return fmt.Errorf("error enabling publisher confirms: %w", err)
		}
		p.hasOutstanding = false

		// Might be overkill to set up notifications for both connection and channel
		// closures, but this might save us from getting into weird half-open states.
		p.chanClosures = p.channel.NotifyClose(make(chan *amqp.Error, 1))
		p.confirmations = p.channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	}
	return nil
}

// closeOnTimeout closes the connection if the context contains an error (timeout)
func (p *Publisher) CloseOnTimeout(ctx context.Context, logger *logrus.Entry) {
	if ctx.Err() != nil {
		logger.Info("Forcibly closing RabbitMQ connection due to timeout")
		p.Close()
	}
}

func (p *Publisher) tryPublish(ctx context.Context, logger *logrus.Entry, topic string, message amqp.Publishing) error {
	if err := p.ensureConnection(logger); err != nil {
		return err
	}

	// Only one goroutine should be publishing at the same time so we won't
	// have to correlate delivery tags to figure out which message was acked.
	p.publishMu.Lock()
	defer p.publishMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	defer p.CloseOnTimeout(ctx, logger) // will always be called, but connection will be closed if ctx contains error

	// If a previous tryPublish call's context expires while it's waiting for
	// its confirmation there could be a confirmation already queued up in
	// the next tryPublish call. That wouldn't have to be a problem in itself
	// (although we'd have to track the delivery tag so we won't claim delivery
	// victory over an old confirmation), but if many publishers bail out
	// (e.g. because of a retry loop that reuses the same expired context)
	// we'll fill up the confirmation channel and eventually block the whole
	// AMQP channel and deadlock everything. We mitigate this by only allowing
	// one in-flight outbound message and draining the confirmation channel
	// prior to each publish operation.
	if p.hasOutstanding {
		c, err := p.awaitConfirmation(ctx)
		if err != nil {
			return err
		}
		if !c.Ack {
			logger.Info("A previous message was nacked by the broker")
		}
	}

	if err := p.channel.Publish(p.config.ExchangeName, topic, false, false, message); err != nil {
		return fmt.Errorf("error publishing message: %w", err)
	}
	p.hasOutstanding = true

	c, err := p.awaitConfirmation(ctx)
	if err != nil {
		return err
	}
	if !c.Ack {
		return fmt.Errorf("message nacked")
	}
	return nil
}
