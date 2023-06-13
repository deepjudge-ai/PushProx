// Copyright 2020 The Prometheus Authors
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
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	kingpin "gopkg.in/alecthomas/kingpin.v2"

	"github.com/Showmax/go-fqdn"
	"github.com/cenkalti/backoff/v4"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/rancher/pushprox/util"
)

var (
	myFqdn             = kingpin.Flag("fqdn", "FQDN to register with").Default(fqdn.Get()).String()
	proxyURL           = kingpin.Flag("proxy-url", "Push proxy to talk to.").Required().String()
	caCertFile         = kingpin.Flag("tls.cacert", "<file> CA certificate to verify peer against").String()
	tlsCert            = kingpin.Flag("tls.cert", "<cert> Client certificate file").String()
	tlsKey             = kingpin.Flag("tls.key", "<key> Private key file").String()
	metricsAddr        = kingpin.Flag("metrics-addr", "Serve Prometheus metrics at this address").Default(":9369").String()
	tokenPath          = kingpin.Flag("token-path", "Uses an OAuth 2.0 Bearer token found in this path to make scrape requests").String()
	insecureSkipVerify = kingpin.Flag("insecure-skip-verify", "Disable SSL security checks for client").Default("false").Bool()
	useLocalhost       = kingpin.Flag("use-localhost", "Use 127.0.0.1 to scrape metrics instead of FQDN").Default("false").Bool()
	allowPort          = kingpin.Flag("allow-port", "Restricts the proxy to only being allowed to scrape the given port").Default("*").String()

	retryInitialWait = kingpin.Flag("proxy.retry.initial-wait", "Amount of time to wait after proxy failure").Default("1s").Duration()
	retryMaxWait     = kingpin.Flag("proxy.retry.max-wait", "Maximum amount of time to wait between proxy poll retries").Default("5s").Duration()

	matchStrings = kingpin.Flag("match", "federate matches").Default().Strings()
)

var (
	scrapeErrorCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pushprox_client_scrape_errors_total",
			Help: "Number of scrape errors",
		},
	)
	pushErrorCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pushprox_client_push_errors_total",
			Help: "Number of push errors",
		},
	)
	pollErrorCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pushprox_client_poll_errors_total",
			Help: "Number of poll errors",
		},
	)
)

func createURL(host string, path string, params url.Values) string {
	u := &url.URL{
		Scheme:   "http",
		Host:     host,
		Path:     path,
		RawQuery: params.Encode(),
	}
	return u.String()
}

func printPostResponse(resp *http.Response) {
	defer resp.Body.Close()

	// Read the response body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("Error reading response:", err)
		return
	}

	// Print the response status code and body
	fmt.Println("Response Status:", resp.Status)
	response := string(body)
	fmt.Println("Response Body:", response)
}

func scrapeFederatedPrometheusEndpoint() (*http.Response, error) {
	fmt.Sprintln("We adpapt the URL to federate endpoint")

	request, err := http.NewRequest("GET", "prometheus-server.prometheus.svc.cluster.local:80", nil)
	request.URL.Scheme = "http"
	host := "prometheus-server.prometheus.svc.cluster.local"
	path := "federate"
	parameters := url.Values{}

	for _, s := range *matchStrings {
		parameters.Add("match[]", s)
	}

	url := createURL(host, path, parameters)

	request, err = http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Println("Error creating request:", err)
		return nil, err
	}
	fmt.Printf("make call to: %s", url)
	client := &http.Client{}
	return client.Do(request)
}

func init() {
	prometheus.MustRegister(pushErrorCounter, pollErrorCounter, scrapeErrorCounter)
}

func newBackOffFromFlags() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = *retryInitialWait
	b.Multiplier = 1.5
	b.MaxInterval = *retryMaxWait
	b.MaxElapsedTime = time.Duration(0)
	return b
}

// Coordinator for scrape requests and responses
type Coordinator struct {
	logger log.Logger
}

func (c *Coordinator) handleErr(request *http.Request, client *http.Client, err error) {
	level.Error(c.logger).Log("err", err)
	scrapeErrorCounter.Inc()
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       ioutil.NopCloser(strings.NewReader(err.Error())),
		Header:     http.Header{},
	}
	if err = c.doPush(resp, request, client); err != nil {
		pushErrorCounter.Inc()
		level.Warn(c.logger).Log("msg", "Failed to push failed scrape response:", "err", err)
		return
	}
	level.Info(c.logger).Log("msg", "Pushed failed scrape response")
}

func (c *Coordinator) doScrape(request *http.Request, client *http.Client) {
	logger := log.With(c.logger, "scrape_id", request.Header.Get("id"))
	timeout, err := util.GetHeaderTimeout(request.Header)
	if err != nil {
		c.handleErr(request, client, err)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	request = request.WithContext(ctx)
	// We cannot handle https requests at the proxy, as we would only
	// see a CONNECT, so use a URL parameter to trigger it.
	params := request.URL.Query()
	if params.Get("_scheme") == "https" {
		request.URL.Scheme = "https"
		params.Del("_scheme")
		request.URL.RawQuery = params.Encode()
	}

	if *tokenPath != "" {
		token, err := ioutil.ReadFile(*tokenPath)
		if err != nil {
			c.handleErr(request, client, fmt.Errorf("cannot read token from token-path"))
			return
		}
		request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		request.URL.Scheme = "https"
	}

	// We disable the check
	// if request.URL.Hostname() != *myFqdn {
	// 	c.handleErr(request, client, errors.New("scrape target doesn't match client fqdn"))
	// 	return
	// }

	port := request.URL.Port()
	if len(port) > 0 {
		if *allowPort != "*" && *allowPort != port {
			c.handleErr(request, client, fmt.Errorf("client does not have permissions to scrape port %s", port))
			return
		}
		if useLocalhost != nil && *useLocalhost {
			request.URL.Host = fmt.Sprintf("127.0.0.1:%s", port)
		}
	}

	// Hard code to prometheus federate endpoint
	scrapeResp, err := scrapeFederatedPrometheusEndpoint()
	// printPostResponse(scrapeResp)
	if err != nil {
		msg := fmt.Sprintf("failed to scrape %s", request.URL.String())
		c.handleErr(request, client, errors.Wrap(err, msg))
		return
	}
	level.Info(logger).Log("msg", "Retrieved scrape response")
	if err = c.doPush(scrapeResp, request, client); err != nil {
		pushErrorCounter.Inc()
		level.Warn(logger).Log("msg", "Failed to push scrape response:", "err", err)
		return
	}
	level.Info(logger).Log("msg", "Pushed scrape result")
}

// Report the result of the scrape back up to the proxy.
func (c *Coordinator) doPush(resp *http.Response, origRequest *http.Request, client *http.Client) error {
	resp.Header.Set("id", origRequest.Header.Get("id")) // Link the request and response
	// Remaining scrape deadline.
	deadline, _ := origRequest.Context().Deadline()
	resp.Header.Set("X-Prometheus-Scrape-Timeout", fmt.Sprintf("%f", float64(time.Until(deadline))/1e9))

	base, err := url.Parse(*proxyURL)
	if err != nil {
		return err
	}
	u, err := url.Parse("push")
	if err != nil {
		return err
	}
	url := base.ResolveReference(u)

	buf := &bytes.Buffer{}
	resp.Write(buf)
	request := &http.Request{
		Method:        "POST",
		URL:           url,
		Body:          ioutil.NopCloser(buf),
		ContentLength: int64(buf.Len()),
	}
	request = request.WithContext(origRequest.Context())
	if _, err = client.Do(request); err != nil {
		return err
	}
	return nil
}

func (c *Coordinator) doPoll(client *http.Client) error {
	base, err := url.Parse(*proxyURL)
	if err != nil {
		level.Error(c.logger).Log("msg", "Error parsing url:", "err", err)
		return errors.Wrap(err, "error parsing url")
	}
	u, err := url.Parse("poll")
	if err != nil {
		level.Error(c.logger).Log("msg", "Error parsing url:", "err", err)
		return errors.Wrap(err, "error parsing url poll")
	}
	url := base.ResolveReference(u)
	resp, err := client.Post(url.String(), "", strings.NewReader(*myFqdn))
	if err != nil {
		level.Error(c.logger).Log("msg", "Error polling:", "err", err)
		return errors.Wrap(err, "error polling")
	}
	defer resp.Body.Close()

	request, err := http.ReadRequest(bufio.NewReader(resp.Body))
	if err != nil {
		level.Error(c.logger).Log("msg", "Error reading request:", "err", err)
		return errors.Wrap(err, "error reading request")
	}
	level.Info(c.logger).Log("msg", "Got scrape request", "scrape_id", request.Header.Get("id"), "url", request.URL)

	request.RequestURI = ""

	go c.doScrape(request, client)

	return nil
}

func (c *Coordinator) loop(bo backoff.BackOff, client *http.Client) {
	op := func() error {
		return c.doPoll(client)
	}

	for {
		if err := backoff.RetryNotify(op, bo, func(err error, _ time.Duration) {
			pollErrorCounter.Inc()
		}); err != nil {
			level.Error(c.logger).Log("err", err)
		}
	}
}

func main() {
	promlogConfig := promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, &promlogConfig)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promlog.New(&promlogConfig)
	coordinator := Coordinator{logger: logger}

	// if matchStrings array is empty
	if len(*matchStrings) == 0 {
		level.Error(coordinator.logger).Log("msg", "minimum one --match flag must be specified.")
		os.Exit(-1)
	}

	for _, s := range *matchStrings {
		fmt.Println("Use match value ", s)
	}

	if *proxyURL == "" {
		level.Error(coordinator.logger).Log("msg", "--proxy-url flag must be specified.")
		os.Exit(1)
	}
	// Make sure proxyURL ends with a single '/'
	*proxyURL = strings.TrimRight(*proxyURL, "/") + "/"
	level.Info(coordinator.logger).Log("msg", "URL and FQDN info", "proxy_url", *proxyURL, "fqdn", *myFqdn)

	tlsConfig := &tls.Config{}
	if *tlsCert != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			level.Error(coordinator.logger).Log("msg", "Certificate or Key is invalid", "err", err)
			os.Exit(1)
		}

		// Setup HTTPS client
		tlsConfig.Certificates = []tls.Certificate{cert}

		tlsConfig.BuildNameToCertificate()
	}

	if insecureSkipVerify != nil {
		tlsConfig.InsecureSkipVerify = *insecureSkipVerify
	}

	if *caCertFile != "" {
		caCert, err := ioutil.ReadFile(*caCertFile)
		if err != nil {
			level.Error(coordinator.logger).Log("msg", "Not able to read cacert file", "err", err)
			os.Exit(1)
		}
		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
			level.Error(coordinator.logger).Log("msg", "Failed to use cacert file as ca certificate")
			os.Exit(1)
		}

		tlsConfig.RootCAs = caCertPool
	}

	if *metricsAddr != "" {
		go func() {
			if err := http.ListenAndServe(*metricsAddr, promhttp.Handler()); err != nil {
				level.Warn(coordinator.logger).Log("msg", "ListenAndServe", "err", err)
			}
		}()
	}

	if useLocalhost != nil && *useLocalhost && *allowPort == "*" {
		level.Error(coordinator.logger).Log("msg", "client must restrict access on localhost to a single port")
		os.Exit(1)
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsConfig,
	}

	client := &http.Client{Transport: transport}

	coordinator.loop(newBackOffFromFlags(), client)
}
