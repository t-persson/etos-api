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
package stream

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// FileStreamer will create a stream that reads from a file and publishes them
// to a consumer.
type FileStreamer struct {
	interval time.Duration
	logger   *logrus.Entry
}

func NewFileStreamer(interval time.Duration, logger *logrus.Entry) (Streamer, error) {
	return &FileStreamer{interval: interval, logger: logger}, nil
}

// CreateStream does nothing.
func (s *FileStreamer) CreateStream(ctx context.Context, logger *logrus.Entry, name string) error {
	return os.WriteFile(name, nil, 0644)
}

// NewStream creates a new stream struct to consume from.
func (s *FileStreamer) NewStream(ctx context.Context, logger *logrus.Entry, name string) (Stream, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return &FileStream{ctx: ctx, name: name, file: file, interval: s.interval, logger: logger}, nil
}

// Close does nothing.
func (s *FileStreamer) Close() {}

// FileStream is a structure implementing the Stream interface. Used to consume events
// from a file.
// Each line in the file is a message and its offset is the zero-based line number.
type FileStream struct {
	ctx      context.Context
	logger   *logrus.Entry
	name     string
	file     io.ReadCloser
	interval time.Duration
	start    int64
	position atomic.Int64
	channel  chan<- Message
}

// WithChannel adds a channel for receiving events from the stream. If no
// channel is added, then events will be logged.
func (s *FileStream) WithChannel(ch chan<- Message) Stream {
	s.channel = ch
	return s
}

// WithOffset adds an offset to the file stream. OffsetFirst means start from the beginning.
func (s *FileStream) WithOffset(offset int64) Stream {
	s.start = max(offset, 0)
	s.position.Store(s.start)
	return s
}

// WithFilter adds a filter to the file stream.
func (s *FileStream) WithFilter(filter []string) Stream {
	s.logger.Warning("filter is not yet supported by file stream")
	return s
}

// Consume will start consuming the file, non blocking. A channel is returned where
// an error is sent when the consumer closes down.
func (s *FileStream) Consume(ctx context.Context) (<-chan error, error) {
	closed := make(chan error, 1)
	go func() {
		defer close(closed)
		scanner := bufio.NewReader(s.file)
		interval := time.NewTicker(s.interval)
		defer interval.Stop()
		var offset int64
		for {
			select {
			case <-ctx.Done():
				closed <- ctx.Err()
				return
			case <-interval.C:
				isPrefix := true
				var err error
				var line []byte
				var event []byte

				for isPrefix && err == nil {
					line, isPrefix, err = scanner.ReadLine()
					event = append(event, line...)
				}
				if err != nil {
					// Don't close the stream just because the file is empty.
					if errors.Is(err, io.EOF) {
						continue
					}
					select {
					case closed <- err:
					case <-ctx.Done():
					}
					return
				}
				offset++
				if offset-1 < s.start {
					continue
				}
				if s.channel != nil {
					select {
					case s.channel <- Message{Offset: offset - 1, Data: event}:
					case <-ctx.Done():
						return
					}
				} else {
					s.logger.Info(event)
				}
				s.position.Store(offset)
			}
		}
	}()
	return closed, nil
}

// FirstOffset returns 0, the offset of the first line, unless the file is empty.
func (s *FileStream) FirstOffset() (int64, error) {
	info, err := os.Stat(s.name)
	if err != nil {
		return 0, err
	}
	if info.Size() == 0 {
		return 0, ErrEmptyStream
	}
	return 0, nil
}

// Position returns the offset of the next line to be read from the file.
func (s *FileStream) Position() int64 {
	return s.position.Load()
}

// Close the file.
func (s *FileStream) Close() {
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			s.logger.WithError(err).Error("failed to close the file")
		}
	}
}
