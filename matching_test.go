package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

type stubMatcher struct {
	result matchResponse
	err    error
}

func (stub stubMatcher) Threshold() float64 { return 0.9 }

func (stub stubMatcher) Model() string { return "test-jev" }

func (stub stubMatcher) Compare(context.Context, personCandidates) (matchResponse, error) {
	return stub.result, stub.err
}

func TestAnalyzeProfileMatchesSelectsHighestSupportedCandidate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "profile_candidates.json")
	now := time.Now().UTC()
	document := candidateDocument{
		GeneratedAt: now,
		UpdatedAt:   now,
		People: []personCandidates{{
			SourceRow: 2,
			FullName:  "Example Person",
			LinkedInCandidates: []profileCandidate{{
				URL: "https://www.linkedin.com/in/example/",
				LinkedInPage: &linkedInProfilePage{
					URL:   "https://www.linkedin.com/in/example/",
					Title: "Example Person | LinkedIn",
					Text:  "Example Person, architect at Example Studio in London",
				},
			}},
			InstagramCandidates: []profileCandidate{
				{URL: "https://www.instagram.com/weak/", InstagramProfile: &instagramProfile{Username: "weak", Biography: "Designer"}},
				{URL: "https://www.instagram.com/strong/", InstagramProfile: &instagramProfile{Username: "strong", FullName: "Example Person", Biography: "Architect at Example Studio, London"}},
			},
		}},
	}
	if err := writeCandidateDocument(path, document); err != nil {
		t.Fatal(err)
	}

	matcher := stubMatcher{result: matchResponse{
		Candidates: []candidateAssessment{
			{CandidateNumber: 1, Score: 0.2},
			{CandidateNumber: 2, Score: 0.94},
		},
	}}
	matched, err := analyzeProfileMatches(context.Background(), matcher, path)
	if err != nil {
		t.Fatal(err)
	}
	person := matched.People[0]
	if got, want := person.BestInstagramURL, "https://www.instagram.com/strong/"; got != want {
		t.Fatalf("best URL = %q, want %q", got, want)
	}
	if person.BestMatchScore == nil || *person.BestMatchScore != 0.94 {
		t.Fatalf("best score = %v, want 0.94", person.BestMatchScore)
	}
	if person.Match == nil || !*person.Match {
		t.Fatal("expected match")
	}

	refreshed, found, err := readCandidateDocument(path)
	if err != nil || !found {
		t.Fatalf("read persisted document: found=%t err=%v", found, err)
	}
	if refreshed.People[0].InstagramCandidates[1].MatchAnalysis == nil {
		t.Fatal("candidate analysis was not persisted")
	}
}

func TestWriteMatchWorkbook(t *testing.T) {
	t.Parallel()
	score := 0.94
	matched := true
	now := time.Now().UTC()
	document := candidateDocument{People: []personCandidates{{
		FullName:         "Example Person",
		BestInstagramURL: "https://www.instagram.com/example/",
		BestMatchScore:   &score,
		Match:            &matched,
		MatchStatus:      "complete",
		MatchModel:       "test-jev",
		AnalyzedAt:       &now,
		LinkedInCandidates: []profileCandidate{{
			URL:          "https://www.linkedin.com/in/example/",
			LinkedInPage: &linkedInProfilePage{Title: "Example Person | LinkedIn"},
		}},
		InstagramCandidates: []profileCandidate{{
			URL:              "https://www.instagram.com/example/",
			InstagramProfile: &instagramProfile{FullName: "Example Person", Biography: "Architect"},
			MatchAnalysis:    &instagramMatchAnalysis{Score: 0.94, Match: true},
		}},
	}}}
	path := filepath.Join(t.TempDir(), "profile_matches.xlsx")
	if err := writeMatchWorkbook(path, document); err != nil {
		t.Fatal(err)
	}
	file, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for cell, want := range map[string]string{
		"A2": "Example Person", "C2": "https://www.instagram.com/example/",
		"D2": "94%", "E2": "Yes",
	} {
		got, err := file.GetCellValue(matchesSheet, cell)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", cell, got, want)
		}
	}
	for cell, want := range map[string]string{
		"A2": "Example Person", "B2": "Complete", "C2": "test-jev",
	} {
		got, err := file.GetCellValue(runDetailsSheet, cell)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Run Details %s = %q, want %q", cell, got, want)
		}
	}
	formula, err := file.GetCellFormula(runDetailsSheet, "E2")
	if err != nil {
		t.Fatal(err)
	}
	if formula != `=""` {
		t.Fatalf("blank overflow blocker formula = %q, want %q", formula, `=""`)
	}
	for _, sheet := range []string{matchesSheet, runDetailsSheet} {
		firstStyle, err := file.GetCellStyle(sheet, "A1")
		if err != nil {
			t.Fatal(err)
		}
		lastStyle, err := file.GetCellStyle(sheet, "XFD1")
		if err != nil {
			t.Fatal(err)
		}
		if lastStyle != firstStyle {
			t.Fatalf("%s far-right header style = %d, want %d", sheet, lastStyle, firstStyle)
		}
	}
}

func testPerson() personCandidates {
	return personCandidates{FullName: "Example Person",
		LinkedInCandidates:  []profileCandidate{{LinkedInPage: &linkedInProfilePage{Text: "Example Person, architect"}}},
		InstagramCandidates: []profileCandidate{{URL: "https://instagram.com/example", InstagramProfile: &instagramProfile{FullName: "Example Person"}}},
	}
}

func TestJevHTTPContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/alpha/decisions" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect request headers or endpoint")
		}
		var request jevRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Model != defaultJevModel || request.State.RequestedName != "Example Person" || len(request.Questions) != 1 || request.Questions["candidate_2"].Type != "noul" {
			t.Errorf("incorrect request: %+v", request)
		}
		fmt.Fprint(w, `{"model":"typesafe/jev-snapshot","answers":{"candidate_2":{"type":"noul","noul":0.9432}}}`)
	}))
	defer server.Close()
	m := &jevMatcher{client: server.Client(), endpoint: server.URL + "/api/alpha/decisions", apiKey: "test-key", model: defaultJevModel, threshold: 0.9}
	person := testPerson()
	person.InstagramCandidates = append([]profileCandidate{{URL: "uncaptured"}}, person.InstagramCandidates...)
	result, err := m.Compare(context.Background(), person)
	if err != nil {
		t.Fatal(err)
	}
	applyMatchResult(&person, result, m.Threshold())
	if *person.BestMatchScore != 0.9432 || !*person.Match || person.MatchModel != "typesafe/jev-snapshot" || person.InstagramCandidates[0].MatchAnalysis != nil {
		t.Fatalf("incorrect result: %+v", person)
	}
}

func TestJevRejectsInvalidResponses(t *testing.T) {
	for _, body := range []string{
		`{}`, `not json`,
		`{"answers":{"candidate_1":{"type":"noul"}}}`,
		`{"answers":{"candidate_1":{"type":"noul","noul":null}}}`,
		`{"answers":{"candidate_1":{"type":"choice","noul":0.9}}}`,
		`{"answers":{"candidate_1":{"type":"noul","noul":1.1}}}`,
		`{"answers":{"candidate_1":{"type":"noul","noul":-0.1}}}`,
		`{"answers":{"candidate_2":{"type":"noul","noul":0.9}}}`,
		`{"answers":{"candidate_1":{"type":"noul","noul":0.9},"extra":{}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			m := &jevMatcher{client: server.Client(), endpoint: server.URL}
			if _, err := m.Compare(context.Background(), testPerson()); err == nil {
				t.Fatal("expected invalid response error")
			}
		})
	}
}

func TestJevHTTPFailureAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, "sensitive response body")
	}))
	defer server.Close()
	m := &jevMatcher{client: server.Client(), endpoint: server.URL}
	if _, err := m.Compare(context.Background(), testPerson()); err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unexpected HTTP error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Compare(ctx, testPerson()); err == nil {
		t.Fatal("expected cancellation")
	}
}

func TestMatchThresholdAndZeroProbability(t *testing.T) {
	for _, score := range []float64{0, 0.89999, 0.9, 1} {
		person := testPerson()
		applyMatchResult(&person, matchResponse{Candidates: []candidateAssessment{{CandidateNumber: 1, Score: score}}}, 0.9)
		if person.Match == nil || *person.Match != (score >= 0.9) || *person.BestMatchScore != score || person.BestInstagramURL == "" {
			t.Fatalf("incorrect result for %v", score)
		}
	}
}

func TestJevConfiguration(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	if _, err := newJevProfileMatcherFromEnv(); err == nil {
		t.Fatal("expected missing key error")
	}
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("JEV_MODEL", "")
	t.Setenv("JEV_MATCH_THRESHOLD", "")
	m, err := newJevProfileMatcherFromEnv()
	if err != nil || m.Model() != defaultJevModel || m.Threshold() != 0.9 {
		t.Fatalf("unexpected defaults: %v", err)
	}
	for _, value := range []string{"0", "-1", "1.1", "NaN", "Inf", "bad"} {
		t.Setenv("JEV_MATCH_THRESHOLD", value)
		if _, err := newJevProfileMatcherFromEnv(); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	t.Setenv("JEV_MATCH_THRESHOLD", "0.95")
	m, err = newJevProfileMatcherFromEnv()
	if err != nil || m.Threshold() != 0.95 {
		t.Fatal("custom threshold was not applied")
	}
}

func TestFailedAndSkippedMatchesRemainUnknown(t *testing.T) {
	for _, missing := range []bool{false, true} {
		person := testPerson()
		if missing {
			person.LinkedInCandidates = nil
		}
		old := true
		person.Match = &old
		path := filepath.Join(t.TempDir(), "candidates.json")
		if err := writeCandidateDocument(path, candidateDocument{People: []personCandidates{person}}); err != nil {
			t.Fatal(err)
		}
		doc, err := analyzeProfileMatches(context.Background(), stubMatcher{err: fmt.Errorf("test failure")}, path)
		if err != nil {
			t.Fatal(err)
		}
		if doc.People[0].Match != nil || doc.People[0].BestMatchScore != nil {
			t.Fatal("unknown result became a decision")
		}
		workbook := filepath.Join(t.TempDir(), "matches.xlsx")
		if err := writeMatchWorkbook(workbook, doc); err != nil {
			t.Fatal(err)
		}
		f, err := excelize.OpenFile(workbook)
		if err != nil {
			t.Fatal(err)
		}
		for _, cell := range []string{"D2", "E2"} {
			value, err := f.GetCellValue(matchesSheet, cell)
			if err != nil || value != "" {
				t.Fatalf("%s should be blank: %q %v", cell, value, err)
			}
		}
		f.Close()
	}
}
