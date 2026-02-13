package httpcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type cacheRoundTripper struct {
	original http.RoundTripper
	cacheDir string
}

// NewRoundTripper creates a chainable RoundTripper that handles cache:// and cachez:// schemes.
func NewRoundTripper(cacheDir string, original http.RoundTripper) (http.RoundTripper, error) {
	if cacheDir == "" {
		return nil, fmt.Errorf("cache directory is required")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	if original == nil {
		original = http.DefaultTransport
	}

	return &cacheRoundTripper{
		original: original,
		cacheDir: cacheDir,
	}, nil
}

// AddCacheRoundTripper registers cache:// and cachez:// handlers on a transport.
func AddCacheRoundTripper(cacheDir string, original http.RoundTripper, transport *http.Transport) error {
	if transport == nil {
		return fmt.Errorf("transport is nil")
	}

	rt, err := NewRoundTripper(cacheDir, original)
	if err != nil {
		return err
	}

	transport.RegisterProtocol("cache", rt)
	transport.RegisterProtocol("cachez", rt)
	return nil
}

func (c *cacheRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if c.original == nil {
		return nil, fmt.Errorf("original RoundTripper is not set")
	}

	switch req.URL.Scheme {
	case "cache":
		return c.roundTripWithValidation(req)
	case "cachez":
		return c.roundTripCacheOnly(req)
	default:
		return c.original.RoundTrip(req)
	}
}

func (c *cacheRoundTripper) roundTripWithValidation(req *http.Request) (*http.Response, error) {
	downstreamURL, err := unwrapScheme(req.URL, "cache")
	if err != nil {
		return nil, err
	}

	cachePath := c.cachePathForURL(downstreamURL)
	cachedBody, cachedInfo, cacheErr := readCacheFile(cachePath)
	hasCache := cacheErr == nil
	if cacheErr != nil && !errors.Is(cacheErr, os.ErrNotExist) {
		return nil, cacheErr
	}

	upstreamReq := req.Clone(req.Context())
	upstreamReq.URL = downstreamURL

	if hasCache && upstreamReq.Header.Get("If-Modified-Since") == "" {
		upstreamReq.Header.Set("If-Modified-Since", cachedInfo.ModTime().UTC().Format(http.TimeFormat))
	}

	resp, err := c.original.RoundTrip(upstreamReq)
	if err != nil {
		if hasCache {
			return cachedResponse(req, cachedBody), nil
		}
		return nil, err
	}

	if hasCache && resp.StatusCode == http.StatusNotModified {
		drainAndClose(resp.Body)
		return cachedResponse(req, cachedBody), nil
	}

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		body, readErr := io.ReadAll(resp.Body)
		drainAndClose(resp.Body)
		if readErr != nil {
			if hasCache {
				return cachedResponse(req, cachedBody), nil
			}
			return nil, readErr
		}

		_ = writeCacheFile(cachePath, body)

		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		return resp, nil
	}

	if hasCache {
		drainAndClose(resp.Body)
		return cachedResponse(req, cachedBody), nil
	}

	return resp, nil
}

func (c *cacheRoundTripper) roundTripCacheOnly(req *http.Request) (*http.Response, error) {
	downstreamURL, err := unwrapScheme(req.URL, "cachez")
	if err != nil {
		return nil, err
	}

	cachePath := c.cachePathForURL(downstreamURL)
	cachedBody, _, err := readCacheFile(cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("cache miss for %s", downstreamURL.String())
		}
		return nil, err
	}

	return cachedResponse(req, cachedBody), nil
}

func (c *cacheRoundTripper) cachePathForURL(u *url.URL) string {
	keyURL := cloneURL(u)
	if keyURL.Scheme == "cache" || keyURL.Scheme == "cachez" {
		keyURL.Scheme = "http"
	}
	hash := sha256.Sum256([]byte(keyURL.String()))
	return filepath.Join(c.cacheDir, hex.EncodeToString(hash[:]))
}

func readCacheFile(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
}

func writeCacheFile(path string, data []byte) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func cachedResponse(req *http.Request, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Body:          io.NopCloser(bytes.NewReader(body)),
		Header:        http.Header{"X-HTTP-Cache": []string{"HIT"}},
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func cloneURL(u *url.URL) *url.URL {
	cloned := *u
	return &cloned
}

func unwrapScheme(u *url.URL, scheme string) (*url.URL, error) {
	raw := u.String()
	prefix := scheme + ":"
	if !strings.HasPrefix(raw, prefix) {
		return nil, fmt.Errorf("url does not start with %s prefix", prefix)
	}

	targetRaw := strings.TrimPrefix(raw, prefix)
	if targetRaw == "" {
		return nil, fmt.Errorf("missing downstream URL after %s prefix", prefix)
	}

	targetURL, err := url.Parse(targetRaw)
	if err != nil {
		return nil, fmt.Errorf("parse downstream URL: %w", err)
	}
	if targetURL.Scheme == "" {
		return nil, fmt.Errorf("downstream URL must include scheme")
	}

	return targetURL, nil
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}
