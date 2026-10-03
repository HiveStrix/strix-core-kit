// Package bccr reads the Banco Central de Costa Rica's reference exchange
// rate from its SDDE REST API ("Estándar electrónico para usar el nuevo
// Sistema de Divulgación de Datos Económicos", in force since 2025-04-07):
// indicator 317 is the buy side (compra) and 318 the sell side (venta), the
// one Ley 7092 art. 5 fixes for income tax.
//
// It is the first BCCR integration of the platform, which is why it lives in
// the kit: payroll needs it to convert USD contracts, and billing and expenses
// receive or type the rate today and will need the same reader.
//
// What it guarantees, and what it leaves to the caller:
//
//   - Values are exact decimals read from the JSON literal, never through a
//     float: 462.08 stays 462.08.
//   - Every Rate carries the bytes the BCCR sent (Raw) and their SHA-256, so
//     a payslip can prove which figure it used without asking again. Store
//     Raw as bytes or text if the hash must be re-verified: a jsonb column
//     reformats the document and the hash stops matching it.
//   - A date with no value is ErrNotPublished, never the previous day's
//     figure: the BCCR publishes every calendar day, weekends and holidays
//     included, so a gap is a real failure and the caller decides (payroll
//     fails the run and asks for an audited manual rate).
//   - There is no cache, global or otherwise. Storing what was fetched is the
//     caller's job (payroll's immutable fx_rates), and a recalculation must
//     use the stored rate instead of asking again.
//   - 429 and 5xx are retried with a doubling backoff that honours
//     Retry-After; nothing else is. A 401/403 (missing, expired or
//     unsubscribed token) matches ErrTokenRejected so the job can alert
//     someone to regenerate it. A 404 matches ErrEndpointNotFound: the
//     standard lists a missing or expired token among its causes, so the
//     job alerts on it too (see ErrEndpointNotFound).
//   - Redirects are never followed: the token only goes to the base URL the
//     client was built with, never in clear to wherever a 3xx points.
//
// Not verified against a live response, because that needs a token: whether
// "fecha" comes as "2006-01-02" (what the standard shows) or with a time
// part (both are accepted), when the value of day t is published, how long a
// token lives and what triggers a 429.
package bccr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	// IndicatorBuy is the reference buy rate (tipo de cambio de compra).
	IndicatorBuy = 317
	// IndicatorSell is the reference sell rate (tipo de cambio de venta).
	IndicatorSell = 318

	// DefaultBaseURL is the SDDE API root published by the standard (§1.3).
	DefaultBaseURL = "https://apim.bccr.fi.cr/SDDE/api/Bccr.GE.SDDE.Publico.Indicadores.API"

	userAgent = "strix-core-kit-bccr"
	// maxBody bounds what is read from a response. A year of one daily
	// indicator is a few tens of KB; anything near this is not a series.
	maxBody = 4 << 20
)

// ErrNotPublished is returned when the BCCR has no value for the requested
// date: the day is missing from the series or its value is null.
var ErrNotPublished = errors.New("bccr: no value published for that date")

// ErrTokenRejected matches an APIError whose status is 401 or 403: the token
// is missing, expired, or its account is not subscribed. It does not match a
// 404, which can also be an expired token: see ErrEndpointNotFound.
var ErrTokenRejected = errors.New("bccr: token rejected")

// ErrEndpointNotFound matches an APIError whose status is 404. The standard
// gives it two causes, a URL that is not the documented endpoint or a request
// without valid access ("Problemas frecuentes" 1 and Anexo C: check the
// Bearer token and that it has not expired). The client builds the documented
// URL itself, so a job that alerts on ErrTokenRejected alerts on this too;
// it is kept apart because it can also mean the API moved. It is not retried.
var ErrEndpointNotFound = errors.New("bccr: endpoint not found (wrong URL or no valid token)")

// errRedirect is what the client answers to a 3xx: the token is bound to the
// configured base URL and must not follow a redirect anywhere, least of all
// to plain http on the same host, where Go would forward Authorization.
var errRedirect = errors.New("bccr: the API answered with a redirect; not following it with the token")

// APIError is a response the API answered with something other than data: a
// non-2xx status (after the retries, for 429/5xx), or a 200 whose body says
// estado=false.
type APIError struct {
	StatusCode int
	// Code and Message come from the body ({CodigoError, Mensaje}) when it
	// has them.
	Code    string
	Message string
}

func (e *APIError) Error() string {
	msg := e.Message
	if e.Code != "" {
		msg = e.Code + " " + msg
	}
	return fmt.Sprintf("bccr: http %d: %s", e.StatusCode, strings.TrimSpace(msg))
}

// Unwrap lets errors.Is(err, ErrTokenRejected) single out the case a person
// has to fix by generating a new token, and errors.Is(err,
// ErrEndpointNotFound) the 404 that may be the same case.
func (e *APIError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrTokenRejected
	case http.StatusNotFound:
		return ErrEndpointNotFound
	}
	return nil
}

// Config configures a Client. Only Token is required.
type Config struct {
	// Token is the Bearer token generated in Indicadores Económicos → Mi
	// Perfil → Generar token. It is a platform secret (Vault → ESO), and it
	// only ever travels in the Authorization header.
	Token string
	// BaseURL defaults to DefaultBaseURL. It must be https: the token must
	// never travel in clear.
	BaseURL string
	// Timeout bounds each attempt, retries apart. Defaults to 20 s.
	Timeout time.Duration
	// MaxAttempts is how many requests one call may make, the first one
	// included; 1 disables retries. Defaults to 4.
	MaxAttempts int
	// Backoff is the wait before the first retry, doubling after each one up
	// to MaxBackoff. Default 1 s and 30 s. A longer Retry-After from the
	// server wins, still capped by MaxBackoff.
	Backoff    time.Duration
	MaxBackoff time.Duration
	// HTTPClient defaults to a plain client. Go clients refuse anything below
	// TLS 1.2, which is what the standard requires (§1.4). The client uses a
	// copy of it whose CheckRedirect refuses every redirect; the one passed
	// is not modified.
	HTTPClient *http.Client
}

// Client reads indicator series from the SDDE API. It is safe for concurrent
// use and keeps no state between calls.
type Client struct {
	cfg   Config
	base  string
	http  *http.Client
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// New validates cfg, fills its defaults and builds the client.
func New(cfg Config) (*Client, error) {
	cfg.Token = strings.TrimSpace(cfg.Token)
	if cfg.Token == "" {
		return nil, errors.New("bccr: a token is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("bccr: base URL %q must be an absolute https URL", cfg.BaseURL)
	}
	if cfg.Timeout < 0 || cfg.MaxAttempts < 0 || cfg.Backoff < 0 || cfg.MaxBackoff < 0 {
		return nil, errors.New("bccr: timeouts, attempts and backoffs must not be negative")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 4
	}
	if cfg.Backoff == 0 {
		cfg.Backoff = time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		copied := *cfg.HTTPClient
		hc = &copied
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return &Client{
		cfg:   cfg,
		base:  strings.TrimRight(cfg.BaseURL, "/"),
		http:  hc,
		now:   time.Now,
		sleep: sleepContext,
	}, nil
}

// Rate is one published value of an indicator.
type Rate struct {
	Indicator int
	// Date is the calendar day the value belongs to, at 00:00 UTC.
	Date time.Time
	// Value is the colones per dollar, exact.
	Value decimal.Decimal
	// Endpoint is the URL that was asked. It carries no credential.
	Endpoint string
	// Raw is the whole response body the value came in, as received; the
	// rates of one Series call share it. RawSHA256 is its hex SHA-256.
	Raw       []byte
	RawSHA256 string
	// FetchedAt is when the response was received.
	FetchedAt time.Time
}

// Rate returns the value of indicator for the calendar day of date (in
// date's own location). A day with no value is ErrNotPublished, never the
// nearest one.
func (c *Client) Rate(ctx context.Context, indicator int, date time.Time) (Rate, error) {
	rates, err := c.Series(ctx, indicator, date, date)
	if err != nil {
		return Rate{}, err
	}
	day := civil(date)
	for _, r := range rates {
		if r.Date.Equal(day) {
			return r, nil
		}
	}
	return Rate{}, fmt.Errorf("%w: indicator %d on %s", ErrNotPublished, indicator, day.Format(time.DateOnly))
}

// Series returns the values of indicator published for every calendar day
// in [from, to] (each in its own location), ordered by date. A day without a
// value is simply absent — the value of today may not be out yet — so a
// caller that needs a given day checks for it, or asks Rate.
func (c *Client) Series(ctx context.Context, indicator int, from, to time.Time) ([]Rate, error) {
	if indicator <= 0 {
		return nil, fmt.Errorf("bccr: invalid indicator %d", indicator)
	}
	first, last := civil(from), civil(to)
	if last.Before(first) {
		return nil, fmt.Errorf("bccr: range ends %s before it starts %s", last.Format(time.DateOnly), first.Format(time.DateOnly))
	}

	q := url.Values{}
	q.Set("fechaInicio", first.Format("2006/01/02"))
	q.Set("fechaFin", last.Format("2006/01/02"))
	q.Set("idioma", "ES")
	endpoint := fmt.Sprintf("%s/indicadoresEconomicos/%d/series?%s", c.base, indicator, q.Encode())

	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	fetchedAt := c.now()
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	points, err := parseSeries(body, indicator)
	if err != nil {
		return nil, err
	}
	rates := make([]Rate, 0, len(points))
	for _, p := range points {
		if p.date.Before(first) || p.date.After(last) {
			continue
		}
		rates = append(rates, Rate{
			Indicator: indicator,
			Date:      p.date,
			Value:     p.value,
			Endpoint:  endpoint,
			Raw:       body,
			RawSHA256: digest,
			FetchedAt: fetchedAt,
		})
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i].Date.Before(rates[j].Date) })
	return rates, nil
}

// get performs the request, retrying 429 and 5xx. A transport error (refused
// connection, attempt timeout) is returned as is: the job that called runs
// again, and a retry here would only hide that the network path is broken.
func (c *Client) get(ctx context.Context, endpoint string) ([]byte, error) {
	wait := c.cfg.Backoff
	for attempt := 1; ; attempt++ {
		body, status, retryAfter, err := c.once(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		if status >= 200 && status < 300 {
			return body, nil
		}
		apiErr := newAPIError(status, body)
		if !(status == http.StatusTooManyRequests || status >= 500) || attempt >= c.cfg.MaxAttempts {
			return nil, apiErr
		}
		d := max(wait, retryAfter)
		d = min(d, c.cfg.MaxBackoff)
		if err := c.sleep(ctx, d); err != nil {
			return nil, fmt.Errorf("bccr: waiting to retry after %v: %w", apiErr, err)
		}
		wait = min(wait*2, c.cfg.MaxBackoff)
	}
}

func (c *Client) once(ctx context.Context, endpoint string) (body []byte, status int, retryAfter time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("bccr: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("bccr: %w", err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("bccr: read response: %w", err)
	}
	if len(body) > maxBody {
		return nil, 0, 0, fmt.Errorf("bccr: response larger than %d bytes", maxBody)
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
		retryAfter = time.Duration(secs) * time.Second
	}
	return body, resp.StatusCode, retryAfter, nil
}

func newAPIError(status int, body []byte) *APIError {
	e := &APIError{StatusCode: status}
	var payload struct {
		CodigoError json.RawMessage `json:"CodigoError"`
		Mensaje     string          `json:"Mensaje"`
	}
	if json.Unmarshal(body, &payload) == nil {
		e.Code = strings.Trim(string(payload.CodigoError), `"`)
		e.Message = payload.Mensaje
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	if r := []rune(e.Message); len(r) > 200 {
		e.Message = string(r[:200])
	}
	return e
}

type point struct {
	date  time.Time
	value decimal.Decimal
}

// parseSeries reads the series of indicator out of a series response:
//
//	{"estado": true, "mensaje": "...", "datos": [{"codigoIndicador": "318",
//	  "nombreIndicador": "...", "series": [{"fecha": "2026-10-03",
//	  "valorDatoPorPeriodo": 462.08}]}]}
//
// Days whose value is null are left out. A body for another indicator, a
// date that does not parse, a repeated day or a value that is not a positive
// number is an error: it is not a rate anyone should pay with.
//
// Only a body that IS a series response may say "nothing published": one
// with estado true or a datos key. A 200 with neither ({}, a gateway policy
// page in JSON) is an unrecognized response, not ErrNotPublished, which would
// send payroll to the manual-rate flow over a broken reply.
func parseSeries(body []byte, indicator int) ([]point, error) {
	var resp struct {
		Estado  *bool  `json:"estado"`
		Mensaje string `json:"mensaje"`
		Datos   *[]struct {
			CodigoIndicador json.RawMessage `json:"codigoIndicador"`
			Series          []struct {
				Fecha string          `json:"fecha"`
				Valor json.RawMessage `json:"valorDatoPorPeriodo"`
			} `json:"series"`
		} `json:"datos"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("bccr: unreadable response: %w", err)
	}
	if resp.Estado != nil && !*resp.Estado {
		return nil, &APIError{StatusCode: http.StatusOK, Message: resp.Mensaje}
	}
	if resp.Estado == nil && resp.Datos == nil {
		return nil, errors.New("bccr: unrecognized response: neither estado nor datos")
	}
	if resp.Datos == nil || len(*resp.Datos) == 0 {
		return nil, nil
	}

	var points []point
	matched := false
	seen := map[time.Time]bool{}
	for _, entry := range *resp.Datos {
		code, err := strconv.Atoi(strings.Trim(string(entry.CodigoIndicador), `"`))
		if err != nil || code != indicator {
			continue
		}
		matched = true
		for _, s := range entry.Series {
			date, err := parseFecha(s.Fecha)
			if err != nil {
				return nil, err
			}
			if seen[date] {
				return nil, fmt.Errorf("bccr: indicator %d lists %s twice", indicator, s.Fecha)
			}
			seen[date] = true
			raw := strings.TrimSpace(string(s.Valor))
			if raw == "" || raw == "null" {
				continue
			}
			value, err := decimal.NewFromString(strings.Trim(raw, `"`))
			if err != nil {
				return nil, fmt.Errorf("bccr: indicator %d on %s: value %s is not a number", indicator, s.Fecha, raw)
			}
			if !value.IsPositive() {
				return nil, fmt.Errorf("bccr: indicator %d on %s: value %s is not an exchange rate", indicator, s.Fecha, value)
			}
			points = append(points, point{date: date, value: value})
		}
	}
	if !matched {
		return nil, fmt.Errorf("bccr: the response does not carry indicator %d", indicator)
	}
	return points, nil
}

// parseFecha accepts "2006-01-02", the format the standard shows, and the
// same date followed by a time part, which .NET serializers often add.
func parseFecha(s string) (time.Time, error) {
	if len(s) >= 10 && (len(s) == 10 || s[10] == 'T' || s[10] == ' ') {
		if t, err := time.Parse(time.DateOnly, s[:10]); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bccr: unexpected date %q", s)
}

// civil is the calendar day of t, in t's own location, at 00:00 UTC.
func civil(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
