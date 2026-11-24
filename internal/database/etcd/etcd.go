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
package etcd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/eiffel-community/etos-api/internal/config"
	"github.com/eiffel-community/etos-api/internal/database"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// TODO: refactor the client so that it does not store data it fetched.
// However, without it implementing the database.Opener interface would be more complex.
type Etcd struct {
	database.Deleter
	cfg        config.Config
	client     *clientv3.Client
	ID         uuid.UUID
	ctx        context.Context
	treePrefix string
	data       []byte
	hasRead    bool
}

// New returns a new Etcd Object/Struct.
func New(cfg config.Config, logger *logrus.Logger, treePrefix string) database.Opener {
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{cfg.DatabaseURI()},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		panic(err)
	}

	return Etcd{
		client:     client,
		cfg:        cfg,
		treePrefix: treePrefix,
	}
}

// Open returns a copy of an Etcd client with ID and context added
func (etcd Etcd) Open(ctx context.Context, id uuid.UUID) io.ReadWriter {
	return &Etcd{
		client: etcd.client,
		cfg:    etcd.cfg,
		ID:     id,
		ctx:    ctx,
	}
}

// Write writes data to etcd
func (etcd Etcd) Write(p []byte) (int, error) {
	if etcd.ID == uuid.Nil {
		return 0, errors.New("please create a new etcd client using Open")
	}
	key := fmt.Sprintf("%s/%s", etcd.treePrefix, etcd.ID.String())

	_, err := etcd.client.Put(etcd.ctx, key, string(p))
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read reads data from etcd and returns p bytes to user
func (etcd *Etcd) Read(p []byte) (n int, err error) {
	if etcd.ID == uuid.Nil {
		return 0, errors.New("please create a new etcd client using NewWithID")
	}

	key := fmt.Sprintf("%s/%s", etcd.treePrefix, etcd.ID.String())

	if !etcd.hasRead {
		resp, err := etcd.client.Get(etcd.ctx, key)
		if err != nil {
			return 0, err
		}
		if len(resp.Kvs) == 0 {
			return 0, io.EOF
		}
		etcd.data = resp.Kvs[0].Value
		etcd.hasRead = true
	}

	if len(etcd.data) == 0 {
		return 0, io.EOF
	}

	// Copy as much data as possible to p
	// The copy function copies the minimum of len(p) and len(etcd.data) bytes from etcd.data to p
	// It returns the number of bytes copied, which is stored in n
	n = copy(p, etcd.data)

	// Update etcd.data to remove the portion of data that has already been copied to p
	// etcd.data[n:] creates a new slice that starts from the n-th byte to the end of the original slice
	// This effectively removes the first n bytes from etcd.data, ensuring that subsequent reads start from the correct position
	etcd.data = etcd.data[n:]

	if n == 0 {
		return 0, io.EOF
	}

	return n, nil
}

// Delete deletes the current key from the database
func (etcd Etcd) Delete() error {
	key := fmt.Sprintf("%s/%s", etcd.treePrefix, etcd.ID.String())
	_, err := etcd.client.Delete(etcd.ctx, key)
	if err != nil {
		return fmt.Errorf("failed to delete key %s: %w", key, err)
	}
	return nil
}
