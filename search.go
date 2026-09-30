package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tinyFishSearchEndpoint  = "https://api.search.tinyfish.ai"
	tinyFishSearchType      = "web"
	searchRequestTimeout    = 20 * time.Second
	searchMaximumAttempts   = 4
	searchMaximumBodySize   = 2 << 20
	searchRequestsPerMinute = 30
	searchRequestsPerHour   = 500
)

type tinyFishSearchClient struct {
	apiKey      string
	endpoint    string
	language    string
	httpClient  *http.Client
	rateLimiter *rollingWindowLimiter
	retrySleep  func(time.Duration)
}

// rollingWindowLimiter counts actual HTTP attempts (including retries), so a
// retry can never accidentally push the account over either TinyFish quota.
type rollingWindowLimiter struct {
	mu         sync.Mutex
	timestamps []time.Time
	minuteMax  int
	hourMax    int
}

func newTinyFishRateLimiter() *rollingWindowLimiter {
	return &rollingWindowLimiter{
		minuteMax: searchRequestsPerMinute,
		hourMax:   searchRequestsPerHour,
	}
}

type tinyFishSearchResponse struct {
	Query        string             `json:"query,omitempty"`
	Results      []profileCandidate `json:"results"`
	TotalResults int                `json:"total_results,omitempty"`
	Page         int                `json:"page,omitempty"`
}

type profileCandidate struct {
	Position              int                     `json:"position"`
	Title                 string                  `json:"title"`
	Snippet               string                  `json:"snippet,omitempty"`
	URL                   string                  `json:"url"`
	Username              string                  `json:"username,omitempty"`
	InstagramProfile      *instagramProfile       `json:"instagram_profile,omitempty"`
	InstagramCaptureError string                  `json:"instagram_capture_error,omitempty"`
	InstagramCapturedAt   *time.Time              `json:"instagram_captured_at,omitempty"`
	LinkedInPage          *linkedInProfilePage    `json:"linkedin_page,omitempty"`
	LinkedInCaptureError  string                  `json:"linkedin_capture_error,omitempty"`
	LinkedInCapturedAt    *time.Time              `json:"linkedin_captured_at,omitempty"`
	MatchAnalysis         *instagramMatchAnalysis `json:"match_analysis,omitempty"`
}

func newTinyFishSearchClientFromEnv() (*tinyFishSearchClient, error) {
	apiKey := strings.TrimSpace(os.Getenv("TINYFISH_API_KEY"))
	if apiKey == "" {
		return nil, fmt.Errorf("TINYFISH_API_KEY is not set")
	}

	return &tinyFishSearchClient{
		apiKey:      apiKey,
		endpoint:    tinyFishSearchEndpoint,
		language:    "en",
		httpClient:  &http.Client{Timeout: searchRequestTimeout},
		rateLimiter: newTinyFishRateLimiter(),
	}, nil
}

func (client *tinyFishSearchClient) findProfileURL(name string, platform platformSearch) (string, bool, error) {
	candidates, err := client.findProfileCandidates(name, platform)
	if err != nil {
		return "", false, err
	}
	if len(candidates) == 0 {
		return "", false, nil
	}
	return candidates[0].URL, true, nil
}

func (client *tinyFishSearchClient) findProfileCandidates(name string, platform platformSearch) ([]profileCandidate, error) {
	query := profileSearchQuery(name)
	purpose := profileSearchPurpose(name, platform.site)
	response, err := client.search(query, purpose, platform.includeDomain)
	if err != nil {
		return nil, err
	}
	return filterProfileCandidates(response.Results, platform), nil
}

func filterProfileCandidates(results []profileCandidate, platform platformSearch) []profileCandidate {
	candidates := make([]profileCandidate, 0, len(results))
	seenURLs := make(map[string]struct{}, len(results))
	for _, result := range results {
		if !isPlatformProfileURL(platform.site, result.URL) {
			continue
		}
		key := strings.ToLower(strings.TrimRight(result.URL, "/"))
		if _, duplicate := seenURLs[key]; duplicate {
			continue
		}
		seenURLs[key] = struct{}{}
		result.Username = platformProfileUsername(platform.site, result.URL)
		candidates = append(candidates, result)
		if platform.maximumCandidates > 0 && len(candidates) >= platform.maximumCandidates {
			break
		}
	}
	return candidates
}

func platformProfileUsername(site, profileURL string) string {
	switch site {
	case "instagram.com":
		username, _ := instagramUsernameFromURL(profileURL)
		return username
	case "linkedin.com/in":
		parsed, err := url.Parse(profileURL)
		if err != nil {
			return ""
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) == 2 {
			return parts[1]
		}
	}
	return ""
}

func profileSearchQuery(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	name = strings.ReplaceAll(name, `"`, "")
	return fmt.Sprintf(`"%s"`, name)
}

func profileSearchPurpose(name, site string) string {
	name = strings.Join(strings.Fields(name), " ")
	switch site {
	case "instagram.com":
		return fmt.Sprintf("Find direct Instagram user profile pages for the person named %s. Return URLs shaped like https://www.instagram.com/<username>/, including profiles that may be private. Exclude posts, reels, stories, and non-Instagram pages.", name)
	case "linkedin.com/in":
		return fmt.Sprintf("Find direct LinkedIn personal profile pages for the person named %s. Return URLs shaped like https://www.linkedin.com/in/<profile-slug>/ and exclude company, directory, post, and non-LinkedIn pages.", name)
	default:
		return fmt.Sprintf("Find direct personal profile pages for the person named %s on %s.", name, site)
	}
}

func (client *tinyFishSearchClient) search(query, purpose, includeDomain string) (tinyFishSearchResponse, error) {
	var lastStatus int
	for attempt := 0; attempt < searchMaximumAttempts; attempt++ {
		client.waitForRateLimit()

		response, retryAfter, status, err := client.sendSearchRequest(query, purpose, includeDomain)
		lastStatus = status
		if err == nil {
			return response, nil
		}
		if !retryableSearchError(status, err) {
			return tinyFishSearchResponse{}, err
		}
		if attempt == searchMaximumAttempts-1 {
			return tinyFishSearchResponse{}, fmt.Errorf("TinyFish Search failed after %d attempts: %w", searchMaximumAttempts, err)
		}

		if retryAfter <= 0 {
			retryAfter = time.Duration(1<<attempt) * time.Second
		}
		Warnf("TinyFish Search attempt %d/%d failed: %v; retrying in %s", attempt+1, searchMaximumAttempts, err, retryAfter)
		if client.retrySleep != nil {
			client.retrySleep(retryAfter)
		} else {
			time.Sleep(retryAfter)
		}
	}

	return tinyFishSearchResponse{}, fmt.Errorf("TinyFish Search failed with HTTP %d", lastStatus)
}

// Search uses GET, so retrying a transient failure does not mutate remote state.
func retryableSearchError(status int, err error) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	// Do not retry authentication, request validation, or invalid JSON responses.
	if status != 0 && status != http.StatusOK {
		return false
	}
	var networkError net.Error
	return (errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (client *tinyFishSearchClient) waitForRateLimit() {
	if client.rateLimiter == nil {
		return
	}
	client.rateLimiter.Wait()
}

func (limiter *rollingWindowLimiter) Wait() {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	for {
		now := time.Now()
		hourCutoff := now.Add(-time.Hour)
		firstActive := 0
		for firstActive < len(limiter.timestamps) && !limiter.timestamps[firstActive].After(hourCutoff) {
			firstActive++
		}
		if firstActive > 0 {
			limiter.timestamps = append(limiter.timestamps[:0], limiter.timestamps[firstActive:]...)
		}

		var wait time.Duration
		if limiter.minuteMax > 0 && len(limiter.timestamps) >= limiter.minuteMax {
			minuteReady := limiter.timestamps[len(limiter.timestamps)-limiter.minuteMax].Add(time.Minute)
			wait = time.Until(minuteReady)
		}
		if limiter.hourMax > 0 && len(limiter.timestamps) >= limiter.hourMax {
			hourWait := time.Until(limiter.timestamps[len(limiter.timestamps)-limiter.hourMax].Add(time.Hour))
			if hourWait > wait {
				wait = hourWait
			}
		}
		if wait <= 0 {
			limiter.timestamps = append(limiter.timestamps, now)
			return
		}
		Infof("TinyFish free-tier quota reached; resuming searches in %s", wait.Round(time.Second))
		time.Sleep(wait)
	}
}

func (client *tinyFishSearchClient) sendSearchRequest(query, purpose, includeDomain string) (tinyFishSearchResponse, time.Duration, int, error) {
	requestURL, err := url.Parse(client.endpoint)
	if err != nil {
		return tinyFishSearchResponse{}, 0, 0, fmt.Errorf("parse endpoint: %w", err)
	}
	parameters := requestURL.Query()
	parameters.Set("query", query)
	parameters.Set("purpose", purpose)
	parameters.Set("include_domains", includeDomain)
	parameters.Set("language", client.language)
	parameters.Set("domain_type", tinyFishSearchType)
	requestURL.RawQuery = parameters.Encode()

	request, err := http.NewRequest(http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return tinyFishSearchResponse{}, 0, 0, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("X-API-Key", client.apiKey)
	request.Header.Set("Accept", "application/json")

	httpResponse, err := client.httpClient.Do(request)
	if err != nil {
		return tinyFishSearchResponse{}, 0, 0, fmt.Errorf("send request: %w", err)
	}
	defer httpResponse.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, searchMaximumBodySize+1))
	if err != nil {
		return tinyFishSearchResponse{}, 0, httpResponse.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if len(body) > searchMaximumBodySize {
		return tinyFishSearchResponse{}, 0, httpResponse.StatusCode, fmt.Errorf("response exceeds %d bytes", searchMaximumBodySize)
	}

	if httpResponse.StatusCode != http.StatusOK {
		message := strings.Join(strings.Fields(string(body)), " ")
		if len(message) > 300 {
			message = message[:300] + "..."
		}
		if message == "" {
			message = http.StatusText(httpResponse.StatusCode)
		}
		return tinyFishSearchResponse{}, parseRetryAfter(httpResponse.Header.Get("Retry-After")), httpResponse.StatusCode,
			fmt.Errorf("HTTP %d: %s", httpResponse.StatusCode, message)
	}

	var response tinyFishSearchResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return tinyFishSearchResponse{}, 0, httpResponse.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	return response, 0, httpResponse.StatusCode, nil
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		if delay := time.Until(retryAt); delay > 0 {
			return delay
		}
	}
	return 0
}
