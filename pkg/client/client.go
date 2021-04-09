// Copyright (c) 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"sync"
	"time"

	"github.com/gardener/logging/pkg/batch"
	"github.com/gardener/logging/pkg/buffer"
	"github.com/gardener/logging/pkg/config"
	"github.com/gardener/logging/pkg/metrics"
	"github.com/go-kit/kit/log"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/promtail/client"
	"github.com/prometheus/common/model"
)

const (
	minWaitCheckFrequency       = 10 * time.Millisecond
	waitCheckFrequencyDelimiter = 10
)

type newClientFunc func(cfg client.Config, logger log.Logger) (client.Client, error)

// NewClient creates a new client based on the fluentbit configuration.
func NewClient(cfg *config.Config, logger log.Logger) (client.Client, error) {
	var ncf newClientFunc

	if cfg.ClientConfig.SortByTimestamp {
		ncf = func(c client.Config, logger log.Logger) (client.Client, error) {
			return New(c, cfg.ClientConfig.NumberOfBatchIDs, logger)
		}
	} else {
		ncf = NewPromtailClient
	}

	if cfg.ClientConfig.BufferConfig.Buffer {
		return buffer.NewBuffer(cfg, logger, ncf)
	}
	return ncf(cfg.ClientConfig.GrafanaLokiConfig, logger)
}

type sortedClient struct {
	logger           log.Logger
	lokiclient       client.Client
	batch            *batch.Batch
	batchWait        time.Duration
	batchLock        sync.Mutex
	batchSize        int
	batchID          uint64
	numberOfBatchIDs uint64
	quit             chan struct{}
	once             sync.Once
	entries          chan entry
	wg               sync.WaitGroup
}

type entry struct {
	labels model.LabelSet
	logproto.Entry
}

// New makes a new Client.
func New(cfg client.Config, numberOfBatchIds uint64, logger log.Logger) (client.Client, error) {
	batchWait := cfg.BatchWait
	cfg.BatchWait = 5 * time.Second

	lokiclient, err := NewPromtailClient(cfg, logger)
	if err != nil {
		return nil, err
	}

	c := &sortedClient{
		logger:           log.With(logger, "component", "client", "host", cfg.URL.Host),
		lokiclient:       lokiclient,
		batchWait:        batchWait,
		batchSize:        cfg.BatchSize,
		batchID:          0,
		numberOfBatchIDs: numberOfBatchIds,
		batch:            batch.NewBatch(0),
		quit:             make(chan struct{}),
		entries:          make(chan entry),
	}

	c.wg.Add(1)
	go c.run()
	return c, nil
}

func (c *sortedClient) run() {
	maxWaitCheckFrequency := c.batchWait / waitCheckFrequencyDelimiter
	if maxWaitCheckFrequency < minWaitCheckFrequency {
		maxWaitCheckFrequency = minWaitCheckFrequency
	}

	maxWaitCheck := time.NewTicker(maxWaitCheckFrequency)

	defer func() {
		if c.batch != nil {
			c.sendBatch()
		}
		c.wg.Done()
	}()

	for {
		select {
		case <-c.quit:
			return

		case e := <-c.entries:

			// If the batch doesn't exist yet, we create a new one with the entry
			if c.batch == nil {
				c.newBatch(e)
				break
			}

			// If adding the entry to the batch will increase the size over the max
			// size allowed, we do send the current batch and then create a new one
			if c.batch.SizeBytesAfter(e.Line) > c.batchSize {
				c.sendBatch()
				c.newBatch(e)
				break
			}

			// The max size of the batch isn't reached, so we can add the entry
			c.addToBatch(e)

		case <-maxWaitCheck.C:
			// Send batche if max wait time has been reached

			if !c.isBatchWaitExceeded() {
				continue
			}

			c.sendBatch()
		}
	}
}

func (c *sortedClient) isBatchWaitExceeded() bool {
	c.batchLock.Lock()
	defer c.batchLock.Unlock()
	return c.batch != nil && c.batch.Age() > c.batchWait
}

func (c *sortedClient) sendBatch() {
	c.batchLock.Lock()
	defer c.batchLock.Unlock()

	if c.batch == nil {
		return
	}

	c.batch.Sort()
	for _, stream := range c.batch.Streams {
		for _, entry := range stream.Entries {
			_ = c.lokiclient.Handle(stream.Labels, entry.Timestamp, entry.Line)
		}
	}
	c.batch = nil
}

func (c *sortedClient) newBatch(e entry) {
	c.batchLock.Lock()
	defer c.batchLock.Unlock()
	if c.batch == nil {
		c.batchID++
		c.batch = batch.NewBatch(c.batchID % c.numberOfBatchIDs)
	}

	c.batch.Add(e.labels.Clone(), e.Timestamp, e.Line)
}

func (c *sortedClient) addToBatch(e entry) {
	c.newBatch(e)
}

// Stop the client.
func (c *sortedClient) Stop() {
	c.once.Do(func() { close(c.quit) })
	c.wg.Wait()
}

// Handle implement EntryHandler; adds a new line to the next batch; send is async.
func (c *sortedClient) Handle(ls model.LabelSet, t time.Time, s string) error {
	c.entries <- entry{ls, logproto.Entry{
		Timestamp: t,
		Line:      s,
	}}
	return nil
}

type promtailClientWithForwardedLogsMetricCounter struct {
	lokiclient client.Client
	host       string
}

// NewPromtailClient return promtail client which increments the ForwardedLogs counter on
// successful call of the Handle function
func NewPromtailClient(cfg client.Config, logger log.Logger) (client.Client, error) {
	c, err := client.New(cfg, logger)
	if err != nil {
		return nil, err
	}
	return &promtailClientWithForwardedLogsMetricCounter{
		lokiclient: c,
		host:       cfg.URL.Hostname(),
	}, nil
}

func (c *promtailClientWithForwardedLogsMetricCounter) Handle(ls model.LabelSet, t time.Time, s string) error {
	if err := c.lokiclient.Handle(ls, t, s); err != nil {
		return err
	}
	metrics.ForwardedLogs.WithLabelValues(c.host).Inc()
	return nil
}

// Stop the client.
func (c *promtailClientWithForwardedLogsMetricCounter) Stop() {
	c.lokiclient.Stop()
}
