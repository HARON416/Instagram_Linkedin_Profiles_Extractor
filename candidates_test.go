package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestDiscoverProfileCandidatesStoresAllValidResults(t *testing.T) {
	t.Parallel()

	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestCount++
		query := request.URL.Query().Get("query")
		domain := request.URL.Query().Get("include_domains")
		if query != `"Example Person"` {
			http.Error(response, "unexpected query: "+query, http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch domain {
		case "instagram.com":
			_, _ = response.Write([]byte(`{"results":[
				{"position":1,"title":"Post","snippet":"Not a profile","url":"https://www.instagram.com/reel/abc/"},
				{"position":2,"title":"First profile","snippet":"First bio preview","url":"https://www.instagram.com/first.profile/"},
				{"position":3,"title":"Second profile","snippet":"Second bio preview","url":"https://www.instagram.com/second.profile/"},
				{"position":4,"title":"Duplicate","url":"https://www.instagram.com/first.profile"}
			]}`))
		case "linkedin.com":
			_, _ = response.Write([]byte(`{"results":[
				{"position":1,"title":"Company","url":"https://www.linkedin.com/company/example/"},
				{"position":2,"title":"Profile","snippet":"Role preview","url":"https://www.linkedin.com/in/example-person/"},
				{"position":3,"title":"Second profile","url":"https://www.linkedin.com/in/second-person/"}
			]}`))
		default:
			http.Error(response, "unexpected include domain: "+domain, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := &tinyFishSearchClient{
		apiKey:     "test-key",
		endpoint:   server.URL,
		language:   "en",
		httpClient: server.Client(),
	}
	records := []nameRecord{{
		SourceRow: 2,
		FirstName: "Example",
		LastName:  "Person",
		FullName:  "Example Person",
	}}
	outputPath := filepath.Join(t.TempDir(), "profile_candidates.json")

	if err := discoverProfileCandidates(client, records, outputPath); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var document candidateDocument
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.People) != 1 {
		t.Fatalf("people = %d, want 1", len(document.People))
	}
	person := document.People[0]
	if !person.Complete {
		t.Fatal("expected discovery to be complete")
	}
	if len(person.InstagramCandidates) != 2 {
		t.Fatalf("Instagram candidates = %d, want 2", len(person.InstagramCandidates))
	}
	if len(person.LinkedInCandidates) != 1 {
		t.Fatalf("LinkedIn candidates = %d, want 1", len(person.LinkedInCandidates))
	}
	if got, want := person.InstagramCandidates[1].URL, "https://www.instagram.com/second.profile/"; got != want {
		t.Fatalf("second Instagram URL = %q, want %q", got, want)
	}
	if got, want := person.InstagramCandidates[1].Username, "second.profile"; got != want {
		t.Fatalf("second Instagram username = %q, want %q", got, want)
	}
	document.People[0].InstagramCandidates[0].InstagramProfile = &instagramProfile{
		Username:  "first.profile",
		Biography: "stale biography",
	}
	if err := writeCandidateDocument(outputPath, document); err != nil {
		t.Fatal(err)
	}

	client = &tinyFishSearchClient{
		apiKey:     "test-key",
		endpoint:   server.URL,
		language:   "en",
		httpClient: server.Client(),
	}
	if err := discoverProfileCandidates(client, records, outputPath); err != nil {
		t.Fatal(err)
	}
	if got := requestCount; got != 4 {
		t.Fatalf("requests after fresh discovery = %d, want 4", got)
	}
	refreshed, found, err := readCandidateDocument(outputPath)
	if err != nil || !found {
		t.Fatalf("read refreshed candidates: found=%t, err=%v", found, err)
	}
	if gotProfile := refreshed.People[0].InstagramCandidates[0].InstagramProfile; gotProfile != nil {
		t.Fatalf("stale enrichment was reused: %#v", gotProfile)
	}
}

func TestDiscoverProfileCandidatesSearchesEachPersonInstagramThenLinkedIn(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.URL.Query().Get("query")+" "+request.URL.Query().Get("include_domains"))
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	client := &tinyFishSearchClient{
		apiKey:     "test-key",
		endpoint:   server.URL,
		language:   "en",
		httpClient: server.Client(),
	}
	records := []nameRecord{
		{SourceRow: 2, FullName: "First Person"},
		{SourceRow: 3, FullName: "Second Person"},
	}
	outputPath := filepath.Join(t.TempDir(), "profile_candidates.json")

	if err := discoverProfileCandidates(client, records, outputPath); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`"First Person" instagram.com`,
		`"First Person" linkedin.com`,
		`"Second Person" instagram.com`,
		`"Second Person" linkedin.com`,
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("request order = %#v, want %#v", requests, want)
	}
}

func TestRodWindowCount(t *testing.T) {
	t.Setenv("ROD_WINDOWS", "7")
	if got := rodWindowCount(); got != 7 {
		t.Fatalf("rodWindowCount() = %d, want 7", got)
	}
	t.Setenv("ROD_WINDOWS", "100")
	if got := rodWindowCount(); got != maximumRodWindowCount {
		t.Fatalf("capped rodWindowCount() = %d, want %d", got, maximumRodWindowCount)
	}
}

func TestRodWindowCountUsesSafeDefault(t *testing.T) {
	t.Setenv("ROD_WINDOWS", "")
	t.Setenv("ROD_WORKERS", "")
	if got := rodWindowCount(); got != 10 {
		t.Fatalf("default rodWindowCount() = %d, want 10", got)
	}
}
