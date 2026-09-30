package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestReplayInstagramRequestPreservesCapturedRequest(t *testing.T) {
	t.Parallel()

	const requestBody = "doc_id=fresh-doc-id&variables=fresh-variables"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if request.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", request.Method)
		}
		if string(body) != requestBody {
			t.Errorf("body = %q, want %q", body, requestBody)
		}
		if got := request.Header.Get("X-Fb-Friendly-Name"); got != instagramProfileQueryName {
			t.Errorf("friendly name = %q", got)
		}
		if got := request.Header.Get("Cookie"); got != "sessionid=fresh-session" {
			t.Errorf("cookie = %q", got)
		}
		for header, expected := range map[string]string{
			"Accept-Language": "en-US,en;q=0.9",
			"Priority":        "u=1, i",
			"Sec-Fetch-Dest":  "empty",
			"Sec-Fetch-Mode":  "cors",
			"Sec-Fetch-Site":  "same-origin",
		} {
			if got := request.Header.Get(header); got != expected {
				t.Errorf("%s = %q, want %q", header, got, expected)
			}
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{"data":{"user":{"username":"example"}}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	responses := make(chan instagramProfileResponse, 1)
	replayInstagramRequest(context.Background(), client, instagramGraphQLRequest{
		method: http.MethodPost,
		url:    "https://www.instagram.com/api/graphql",
		headers: http.Header{
			"Accept-Language":    {"en-US,en;q=0.9"},
			"Content-Type":       {"application/x-www-form-urlencoded"},
			"Cookie":             {"sessionid=fresh-session"},
			"Priority":           {"u=1, i"},
			"Sec-Fetch-Dest":     {"empty"},
			"Sec-Fetch-Mode":     {"cors"},
			"Sec-Fetch-Site":     {"same-origin"},
			"X-Fb-Friendly-Name": {instagramProfileQueryName},
		},
		body: []byte(requestBody),
	}, responses)

	response := <-responses
	if response.statusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.statusCode)
	}
	if username, err := instagramUsernameFromResponse(response.body); err != nil || username != "example" {
		t.Fatalf("response username = %q, err = %v", username, err)
	}
}

func TestCloneInstagramHeaders(t *testing.T) {
	t.Parallel()

	headers := cloneInstagramHeaders(proto.NetworkHeaders{
		"x-fb-friendly-name": gson.New(instagramProfileQueryName),
		"accept-encoding":    gson.New("br, gzip"),
		"content-length":     gson.New("123"),
	})

	if got := headers.Get("X-Fb-Friendly-Name"); got != instagramProfileQueryName {
		t.Fatalf("friendly name = %q", got)
	}
	if got := headers.Get("Accept-Encoding"); got != "br, gzip" {
		t.Fatalf("accept-encoding = %q, want captured value", got)
	}
	if got := headers.Get("Content-Length"); got != "123" {
		t.Fatalf("content-length = %q, want captured value", got)
	}
}

func TestFormatAcceptLanguage(t *testing.T) {
	t.Parallel()

	if got := formatAcceptLanguage("en-US,en,sw"); got != "en-US,en;q=0.9,sw;q=0.8" {
		t.Fatalf("accept-language = %q", got)
	}
}

func TestCompleteInstagramHeaders(t *testing.T) {
	t.Parallel()

	headers := http.Header{"Accept-Language": {"en-KE,en;q=0.9"}}
	completeInstagramHeaders(headers, "en-US,en;q=0.9")

	for header, expected := range map[string]string{
		"Accept-Language": "en-KE,en;q=0.9",
		"Priority":        "u=1, i",
		"Sec-Fetch-Dest":  "empty",
		"Sec-Fetch-Mode":  "cors",
		"Sec-Fetch-Site":  "same-origin",
	} {
		if got := headers.Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}
}

func TestCompleteInstagramHeadersWithoutPage(t *testing.T) {
	for _, language := range []string{"", "sw,en;q=0.9"} {
		headers := make(http.Header)
		completeInstagramHeaders(headers, language)
		want := language
		if want == "" {
			want = "en-US,en;q=0.9"
		}
		if headers.Get("Accept-Language") != want {
			t.Fatalf("language = %q", headers.Get("Accept-Language"))
		}
	}
}

func TestInstagramReplayCancellationStopsInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	responses := make(chan instagramProfileResponse, 1)
	go func() {
		defer close(done)
		replayInstagramRequest(ctx, server.Client(), instagramGraphQLRequest{method: "GET", url: server.URL, headers: make(http.Header)}, responses)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("replay did not stop after cancellation")
	}
	if len(responses) != 0 {
		t.Fatal("canceled replay published a response")
	}
}

func TestInstagramHTTPFailuresReachCaptureImmediately(t *testing.T) {
	for _, code := range []int{429, 401, 403, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(code)
			}))
			defer server.Close()
			responses := make(chan instagramProfileResponse, 1)
			replayInstagramRequest(context.Background(), server.Client(), instagramGraphQLRequest{method: "GET", url: server.URL, headers: make(http.Header)}, responses)
			// A buffered HTTP failure must be returned without waiting for the 30s timeout.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := waitForInstagramProfileResponse(ctx, responses, "Example", "https://instagram.com/example/")
			if err == nil || !strings.Contains(err.Error(), fmt.Sprint(code)) {
				t.Fatalf("unexpected HTTP error: %v", err)
			}
			if code == 429 {
				var limited *instagramRateLimitError
				if !errors.As(err, &limited) || limited.retryAfter != 2*time.Minute {
					t.Fatalf("lost retry metadata: %v", err)
				}
			}
		})
	}
}

func TestInstagramCooldownPersistsAndRespectsRetryAfter(t *testing.T) {
	now := time.Now()
	pacer := &instagramPacer{}
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		err := &instagramRateLimitError{}
		pacer.observe(err, now)
		if pacer.next.Sub(now) != want || err.retryAfter != want {
			t.Fatalf("cooldown = %s, want %s", pacer.next.Sub(now), want)
		}
	}
	pacer.observe(&instagramRateLimitError{retryAfter: 20 * time.Minute}, now)
	if pacer.next.Sub(now) != 20*time.Minute {
		t.Fatal("server cooldown was shortened")
	}
	pacer.observe(nil, now)
	if pacer.strikes != 0 {
		t.Fatal("success did not reset escalation")
	}
}

func TestInstagramPacingAndCancellation(t *testing.T) {
	pacer := &instagramPacer{}
	before := time.Now()
	if err := pacer.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pacer.next.Before(before.Add(5 * time.Second)) {
		t.Fatal("next profile was not paced")
	}
	next := pacer.next
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pacer.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v", err)
	}
	if pacer.next != next {
		t.Fatal("cancellation changed pacing state")
	}
}

func TestInstagramCaptureReportsTransportAndProfileErrors(t *testing.T) {
	for _, response := range []instagramProfileResponse{
		{err: fmt.Errorf("transport failed")},
		{statusCode: 200, body: []byte(`not json`)},
		{statusCode: 200, body: []byte(`{"data":{"user":{"username":"different"}}}`)},
	} {
		responses := make(chan instagramProfileResponse, 1)
		responses <- response
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := waitForInstagramProfileResponse(ctx, responses, "Example", "https://instagram.com/example/")
		cancel()
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected immediate capture failure: %v", err)
		}
	}
	responses := make(chan instagramProfileResponse, 1)
	responses <- instagramProfileResponse{statusCode: 200, body: []byte(`{"data":{"user":{"username":"example"}}}`)}
	if _, err := waitForInstagramProfileResponse(context.Background(), responses, "Example", "https://instagram.com/example/"); err != nil {
		t.Fatal(err)
	}
}
