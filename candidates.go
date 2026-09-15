package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	candidatesOutputPath        = "profile_candidates.json"
	candidateCheckpointInterval = 10
	searchWorkerCount           = 1
)

type candidateSearchJob struct {
	recordIndex int
}

type candidateSearchResult struct {
	job                 candidateSearchJob
	instagramCandidates []profileCandidate
	linkedInCandidates  []profileCandidate
	duration            time.Duration
	err                 error
}

type candidateDocument struct {
	GeneratedAt           time.Time          `json:"generated_at"`
	UpdatedAt             time.Time          `json:"updated_at"`
	SourceWorkbook        string             `json:"source_workbook"`
	SearchProvider        string             `json:"search_provider"`
	InstagramDataProvider string             `json:"instagram_data_provider"`
	LinkedInDataProvider  string             `json:"linkedin_data_provider"`
	MatchingProvider      string             `json:"matching_provider,omitempty"`
	MatchingModel         string             `json:"matching_model,omitempty"`
	People                []personCandidates `json:"people"`
}

type personCandidates struct {
	SourceRow             int                `json:"source_row"`
	FirstName             string             `json:"first_name"`
	LastName              string             `json:"last_name"`
	FullName              string             `json:"full_name"`
	InstagramCandidates   []profileCandidate `json:"instagram_candidates"`
	LinkedInCandidates    []profileCandidate `json:"linkedin_candidates"`
	Complete              bool               `json:"complete"`
	SearchedAt            *time.Time         `json:"searched_at,omitempty"`
	EnrichmentComplete    bool               `json:"enrichment_complete,omitempty"`
	EnrichedAt            *time.Time         `json:"enriched_at,omitempty"`
	MatchStatus           string             `json:"match_status,omitempty"`
	BestInstagramURL      string             `json:"best_instagram_url,omitempty"`
	BestMatchScore        *int               `json:"best_match_score,omitempty"`
	MatchDecision         string             `json:"match_decision,omitempty"`
	MatchSummary          string             `json:"match_summary,omitempty"`
	ManualReview          bool               `json:"manual_review,omitempty"`
	BaselineNameSupported bool               `json:"baseline_name_supported,omitempty"`
	BaselineReason        string             `json:"baseline_reason,omitempty"`
	GeminiModel           string             `json:"gemini_model,omitempty"`
	GeminiError           string             `json:"gemini_error,omitempty"`
	AnalyzedAt            *time.Time         `json:"analyzed_at,omitempty"`
}

func discoverProfileCandidates(
	client *tinyFishSearchClient,
	records []nameRecord,
	outputPath string,
) error {
	return discoverProfileCandidatesWithCallback(client, records, outputPath, nil)
}

func discoverProfileCandidatesWithCallback(
	client *tinyFishSearchClient,
	records []nameRecord,
	outputPath string,
	onPersonDiscovered func(int, personCandidates),
) error {
	now := time.Now().UTC()
	document := candidateDocument{
		GeneratedAt:           now,
		UpdatedAt:             now,
		SourceWorkbook:        namesWorkbookPath,
		SearchProvider:        "tinyfish_search",
		InstagramDataProvider: "browser_cdp_response",
		LinkedInDataProvider:  "browser_rod_rendered_primary_content",
		People:                make([]personCandidates, len(records)),
	}
	for recordIndex, record := range records {
		document.People[recordIndex] = personCandidates{
			SourceRow: record.SourceRow,
			FirstName: record.FirstName,
			LastName:  record.LastName,
			FullName:  record.FullName,
		}
	}

	totalJobs := len(records)
	jobs := make(chan candidateSearchJob)
	results := make(chan candidateSearchResult, searchWorkerCount)
	workers := searchWorkerCount
	if totalJobs < workers {
		workers = totalJobs
	}
	var workersDone sync.WaitGroup
	workersDone.Add(workers)
	for range workers {
		go func() {
			defer workersDone.Done()
			for job := range jobs {
				record := records[job.recordIndex]
				startedAt := time.Now()
				Infof("Searching Instagram candidates for %q", record.FullName)
				instagramCandidates, instagramErr := client.findProfileCandidates(record.FullName, platforms[0])
				Infof("Searching LinkedIn candidates for %q", record.FullName)
				linkedInCandidates, linkedInErr := client.findProfileCandidates(record.FullName, platforms[1])
				results <- candidateSearchResult{
					job:                 job,
					instagramCandidates: instagramCandidates,
					linkedInCandidates:  linkedInCandidates,
					duration:            time.Since(startedAt),
					err:                 errors.Join(instagramErr, linkedInErr),
				}
			}
		}()
	}
	go func() {
		for recordIndex := range records {
			jobs <- candidateSearchJob{recordIndex: recordIndex}
		}
		close(jobs)
		workersDone.Wait()
		close(results)
	}()

	completedPeople := 0
	var firstError error
	for result := range results {
		record := records[result.job.recordIndex]
		Infof("Instagram and LinkedIn search for %q finished in %s", record.FullName, result.duration.Round(time.Millisecond))
		person := &document.People[result.job.recordIndex]
		person.InstagramCandidates = result.instagramCandidates
		person.LinkedInCandidates = result.linkedInCandidates
		if result.err != nil {
			if firstError == nil {
				firstError = fmt.Errorf("search profiles for %q: %w", record.FullName, result.err)
			}
			continue
		}
		Successf("Found %d Instagram and %d LinkedIn candidate(s) for %q", len(result.instagramCandidates), len(result.linkedInCandidates), record.FullName)
		searchedAt := time.Now().UTC()
		person.SearchedAt = &searchedAt
		person.Complete = true
		document.UpdatedAt = searchedAt
		completedPeople++
		if onPersonDiscovered != nil {
			onPersonDiscovered(result.job.recordIndex, *person)
		}
		if completedPeople > 0 && (completedPeople%candidateCheckpointInterval == 0 || completedPeople == len(records)) {
			if err := writeCandidateDocument(outputPath, document); err != nil {
				return fmt.Errorf("checkpoint after %d people: %w", completedPeople, err)
			}
			Infof("Saved candidate checkpoint for %d/%d people", completedPeople, len(records))
		}
	}
	if firstError != nil {
		document.UpdatedAt = time.Now().UTC()
		if err := writeCandidateDocument(outputPath, document); err != nil {
			return fmt.Errorf("%v; save partial results: %w", firstError, err)
		}
		return firstError
	}

	document.UpdatedAt = time.Now().UTC()
	if err := writeCandidateDocument(outputPath, document); err != nil {
		return fmt.Errorf("save final candidate document: %w", err)
	}
	return nil
}
func readCandidateDocument(path string) (candidateDocument, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return candidateDocument{}, false, nil
		}
		return candidateDocument{}, false, err
	}
	defer file.Close()

	var document candidateDocument
	if err := json.NewDecoder(file).Decode(&document); err != nil {
		return candidateDocument{}, false, err
	}
	return document, true, nil
}

func writeCandidateDocument(path string, document candidateDocument) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".profile-candidates-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}

	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
