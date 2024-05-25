package bramble

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vektah/gqlparser/v2/ast"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

// GraphQLClient is a GraphQL client.
type GraphQLClient struct {
	HTTPClient      *http.Client
	MaxResponseSize int64
	UserAgent       string

	tracer trace.Tracer
}

// ClientOpt is a function used to set a GraphQL client option
type ClientOpt func(*GraphQLClient)

// NewClient creates a new GraphQLClient from the given options.
func NewClient(opts ...ClientOpt) *GraphQLClient {
	return NewClientWithPlugins(nil, opts...)
}

func NewClientWithPlugins(plugins []Plugin, opts ...ClientOpt) *GraphQLClient {
	var transport http.RoundTripper = http.DefaultTransport
	transport = otelhttp.NewTransport(transport)

	c := &GraphQLClient{
		HTTPClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
		},
		tracer:          otel.GetTracerProvider().Tracer(instrumentationName),
		MaxResponseSize: 1024 * 1024,
	}

	for _, opt := range opts {
		opt(c)
	}

	for _, plugin := range plugins {
		c.HTTPClient.Transport = plugin.WrapGraphQLClientTransport(c.HTTPClient.Transport)
	}
	return c
}

func NewClientWithoutKeepAlive(opts ...ClientOpt) *GraphQLClient {
	var defaultTransport = http.DefaultTransport.(*http.Transport).Clone()
	defaultTransport.DisableKeepAlives = true

	var transport http.RoundTripper = defaultTransport
	transport = otelhttp.NewTransport(transport)

	c := &GraphQLClient{
		HTTPClient: &http.Client{
			Timeout:   5 * time.Second,
			Transport: transport,
		},
		tracer:          otel.GetTracerProvider().Tracer(instrumentationName),
		MaxResponseSize: 1024 * 1024,
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// WithHTTPClient sets a custom HTTP client to be used when making downstream queries.
func WithHTTPClient(client *http.Client) ClientOpt {
	return func(s *GraphQLClient) {
		s.HTTPClient = client
	}
}

// WithMaxResponseSize sets the max allowed response size. The client will only
// read up to maxResponseSize and that size is exceeded an an error will be
// returned.
func WithMaxResponseSize(maxResponseSize int64) ClientOpt {
	return func(s *GraphQLClient) {
		s.MaxResponseSize = maxResponseSize
	}
}

// WithUserAgent set the user agent used by the client.
func WithUserAgent(userAgent string) ClientOpt {
	return func(s *GraphQLClient) {
		s.UserAgent = userAgent
	}
}

// Request executes a GraphQL request.
func (c *GraphQLClient) Request(ctx context.Context, url string, request *Request, out interface{}) error {
	ctx, span := c.tracer.Start(ctx, "GraphQL Request",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			semconv.GraphqlOperationTypeKey.String(string(request.OperationType)),
			semconv.GraphqlOperationName(request.OperationName),
			semconv.GraphqlDocument(request.Query),
		),
	)

	defer span.End()

	traceErr := func(err error) error {
		if err == nil {
			return err
		}

		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	var buf bytes.Buffer
	contentType := "application/json; charset=utf-8"
	if ct := request.Headers.Get("Content-Type"); !strings.Contains(ct, "multipart") {
		err := json.NewEncoder(&buf).Encode(request)
		if err != nil {
			return fmt.Errorf("unable to encode request body: %w", err)
		}
	} else {
		mpt, err := prepareMultipartData(request)
		if err != nil {
			return fmt.Errorf("unable to encode request body: %w", err)
		}
		buf = mpt.buf
		contentType = mpt.contentType
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return traceErr(fmt.Errorf("unable to create request: %w", err))
	}

	if request.Headers != nil {
		httpReq.Header = request.Headers.Clone()
	}

	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Accept", "application/json")

	if c.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.UserAgent)
	}

	res, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		if os.IsTimeout(err) {
			promServiceTimeoutErrorCounter.With(prometheus.Labels{
				"service": url,
			}).Inc()

			// Return raw timeout error to allow caller to handle it since a
			// downstream caller may want to retry, and they will have to jump
			// through hoops to detect this error otherwise.
			return traceErr(err)
		}
		return traceErr(fmt.Errorf("error during request: %w", err))
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return traceErr(fmt.Errorf("unexpected response code: %s", res.Status))
	}

	maxResponseSize := c.MaxResponseSize
	if maxResponseSize == 0 {
		maxResponseSize = math.MaxInt64
	}

	limitReader := io.LimitedReader{
		R: res.Body,
		N: maxResponseSize,
	}

	graphqlResponse := Response{
		Data: out,
	}

	if err = json.NewDecoder(&limitReader).Decode(&graphqlResponse); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			if limitReader.N == 0 {
				return traceErr(fmt.Errorf("response exceeded maximum size of %d bytes", maxResponseSize))
			}
		}
		return traceErr(fmt.Errorf("error decoding response: %w", err))
	}

	if len(graphqlResponse.Errors) > 0 {
		return traceErr(graphqlResponse.Errors)
	}

	return nil
}

// Request is a GraphQL request.
type Request struct {
	OperationType string                 `json:"operationType,omitempty"`
	Query         string                 `json:"query"`
	OperationName string                 `json:"operationName,omitempty"`
	Variables     map[string]interface{} `json:"variables,omitempty"`
	Headers       http.Header            `json:"-"`
}

// NewRequest creates a new GraphQL requests from the provided body.
func NewRequest(query string) *Request {
	return &Request{
		Query: query,
	}
}

func (r *Request) WithHeaders(headers http.Header) *Request {
	r.Headers = headers
	return r
}

func (r *Request) WithOperationType(operation string) *Request {
	op := strings.ToLower(operation)
	switch op {
	case "query", "mutation", "subscription":
		r.OperationType = op
	default:
		r.OperationType = "query"
	}

	return r
}

func (r *Request) WithOperationName(operationName string) *Request {

	r.OperationName = operationName
	return r
}

func (r *Request) WithVariables(variables map[string]interface{}) *Request {
	r.Variables = variables
	return r
}

// Response is a GraphQL response
type Response struct {
	Errors GraphqlErrors `json:"errors"`
	Data   interface{}
}

// GraphqlErrors represents a list of GraphQL errors, as returned in a GraphQL
// response.
type GraphqlErrors []GraphqlError

// GraphqlError is a single GraphQL error
type GraphqlError struct {
	Message    string                 `json:"message"`
	Path       ast.Path               `json:"path,omitempty"`
	Extensions map[string]interface{} `json:"extensions"`
}

// Error returns a string representation of the error list
func (e GraphqlErrors) Error() string {
	var errs []string
	for _, err := range e {
		errs = append(errs, err.Message)
	}
	return strings.Join(errs, ",")
}

func GenerateUserAgent(operation string) string {
	return fmt.Sprintf("Bramble/%s (%s)", Version, operation)
}

type parseMultipartVariablesResult struct {
	fileMap map[string][]string
	files   map[string]graphql.Upload
	m       map[string]any
}

type parseMultipartVariablesStackItem struct {
	key  string
	path string
	data map[string]interface{}
}

func parseMultipartVariables(variables map[string]any) parseMultipartVariablesResult {
	stack := []parseMultipartVariablesStackItem{{key: "", data: variables}}

	index := 0
	fileMap := map[string][]string{}
	files := map[string]graphql.Upload{}
	for len(stack) > 0 {
		currentItem := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		for key, value := range currentItem.data {
			var currentPath string
			if currentItem.path == "" {
				currentPath = key
			} else {
				currentPath = currentItem.path + "." + key
			}

			switch v := value.(type) {
			case graphql.Upload:
				currentItem.data[key] = nil
				fileIndex := fmt.Sprintf("file%d", index)
				fileMap[fileIndex] = []string{fmt.Sprintf("variables.%s", currentPath)}
				index += 1
				files[fileIndex] = v
			case *graphql.Upload:
				currentItem.data[key] = nil
				fileIndex := fmt.Sprintf("file%d", index)
				fileMap[fileIndex] = []string{fmt.Sprintf("variables.%s", currentPath)}
				index += 1
				files[fileIndex] = *v
			case map[string]any:
				stack = append(stack, parseMultipartVariablesStackItem{key: key, data: v, path: currentPath})
			default:
			}
		}
	}
	return parseMultipartVariablesResult{
		fileMap: fileMap,
		files:   files,
		m:       variables,
	}
}

type prepareMultipartDataResult struct {
	buf         bytes.Buffer
	contentType string
}

func prepareMultipartData(request *Request) (*prepareMultipartDataResult, error) {
	res := parseMultipartVariables(request.Variables)

	var fw io.Writer
	var buf bytes.Buffer
	mpw := multipart.NewWriter(&buf)
	fw, err := mpw.CreateFormField("operations")
	if err != nil {
		return nil, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	fw.Write(input)
	fw, err = mpw.CreateFormField("map")
	if err != nil {
		return nil, err
	}
	for fileIndex, path := range res.fileMap {
		fw.Write([]byte(fmt.Sprintf(
			"{\"%s\": [\"%s\"]}", fileIndex, path[0],
		)))
		fw, fileErr := mpw.CreateFormFile(fileIndex, res.files[fileIndex].Filename)
		if fileErr != nil {
			return nil, fileErr
		}
		io.Copy(fw, res.files[fileIndex].File)
	}
	err = mpw.Close()
	if err != nil {
		return nil, err
	}
	contentType := mpw.FormDataContentType()
	return &prepareMultipartDataResult{
		buf:         buf,
		contentType: contentType,
	}, nil
}
