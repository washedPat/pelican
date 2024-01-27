/***************************************************************
 *
 * Copyright (C) 2024, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

package origin_ui

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/pelicanplatform/pelican/broker"
	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/utils"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

var (
	// We have a custom transport object to force all our connections to the
	// localhost to avoid potentially going over the external network to talk
	// to our xrootd child process.
	transport *http.Transport

	onceTransport sync.Once
)

// Return a custom HTTP transport object; starts with the default transport for
// Pelican but forces all connections to go to the local xrootd port.
func getTransport() *http.Transport {
	onceTransport.Do(func() {
		var copyTransport http.Transport = *config.GetTransport()
		transport = &copyTransport
		// When creating a new socket out to the remote server, ignore the actual
		// requested address and return a localhost socket.
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, "tcp", "localhost:"+strconv.Itoa(param.Xrootd_Port.GetInt()))
		}
	})
	return transport
}

func proxyOrigin(resp http.ResponseWriter, req *http.Request) {
	url := req.URL
	url.Scheme = "https"
	url.Host = param.Server_Hostname.GetString() + ":" + strconv.Itoa(param.Xrootd_Port.GetInt())

	log.Debugln("Will proxy request to URL", url.String())
	transport = getTransport()
	xrdResp, err := transport.RoundTrip(req)
	if err != nil {
		log.Infoln("Failed to talk to xrootd service:", err)
		resp.WriteHeader(http.StatusServiceUnavailable)
		if _, err := resp.Write([]byte(`Failed to connect to local xrootd instance`)); err != nil {
			log.Infoln("Failed to write response to client:", err)
		}
		return
	}
	defer xrdResp.Body.Close()

	utils.CopyHeader(resp.Header(), xrdResp.Header)
	resp.WriteHeader(xrdResp.StatusCode)
	if _, err = io.Copy(resp, xrdResp.Body); err != nil {
		log.Warningln("Failed to copy response body from Xrootd to remote cache:", err)
	}
}

// Launch goroutines that continuously poll the broker
func LaunchBrokerListener(ctx context.Context, egrp *errgroup.Group) (err error) {
	listenerChan := make(chan net.Listener)
	// Startup 5 continuous polling routines
	for cnt := 0; cnt < 5; cnt += 1 {
		err = broker.LaunchRequestMonitor(ctx, egrp, listenerChan)
		if err != nil {
			return
		}
	}
	// Start routine which receives the reverse listener and then launches
	// a simple proxying HTTPS server for that connection
	egrp.Go(func() (err error) {
		for {
			select {
			case <-ctx.Done():
				return
			case listener := <-listenerChan:
				srv := http.Server{
					Handler: http.HandlerFunc(proxyOrigin),
				}
				go func() {
					// A one-shot listener should do a single "accept" then shutdown.
					err = srv.Serve(listener)
					if !errors.Is(err, net.ErrClosed) {
						log.Errorln("Failed to serve reverse connection:", err)
					}
				}()
			}
		}
	})
	return
}
