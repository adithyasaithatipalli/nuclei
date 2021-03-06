package executer

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/nuclei/v2/internal/bufwriter"
	"github.com/projectdiscovery/nuclei/v2/internal/progress"
	"github.com/projectdiscovery/nuclei/v2/pkg/colorizer"
	"github.com/projectdiscovery/nuclei/v2/pkg/globalratelimiter"
	"github.com/projectdiscovery/nuclei/v2/pkg/matchers"
	"github.com/projectdiscovery/nuclei/v2/pkg/requests"
	"github.com/projectdiscovery/nuclei/v2/pkg/templates"
	"github.com/projectdiscovery/rawhttp"
	"github.com/projectdiscovery/retryablehttp-go"
	"github.com/remeh/sizedwaitgroup"
	"golang.org/x/net/proxy"
)

const (
	two = 2
	ten = 10
)

// HTTPExecuter is client for performing HTTP requests
// for a template.
type HTTPExecuter struct {
	coloredOutput   bool
	debug           bool
	Results         bool
	jsonOutput      bool
	jsonRequest     bool
	httpClient      *retryablehttp.Client
	rawHttpClient   *rawhttp.Client
	template        *templates.Template
	bulkHTTPRequest *requests.BulkHTTPRequest
	writer          *bufwriter.Writer
	customHeaders   requests.CustomHeaders
	CookieJar       *cookiejar.Jar

	colorizer        colorizer.NucleiColorizer
	decolorizer      *regexp.Regexp
	stopAtFirstMatch bool
}

// HTTPOptions contains configuration options for the HTTP executer.
type HTTPOptions struct {
	Debug            bool
	JSON             bool
	JSONRequests     bool
	CookieReuse      bool
	ColoredOutput    bool
	Template         *templates.Template
	BulkHTTPRequest  *requests.BulkHTTPRequest
	Writer           *bufwriter.Writer
	Timeout          int
	Retries          int
	ProxyURL         string
	ProxySocksURL    string
	CustomHeaders    requests.CustomHeaders
	CookieJar        *cookiejar.Jar
	Colorizer        *colorizer.NucleiColorizer
	Decolorizer      *regexp.Regexp
	StopAtFirstMatch bool
}

// NewHTTPExecuter creates a new HTTP executer from a template
// and a HTTP request query.
func NewHTTPExecuter(options *HTTPOptions) (*HTTPExecuter, error) {
	var proxyURL *url.URL

	var err error

	if options.ProxyURL != "" {
		proxyURL, err = url.Parse(options.ProxyURL)
	}

	if err != nil {
		return nil, err
	}

	// Create the HTTP Client
	client := makeHTTPClient(proxyURL, options)
	// nolint:bodyclose // false positive there is no body to close yet
	client.CheckRetry = retryablehttp.HostSprayRetryPolicy()

	if options.CookieJar != nil {
		client.HTTPClient.Jar = options.CookieJar
	} else if options.CookieReuse {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, err
		}
		client.HTTPClient.Jar = jar
	}

	// initiate raw http client
	rawClient := rawhttp.NewClient(rawhttp.DefaultOptions)

	executer := &HTTPExecuter{
		debug:            options.Debug,
		jsonOutput:       options.JSON,
		jsonRequest:      options.JSONRequests,
		httpClient:       client,
		rawHttpClient:    rawClient,
		template:         options.Template,
		bulkHTTPRequest:  options.BulkHTTPRequest,
		writer:           options.Writer,
		customHeaders:    options.CustomHeaders,
		CookieJar:        options.CookieJar,
		coloredOutput:    options.ColoredOutput,
		colorizer:        *options.Colorizer,
		decolorizer:      options.Decolorizer,
		stopAtFirstMatch: options.StopAtFirstMatch,
	}

	return executer, nil
}

func (e *HTTPExecuter) ExecuteParallelHTTP(p progress.IProgress, reqURL string) (result Result) {
	result.Matches = make(map[string]interface{})
	result.Extractions = make(map[string]interface{})
	dynamicvalues := make(map[string]interface{})

	// verify if the URL is already being processed
	if e.bulkHTTPRequest.HasGenerator(reqURL) {
		return
	}

	remaining := e.bulkHTTPRequest.GetRequestCount()
	e.bulkHTTPRequest.CreateGenerator(reqURL)

	// Workers that keeps enqueuing new requests
	maxWorkers := e.bulkHTTPRequest.Threads
	swg := sizedwaitgroup.New(maxWorkers)
	for e.bulkHTTPRequest.Next(reqURL) && !result.Done {
		request, err := e.bulkHTTPRequest.MakeHTTPRequest(reqURL, dynamicvalues, e.bulkHTTPRequest.Current(reqURL))
		if err != nil {
			result.Error = err
			p.Drop(remaining)
		} else {
			swg.Add()
			go func(httpRequest *requests.HTTPRequest) {
				defer swg.Done()

				globalratelimiter.Take(reqURL)

				// If the request was built correctly then execute it
				err = e.handleHTTP(reqURL, httpRequest, dynamicvalues, &result)
				if err != nil {
					result.Error = errors.Wrap(err, "could not handle http request")
					p.Drop(remaining)
				}
			}(request)
		}
		e.bulkHTTPRequest.Increment(reqURL)
	}

	swg.Wait()

	return result
}

func (e *HTTPExecuter) ExecuteTurboHTTP(p progress.IProgress, reqURL string) (result Result) {
	result.Matches = make(map[string]interface{})
	result.Extractions = make(map[string]interface{})
	dynamicvalues := make(map[string]interface{})

	// verify if the URL is already being processed
	if e.bulkHTTPRequest.HasGenerator(reqURL) {
		return
	}

	remaining := e.bulkHTTPRequest.GetRequestCount()
	e.bulkHTTPRequest.CreateGenerator(reqURL)

	// need to extract the target from the url
	URL, err := url.Parse(reqURL)
	if err != nil {
		return
	}

	pipeOptions := rawhttp.DefaultPipelineOptions
	pipeOptions.Host = URL.Host
	pipeOptions.MaxConnections = 1
	if e.bulkHTTPRequest.PipelineMaxWorkers > 0 {
		pipeOptions.MaxConnections = e.bulkHTTPRequest.PipelineMaxWorkers
	}
	pipeclient := rawhttp.NewPipelineClient(pipeOptions)

	// Workers that keeps enqueuing new requests
	maxWorkers := 150
	if e.bulkHTTPRequest.PipelineMaxWorkers > 0 {
		maxWorkers = e.bulkHTTPRequest.PipelineMaxWorkers
	}

	swg := sizedwaitgroup.New(maxWorkers)
	for e.bulkHTTPRequest.Next(reqURL) && !result.Done {
		request, err := e.bulkHTTPRequest.MakeHTTPRequest(reqURL, dynamicvalues, e.bulkHTTPRequest.Current(reqURL))
		if err != nil {
			result.Error = err
			p.Drop(remaining)
		} else {
			swg.Add()
			go func(httpRequest *requests.HTTPRequest) {
				defer swg.Done()

				// HTTP pipelining ignores rate limit

				// If the request was built correctly then execute it
				request.PipelineClient = pipeclient
				err = e.handleHTTP(reqURL, httpRequest, dynamicvalues, &result)
				if err != nil {
					result.Error = errors.Wrap(err, "could not handle http request")
					p.Drop(remaining)
				}
				request.PipelineClient = nil

			}(request)
		}

		e.bulkHTTPRequest.Increment(reqURL)
	}

	swg.Wait()

	return result
}

// ExecuteHTTP executes the HTTP request on a URL
func (e *HTTPExecuter) ExecuteHTTP(p progress.IProgress, reqURL string) (result Result) {
	// verify if pipeline was requested
	if e.bulkHTTPRequest.Pipeline {
		return e.ExecuteTurboHTTP(p, reqURL)
	}

	if e.bulkHTTPRequest.Threads > 0 {
		return e.ExecuteParallelHTTP(p, reqURL)
	}

	result.Matches = make(map[string]interface{})
	result.Extractions = make(map[string]interface{})
	dynamicvalues := make(map[string]interface{})

	// verify if the URL is already being processed
	if e.bulkHTTPRequest.HasGenerator(reqURL) {
		return
	}

	remaining := e.bulkHTTPRequest.GetRequestCount()
	e.bulkHTTPRequest.CreateGenerator(reqURL)

	for e.bulkHTTPRequest.Next(reqURL) && !result.Done {
		httpRequest, err := e.bulkHTTPRequest.MakeHTTPRequest(reqURL, dynamicvalues, e.bulkHTTPRequest.Current(reqURL))
		if err != nil {
			result.Error = err
			p.Drop(remaining)
		} else {
			globalratelimiter.Take(reqURL)
			// If the request was built correctly then execute it
			err = e.handleHTTP(reqURL, httpRequest, dynamicvalues, &result)
			if err != nil {
				result.Error = errors.Wrap(err, "could not handle http request")
				p.Drop(remaining)
			}
		}

		// Check if has to stop processing at first valid result
		if e.stopAtFirstMatch && result.GotResults {
			p.Drop(remaining)
			break
		}

		// move always forward with requests
		e.bulkHTTPRequest.Increment(reqURL)
		p.Update()
		remaining--
	}

	gologger.Verbosef("Sent for [%s] to %s\n", "http-request", e.template.ID, reqURL)

	return result
}

func (e *HTTPExecuter) handleHTTP(reqURL string, request *requests.HTTPRequest, dynamicvalues map[string]interface{}, result *Result) error {
	e.setCustomHeaders(request)

	var (
		resp *http.Response
		err  error
	)

	if e.debug {
		dumpedRequest, err := requests.Dump(request, reqURL)
		if err != nil {
			return err
		}

		gologger.Infof("Dumped HTTP request for %s (%s)\n\n", reqURL, e.template.ID)
		fmt.Fprintf(os.Stderr, "%s", string(dumpedRequest))
	}

	timeStart := time.Now()
	if request.Pipeline {
		resp, err = request.PipelineClient.DoRaw(request.RawRequest.Method, reqURL, request.RawRequest.Path, requests.ExpandMapValues(request.RawRequest.Headers), ioutil.NopCloser(strings.NewReader(request.RawRequest.Data)))
		if err != nil {
			return err
		}
	} else if request.Unsafe {
		// rawhttp
		// burp uses "\r\n" as new line character
		request.RawRequest.Data = strings.ReplaceAll(request.RawRequest.Data, "\n", "\r\n")
		options := e.rawHttpClient.Options
		options.AutomaticContentLength = request.AutomaticContentLengthHeader
		options.AutomaticHostHeader = request.AutomaticHostHeader
		resp, err = e.rawHttpClient.DoRawWithOptions(request.RawRequest.Method, reqURL, request.RawRequest.Path, requests.ExpandMapValues(request.RawRequest.Headers), ioutil.NopCloser(strings.NewReader(request.RawRequest.Data)), options)
		if err != nil {
			return err
		}
	} else {
		// retryablehttp
		resp, err = e.httpClient.Do(request.Request)
		if err != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return err
		}
	}
	duration := time.Since(timeStart)

	if e.debug {
		dumpedResponse, dumpErr := httputil.DumpResponse(resp, true)
		if dumpErr != nil {
			return errors.Wrap(dumpErr, "could not dump http response")
		}

		gologger.Infof("Dumped HTTP response for %s (%s)\n\n", reqURL, e.template.ID)
		fmt.Fprintf(os.Stderr, "%s\n", string(dumpedResponse))
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		_, copyErr := io.Copy(ioutil.Discard, resp.Body)
		if copyErr != nil {
			resp.Body.Close()
			return copyErr
		}

		resp.Body.Close()

		return errors.Wrap(err, "could not read http body")
	}

	resp.Body.Close()

	// net/http doesn't automatically decompress the response body if an encoding has been specified by the user in the request
	// so in case we have to manually do it
	data, err = requests.HandleDecompression(request, data)
	if err != nil {
		return errors.Wrap(err, "could not decompress http body")
	}

	// Convert response body from []byte to string with zero copy
	body := unsafeToString(data)

	headers := headersToString(resp.Header)
	matcherCondition := e.bulkHTTPRequest.GetMatchersCondition()

	for _, matcher := range e.bulkHTTPRequest.Matchers {
		// Check if the matcher matched
		if !matcher.Match(resp, body, headers, duration) {
			// If the condition is AND we haven't matched, try next request.
			if matcherCondition == matchers.ANDCondition {
				return nil
			}
		} else {
			// If the matcher has matched, and its an OR
			// write the first output then move to next matcher.
			if matcherCondition == matchers.ORCondition {
				result.Lock()
				result.Matches[matcher.Name] = nil
				// probably redundant but ensures we snapshot current payload values when matchers are valid
				result.Meta = request.Meta
				result.GotResults = true
				result.Unlock()
				e.writeOutputHTTP(request, resp, body, matcher, nil)
			}
		}
	}

	// All matchers have successfully completed so now start with the
	// next task which is extraction of input from matchers.
	var extractorResults, outputExtractorResults []string

	for _, extractor := range e.bulkHTTPRequest.Extractors {
		for match := range extractor.Extract(resp, body, headers) {
			if _, ok := dynamicvalues[extractor.Name]; !ok {
				dynamicvalues[extractor.Name] = match
			}

			extractorResults = append(extractorResults, match)

			if !extractor.Internal {
				outputExtractorResults = append(outputExtractorResults, match)
			}
		}
		// probably redundant but ensures we snapshot current payload values when extractors are valid
		result.Lock()
		result.Meta = request.Meta
		result.Extractions[extractor.Name] = extractorResults
		result.Unlock()
	}

	// Write a final string of output if matcher type is
	// AND or if we have extractors for the mechanism too.
	if len(outputExtractorResults) > 0 || matcherCondition == matchers.ANDCondition {
		e.writeOutputHTTP(request, resp, body, nil, outputExtractorResults)
		result.Lock()
		result.GotResults = true
		result.Unlock()
	}

	return nil
}

// Close closes the http executer for a template.
func (e *HTTPExecuter) Close() {}

// makeHTTPClient creates a http client
func makeHTTPClient(proxyURL *url.URL, options *HTTPOptions) *retryablehttp.Client {
	// Multiple Host
	retryablehttpOptions := retryablehttp.DefaultOptionsSpraying
	disableKeepAlives := true
	maxIdleConns := 0
	maxConnsPerHost := 0
	maxIdleConnsPerHost := -1

	if options.BulkHTTPRequest.Threads > 0 {
		// Single host
		retryablehttpOptions = retryablehttp.DefaultOptionsSingle
		disableKeepAlives = false
		maxIdleConnsPerHost = 500
		maxConnsPerHost = 500
	}

	retryablehttpOptions.RetryWaitMax = 10 * time.Second
	retryablehttpOptions.RetryMax = options.Retries
	followRedirects := options.BulkHTTPRequest.Redirects
	maxRedirects := options.BulkHTTPRequest.MaxRedirects

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		MaxConnsPerHost:     maxConnsPerHost,
		TLSClientConfig: &tls.Config{
			Renegotiation:      tls.RenegotiateOnceAsClient,
			InsecureSkipVerify: true,
		},
		DisableKeepAlives: disableKeepAlives,
	}

	// Attempts to overwrite the dial function with the socks proxied version
	if options.ProxySocksURL != "" {
		var proxyAuth *proxy.Auth

		socksURL, err := url.Parse(options.ProxySocksURL)

		if err == nil {
			proxyAuth = &proxy.Auth{}
			proxyAuth.User = socksURL.User.Username()
			proxyAuth.Password, _ = socksURL.User.Password()
		}

		dialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("%s:%s", socksURL.Hostname(), socksURL.Port()), proxyAuth, proxy.Direct)
		dc := dialer.(interface {
			DialContext(ctx context.Context, network, addr string) (net.Conn, error)
		})

		if err == nil {
			transport.DialContext = dc.DialContext
		}
	}

	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}

	return retryablehttp.NewWithHTTPClient(&http.Client{
		Transport:     transport,
		Timeout:       time.Duration(options.Timeout) * time.Second,
		CheckRedirect: makeCheckRedirectFunc(followRedirects, maxRedirects),
	}, retryablehttpOptions)
}

type checkRedirectFunc func(_ *http.Request, requests []*http.Request) error

func makeCheckRedirectFunc(followRedirects bool, maxRedirects int) checkRedirectFunc {
	return func(_ *http.Request, requests []*http.Request) error {
		if !followRedirects {
			return http.ErrUseLastResponse
		}

		if maxRedirects == 0 {
			if len(requests) > ten {
				return http.ErrUseLastResponse
			}

			return nil
		}

		if len(requests) > maxRedirects {
			return http.ErrUseLastResponse
		}

		return nil
	}
}

func (e *HTTPExecuter) setCustomHeaders(r *requests.HTTPRequest) {
	for _, customHeader := range e.customHeaders {
		// This should be pre-computed somewhere and done only once
		tokens := strings.SplitN(customHeader, ":", 2)
		// if it's an invalid header skip it
		if len(tokens) < two {
			continue
		}

		headerName, headerValue := tokens[0], strings.Join(tokens[1:], "")
		if r.RawRequest != nil {
			// rawhttp
			r.RawRequest.Headers[headerName] = headerValue
		} else {
			// retryablehttp
			headerName = strings.TrimSpace(headerName)
			headerValue = strings.TrimSpace(headerValue)
			r.Request.Header[headerName] = []string{headerValue}
		}
	}
}

type Result struct {
	sync.Mutex
	GotResults  bool
	Done        bool
	Meta        map[string]interface{}
	Matches     map[string]interface{}
	Extractions map[string]interface{}
	Error       error
}
