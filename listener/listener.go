// Copyright 2017 Jump Trading
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package listener defines the functions for the publisher of
// messages to the bus.
package listener

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/nats-io/go-nats"

	"github.com/jumptrading/influx-spout/config"
	"github.com/jumptrading/influx-spout/probes"
	"github.com/jumptrading/influx-spout/stats"
)

const (
	// Listener stats counters
	statReceived          = "received"
	statSent              = "sent"
	statReadErrors        = "read_errors"
	statFailedNATSPublish = "failed_nats_publish"

	// The maximum possible UDP read size.
	maxUDPDatagramSize = 65536
)

var statsInterval = 3 * time.Second

// StartListener initialises a listener, starts its statistician
// goroutine and runs it's main loop. It never returns.
//
// The listener reads incoming UDP packets, batches them up and send
// batches onwards to a NATS subject.
func StartListener(c *config.Config) (_ *Listener, err error) {
	listener, err := newListener(c)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			listener.Stop()
		}
	}()

	sc, err := listener.setupUDP(c.ReadBufferBytes)
	if err != nil {
		return nil, err
	}

	listener.wg.Add(2)
	go listener.startStatistician()
	go listener.listenUDP(sc)

	log.Printf("UDP listener publishing to [%s] at %s", c.NATSSubject[0], c.NATSAddress)

	return listener, nil
}

// StartHTTPListener initialises listener configured to accept lines
// from HTTP request bodies instead of via UDP. It starts the listener
// and its statistician and never returns.
func StartHTTPListener(c *config.Config) (*Listener, error) {
	listener, err := newListener(c)
	if err != nil {
		return nil, err
	}
	server := listener.setupHTTP()

	listener.wg.Add(2)
	go listener.startStatistician()
	go listener.listenHTTP(server)

	log.Printf("HTTP listener publishing to [%s] at %s", c.NATSSubject[0], c.NATSAddress)

	return listener, nil
}

// Listener accepts measurements in InfluxDB Line Protocol format via
// UDP or HTTP, batches them and then publishes them to a NATS
// subject.
type Listener struct {
	c      *config.Config
	nc     *nats.Conn
	stats  *stats.Stats
	probes probes.Probes

	batch *batch

	wg   sync.WaitGroup
	stop chan struct{}
	mu   sync.Mutex // only used for HTTP listener
}

// Stop shuts down a running listener. It should be called exactly
// once for every Listener instance.
func (l *Listener) Stop() {
	l.probes.SetReady(false)
	l.probes.SetAlive(false)

	close(l.stop)
	l.wg.Wait()
	l.nc.Close()
	l.probes.Close()
}

func newListener(c *config.Config) (*Listener, error) {
	l := &Listener{
		c:    c,
		stop: make(chan struct{}),
		stats: stats.New(
			statReceived,
			statSent,
			statReadErrors,
			statFailedNATSPublish,
		),
		probes: probes.Listen(c.ProbePort),
		batch:  newBatch(c.ListenerBatchBytes),
	}

	nc, err := nats.Connect(l.c.NATSAddress, nats.MaxReconnects(-1))
	if err != nil {
		return nil, err
	}
	l.nc = nc

	return l, nil
}

func (l *Listener) setupUDP(configBufSize int) (*net.UDPConn, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", l.c.Port))
	if err != nil {
		return nil, fmt.Errorf("failed to create UDP socket: %v", err)
	}
	sc, err := net.ListenUDP("udp", serverAddr)
	if err != nil {
		return nil, err
	}

	bufSize := roundUpToPageSize(configBufSize)
	if bufSize != configBufSize {
		log.Printf("rounding up receive buffer to nearest page size (now %d bytes)", bufSize)
	}
	if err := sc.SetReadBuffer(bufSize); err != nil {
		return nil, err
	}

	log.Printf("listener bound to UDP socket: %v\n", sc.LocalAddr().String())
	return sc, nil
}

func roundUpToPageSize(n int) int {
	pageSize := os.Getpagesize()
	if n <= 0 {
		return pageSize
	}
	return (n + pageSize - 1) / pageSize * pageSize
}

func (l *Listener) listenUDP(sc *net.UDPConn) {
	defer func() {
		sc.Close()
		l.wg.Done()
	}()

	l.probes.SetReady(true)
	for {
		sc.SetReadDeadline(time.Now().Add(time.Second))
		bytesRead, err := l.batch.readOnceFrom(sc)
		if err != nil && !isTimeout(err) {
			l.stats.Inc(statReadErrors)
		}
		if bytesRead > 0 {
			if l.c.Debug {
				log.Printf("listener read %d bytes", bytesRead)
			}
			l.processRead()
		}

		select {
		case <-l.stop:
			return
		default:
		}
	}
}

func (l *Listener) setupHTTP() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		bytesRead, err := l.batch.readFrom(r.Body)
		l.mu.Unlock()
		if err != nil {
			l.stats.Inc(statReadErrors)
		}
		if bytesRead > 0 {
			if l.c.Debug {
				log.Printf("HTTP listener read %d bytes", bytesRead)
			}
			l.mu.Lock()
			l.processRead()
			l.mu.Unlock()
		}
	})
	return &http.Server{
		Addr:    fmt.Sprintf(":%d", l.c.Port),
		Handler: mux,
	}
}

func (l *Listener) listenHTTP(server *http.Server) {
	defer l.wg.Done()

	go func() {
		l.probes.SetReady(true)
		err := server.ListenAndServe()
		if err == nil || err == http.ErrServerClosed {
			return
		}
		log.Fatal(err)
	}()

	// Close the server if the stop channel is closed.
	<-l.stop
	server.Close()
}

func (l *Listener) processRead() {
	statReceived := l.stats.Inc(statReceived)

	// Send when the configured number of reads have been batched or
	// the batch buffer is almost full.

	// If the batch size is within a (maximum) UDP datagram of the
	// configured target batch size, then force a send to avoid
	// growing the batch unnecessarily (allocations hurt performance).
	batchNearlyFull := l.c.ListenerBatchBytes-l.batch.size() <= maxUDPDatagramSize

	if statReceived%l.c.BatchMessages == 0 || batchNearlyFull {
		l.stats.Inc(statSent)
		if err := l.nc.Publish(l.c.NATSSubject[0], l.batch.bytes()); err != nil {
			l.stats.Inc(statFailedNATSPublish)
			l.handleNatsError(err)
		}
		l.batch.reset()
	}
}

func (l *Listener) handleNatsError(err error) {
	log.Printf("NATS Error: %v\n", err)
}

func (l *Listener) startStatistician() {
	defer l.wg.Done()

	labels := map[string]string{
		"component": "listener",
		"name":      l.c.Name,
	}
	for {
		lines := stats.SnapshotToPrometheus(l.stats.Snapshot(), time.Now(), labels)
		l.nc.Publish(l.c.NATSSubjectMonitor, lines)
		select {
		case <-time.After(statsInterval):
		case <-l.stop:
			return
		}
	}
}

type timeouter interface {
	Timeout() bool
}

func isTimeout(err error) bool {
	if timeoutErr, ok := err.(timeouter); ok {
		return timeoutErr.Timeout()
	}
	return false
}
