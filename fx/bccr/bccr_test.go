package bccr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The bodies under testdata follow the shape the SDDE standard documents for
// "Series de indicadores económicos" and "Anexo C – Manejo de errores"; the
// 2026-10-03 figures (455.71 buy, 462.08 sell) are the ones the BCCR
// published that Saturday, the other days are illustrative. error_400.json
// is the standard's own example. No body here was captured from the live API,
// which needs a token.

const (
	testToken = "test-token-do-not-log"
	apiPath   = "/SDDE/api/Bccr.GE.SDDE.Publico.Indicadores.API"
)

type reply struct {
	status     int
	body       []byte
	retryAfter string
}

// fakeSDDE answers each request with the next scripted reply (the last one
// repeats) and records what it was asked.
type fakeSDDE struct {
	mu       sync.Mutex
	replies  []reply
	requests []*http.Request
}

func (f *fakeSDDE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(context.Background()))
	next := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	f.mu.Unlock()
	if next.retryAfter != "" {
		w.Header().Set("Retry-After", next.retryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(next.status)
	_, _ = w.Write(next.body)
}

func (f *fakeSDDE) calls() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.requests...)
}

var fetchedAt = time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)

// newClient builds a client against a TLS test server, with a fixed clock and
// a sleep that records the waits instead of waiting.
func newClient(t *testing.T, cfg Config, replies ...reply) (*Client, *fakeSDDE, *[]time.Duration) {
	t.Helper()
	fake := &fakeSDDE{replies: replies}
	srv := httptest.NewTLSServer(fake)
	t.Cleanup(srv.Close)

	cfg.BaseURL = srv.URL + apiPath
	cfg.HTTPClient = srv.Client()
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	if cfg.Backoff == 0 {
		cfg.Backoff = 100 * time.Millisecond
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 2 * time.Second
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.now = func() time.Time { return fetchedAt }
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	return c, fake, &waits
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return b
}

func ok(body []byte) reply { return reply{status: http.StatusOK, body: body} }

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func TestRateReadsOneDayAndKeepsTheEvidence(t *testing.T) {
	body := fixture(t, "series_318_2026-10-01_03.json")
	c, fake, _ := newClient(t, Config{}, ok(body))

	r, err := c.Rate(context.Background(), IndicatorSell, day(2026, 10, 3))
	if err != nil {
		t.Fatalf("Rate: %v", err)
	}
	if r.Indicator != 318 || !r.Date.Equal(day(2026, 10, 3)) || r.Value.String() != "462.08" {
		t.Errorf("rate = (%d, %s, %s), want (318, 2026-10-03, 462.08)", r.Indicator, r.Date.Format(time.DateOnly), r.Value)
	}
	if !bytes.Equal(r.Raw, body) {
		t.Error("Raw must be the response body as received")
	}
	sum := sha256.Sum256(body)
	if r.RawSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("RawSHA256 = %s, want the SHA-256 of the body", r.RawSHA256)
	}
	if !r.FetchedAt.Equal(fetchedAt) {
		t.Errorf("FetchedAt = %v, want %v", r.FetchedAt, fetchedAt)
	}
	if strings.Contains(r.Endpoint, testToken) || !strings.Contains(r.Endpoint, "/indicadoresEconomicos/318/series?") {
		t.Errorf("Endpoint = %q", r.Endpoint)
	}

	reqs := fake.calls()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Method != http.MethodGet || req.URL.Path != apiPath+"/indicadoresEconomicos/318/series" {
		t.Errorf("request = %s %s", req.Method, req.URL.Path)
	}
	q := req.URL.Query()
	if q.Get("fechaInicio") != "2026/10/03" || q.Get("fechaFin") != "2026/10/03" || q.Get("idioma") != "ES" {
		t.Errorf("query = %v", q)
	}
	if !strings.Contains(req.URL.RawQuery, "fechaInicio=2026%2F10%2F03") {
		t.Errorf("the dates must go encoded as the standard shows them: %s", req.URL.RawQuery)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
}

// The value is read from the JSON literal: no float ever touches it.
func TestValueIsExactNeverAFloat(t *testing.T) {
	for literal, want := range map[string]string{
		`512.123456789012345678`: "512.123456789012345678",
		`"462.08"`:               "462.08",
		`455.71`:                 "455.71",
		`0.1`:                    "0.1",
	} {
		body := []byte(`{"estado":true,"datos":[{"codigoIndicador":"318","series":[{"fecha":"2026-10-03","valorDatoPorPeriodo":` + literal + `}]}]}`)
		c, _, _ := newClient(t, Config{}, ok(body))
		r, err := c.Rate(context.Background(), IndicatorSell, day(2026, 10, 3))
		if err != nil {
			t.Fatalf("%s: %v", literal, err)
		}
		if r.Value.String() != want {
			t.Errorf("%s: value = %s, want %s", literal, r.Value, want)
		}
	}
}

func TestSeriesReturnsTheRangeInOrderSharingOneBody(t *testing.T) {
	body := fixture(t, "series_318_2026-10-01_03.json")
	c, fake, _ := newClient(t, Config{}, ok(body))

	rates, err := c.Series(context.Background(), IndicatorSell, day(2026, 10, 1), day(2026, 10, 3))
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	want := []string{"461.93", "462.15", "462.08"}
	if len(rates) != len(want) {
		t.Fatalf("%d rates, want %d", len(rates), len(want))
	}
	for i, r := range rates {
		if !r.Date.Equal(day(2026, 10, 1+i)) || r.Value.String() != want[i] {
			t.Errorf("rate %d = (%s, %s), want (2026-10-0%d, %s)", i, r.Date.Format(time.DateOnly), r.Value, i+1, want[i])
		}
		if r.RawSHA256 != rates[0].RawSHA256 {
			t.Error("rates from one response must carry the same evidence")
		}
	}
	q := fake.calls()[0].URL.Query()
	if q.Get("fechaInicio") != "2026/10/01" || q.Get("fechaFin") != "2026/10/03" {
		t.Errorf("query = %v", q)
	}

	// A day the response carries but the caller did not ask for is dropped.
	c2, _, _ := newClient(t, Config{}, ok(body))
	rates, err = c2.Series(context.Background(), IndicatorSell, day(2026, 10, 2), day(2026, 10, 2))
	if err != nil || len(rates) != 1 || rates[0].Value.String() != "462.15" {
		t.Fatalf("a one-day range must return that day only: %v (%v)", rates, err)
	}
}

// The day asked is the caller's calendar day, whatever its offset to UTC: 23:30
// of 3 October in Costa Rica is already the 4th in UTC.
func TestRateUsesTheCallersCalendarDay(t *testing.T) {
	c, fake, _ := newClient(t, Config{}, ok(fixture(t, "series_318_2026-10-01_03.json")))
	costaRica := time.FixedZone("CST", -6*60*60)

	r, err := c.Rate(context.Background(), IndicatorSell, time.Date(2026, 10, 3, 23, 30, 0, 0, costaRica))
	if err != nil {
		t.Fatalf("Rate: %v", err)
	}
	if !r.Date.Equal(day(2026, 10, 3)) || r.Value.String() != "462.08" {
		t.Errorf("rate = (%s, %s), want the 3rd", r.Date.Format(time.DateOnly), r.Value)
	}
	if q := fake.calls()[0].URL.Query(); q.Get("fechaInicio") != "2026/10/03" {
		t.Errorf("asked for %s, want 2026/10/03", q.Get("fechaInicio"))
	}
}

func TestDatetimeDatesAndNumericIndicatorCodes(t *testing.T) {
	c, _, _ := newClient(t, Config{}, ok(fixture(t, "series_317_2026-10-03_datetime.json")))
	r, err := c.Rate(context.Background(), IndicatorBuy, day(2026, 10, 3))
	if err != nil {
		t.Fatalf("Rate: %v", err)
	}
	if r.Indicator != 317 || r.Value.String() != "455.71" {
		t.Errorf("rate = (%d, %s), want (317, 455.71)", r.Indicator, r.Value)
	}
}

// A missing day is ErrNotPublished: never the day before, never a zero.
func TestMissingValueIsErrNotPublished(t *testing.T) {
	cases := map[string]struct {
		body []byte
		date time.Time
	}{
		"null value":             {fixture(t, "series_318_null.json"), day(2026, 10, 3)},
		"empty series":           {fixture(t, "series_318_empty.json"), day(2026, 10, 3)},
		"no datos at all":        {[]byte(`{"estado":true,"mensaje":"Consulta exitosa","datos":[]}`), day(2026, 10, 3)},
		"estado true, no datos":  {[]byte(`{"estado":true,"mensaje":"Consulta exitosa"}`), day(2026, 10, 3)},
		"datos empty, no estado": {[]byte(`{"datos":[]}`), day(2026, 10, 3)},
		"day not in the series":  {fixture(t, "series_318_2026-10-01_03.json"), day(2026, 10, 4)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _, _ := newClient(t, Config{}, ok(tc.body))
			_, err := c.Rate(context.Background(), IndicatorSell, tc.date)
			if !errors.Is(err, ErrNotPublished) {
				t.Fatalf("err = %v, want ErrNotPublished", err)
			}
		})
	}

	// Series is not an error: the day is simply absent.
	c, _, _ := newClient(t, Config{}, ok(fixture(t, "series_318_null.json")))
	rates, err := c.Series(context.Background(), IndicatorSell, day(2026, 10, 3), day(2026, 10, 3))
	if err != nil || len(rates) != 0 {
		t.Fatalf("Series over a null day = %v (%v), want empty", rates, err)
	}
}

func TestRetriesTooManyRequestsAndServerErrorsWithDoublingBackoff(t *testing.T) {
	body := fixture(t, "series_318_2026-10-01_03.json")
	c, fake, waits := newClient(t, Config{MaxAttempts: 4},
		reply{status: http.StatusTooManyRequests},
		reply{status: http.StatusServiceUnavailable},
		reply{status: http.StatusInternalServerError, body: []byte(`{"CodigoError":"500","Mensaje":"Error interno"}`)},
		ok(body),
	)
	r, err := c.Rate(context.Background(), IndicatorSell, day(2026, 10, 3))
	if err != nil {
		t.Fatalf("Rate: %v", err)
	}
	if r.Value.String() != "462.08" {
		t.Errorf("value = %s", r.Value)
	}
	if n := len(fake.calls()); n != 4 {
		t.Errorf("%d requests, want 4", n)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	if len(*waits) != len(want) {
		t.Fatalf("waits = %v, want %v", *waits, want)
	}
	for i := range want {
		if (*waits)[i] != want[i] {
			t.Errorf("wait %d = %v, want %v", i, (*waits)[i], want[i])
		}
	}
}

// Retry-After wins when it asks for longer than the backoff, and MaxBackoff
// bounds both.
func TestRetryAfterIsHonouredWithinTheCap(t *testing.T) {
	c, _, waits := newClient(t, Config{MaxAttempts: 4, Backoff: 100 * time.Millisecond, MaxBackoff: 2 * time.Second},
		reply{status: http.StatusTooManyRequests, retryAfter: "1"},
		reply{status: http.StatusTooManyRequests, retryAfter: "60"},
		reply{status: http.StatusTooManyRequests, retryAfter: "not-a-number"},
		ok(fixture(t, "series_318_2026-10-01_03.json")),
	)
	if _, err := c.Rate(context.Background(), IndicatorSell, day(2026, 10, 3)); err != nil {
		t.Fatalf("Rate: %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 400 * time.Millisecond}
	for i := range want {
		if i >= len(*waits) || (*waits)[i] != want[i] {
			t.Fatalf("waits = %v, want %v", *waits, want)
		}
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	c, fake, _ := newClient(t, Config{MaxAttempts: 3}, reply{status: http.StatusBadGateway})
	_, err := c.Rate(context.Background(), IndicatorSell, day(2026, 10, 3))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("err = %v, want an APIError 502", err)
	}
	if errors.Is(err, ErrTokenRejected) || errors.Is(err, ErrNotPublished) {
		t.Errorf("a 502 is neither a token problem nor a missing day: %v", err)
	}
	if n := len(fake.calls()); n != 3 {
		t.Errorf("%d requests, want 3", n)
	}

	single, fake1, waits := newClient(t, Config{MaxAttempts: 1}, reply{status: http.StatusServiceUnavailable})
	if _, err := single.Rate(context.Background(), IndicatorSell, day(2026, 10, 3)); err == nil {
		t.Fatal("expected an error")
	}
	if n := len(fake1.calls()); n != 1 || len(*waits) != 0 {
		t.Errorf("MaxAttempts 1 must not retry: %d requests, %d waits", n, len(*waits))
	}
}

// Only 429 and 5xx are retried. A rejected token is singled out so the job
// can alert, and it never shows up in the error.
func TestOtherStatusesAreNotRetried(t *testing.T) {
	cases := []struct {
		status        int
		body          []byte
		tokenRejected bool
		notFound      bool
		code, message string
	}{
		{http.StatusUnauthorized, []byte(`{"CodigoError":"401","Mensaje":"Token inválido o vencido."}`), true, false, "401", "Token inválido o vencido."},
		{http.StatusForbidden, nil, true, false, "", "Forbidden"},
		// The standard lists an expired token among a 404's causes: it is
		// singled out too, apart from ErrTokenRejected.
		{http.StatusNotFound, []byte(`<html>not found</html>`), false, true, "", "Not Found"},
		{http.StatusBadRequest, fixture(t, "error_400.json"), false, false, "400", "Parámetros de entrada inválidos."},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			c, fake, waits := newClient(t, Config{}, reply{status: tc.status, body: tc.body})
			_, err := c.Rate(context.Background(), IndicatorSell, day(2026, 10, 3))
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status {
				t.Fatalf("err = %v, want an APIError %d", err, tc.status)
			}
			if apiErr.Code != tc.code || apiErr.Message != tc.message {
				t.Errorf("(code, message) = (%q, %q), want (%q, %q)", apiErr.Code, apiErr.Message, tc.code, tc.message)
			}
			if errors.Is(err, ErrTokenRejected) != tc.tokenRejected {
				t.Errorf("errors.Is(ErrTokenRejected) = %v, want %v", !tc.tokenRejected, tc.tokenRejected)
			}
			if errors.Is(err, ErrEndpointNotFound) != tc.notFound {
				t.Errorf("errors.Is(ErrEndpointNotFound) = %v, want %v", !tc.notFound, tc.notFound)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("the error leaks the token: %v", err)
			}
			if n := len(fake.calls()); n != 1 || len(*waits) != 0 {
				t.Errorf("%d requests and %d waits, want 1 and 0", n, len(*waits))
			}
		})
	}
}

// A redirect is never followed: Go would forward Authorization to plain http
// on the same host, so the token would travel in clear.
func TestRedirectsAreNotFollowedWithTheToken(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "series_318_2026-10-01_03.json"))
	}))
	t.Cleanup(plain.Close)

	for name, target := range map[string]string{
		"to plain http on the same host": plain.URL + "/elsewhere",
		"to https on the same host":      "/moved",
	} {
		t.Run(name, func(t *testing.T) {
			var tlsHits int
			secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				tlsHits++
				mu.Unlock()
				http.Redirect(w, r, target, http.StatusFound)
			}))
			t.Cleanup(secure.Close)

			given := secure.Client()
			c, err := New(Config{Token: testToken, BaseURL: secure.URL + apiPath, HTTPClient: given})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.Rate(context.Background(), IndicatorSell, day(2026, 10, 3))
			if !errors.Is(err, errRedirect) {
				t.Fatalf("err = %v, want the redirect refused", err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("the error leaks the token: %v", err)
			}
			if given.CheckRedirect != nil {
				t.Error("the caller's http.Client must not be modified")
			}
			mu.Lock()
			defer mu.Unlock()
			if tlsHits != 1 {
				t.Errorf("%d requests to the API, want 1 (a redirect is not a retry)", tlsHits)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) != 0 {
		t.Fatalf("the plain server was reached %d times (Authorization %q)", len(leaked), leaked)
	}
}

// A transport failure is not a 429/5xx: it is returned, not retried.
func TestAnAttemptThatTimesOutIsNotRetried(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{Token: testToken, BaseURL: srv.URL + apiPath, HTTPClient: srv.Client(), Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Rate(context.Background(), IndicatorSell, day(2026, 10, 3))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the attempt's deadline", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("%d requests, want 1", hits)
	}
}

func TestCancellingWhileWaitingStopsTheRetries(t *testing.T) {
	c, fake, _ := newClient(t, Config{MaxAttempts: 4}, reply{status: http.StatusServiceUnavailable})
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return sleepContext(ctx, time.Minute)
	}
	_, err := c.Rate(ctx, IndicatorSell, day(2026, 10, 3))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := len(fake.calls()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Errorf("an uncancelled wait must end cleanly: %v", err)
	}
}

func TestEstadoFalseIsAnError(t *testing.T) {
	c, _, _ := newClient(t, Config{}, ok([]byte(`{"estado":false,"mensaje":"Indicador no disponible","datos":[]}`)))
	_, err := c.Rate(context.Background(), IndicatorSell, day(2026, 10, 3))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "Indicador no disponible" {
		t.Fatalf("err = %v, want an APIError carrying the message", err)
	}
	if errors.Is(err, ErrNotPublished) {
		t.Error("a refusal is not a missing day")
	}
}

// What is not a rate fails loudly instead of being paid with.
func TestRejectsWhatIsNotARate(t *testing.T) {
	series := func(code, items string) []byte {
		return []byte(`{"estado":true,"datos":[{"codigoIndicador":` + code + `,"series":[` + items + `]}]}`)
	}
	cases := map[string][]byte{
		"another indicator":   fixture(t, "series_317_2026-10-03_datetime.json"),
		"zero":                series(`"318"`, `{"fecha":"2026-10-03","valorDatoPorPeriodo":0}`),
		"negative":            series(`"318"`, `{"fecha":"2026-10-03","valorDatoPorPeriodo":-462.08}`),
		"not a number":        series(`"318"`, `{"fecha":"2026-10-03","valorDatoPorPeriodo":"n/d"}`),
		"a boolean":           series(`"318"`, `{"fecha":"2026-10-03","valorDatoPorPeriodo":true}`),
		"day-first date":      series(`"318"`, `{"fecha":"03/10/2026","valorDatoPorPeriodo":462.08}`),
		"date with a suffix":  series(`"318"`, `{"fecha":"2026-10-03x","valorDatoPorPeriodo":462.08}`),
		"the same day twice":  series(`"318"`, `{"fecha":"2026-10-03","valorDatoPorPeriodo":462.08},{"fecha":"2026-10-03","valorDatoPorPeriodo":462.10}`),
		"malformed json":      []byte(`{"estado":true,"datos":[`),
		"html instead":        []byte(`<html>maintenance</html>`),
		"empty object":        []byte(`{}`),
		"json null":           []byte(`null`),
		"datos null":          []byte(`{"datos":null}`),
		"a gateway policy":    []byte(`{"statusCode":200,"message":"Rate limit policy applied"}`),
		"larger than allowed": append([]byte(`{"estado":true,"mensaje":"`), bytes.Repeat([]byte("x"), maxBody)...),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, _, _ := newClient(t, Config{}, ok(body))
			_, err := c.Rate(context.Background(), IndicatorSell, day(2026, 10, 3))
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, ErrNotPublished) {
				t.Errorf("a broken response is not a missing day: %v", err)
			}
		})
	}
}

func TestInvalidRequestsNeverReachTheAPI(t *testing.T) {
	c, fake, _ := newClient(t, Config{}, ok(fixture(t, "series_318_2026-10-01_03.json")))
	if _, err := c.Series(context.Background(), IndicatorSell, day(2026, 10, 3), day(2026, 10, 2)); err == nil {
		t.Error("expected an error for a range that ends before it starts")
	}
	if _, err := c.Rate(context.Background(), 0, day(2026, 10, 3)); err == nil {
		t.Error("expected an error for indicator 0")
	}
	if n := len(fake.calls()); n != 0 {
		t.Errorf("%d requests, want 0", n)
	}
}

func TestNewValidatesTheConfig(t *testing.T) {
	bad := map[string]Config{
		"no token":          {},
		"blank token":       {Token: " \n"},
		"plain http":        {Token: "t", BaseURL: "http://apim.bccr.fi.cr/SDDE"},
		"relative URL":      {Token: "t", BaseURL: "/SDDE/api"},
		"negative timeout":  {Token: "t", Timeout: -time.Second},
		"negative attempts": {Token: "t", MaxAttempts: -1},
		"negative backoff":  {Token: "t", Backoff: -time.Second},
		"negative max":      {Token: "t", MaxBackoff: -time.Second},
	}
	for name, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	c, err := New(Config{Token: "  secret-from-a-mounted-file\n"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.base != DefaultBaseURL || c.cfg.Timeout != 20*time.Second || c.cfg.MaxAttempts != 4 ||
		c.cfg.Backoff != time.Second || c.cfg.MaxBackoff != 30*time.Second || c.http == nil {
		t.Errorf("defaults not applied: %+v", c.cfg)
	}
	if c.cfg.Token != "secret-from-a-mounted-file" {
		t.Errorf("the token must be trimmed, got %q", c.cfg.Token)
	}
}
