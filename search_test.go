package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTinyFishSearchFindsFirstValidProfileURL(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("X-API-Key"); got != "test-key" {
			t.Errorf("X-API-Key = %q", got)
		}
		if got, want := request.URL.Query().Get("query"), `"Jane Doe"`; got != want {
			t.Errorf("query = %q, want %q", got, want)
		}
		if request.URL.Query().Has("location") {
			t.Error("location should not be sent")
		}
		if got, want := request.URL.Query().Get("language"), "en"; got != want {
			t.Errorf("language = %q, want %q", got, want)
		}
		if got, want := request.URL.Query().Get("include_domains"), "instagram.com"; got != want {
			t.Errorf("include_domains = %q, want %q", got, want)
		}
		if got, want := request.URL.Query().Get("purpose"), "Find direct Instagram user profile pages for the person named Jane Doe. Return URLs shaped like https://www.instagram.com/<username>/, including profiles that may be private. Exclude posts, reels, stories, and non-Instagram pages."; got != want {
			t.Errorf("purpose = %q, want %q", got, want)
		}
		if got := request.URL.Query().Get("domain_type"); got != "web" {
			t.Errorf("domain_type = %q, want web", got)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"results":[
			{"position":1,"title":"Reel","url":"https://www.instagram.com/reel/abc/"},
			{"position":2,"title":"Jane Doe (@jane.doe) • Instagram photos","url":"https://www.instagram.com/jane.doe/"}
		]}`))
	}))
	defer server.Close()

	client := &tinyFishSearchClient{
		apiKey:     "test-key",
		endpoint:   server.URL,
		language:   "en",
		httpClient: server.Client(),
	}

	profileURL, found, err := client.findProfileURL("Jane Doe", platforms[0])
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected a profile result")
	}
	if want := "https://www.instagram.com/jane.doe/"; profileURL != want {
		t.Fatalf("profile URL = %q, want %q", profileURL, want)
	}
}

func TestTinyFishSearchReturnsAPIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, `{"error":"invalid key"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	client := &tinyFishSearchClient{
		apiKey:     "bad-key",
		endpoint:   server.URL,
		httpClient: server.Client(),
	}

	if _, _, err := client.findProfileURL("Jane Doe", platforms[0]); err == nil {
		t.Fatal("expected an API error")
	}
}

func TestTinyFishRetriesTransientHTTPFailures(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if attempts < 3 {
					http.Error(w, "temporary gateway failure", status)
					return
				}
				fmt.Fprint(w, `{"results":[]}`)
			}))
			defer server.Close()
			var delays []time.Duration
			client := &tinyFishSearchClient{endpoint: server.URL, httpClient: server.Client(), rateLimiter: newTinyFishRateLimiter(), retrySleep: func(delay time.Duration) { delays = append(delays, delay) }}
			if _, err := client.search("name", "purpose", "instagram.com"); err != nil {
				t.Fatal(err)
			}
			if attempts != 3 || !reflect.DeepEqual(delays, []time.Duration{time.Second, 2 * time.Second}) {
				t.Fatalf("attempts=%d delays=%v", attempts, delays)
			}
			if len(client.rateLimiter.timestamps) != 3 {
				t.Fatal("retries bypassed quota accounting")
			}
		})
	}
}

func TestTinyFishRetriesAreBoundedAndHonorRetryAfter(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "10")
		http.Error(w, "gateway failed", 502)
	}))
	defer server.Close()
	var delays []time.Duration
	client := &tinyFishSearchClient{endpoint: server.URL, httpClient: server.Client(), retrySleep: func(delay time.Duration) { delays = append(delays, delay) }}
	_, err := client.search("name", "purpose", "instagram.com")
	if err == nil || !strings.Contains(err.Error(), "after 4 attempts") || !strings.Contains(err.Error(), "502") {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 4 || !reflect.DeepEqual(delays, []time.Duration{10 * time.Second, 10 * time.Second, 10 * time.Second}) {
		t.Fatalf("attempts=%d delays=%v", attempts, delays)
	}
}

func TestTinyFishDoesNotRetryPermanentFailures(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				w.WriteHeader(status)
				fmt.Fprint(w, "invalid response")
			}))
			defer server.Close()
			client := &tinyFishSearchClient{endpoint: server.URL, httpClient: server.Client(), retrySleep: func(time.Duration) { t.Error("unexpected retry") }}
			if _, err := client.search("name", "purpose", "instagram.com"); err == nil {
				t.Fatal("expected failure")
			}
			if attempts != 1 {
				t.Fatalf("attempts=%d", attempts)
			}
		})
	}
}

func TestTinyFishRetriesNetworkTimeout(t *testing.T) {
	attempts := 0
	client := &tinyFishSearchClient{endpoint: "https://example.test", retrySleep: func(time.Duration) {}, httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, context.DeadlineExceeded
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"results":[]}`)), Header: make(http.Header)}, nil
	})}}
	if _, err := client.search("name", "purpose", "instagram.com"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d", attempts)
	}
}
