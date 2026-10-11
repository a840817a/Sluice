package fetch

import (
	"context"
	"fmt"
	"net/http"
	"slices"
)

// Origin makes a channel's requests for its video: its MPD or playlists, its
// segments and inits, the byte ranges of a SegmentBase representation, and
// its AES-128 keys. It is the one place that decides what such a request
// carries and what a response must look like to be read:
//
//   - The channel's headers (Headers, rotated live), then the gateway's auth
//     header, so a gateway credential wins over a same-named channel one.
//   - Only the statuses the request accepts; any other status is a
//     StatusError, which carries the code for a caller's retry policy.
//   - A bounded read: a body over the caller's limit, declared or streamed, is
//     ErrTooLarge.
//
// Retrying is the caller's concern. A nil *Origin makes requests with no
// headers.
type Origin struct {
	headers             Headers
	authName, authValue string
}

// NewOrigin returns an Origin that sets headers, which may be nil, and then the
// gateway auth header authName: authValue unless authName is empty.
func NewOrigin(headers Headers, authName, authValue string) *Origin {
	return &Origin{headers: headers, authName: authName, authValue: authValue}
}

// StatusError reports an origin response whose status the request does not
// accept.
type StatusError struct {
	Code int
	URL  string
	// Range is the byte range asked for, "" for a whole-file request.
	Range string
}

func (e *StatusError) Error() string {
	if e.Range != "" {
		return fmt.Sprintf("HTTP %d from %s (range %s)", e.Code, e.URL, e.Range)
	}
	return fmt.Sprintf("HTTP %d from %s", e.Code, e.URL)
}

// Get fetches url with client and returns its body, accepting only 200 OK and
// at most limit bytes (no bound if limit <= 0, which callers should avoid).
func (o *Origin) Get(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	return o.get(ctx, client, url, "", limit, http.StatusOK)
}

// GetRange fetches bytes start through end of url. It accepts 206 Partial
// Content and also 200 OK, which some servers answer a range with, and reads
// no more than the range holds, so an origin that ignores Range and sends the
// whole file is refused rather than buffered. limit, when positive, caps an
// absurdly large range.
func (o *Origin) GetRange(ctx context.Context, client *http.Client, url string, start, end uint64, limit int64) ([]byte, error) {
	if end < start {
		return nil, fmt.Errorf("invalid byte range %d-%d for %s", start, end, url)
	}
	n := int64(end-start) + 1
	if n <= 0 || (limit > 0 && n > limit) {
		n = limit
	}
	return o.get(ctx, client, url, fmt.Sprintf("%d-%d", start, end), n, http.StatusPartialContent, http.StatusOK)
}

func (o *Origin) get(ctx context.Context, client *http.Client, url, byteRange string, limit int64, accept ...int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	o.setHeaders(req)
	if byteRange != "" {
		req.Header.Set("Range", "bytes="+byteRange)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if !slices.Contains(accept, resp.StatusCode) {
		return nil, &StatusError{Code: resp.StatusCode, URL: url, Range: byteRange}
	}
	// When the origin declares a size, refuse an oversized body before reading
	// a single byte of it.
	if limit > 0 && resp.ContentLength > limit {
		return nil, ErrTooLarge
	}
	return ReadAtMost(resp.Body, limit)
}

func (o *Origin) setHeaders(req *http.Request) {
	if o == nil {
		return
	}
	ApplyHeaders(req, o.headers)
	if o.authName != "" {
		req.Header.Set(o.authName, o.authValue)
	}
}
