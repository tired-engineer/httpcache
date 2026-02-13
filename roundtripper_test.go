package httpcache

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type mockRoundTripper struct {
	callCount     int
	roundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.callCount++
	if m.roundTripFunc != nil {
		return m.roundTripFunc(req)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

func TestNewRoundTripper(t *testing.T) {
	if _, err := NewRoundTripper("", nil); err == nil {
		t.Fatal("expected error for empty cache directory")
	}

	dir := filepath.Join(t.TempDir(), "cache")
	rt, err := NewRoundTripper(dir, nil)
	if err != nil {
		t.Fatalf("NewRoundTripper() error = %v", err)
	}
	if rt == nil {
		t.Fatal("expected non-nil round tripper")
	}
}

func TestRoundTripCacheFetchAndStore(t *testing.T) {
	cacheDir := t.TempDir()
	mockRT := &mockRoundTripper{
		roundTripFunc: func(req *http.Request) (*http.Response, error) {
			if req.URL.Scheme != "https" {
				t.Fatalf("expected upstream scheme https, got %s", req.URL.Scheme)
			}
			if ims := req.Header.Get("If-Modified-Since"); ims != "" {
				t.Fatalf("expected no If-Modified-Since on first request, got %q", ims)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("fresh")),
				Request:    req,
			}, nil
		},
	}

	rawRT, err := NewRoundTripper(cacheDir, mockRT)
	if err != nil {
		t.Fatalf("NewRoundTripper() error = %v", err)
	}
	rt := rawRT.(*cacheRoundTripper)

	req := httptest.NewRequest(http.MethodGet, "cache:https://example.com/data", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}

	body := readBody(t, resp.Body)
	if body != "fresh" {
		t.Fatalf("expected fresh response body, got %q", body)
	}

	downstreamURL, err := url.Parse("https://example.com/data")
	if err != nil {
		t.Fatalf("parse downstream URL: %v", err)
	}
	cachePath := rt.cachePathForURL(downstreamURL)
	cachedBytes, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	if string(cachedBytes) != "fresh" {
		t.Fatalf("expected cached body fresh, got %q", string(cachedBytes))
	}
}

func TestRoundTripCacheUsesIfModifiedSinceAndFallsBackToCacheOn304(t *testing.T) {
	cacheDir := t.TempDir()
	mockRT := &mockRoundTripper{}

	rawRT, err := NewRoundTripper(cacheDir, mockRT)
	if err != nil {
		t.Fatalf("NewRoundTripper() error = %v", err)
	}
	rt := rawRT.(*cacheRoundTripper)

	req := httptest.NewRequest(http.MethodGet, "cache:https://example.com/data", nil)
	downstreamURL, err := url.Parse("https://example.com/data")
	if err != nil {
		t.Fatalf("parse downstream URL: %v", err)
	}
	cachePath := rt.cachePathForURL(downstreamURL)
	if err := writeCacheFile(cachePath, []byte("cached-value")); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	modTime := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(cachePath, modTime, modTime); err != nil {
		t.Fatalf("set cache mtime: %v", err)
	}

	mockRT.roundTripFunc = func(req *http.Request) (*http.Response, error) {
		expected := modTime.Format(http.TimeFormat)
		if got := req.Header.Get("If-Modified-Since"); got != expected {
			t.Fatalf("expected If-Modified-Since %q, got %q", expected, got)
		}
		return &http.Response{
			StatusCode: http.StatusNotModified,
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected cached status code 200, got %d", resp.StatusCode)
	}

	body := readBody(t, resp.Body)
	if body != "cached-value" {
		t.Fatalf("expected cached body, got %q", body)
	}
}

func TestRoundTripCacheFallsBackToCacheOnFailure(t *testing.T) {
	cacheDir := t.TempDir()
	mockRT := &mockRoundTripper{
		roundTripFunc: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader("server-error")),
				Request:    req,
			}, nil
		},
	}

	rawRT, err := NewRoundTripper(cacheDir, mockRT)
	if err != nil {
		t.Fatalf("NewRoundTripper() error = %v", err)
	}
	rt := rawRT.(*cacheRoundTripper)

	req := httptest.NewRequest(http.MethodGet, "cache:https://example.com/data", nil)
	downstreamURL, err := url.Parse("https://example.com/data")
	if err != nil {
		t.Fatalf("parse downstream URL: %v", err)
	}
	cachePath := rt.cachePathForURL(downstreamURL)
	if err := writeCacheFile(cachePath, []byte("cached-error-fallback")); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	body := readBody(t, resp.Body)
	if body != "cached-error-fallback" {
		t.Fatalf("expected cached fallback body, got %q", body)
	}
}

func TestRoundTripCachezUsesCacheOnly(t *testing.T) {
	cacheDir := t.TempDir()
	mockRT := &mockRoundTripper{
		roundTripFunc: func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("upstream should not be called")
		},
	}

	rawRT, err := NewRoundTripper(cacheDir, mockRT)
	if err != nil {
		t.Fatalf("NewRoundTripper() error = %v", err)
	}
	rt := rawRT.(*cacheRoundTripper)

	req := httptest.NewRequest(http.MethodGet, "cachez:https://example.com/data", nil)
	downstreamURL, err := url.Parse("https://example.com/data")
	if err != nil {
		t.Fatalf("parse downstream URL: %v", err)
	}
	cachePath := rt.cachePathForURL(downstreamURL)
	if err := writeCacheFile(cachePath, []byte("cachez-value")); err != nil {
		t.Fatalf("write cache file: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if mockRT.callCount != 0 {
		t.Fatalf("expected upstream not to be called, got %d calls", mockRT.callCount)
	}

	body := readBody(t, resp.Body)
	if body != "cachez-value" {
		t.Fatalf("expected cachez body, got %q", body)
	}
}

func TestRoundTripCachezMiss(t *testing.T) {
	rawRT, err := NewRoundTripper(t.TempDir(), &mockRoundTripper{})
	if err != nil {
		t.Fatalf("NewRoundTripper() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "cachez:https://example.com/miss", nil)
	_, err = rawRT.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error for cachez miss")
	}
}

func TestAddCacheRoundTripper(t *testing.T) {
	if err := AddCacheRoundTripper(t.TempDir(), nil, nil); err == nil {
		t.Fatal("expected error for nil transport")
	}

	cacheDir := t.TempDir()
	transport := &http.Transport{}
	if err := AddCacheRoundTripper(cacheDir, nil, transport); err != nil {
		t.Fatalf("AddCacheRoundTripper() error = %v", err)
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte("v1"))
			return
		}

		if r.Header.Get("If-Modified-Since") == "" {
			t.Fatal("expected If-Modified-Since header on second request")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	client := &http.Client{Transport: transport}
	cacheURL := "cache:" + parsed.Scheme + "://" + parsed.Host + "/resource"

	resp1, err := client.Get(cacheURL)
	if err != nil {
		t.Fatalf("first client.Get() error = %v", err)
	}
	if got := readBody(t, resp1.Body); got != "v1" {
		t.Fatalf("expected first body v1, got %q", got)
	}

	resp2, err := client.Get(cacheURL)
	if err != nil {
		t.Fatalf("second client.Get() error = %v", err)
	}
	if got := readBody(t, resp2.Body); got != "v1" {
		t.Fatalf("expected second body from cache v1, got %q", got)
	}
}

func TestRoundTripPassThroughWithoutPrefix(t *testing.T) {
	mockRT := &mockRoundTripper{
		roundTripFunc: func(req *http.Request) (*http.Response, error) {
			if req.URL.Scheme != "https" {
				t.Fatalf("expected scheme https, got %s", req.URL.Scheme)
			}
			if req.URL.Host != "example.com" {
				t.Fatalf("expected host example.com, got %s", req.URL.Host)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("pass-through")),
				Request:    req,
			}, nil
		},
	}

	rt, err := NewRoundTripper(t.TempDir(), mockRT)
	if err != nil {
		t.Fatalf("NewRoundTripper() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/data", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if mockRT.callCount != 1 {
		t.Fatalf("expected original transport to be called once, got %d", mockRT.callCount)
	}
	if got := readBody(t, resp.Body); got != "pass-through" {
		t.Fatalf("expected pass-through body, got %q", got)
	}
}

func readBody(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}
