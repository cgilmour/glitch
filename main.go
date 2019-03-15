// Copyright (c) 2019 Caleb Gilmour

// From: https://github.com/paninetworks/testing-tools/blob/master/benchmarking/cmd/test-http-server/main.go
// Copyright (c) 2016 Pani Networks
// All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package main

import (
	"flag"
	"fmt"
	_ "github.com/cgilmour/maxopen"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	port         = flag.Uint("port", 8080, "Port to listen for HTTP requests")
)

func main() {
	flag.Parse()

	// a HandleFunc that delays the response of a request.
	// The delay duration is the second part of the URL path (in milliseconds), eg:
	// A request to /delay/100 will sleep for 100ms before sending the response.
	http.HandleFunc("/delay/", func(w http.ResponseWriter, r *http.Request) {
		// Attempt to parse the request's delay duration
		_, duration := path.Split(r.URL.Path)
		if duration == "" {
			http.NotFound(w, r)
			return
		}
		ms, err := strconv.Atoi(duration)
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprintf(w, "Expected a number. Unable to parse %s as a number: %v\n", duration, err)
			return
		}
		if ms < 0 {
			w.WriteHeader(http.StatusUnavailableForLegalReasons)
			fmt.Fprintf(w, "The negative duration %d is not permitted, as it violates the laws of time travel\n", ms)
			return
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)

		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	// a HandleFunc that panics the request
	http.HandleFunc("/panic/", func(w http.ResponseWriter, r *http.Request) {
		panic("glitching")
	})

	// a HandleFunc that responds with the HTTP status code and message contained in the URL
	// The status code is the second part of the URL, and the message is an optional third part, eg:
	// A request to /status/503/abcd will respond with a HTTP 503 error and a body of "abcd".
	http.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		// Attempt to parse the status and message from the URL
		sp := strings.SplitN(r.URL.Path, "/", 4)
		if len(sp) < 3 {
			w.WriteHeader(http.StatusNotImplemented)
			fmt.Fprintf(w, "the URL is missing a numeric status code")
			return
		}

		// index 2 is the status code
		sc, err := strconv.Atoi(sp[2])
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprintf(w, "Expected a number. Unable to parse %s as a number: %v\n", sp[2], err)
			return
		}
		if sc < 0 || sc > 1000 {
			w.WriteHeader(http.StatusUnavailableForLegalReasons)
			fmt.Fprintf(w, "The value %d is not permitted, out of the range of sensibility for HTTP", sc)
			return
		}

		// Write the header
		w.WriteHeader(sc)
		if len(sp) > 3 {
			// Write the message as well
			fmt.Fprintln(w, sp[3])
		}
	})



	// Initialize a server
	server := http.Server{Addr: fmt.Sprintf(":%d", *port)}
	server.SetKeepAlivesEnabled(false)
	errCh := make(chan struct{})

	// Counters for reporting server activity at regular intervals.
	bc := &byteCounter{}
	cc := &connCounter{}

	// try to start the server.
	go func() {
		defer close(errCh)
		ln, err := net.Listen("tcp", server.Addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error from net.Listen: %s\n", err)
			return
		}
		err = server.Serve(&timedListener{Listener: ln, bc: bc, cc: cc})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error from server.Serve: %s\n", err)
			return
		}
	}()

	// check every 100ms if there's something to report.
	go func() {
		tickCh := time.Tick(100 * time.Millisecond)
		idle := false
		idleTime := time.Time{}
		for range tickCh {
			rx, tx := bc.Sample()
			n := cc.Sample()
			if rx == 0 && tx == 0 && n == 0 {
				// transition to idle state
				if !idle {
					idleTime = time.Now()
				}
				idle = true
				continue
			}
			if idle {
				// transition from idle state
				fmt.Println("Idle for", time.Now().Sub(idleTime))
				idle = false
			}
			fmt.Println("connections:", n, "bytes sent:", tx, "bytes received:", rx)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, os.Kill)

	// stop on a ^C or an error while initializing or running the server.
	select {
	case <-sigCh:
	case <-errCh:
	}
}


// this is a net.Listener that captures information for connections that arrive on the server.
type timedListener struct {
	net.Listener
	bc *byteCounter
	cc *connCounter
}

func (tl *timedListener) Accept() (net.Conn, error) {
	conn, err := tl.Listener.Accept()
	// increase connection count for this sample
	tl.cc.m.Lock()
	tl.cc.n++
	tl.cc.m.Unlock()
	if err != nil {
		fmt.Println("Accept() error was", err)
	}
	// wrap the connection, so it can capture additional byteCounter information
	return &timedConn{Conn: conn, openTS: time.Now(), bc: tl.bc}, err
}

type connCounter struct {
	m sync.Mutex
	n int
}

// Sample() reports and resets the number of connections since it was last called.
func (c *connCounter) Sample() int {
	c.m.Lock()
	n := c.n
	c.n = 0
	c.m.Unlock()
	return n
}

// a timedConnection captures when the connection was opened/closed, and increases counters during
// Read() and Write() calls.
type timedConn struct {
	net.Conn
	openTS  time.Time
	closeTS time.Time
	bc      *byteCounter
}

func (tc *timedConn) Read(b []byte) (int, error) {
	n, err := tc.Conn.Read(b)
	tc.bc.m.Lock()
	tc.bc.rx += n
	tc.bc.m.Unlock()
	return n, err
}

func (tc *timedConn) Write(b []byte) (int, error) {
	n, err := tc.Conn.Write(b)
	tc.bc.m.Lock()
	tc.bc.tx += n
	tc.bc.m.Unlock()
	return n, err
}

func (tc timedConn) Close() error {
	tc.closeTS = time.Now()
	// fmt.Printf("Opened: %s, Closed: %s, Delta: %s\n", tc.openTS, tc.closeTS, tc.closeTS.Sub(tc.openTS))
	return tc.Conn.Close()
}

// The byteCounter captures bytes read and written for the timedConn it is related to.
type byteCounter struct {
	m  sync.Mutex
	rx int
	tx int
}

// Sample() reports and resets the number of bytes read and written since it was last called
func (bc *byteCounter) Sample() (int, int) {
	bc.m.Lock()
	rx, tx := bc.rx, bc.tx
	bc.rx, bc.tx = 0, 0
	bc.m.Unlock()
	return rx, tx
}
