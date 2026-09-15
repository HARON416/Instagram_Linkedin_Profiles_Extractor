package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
