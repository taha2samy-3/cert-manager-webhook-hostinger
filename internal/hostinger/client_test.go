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
