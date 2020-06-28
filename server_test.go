package httpretty

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// inspect a request (not concurrency safe).
func inspect(next http.Handler, wait int) *inspectHandler {
	is := &inspectHandler{
		next: next,
	}

	is.wg.Add(wait)

	return is
}

type inspectHandler struct {
	next http.Handler

	wg  sync.WaitGroup
	req *http.Request
}

func (h *inspectHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.req = req
	h.next.ServeHTTP(w, req)
	h.wg.Done()
}

func (h *inspectHandler) Wait() {
	h.wg.Wait()
}

func TestIncoming(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(helloHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

	if err != nil {
		t.Errorf("cannot create request: %v", err)
	}

	go func() {
		client := newServerClient()

		resp, err := client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}

		testBody(t, resp.Body, []byte("Hello, world!"))
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to http://%s/
* Request from %s
> GET / HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

< HTTP/1.1 200 OK

Hello, world!
`, is.req.Host, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingNotFound(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		ResponseHeader: true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(http.NotFoundHandler()), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

	if err != nil {
		t.Errorf("cannot create request: %v", err)
	}

	go func() {
		client := newServerClient()

		resp, err := client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("got status codem %v, wanted %v", resp.StatusCode, http.StatusNotFound)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to http://%s/
* Request from %s
> GET / HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

< HTTP/1.1 404 Not Found
< Content-Type: text/plain; charset=utf-8
< X-Content-Type-Options: nosniff

`, is.req.Host, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func outgoingGetServer(client *http.Client, ts *httptest.Server, done func()) {
	defer done()

	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

	if err != nil {
		panic(err)
	}

	if _, err := client.Do(req); err != nil {
		panic(err)
	}
}

func TestIncomingConcurrency(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		TLS:            true,
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	logger.SetFlusher(OnEnd)

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	ts := httptest.NewServer(logger.Middleware(helloHandler{}))
	defer ts.Close()

	var wg sync.WaitGroup
	concurrency := 100
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		client := &http.Client{
			Transport: newTransport(),
		}

		go outgoingGetServer(client, ts, wg.Done)
	}

	wg.Wait()

	got := buf.String()

	gotConcurrency := strings.Count(got, "< HTTP/1.1 200 OK")

	if concurrency != gotConcurrency {
		t.Errorf("logged %d requests, wanted %d", concurrency, gotConcurrency)
	}

	want := fmt.Sprintf(`> GET / HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

< HTTP/1.1 200 OK

Hello, world!`, ts.Listener.Addr())

	if !strings.Contains(got, want) {
		t.Errorf("Request doesn't contain expected body")
	}
}

func TestIncomingMinimal(t *testing.T) {
	t.Parallel()

	// only prints the request URI and remote address that requested it.
	logger := &Logger{}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(helloHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()
	uri := fmt.Sprintf("%s/incoming", ts.URL)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodGet, uri, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		req.AddCookie(&http.Cookie{
			Name:  "food",
			Value: "sorbet",
		})

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
`, uri, is.req.RemoteAddr)

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingSanitized(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(helloHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()
	uri := fmt.Sprintf("%s/incoming", ts.URL)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodGet, uri, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		req.AddCookie(&http.Cookie{
			Name:  "food",
			Value: "sorbet",
		})

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /incoming HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> Cookie: food=████████████████████
> User-Agent: Robot/0.1 crawler@example.com

< HTTP/1.1 200 OK

Hello, world!
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

type hideHandler struct {
	next http.Handler
}

func (h hideHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	req = req.WithContext(WithHide(context.Background()))
	h.next.ServeHTTP(w, req)
}

func TestIncomingHide(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(hideHandler{
		next: logger.Middleware(helloHandler{}),
	}, 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	if buf.Len() != 0 {
		t.Errorf("request should not be logged, got %v", buf.String())
	}
}

func TestIncomingFilter(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.SetFilter(filteredURIs)

	ts := httptest.NewServer(logger.Middleware(helloHandler{}))
	defer ts.Close()

	testCases := []struct {
		uri  string
		want string
	}{
		{uri: "filtered"},
		{uri: "unfiltered", want: "* Request"},
		{uri: "other", want: "filter error triggered"},
	}
	for _, tc := range testCases {
		t.Run(tc.uri, func(t *testing.T) {
			var buf bytes.Buffer
			logger.SetOutput(&buf)

			client := newServerClient()
			_, err := client.Get(fmt.Sprintf("%s/%s", ts.URL, tc.uri))

			if err != nil {
				t.Errorf("cannot create request: %v", err)
			}

			if tc.want == "" && buf.Len() != 0 {
				t.Errorf("wanted input to be filtered, got %v instead", buf.String())
			}

			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf(`expected input to contain "%v", got %v instead`, tc.want, buf.String())
			}
		})
	}
}

func TestIncomingFilterPanicked(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.SetFilter(func(req *http.Request) (bool, error) {
		panic("evil panic")
	})

	is := inspect(logger.Middleware(helloHandler{}), 1)
	ts := httptest.NewServer(is)
	defer ts.Close()

	client := newServerClient()
	_, err := client.Get(ts.URL)

	if err != nil {
		t.Errorf("cannot create request: %v", err)
	}

	want := fmt.Sprintf(`* cannot filter request: GET /: panic: evil panic
* Request to %v/
* Request from %v
> GET / HTTP/1.1
> Host: %v
> Accept-Encoding: gzip
> User-Agent: Go-http-client/1.1

< HTTP/1.1 200 OK

Hello, world!
`, ts.URL, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf(`expected input to contain "%v", got %v instead`, want, got)
	}
}

func TestIncomingSkipHeader(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.SkipHeader([]string{
		"user-agent",
		"content-type",
	})

	is := inspect(logger.Middleware(jsonHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	client := newServerClient()

	uri := fmt.Sprintf("%s/json", ts.URL)

	go func() {
		req, err := http.NewRequest(http.MethodGet, uri, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /json HTTP/1.1
> Host: %s
> Accept-Encoding: gzip

< HTTP/1.1 200 OK

{"result":"Hello, world!","number":3.14}
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingBodyFilter(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.SetBodyFilter(func(h http.Header) (skip bool, err error) {
		mediatype, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
		return mediatype == "application/json", nil
	})

	is := inspect(logger.Middleware(jsonHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	client := newServerClient()

	uri := fmt.Sprintf("%s/json", ts.URL)

	go func() {
		req, err := http.NewRequest(http.MethodGet, uri, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /json HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

< HTTP/1.1 200 OK
< Content-Type: application/json; charset=utf-8

`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingBodyFilterSoftError(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.SetBodyFilter(func(h http.Header) (skip bool, err error) {
		// filter anyway, but print soft error saying something went wrong during the filtering.
		return true, errors.New("incomplete implementation")
	})

	is := inspect(logger.Middleware(jsonHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	client := newServerClient()

	uri := fmt.Sprintf("%s/json", ts.URL)

	go func() {
		req, err := http.NewRequest(http.MethodGet, uri, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /json HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

* error on request body filter: incomplete implementation
< HTTP/1.1 200 OK
< Content-Type: application/json; charset=utf-8

* error on response body filter: incomplete implementation
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingBodyFilterPanicked(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.SetBodyFilter(func(h http.Header) (skip bool, err error) {
		panic("evil panic")
	})

	is := inspect(logger.Middleware(jsonHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	client := newServerClient()

	uri := fmt.Sprintf("%s/json", ts.URL)

	go func() {
		req, err := http.NewRequest(http.MethodGet, uri, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /json HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

* panic while filtering body: evil panic
< HTTP/1.1 200 OK
< Content-Type: application/json; charset=utf-8

* panic while filtering body: evil panic
{"result":"Hello, world!","number":3.14}
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingWithTimeRequest(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		Time:           true,
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(helloHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	go func() {
		client := &http.Client{
			Transport: newTransport(),
		}

		req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	got := buf.String()

	if !strings.Contains(got, "* Request at ") {
		t.Error("missing printing start time of request")
	}

	if !strings.Contains(got, "* Request took ") {
		t.Error("missing printing request duration")
	}
}

func TestIncomingFormattedJSON(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.Formatters = []Formatter{
		&JSONFormatter{},
	}

	is := inspect(logger.Middleware(jsonHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	client := newServerClient()

	uri := fmt.Sprintf("%s/json", ts.URL)

	go func() {
		req, err := http.NewRequest(http.MethodGet, uri, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /json HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

< HTTP/1.1 200 OK
< Content-Type: application/json; charset=utf-8

{
    "result": "Hello, world!",
    "number": 3.14
}
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingBadJSON(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.Formatters = []Formatter{
		&JSONFormatter{},
	}

	is := inspect(logger.Middleware(badJSONHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/json", ts.URL)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodGet, uri, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /json HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

< HTTP/1.1 200 OK
< Content-Type: application/json; charset=utf-8

* body cannot be formatted: invalid character '}' looking for beginning of value
{"bad": }
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingFormatterPanicked(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.Formatters = []Formatter{
		&panickingFormatter{},
	}

	is := inspect(logger.Middleware(badJSONHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/json", ts.URL)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodGet, uri, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /json HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

< HTTP/1.1 200 OK
< Content-Type: application/json; charset=utf-8

* body cannot be formatted: panic: evil formatter
{"bad": }
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingFormatterMatcherPanicked(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.Formatters = []Formatter{
		&panickingFormatterMatcher{},
	}

	is := inspect(logger.Middleware(badJSONHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/json", ts.URL)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodGet, uri, nil)
		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /json HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

< HTTP/1.1 200 OK
< Content-Type: application/json; charset=utf-8

* panic while testing body format: evil matcher
{"bad": }
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingForm(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(formHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	logger.Formatters = []Formatter{
		&JSONFormatter{},
	}

	uri := fmt.Sprintf("%s/form", ts.URL)

	go func() {
		client := newServerClient()

		form := url.Values{}
		form.Add("foo", "bar")
		form.Add("email", "root@example.com")

		req, err := http.NewRequest(http.MethodPost, uri, strings.NewReader(form.Encode()))

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> POST /form HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> Content-Length: 32
> User-Agent: Go-http-client/1.1

email=root%%40example.com&foo=bar
< HTTP/1.1 200 OK

form received
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingBinaryBody(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Date"] = nil
		fmt.Fprint(w, "\x25\x50\x44\x46\x2d\x31\x2e\x33\x0a\x25\xc4\xe5\xf2\xe5\xeb\xa7")
	})), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/convert", ts.URL)

	go func() {
		client := newServerClient()

		b := []byte("RIFF\x00\x00\x00\x00WEBPVP")
		req, err := http.NewRequest(http.MethodPost, uri, bytes.NewReader(b))
		req.Header.Add("Content-Type", "image/webp")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> POST /convert HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> Content-Length: 14
> Content-Type: image/webp
> User-Agent: Go-http-client/1.1

* body contains binary data
< HTTP/1.1 200 OK

* body contains binary data
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingBinaryBodyNoMediatypeHeader(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Date"] = nil
		w.Header()["Content-Type"] = nil
		fmt.Fprint(w, "\x25\x50\x44\x46\x2d\x31\x2e\x33\x0a\x25\xc4\xe5\xf2\xe5\xeb\xa7")
	})), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/convert", ts.URL)

	go func() {
		client := newServerClient()

		b := []byte("RIFF\x00\x00\x00\x00WEBPVP")
		req, err := http.NewRequest(http.MethodPost, uri, bytes.NewReader(b))

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> POST /convert HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> Content-Length: 14
> User-Agent: Go-http-client/1.1

* body contains binary data
< HTTP/1.1 200 OK

* body contains binary data
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingLongRequest(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(longRequestHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/long-request", ts.URL)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodPut, uri, strings.NewReader(petition))

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> PUT /long-request HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> Content-Length: 9846
> User-Agent: Go-http-client/1.1

%s
< HTTP/1.1 200 OK

long request received
`, uri, is.req.RemoteAddr, ts.Listener.Addr(), petition)

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingLongResponse(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.MaxResponseBody = int64(len(petition) + 1000) // value larger than the text

	is := inspect(logger.Middleware(longResponseHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/long-response", ts.URL)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodGet, uri, nil)

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		resp, err := client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}

		testBody(t, resp.Body, []byte(petition))
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /long-response HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Go-http-client/1.1

< HTTP/1.1 200 OK
< Content-Length: 9846

%s
`, uri, is.req.RemoteAddr, ts.Listener.Addr(), petition)

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingLongResponseHead(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.MaxResponseBody = int64(len(petition) + 1000) // value larger than the text

	is := inspect(logger.Middleware(longResponseHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	client := newServerClient()

	uri := fmt.Sprintf("%s/long-response", ts.URL)

	go func() {
		req, err := http.NewRequest(http.MethodHead, uri, nil)

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> HEAD /long-response HTTP/1.1
> Host: %s
> User-Agent: Go-http-client/1.1

< HTTP/1.1 200 OK
< Content-Length: 9846

`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingTooLongResponse(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.MaxResponseBody = 5000 // value smaller than the text

	is := inspect(logger.Middleware(longResponseHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/long-response", ts.URL)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodGet, uri, nil)

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		resp, err := client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}

		testBody(t, resp.Body, []byte(petition))
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /long-response HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Go-http-client/1.1

< HTTP/1.1 200 OK
< Content-Length: 9846

* body is too long (9846 bytes) to print, skipping (longer than 5000 bytes)
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingLongResponseUnknownLength(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:   true,
		RequestBody:     true,
		ResponseHeader:  true,
		ResponseBody:    true,
		MaxResponseBody: 10000000,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	repeat := 100
	is := inspect(logger.Middleware(longResponseUnknownLengthHandler{repeat: repeat}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/long-response", ts.URL)
	repeatedBody := strings.Repeat(petition, repeat+1)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodGet, uri, nil)

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		resp, err := client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}

		testBody(t, resp.Body, []byte(repeatedBody))
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /long-response HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Go-http-client/1.1

< HTTP/1.1 200 OK

%s
`, uri, is.req.RemoteAddr, ts.Listener.Addr(), repeatedBody)

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingLongResponseUnknownLengthTooLong(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.MaxResponseBody = 5000 // value smaller than the text

	is := inspect(logger.Middleware(longResponseUnknownLengthHandler{}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/long-response", ts.URL)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodGet, uri, nil)

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		resp, err := client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}

		testBody(t, resp.Body, []byte(petition))
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /long-response HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> User-Agent: Go-http-client/1.1

< HTTP/1.1 200 OK

* body is too long (9846 bytes) to print, skipping (longer than 5000 bytes)
`, uri, is.req.RemoteAddr, ts.Listener.Addr())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingMultipartForm(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		RequestHeader: true,
		// TODO(henvic): print request body once support for printing out multipart/formdata body is added.
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	logger.Formatters = []Formatter{
		&JSONFormatter{},
	}

	is := inspect(logger.Middleware(multipartHandler{t}), 1)

	ts := httptest.NewServer(is)
	defer ts.Close()

	uri := fmt.Sprintf("%s/multipart-upload", ts.URL)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	multipartTestdata(writer, body)

	go func() {
		client := newServerClient()

		req, err := http.NewRequest(http.MethodPost, uri, body)

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		req.Header.Set("Content-Type", writer.FormDataContentType())

		_, err = client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> POST /multipart-upload HTTP/1.1
> Host: %s
> Accept-Encoding: gzip
> Content-Length: 10355
> Content-Type: %s
> User-Agent: Go-http-client/1.1

< HTTP/1.1 200 OK

upload received
`, uri, is.req.RemoteAddr, ts.Listener.Addr(), writer.FormDataContentType())

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingTLS(t *testing.T) {
	t.Parallel()

	logger := &Logger{
		TLS:            true,
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(helloHandler{}), 1)

	ts := httptest.NewTLSServer(is)
	defer ts.Close()

	go func() {
		client := ts.Client()

		req, err := http.NewRequest(http.MethodGet, ts.URL, nil)

		req.Host = "example.com" // overriding the Host header to send

		req.Header.Add("User-Agent", "Robot/0.1 crawler@example.com")

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		resp, err := client.Do(req)

		if err != nil {
			t.Errorf("cannot connect to the server: %v", err)
		}

		testBody(t, resp.Body, []byte("Hello, world!"))
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to https://example.com/
* Request from %s
* TLS connection using TLS 1.3 / TLS_AES_128_GCM_SHA256
> GET / HTTP/1.1
> Host: example.com
> Accept-Encoding: gzip
> User-Agent: Robot/0.1 crawler@example.com

< HTTP/1.1 200 OK

Hello, world!
`, is.req.RemoteAddr)

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingMutualTLS(t *testing.T) {
	t.Parallel()

	caCert, err := ioutil.ReadFile("testdata/cert.pem")

	if err != nil {
		panic(err)
	}

	clientCert, err := ioutil.ReadFile("testdata/cert-client.pem")

	if err != nil {
		panic(err)
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	caCertPool.AppendCertsFromPEM(clientCert)

	tlsConfig := &tls.Config{
		ClientCAs:  caCertPool,
		ClientAuth: tls.RequireAndVerifyClientCert,
	}

	logger := &Logger{
		TLS:            true,
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(helloHandler{}), 1)

	// NOTE(henvic): Using httptest directly turned out complicated.
	// See https://venilnoronha.io/a-step-by-step-guide-to-mtls-in-go
	server := &http.Server{
		TLSConfig: tlsConfig,
		Handler:   is,
	}

	listener, err := netListener()

	if err != nil {
		panic(fmt.Sprintf("failed to listen on a port: %v", err))
	}

	defer listener.Close()

	go func() {
		// Certificate generated with
		// $ openssl req -newkey rsa:2048 \
		// -new -nodes -x509 \
		// -days 36500 \
		// -out cert.pem \
		// -keyout key.pem \
		// -subj "/C=US/ST=California/L=Carmel-by-the-Sea/O=Plifk/OU=Cloud/CN=localhost"
		if errcp := server.ServeTLS(listener, "testdata/cert.pem", "testdata/key.pem"); errcp != http.ErrServerClosed {
			t.Errorf("server exit with unexpected error: %v", errcp)
		}
	}()

	defer server.Shutdown(context.Background())

	// Certificate generated with
	// $ openssl req -newkey rsa:2048 \
	// -new -nodes -x509 \
	// -days 36500 \
	// -out cert-client.pem \
	// -keyout key-client.pem \
	// -subj "/C=NL/ST=Zuid-Holland/L=Rotterdam/O=Client/OU=User/CN=User"
	cert, err := tls.LoadX509KeyPair("testdata/cert-client.pem", "testdata/key-client.pem")

	if err != nil {
		t.Errorf("failed to load X509 key pair: %v", err)
	}

	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])

	if err != nil {
		t.Errorf("failed to parse certificate for copying Leaf field")
	}

	// Create a HTTPS client and supply the created CA pool and certificate
	clientTLSConfig := &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
	}

	_, port, err := net.SplitHostPort(listener.Addr().String())

	if err != nil {
		panic(err)
	}

	host := fmt.Sprintf("https://localhost:%s/mutual-tls-test", port)

	go func() {
		transport := newTransport()
		transport.TLSClientConfig = clientTLSConfig

		client := &http.Client{
			Transport: transport,
		}

		resp, err := client.Get(host)

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		testBody(t, resp.Body, []byte("Hello, world!"))
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
* TLS connection using TLS 1.3 / TLS_AES_128_GCM_SHA256
* ALPN: h2 accepted
* Client certificate:
*  subject: CN=User,OU=User,O=Client,L=Rotterdam,ST=Zuid-Holland,C=NL
*  start date: Sat Jan 25 20:12:36 UTC 2020
*  expire date: Mon Jan  1 20:12:36 UTC 2120
*  issuer: CN=User,OU=User,O=Client,L=Rotterdam,ST=Zuid-Holland,C=NL
> GET /mutual-tls-test HTTP/2.0
> Host: localhost:%s
> Accept-Encoding: gzip
> User-Agent: Go-http-client/2.0

< HTTP/2.0 200 OK

Hello, world!
`, host, is.req.RemoteAddr, port)

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func TestIncomingMutualTLSNoSafetyLogging(t *testing.T) {
	t.Parallel()

	caCert, err := ioutil.ReadFile("testdata/cert.pem")

	if err != nil {
		panic(err)
	}

	clientCert, err := ioutil.ReadFile("testdata/cert-client.pem")

	if err != nil {
		panic(err)
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	caCertPool.AppendCertsFromPEM(clientCert)

	tlsConfig := &tls.Config{
		ClientCAs:  caCertPool,
		ClientAuth: tls.RequireAndVerifyClientCert,
	}

	logger := &Logger{
		// TLS must be false
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
	}

	var buf bytes.Buffer
	logger.SetOutput(&buf)

	is := inspect(logger.Middleware(helloHandler{}), 1)

	// NOTE(henvic): Using httptest directly turned out complicated.
	// See https://venilnoronha.io/a-step-by-step-guide-to-mtls-in-go
	server := &http.Server{
		TLSConfig: tlsConfig,
		Handler:   is,
	}

	listener, err := netListener()

	if err != nil {
		panic(fmt.Sprintf("failed to listen on a port: %v", err))
	}

	defer listener.Close()

	go func() {
		// Certificate generated with
		// $ openssl req -newkey rsa:2048 \
		// -new -nodes -x509 \
		// -days 36500 \
		// -out cert.pem \
		// -keyout key.pem \
		// -subj "/C=US/ST=California/L=Carmel-by-the-Sea/O=Plifk/OU=Cloud/CN=localhost"
		if errcp := server.ServeTLS(listener, "testdata/cert.pem", "testdata/key.pem"); errcp != http.ErrServerClosed {
			t.Errorf("server exit with unexpected error: %v", errcp)
		}
	}()

	defer server.Shutdown(context.Background())

	// Certificate generated with
	// $ openssl req -newkey rsa:2048 \
	// -new -nodes -x509 \
	// -days 36500 \
	// -out cert-client.pem \
	// -keyout key-client.pem \
	// -subj "/C=NL/ST=Zuid-Holland/L=Rotterdam/O=Client/OU=User/CN=User"
	cert, err := tls.LoadX509KeyPair("testdata/cert-client.pem", "testdata/key-client.pem")

	if err != nil {
		t.Errorf("failed to load X509 key pair: %v", err)
	}

	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])

	if err != nil {
		t.Errorf("failed to parse certificate for copying Leaf field")
	}

	// Create a HTTPS client and supply the created CA pool and certificate
	clientTLSConfig := &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
	}

	_, port, err := net.SplitHostPort(listener.Addr().String())

	if err != nil {
		panic(err)
	}

	host := fmt.Sprintf("https://localhost:%s/mutual-tls-test", port)

	go func() {
		transport := newTransport()
		transport.TLSClientConfig = clientTLSConfig

		client := &http.Client{
			Transport: transport,
		}

		resp, err := client.Get(host)

		if err != nil {
			t.Errorf("cannot create request: %v", err)
		}

		testBody(t, resp.Body, []byte("Hello, world!"))
	}()

	is.Wait()

	want := fmt.Sprintf(`* Request to %s
* Request from %s
> GET /mutual-tls-test HTTP/2.0
> Host: localhost:%s
> Accept-Encoding: gzip
> User-Agent: Go-http-client/2.0

< HTTP/2.0 200 OK

Hello, world!
`, host, is.req.RemoteAddr, port)

	if got := buf.String(); got != want {
		t.Errorf("logged HTTP request %s; want %s", got, want)
	}
}

func newServerClient() *http.Client {
	return &http.Client{
		Transport: newTransport(),
	}
}
