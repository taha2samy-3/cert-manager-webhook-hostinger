package hostinger

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode([]RecordSet{{Name: "www", Type: "A", Records: []Record{{Content: "1.2.3.4"}}}})
	}))
	defer srv.Close()

	sets, err := NewClient(srv.URL, "tok", "test").GetRecords(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || sets[0].Name != "www" {
		t.Fatalf("sets = %+v", sets)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestClientGivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "tok", "").DeleteRecords(context.Background(), "example.com", []DeleteFilter{{Name: "a", Type: "TXT"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != maxRetries+1 {
		t.Fatalf("calls = %d, want %d", calls.Load(), maxRetries+1)
	}
}

func fastBackoff(t *testing.T) {
	t.Helper()
	old := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = old })
}

func TestClientRetriesServerErrors(t *testing.T) {
	fastBackoff(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadGateway)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = w.Write([]byte(`{"message":"Request accepted"}`))
		}
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "tok", "").UpdateRecords(context.Background(), "example.com", false,
		[]RecordSet{{Name: "a", Type: "TXT", TTL: 60, Records: []Record{{Content: "v"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestClientRetriesNetworkErrors(t *testing.T) {
	fastBackoff(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			// Drop the connection without an answer.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "tok", "").GetRecords(context.Background(), "example.com"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestClientDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "tok", "").GetRecords(context.Background(), "example.com")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

// A Retry-After that does not fit in the time budget returns the 429 at
// once instead of waiting and then failing with a timeout.
func TestClientDoesNotWaitPastTheDeadline(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := NewClient(srv.URL, "tok", "").GetRecords(ctx, "example.com")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second || calls.Load() != 1 {
		t.Fatalf("took %v and %d calls, want an immediate error after 1 call", elapsed, calls.Load())
	}
}

func TestClientNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "tok", "").GetRecords(context.Background(), "example.com")
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false", err)
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		header  string
		attempt int
		want    time.Duration
	}{
		{"5", 0, 5 * time.Second},
		{"", 0, time.Second},
		{"", 2, 4 * time.Second},
		{"garbage", 1, 2 * time.Second},
		{"3600", 0, maxRetryAfter},
	}
	for _, tt := range tests {
		if got := retryAfter(tt.header, tt.attempt); got != tt.want {
			t.Errorf("retryAfter(%q, %d) = %v, want %v", tt.header, tt.attempt, got, tt.want)
		}
	}
}
