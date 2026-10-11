package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gravwell/tilemap"
)

const (
	retryBase = 500 * time.Millisecond
	retryMax  = 15 * time.Second

	// checkTimeout bounds the preflight request, independent of -timeout
	checkTimeout = time.Second
)

var (
	errNotFound = errors.New("tile not found on server")
)

// endpoint is a pre-split URL template so building a tile URL is a handful
// of appends rather than a format or replace per request.
type endpoint struct {
	parts []string // literal pieces
	kinds []byte   // placeholder following parts[i]: 'x', 'y' or 0 for none
}

func parseEndpoint(s string, zoom int) (*endpoint, error) {
	if !strings.Contains(s, "{x}") || !strings.Contains(s, "{y}") {
		if strings.Contains(s, "{x}") || strings.Contains(s, "{y}") {
			return nil, errors.New("endpoint must contain both {x} and {y} or neither")
		}
		s = strings.TrimRight(s, "/") + "/{z}/{x}/{y}.png"
	}
	s = strings.ReplaceAll(s, "{z}", strconv.Itoa(zoom))
	u, err := url.Parse(strings.NewReplacer("{x}", "0", "{y}", "0").Replace(s))
	if err != nil {
		return nil, err
	} else if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	} else if u.Host == "" {
		return nil, errors.New("missing host")
	}
	ep := &endpoint{}
	for len(s) > 0 {
		i := strings.IndexAny(s, "{")
		if i < 0 {
			ep.parts = append(ep.parts, s)
			ep.kinds = append(ep.kinds, 0)
			break
		}
		switch {
		case strings.HasPrefix(s[i:], "{x}"):
			ep.parts = append(ep.parts, s[:i])
			ep.kinds = append(ep.kinds, 'x')
			s = s[i+3:]
		case strings.HasPrefix(s[i:], "{y}"):
			ep.parts = append(ep.parts, s[:i])
			ep.kinds = append(ep.kinds, 'y')
			s = s[i+3:]
		default:
			// a stray brace, keep it literal
			ep.parts = append(ep.parts, s[:i+1])
			ep.kinds = append(ep.kinds, 0)
			s = s[i+1:]
		}
	}
	return ep, nil
}

func (ep *endpoint) url(x, y int) string {
	var sb strings.Builder
	for i, p := range ep.parts {
		sb.WriteString(p)
		switch ep.kinds[i] {
		case 'x':
			sb.WriteString(strconv.Itoa(x))
		case 'y':
			sb.WriteString(strconv.Itoa(y))
		}
	}
	return sb.String()
}

type fetcher struct {
	ep        *endpoint
	cli       *http.Client
	retries   int
	ua        string
	allowMiss bool
}

func newFetcher(ep *endpoint, parallelism int, timeout time.Duration, retries int, ua string, allowMiss bool) *fetcher {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          parallelism,
		MaxIdleConnsPerHost:   parallelism,
		MaxConnsPerHost:       parallelism,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true, // tiles are already compressed
		ForceAttemptHTTP2:     true,
	}
	return &fetcher{
		ep:        ep,
		cli:       &http.Client{Transport: tr, Timeout: timeout},
		retries:   retries,
		ua:        ua,
		allowMiss: allowMiss,
	}
}

func (f *fetcher) newBuffer() *bytes.Buffer {
	return bytes.NewBuffer(make([]byte, 0, 64*1024))
}

// check performs a single request for the first tile to make sure the
// endpoint is reachable and shaped the way we expect before doing any work.
// It is deliberately strict: one attempt, a short deadline, no retries, and a
// 404 is a failure even when -allow-missing is set.  The long timeout and
// the retry loop only apply to the real run.
func (f *fetcher) check(ctx context.Context) error {
	var st stats
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	_, err := f.doRequest(ctx, f.newBuffer(), f.ep.url(0, 0), &st)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errNotFound):
		return fmt.Errorf("%s: 404 Not Found, check the endpoint path", f.ep.url(0, 0))
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("no response within %v", checkTimeout)
	}
	return err
}

// get fetches tile x, y into buf, retrying transient failures with backoff.
// The returned slice aliases buf and is only valid until the next call.
func (f *fetcher) get(ctx context.Context, buf *bytes.Buffer, x, y int, st *stats) ([]byte, error) {
	u := f.ep.url(x, y)
	delay := retryBase
	for attempt := 0; ; attempt++ {
		buff, err := f.doRequest(ctx, buf, u, st)
		if err == nil {
			return buff, nil
		}
		var te *transientError
		if !errors.As(err, &te) || ctx.Err() != nil {
			return nil, err
		}
		if f.retries > 0 && attempt >= f.retries {
			return nil, fmt.Errorf("giving up after %d attempts: %w", attempt+1, err)
		}
		st.retries.Add(1)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if delay *= 2; delay > retryMax {
			delay = retryMax
		}
	}
}

func (f *fetcher) doRequest(ctx context.Context, buf *bytes.Buffer, u string, st *stats) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.ua)
	resp, err := f.cli.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &transientError{err}
	}
	defer func() {
		// drain so the connection can be reused
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		if f.allowMiss {
			return nil, errNotFound
		}
		return nil, fmt.Errorf("%s: %s (pass -allow-missing to skip holes)", u, resp.Status)
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusRequestTimeout:
		return nil, &transientError{fmt.Errorf("%s: %s", u, resp.Status)}
	default:
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	buf.Reset()
	if _, err = buf.ReadFrom(io.LimitReader(resp.Body, tilemap.MaxTileSize+1)); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &transientError{err}
	}
	if buf.Len() == 0 {
		return nil, &transientError{fmt.Errorf("%s: empty response", u)}
	} else if buf.Len() > tilemap.MaxTileSize {
		return nil, fmt.Errorf("%s: tile exceeds %d bytes", u, tilemap.MaxTileSize)
	}
	st.netBytes.Add(uint64(buf.Len()))
	return buf.Bytes(), nil
}

// transientError marks a failure that is worth retrying.
type transientError struct {
	err error
}

func (t *transientError) Error() string { return t.err.Error() }
func (t *transientError) Unwrap() error { return t.err }
