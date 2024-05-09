package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// An individual Request is used to communicate with the external API. A Request
// is generated via (*Endpoint).NewRequest()
type Request struct {
	endPoint *Endpoint
	queries  []reqQuery
	headers  []reqHeader
	bodyKV   []reqBody
	bodyTXT  string
	hasBody  bool
	Options  *Options
}

type reqQuery keyValuePair
type reqHeader keyValuePair
type reqBody keyValuePair

type valueDataType int
const (
	vdtString valueDataType = iota
	vdtInt
	vdtBool
)

type valueData struct {
	is valueDataType
	s string
	i int
	b bool
}

func (v valueData) string() string {
	var rv string
	switch v.is {
	case vdtString: rv = v.s
	case vdtInt: rv = strconv.Itoa(v.i)
	case vdtBool: rv = strconv.FormatBool(v.b)
	}
	return rv
}

type keyValuePair struct {
	key   string
	value valueData
}

// Initialise new empty API request on specified endpoint
func (e *Endpoint) NewRequest() *Request {
	opt := *e.parent.Options
	return &Request{
		endPoint: e,
		Options: &opt,
	}
}

// Add Query (passed in GET URL) to a request
func (r *Request) AddQuery(key, value string) *Request {
	query := reqQuery{}
	query.key = key
	query.value.is = vdtString
	query.value.s = value
	r.queries = append(r.queries, query)
	return r
}

// AddQueryBool (passed in GET URL) adds a bool value to a request
func (r *Request) AddQueryBool(key string, value bool) *Request {
	query := reqQuery{}
	query.key = key
	query.value.is = vdtBool
	query.value.b = value
	r.queries = append(r.queries, query)
	return r
}

// AddQueryInt (passed in GET URL) adds an int value to a request
func (r *Request) AddQueryInt(key string, value int) *Request {
	query := reqQuery{}
	query.key = key
	query.value.is = vdtInt
	query.value.i = value
	r.queries = append(r.queries, query)
	return r
}

// Add Header to a request
func (r *Request) AddHeader(key, value string) *Request {
	header := reqHeader{}
	header.key = key
	header.value.is = vdtString
	header.value.s = value
	r.headers = append(r.headers, header)
	return r
}

// FormEncoded adds a predefined (Content-Type) header to a request
func (r *Request) FormEncoded() {
	r.AddHeader("Content-Type", "application/x-www-form-urlencoded")
}

// Add a line (in "key=value" format) to the Body of a request
func (r *Request) AddBodyKV(key, value string) *Request {
	body := reqBody{}
	body.key = key
	body.value.is = vdtString
	body.value.s = value
	r.bodyKV = append(r.bodyKV, body)
	r.hasBody = true
	return r
}

// Set the body of the request to a block of JSON-formatted text
//
// TODO: implement proper error handling here
func (r *Request) SetBodyJSON(v any) *Request {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}

	r.bodyTXT = string(b)
	r.hasBody = true
	return r
}

// (*Request).RawQueryURL() generates the GET URL that would be generated by the
// Request and its query key/value pairs, and returns it as a string. This can be
// useful for Callback situations.
func (r *Request) RawQueryURL() (string, error) {
	epURL := r.endPoint.URL()
	httpReq, err := http.NewRequest("GET", epURL, nil)
	if err != nil {
		return "", &PackageError{err}
	}

	httpQuery := httpReq.URL.Query()
	for _, qry := range r.queries {
		httpQuery.Add(qry.key, qry.value.string())
	}
	httpReq.URL.RawQuery = httpQuery.Encode()
	return httpReq.URL.String(), nil
}

// (*Request).GET() processes a GET call to the API
func (r *Request) GET() (*Response, error) {
	res, err := r.callAPIWithTimeout("GET")
	if err == nil { return res, err }

	// todo: check error type; is it a transient error?
	if r.Options.retries > 0 {
		for retry := uint(1); retry <= r.Options.retries; retry++ {
			time.Sleep(500 * time.Millisecond)
			res, err := r.callAPIWithTimeout("GET")
			if err == nil { return res, err }
		}
	}

	return res, err
}

// (*Request).POST() processes a POST call to the API
func (r *Request) POST() (*Response, error) {
	return r.callAPIWithTimeout("POST")
}

type apiCallReturn struct {
	r *Response
	e error
}

// callAPIWithTimeout() handles the call using the specified method, optionally
// implementing timeout
func (r *Request) callAPIWithTimeout(method string) (*Response, error) {
	if r.Options.timeout <= 0 {
		return r.callAPI(method)
	}
 
	duration := time.Millisecond * time.Duration(r.Options.timeout)
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	// call r.CallAPI via a goroutine
	ch := make(chan apiCallReturn)
	go func() {
		res, err := r.callAPI(method)
		ch <- apiCallReturn{
			r: res,
			e: err,
		}
	}()

	// wait for a value returning from our goroutine (or from ctx)
	for {
		select {
		case <- ctx.Done():
			return nil, ErrTimeout
		case resp := <- ch:
			return resp.r, &PackageError{resp.e}
		}
	}

}

// callAPI() handles the call using the specified method
func (r *Request) callAPI(method string) (*Response, error) {
	epURL := r.endPoint.URL()
	httpClient := http.Client{}
	httpReq, err := r.genHTTPReq(method, epURL)
	if err != nil {
		return nil, &PackageError{fmt.Errorf("error in %s(): creating *http.Request: %w", method, err)}
	}

	r.populateHTTPRequest(httpReq)
	res, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, &PackageError{fmt.Errorf("error in %s(): communicating with api: %w", method, err)}
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, &PackageError{fmt.Errorf("error in %s(): reading body of response: %w", method, err)}
	}

	rv := newResponse(res.StatusCode, string(body))
	if rv.Status != http.StatusOK {
		return rv, newQueryError(rv, r)
	}

	return rv, nil
}

func (r *Request) genHTTPReq(method, epURL string) (*http.Request, error) {
	if r.hasBody {

		var bodyString *strings.Reader
		if len(r.bodyTXT) > 0 {
			bodyString = strings.NewReader(r.bodyTXT)
		} else if len(r.bodyKV) > 0 {
			form := url.Values{}
			for _, body := range r.bodyKV {
				form.Add(body.key, body.value.string())
			}
			bodyString = strings.NewReader(form.Encode())
		}
		return http.NewRequest(method, epURL, bodyString)
	} else {
		return http.NewRequest(method, epURL, nil)
	}
}

func (r *Request) populateHTTPRequest(httpReq *http.Request) {
	httpQuery := httpReq.URL.Query()
	for _, qry := range r.queries {
		httpQuery.Add(qry.key, qry.value.string())
	}
	httpReq.URL.RawQuery = httpQuery.Encode()

	for _, hdr := range r.headers {
		httpReq.Header.Set(hdr.key, hdr.value.string())
	}
}
