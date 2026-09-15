package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

type stubGeminiMatcher struct {
	result geminiMatchResponse
	err    error
}

func (stub stubGeminiMatcher) Model() string { return "test-gemini" }

func (stub stubGeminiMatcher) Compare(context.Context, personCandidates) (geminiMatchResponse, error) {
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

	matcher := stubGeminiMatcher{result: geminiMatchResponse{
		BaselineNameSupported: true,
		BaselineReason:        "The requested name appears in the LinkedIn content.",
		Candidates: []geminiCandidateAssessment{
			{CandidateNumber: 1, NameScore: 10, OccupationScore: 10, EvidenceSufficient: true, Summary: "Weak evidence"},
			{CandidateNumber: 2, NameScore: 30, OccupationScore: 25, OrganizationScore: 15, LocationScore: 10, LinkScore: 5, DistinctiveScore: 5, EvidenceSufficient: true, Summary: "Strong professional overlap"},
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
	if person.BestMatchScore == nil || *person.BestMatchScore != 90 {
		t.Fatalf("best score = %v, want 90", person.BestMatchScore)
	}
	if got, want := person.MatchDecision, "likely_match"; got != want {
		t.Fatalf("decision = %q, want %q", got, want)
	}
	if person.ManualReview {
		t.Fatal("90% likely match should not require manual review")
	}

	refreshed, found, err := readCandidateDocument(path)
	if err != nil || !found {
		t.Fatalf("read persisted document: found=%t err=%v", found, err)
	}
	if refreshed.People[0].InstagramCandidates[1].MatchAnalysis == nil {
		t.Fatal("candidate analysis was not persisted")
	}
}

func TestMatchDecisionAbstainsWhenEvidenceOrBaselineIsMissing(t *testing.T) {
	t.Parallel()
	if got := matchDecision(100, false, true); got != "baseline_unverified" {
		t.Fatalf("unsupported baseline decision = %q", got)
	}
	if got := matchDecision(100, true, false); got != "insufficient_evidence" {
		t.Fatalf("insufficient evidence decision = %q", got)
	}
	if got := matchDecision(79, true, true); got != "possible_match" {
		t.Fatalf("79 score decision = %q", got)
	}
	if got := matchDecision(59, true, true); got != "unlikely_match" {
		t.Fatalf("59 score decision = %q", got)
	}
}

func TestSanitizeMatchSummaryRemovesCandidateReferences(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"Candidate 1 is an exact match to the baseline, sharing an identical name.": "An exact match to the baseline, sharing an identical name.",
		"Candidate 2: Strong professional overlap.":                                 "Strong professional overlap.",
		"The details are stronger than Candidate 1.":                                "The details are stronger than the Instagram profile.",
	} {
		if got := sanitizeMatchSummary(input); got != want {
			t.Errorf("sanitizeMatchSummary(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWriteMatchWorkbook(t *testing.T) {
	t.Parallel()
	score := 90
	now := time.Now().UTC()
	document := candidateDocument{People: []personCandidates{{
		FullName:         "Example Person",
		BestInstagramURL: "https://www.instagram.com/example/",
		BestMatchScore:   &score,
		MatchDecision:    "likely_match",
		MatchSummary:     "Strong overlap",
		MatchStatus:      "complete",
		GeminiModel:      "test-gemini",
		AnalyzedAt:       &now,
		LinkedInCandidates: []profileCandidate{{
			URL:          "https://www.linkedin.com/in/example/",
			LinkedInPage: &linkedInProfilePage{Title: "Example Person | LinkedIn"},
		}},
		InstagramCandidates: []profileCandidate{{
			URL:              "https://www.instagram.com/example/",
			InstagramProfile: &instagramProfile{FullName: "Example Person", Biography: "Architect"},
			MatchAnalysis:    &instagramMatchAnalysis{Score: 90, Decision: "likely_match"},
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
		"D2": "90%", "E2": "Likely Match", "F2": "Strong overlap", "G2": "No",
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
		"A2": "Example Person", "B2": "Complete", "C2": "test-gemini",
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
