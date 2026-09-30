package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	instagramGraphQLPattern      = "*://www.instagram.com/api/graphql*"
	instagramProfileQueryName    = "PolarisProfilePageContentQuery"
	instagramResponseWaitTimeout = 30 * time.Second
	instagramReplayTimeout       = 30 * time.Second
	instagramMaxResponseSize     = 16 << 20
)

type instagramRateLimitError struct{ retryAfter time.Duration }

func (e *instagramRateLimitError) Error() string {
	return fmt.Sprintf("Instagram rate-limited the request (HTTP 429); cooldown %s", e.retryAfter.Round(time.Second))
}

// One instance is shared by every Instagram candidate in a run.
// The navigation goroutine owns this state; replay workers never access it.
type instagramPacer struct {
	next    time.Time
	strikes int
}

func (p *instagramPacer) wait(ctx context.Context) error {
	if delay := time.Until(p.next); delay > 0 {
		Infof("Pausing Instagram requests for %s", delay.Round(time.Second))
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.next = time.Now().Add(5 * time.Second)
	return nil
}

func (p *instagramPacer) observe(err error, now time.Time) {
	var limited *instagramRateLimitError
	if errors.As(err, &limited) {
		delay := time.Minute
		for i := 0; i < p.strikes && delay < 5*time.Minute; i++ {
			delay *= 2
		}
		if delay > 5*time.Minute {
			delay = 5 * time.Minute
		}
		if p.strikes < 4 {
			p.strikes++
		}
		// A server-provided delay is never shortened to our fallback cap.
		if limited.retryAfter > delay {
			delay = limited.retryAfter
		}
		limited.retryAfter = delay
		p.next = now.Add(delay)
	} else if err == nil {
		p.strikes = 0
	}
}

type instagramProfileResponse struct {
	err        error
	retryAfter time.Duration
	statusCode int
	body       []byte
}

type instagramGraphQLRequest struct {
	method  string
	url     string
	headers http.Header
	body    []byte
}

type instagramProfilePayload struct {
	Data struct {
		User *instagramProfile `json:"user"`
	} `json:"data"`
}

type instagramProfile struct {
	PK                string             `json:"pk"`
	ID                string             `json:"id"`
	Username          string             `json:"username"`
	FullName          string             `json:"full_name"`
	Biography         string             `json:"biography"`
	Category          string             `json:"category"`
	FollowerCount     int                `json:"follower_count"`
	FollowingCount    int                `json:"following_count"`
	MediaCount        int                `json:"media_count"`
	IsPrivate         bool               `json:"is_private"`
	IsVerified        bool               `json:"is_verified"`
	IsBusiness        bool               `json:"is_business"`
	AccountType       int                `json:"account_type"`
	ProfilePictureURL string             `json:"profile_pic_url"`
	HDProfilePicture  instagramPicture   `json:"hd_profile_pic_url_info"`
	ExternalURL       string             `json:"external_url"`
	ThreadsUsername   string             `json:"text_post_app_badge_label"`
	BioLinks          []instagramBioLink `json:"bio_links"`
}

type instagramPicture struct {
	URL string `json:"url"`
}

type instagramBioLink struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	LinkType string `json:"link_type"`
	IsPinned bool   `json:"is_pinned"`
}

func (profile *instagramProfile) identifier() string {
	if profile.ID != "" {
		return profile.ID
	}
	return profile.PK
}

func (profile *instagramProfile) profilePictureURL() string {
	if profile.HDProfilePicture.URL != "" {
		return profile.HDProfilePicture.URL
	}
	return profile.ProfilePictureURL
}

func enableBrowserCaching(page *rod.Page) {
	if err := (proto.NetworkSetCacheDisabled{CacheDisabled: false}).Call(page); err != nil {
		Warnf("Unable to enable the browser cache: %v", err)
	}
	if err := (proto.NetworkSetBypassServiceWorker{Bypass: false}).Call(page); err != nil {
		Warnf("Unable to enable browser service workers: %v", err)
	}
}

func startBrowserResourceBlocker(page *rod.Page) func() {
	// Block heavy resources through CDP independently of GraphQL request replay.
	patterns := []string{
		"*.jpg*", "*.jpeg*", "*.png*", "*.gif*", "*.webp*", "*.avif*", "*.svg*", "*.ico*",
		"*.mp4*", "*.webm*", "*.mov*", "*.avi*",
		"*.woff*", "*.woff2*", "*.ttf*", "*.otf*",
		"*://*.doubleclick.net/*",
		"*://*.google-analytics.com/*",
		"*://*.googleadservices.com/*",
		"*://*.googletagmanager.com/*",
	}
	if err := page.SetBlockedURLs(patterns); err != nil {
		Warnf("Unable to block heavy browser resources: %v", err)
	}
	return func() {
		if err := page.SetBlockedURLs([]string{}); err != nil {
			Warnf("Unable to clear blocked browser resources: %v", err)
		}
	}
}

// startInstagramReplayInterceptor captures the browser's current profile GraphQL
// request and replays it through Go using the active session and request payload.
func startInstagramReplayInterceptor(page *rod.Page) (func(), <-chan instagramProfileResponse) {
	// Read page JavaScript only on the navigation goroutine, before interception.
	acceptLanguage := "en-US,en;q=0.9"
	if result, err := page.Eval(`() => navigator.languages.join(",")`); err == nil {
		if languages := result.Value.Str(); languages != "" {
			acceptLanguage = formatAcceptLanguage(languages)
		}
	}
	replayContext, cancel := context.WithCancel(page.GetContext())
	replayPage := page.Context(replayContext)
	var lifecycle sync.Mutex
	var workers sync.WaitGroup
	stopping := false
	replayStarted := false

	responses := make(chan instagramProfileResponse, 4)
	router := page.HijackRequests()
	client := &http.Client{
		Timeout: instagramReplayTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	router.MustAdd(instagramGraphQLPattern, func(ctx *rod.Hijack) {
		if ctx.Request.Type() != proto.NetworkResourceTypeXHR &&
			ctx.Request.Type() != proto.NetworkResourceTypeFetch {
			ctx.ContinueRequest(&proto.FetchContinueRequest{})
			return
		}

		if !hasHeaderValue(ctx.Request.Headers(), "x-fb-friendly-name", instagramProfileQueryName) {
			ctx.ContinueRequest(&proto.FetchContinueRequest{})
			return
		}

		captured := instagramGraphQLRequest{
			method:  ctx.Request.Method(),
			url:     ctx.Request.URL().String(),
			headers: cloneInstagramHeaders(ctx.Request.Headers()),
			body:    []byte(ctx.Request.Body()),
		}

		// Never fulfill the browser request with the Go HTTP response. Let Chrome
		// finish its original request unchanged, and replay our private copy in a
		// separate goroutine.
		ctx.ContinueRequest(&proto.FetchContinueRequest{})
		lifecycle.Lock()
		if stopping || replayStarted {
			lifecycle.Unlock()
			return
		}
		replayStarted = true
		workers.Add(1)
		lifecycle.Unlock()
		go func() {
			defer workers.Done()
			// Fetch.requestPaused doesn't always expose Cookie. Ask Chrome for the
			// cookies that apply to this exact URL so the replay uses the active login.
			if cookies, err := replayPage.Cookies([]string{captured.url}); err == nil {
				if cookie := cookieHeader(cookies); cookie != "" {
					captured.headers.Set("Cookie", cookie)
				}
			}
			completeInstagramHeaders(captured.headers, acceptLanguage)
			replayInstagramRequest(replayContext, client, captured, responses)
		}()
	})

	go router.Run()
	return func() {
		lifecycle.Lock()
		stopping = true
		lifecycle.Unlock()
		cancel()
		if err := router.Stop(); err != nil {
			Warnf("Unable to stop Instagram GraphQL interceptor: %v", err)
		}
		workers.Wait()
		client.CloseIdleConnections()
	}, responses
}

func cloneInstagramHeaders(headers proto.NetworkHeaders) http.Header {
	cloned := make(http.Header, len(headers))
	for name, value := range headers {
		cloned.Set(name, value.String())
	}
	return cloned
}

func cookieHeader(cookies []*proto.NetworkCookie) string {
	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(parts, "; ")
}

// completeInstagramHeaders is pure: replay workers must never evaluate page
// JavaScript concurrently with navigation or Rod's WaitLoad helper.
func completeInstagramHeaders(headers http.Header, acceptLanguage string) {
	setHeaderIfMissing(headers, "Priority", "u=1, i")
	setHeaderIfMissing(headers, "Sec-Fetch-Dest", "empty")
	setHeaderIfMissing(headers, "Sec-Fetch-Mode", "cors")
	setHeaderIfMissing(headers, "Sec-Fetch-Site", "same-origin")
	if acceptLanguage == "" {
		acceptLanguage = "en-US,en;q=0.9"
	}
	setHeaderIfMissing(headers, "Accept-Language", acceptLanguage)
}

func setHeaderIfMissing(headers http.Header, name, value string) {
	if headers.Get(name) == "" {
		headers.Set(name, value)
	}
}

func formatAcceptLanguage(browserLanguages string) string {
	languages := strings.Split(browserLanguages, ",")
	formatted := make([]string, 0, len(languages))
	for index, language := range languages {
		language = strings.TrimSpace(language)
		if language == "" {
			continue
		}
		if index == 0 {
			formatted = append(formatted, language)
			continue
		}
		quality := 1.0 - float64(index)/10
		if quality < 0.1 {
			quality = 0.1
		}
		formatted = append(formatted, fmt.Sprintf("%s;q=%.1f", language, quality))
	}
	return strings.Join(formatted, ",")
}

func replayInstagramRequest(
	ctx context.Context,
	client *http.Client,
	captured instagramGraphQLRequest,
	responses chan<- instagramProfileResponse,
) {

	publish := func(result instagramProfileResponse) {
		if ctx.Err() != nil {
			return
		}
		select {
		case responses <- result:
		case <-ctx.Done():
		}
	}
	request, err := http.NewRequestWithContext(ctx, captured.method, captured.url, bytes.NewReader(captured.body))
	if err != nil {
		publish(instagramProfileResponse{err: fmt.Errorf("prepare Instagram replay: %w", err)})
		return
	}
	request.Header = captured.headers.Clone()
	request.Header.Del("Accept-Encoding")
	request.Header.Del("Content-Length")
	request.ContentLength = int64(len(captured.body))
	response, err := client.Do(request)
	if err != nil {
		publish(instagramProfileResponse{err: fmt.Errorf("send Instagram replay: %w", err)})
		return
	}
	defer response.Body.Close()
	// Publish HTTP failures without waiting for a body that may be empty or slow.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		publish(instagramProfileResponse{statusCode: response.StatusCode, retryAfter: parseRetryAfter(response.Header.Get("Retry-After"))})
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, instagramMaxResponseSize+1))
	if err != nil {
		publish(instagramProfileResponse{err: fmt.Errorf("read Instagram replay: %w", err)})
		return
	}
	if len(body) > instagramMaxResponseSize {
		publish(instagramProfileResponse{err: fmt.Errorf("Instagram response exceeds %d bytes", instagramMaxResponseSize)})
		return
	}
	publish(instagramProfileResponse{statusCode: response.StatusCode, body: body})
}

func compactInstagramError(body []byte) string {
	message := strings.Join(strings.Fields(string(body)), " ")
	const maximumLength = 300
	if len(message) > maximumLength {
		return message[:maximumLength] + "..."
	}
	if message == "" {
		return "empty response body"
	}
	return message
}

func hasHeaderValue(headers proto.NetworkHeaders, name, expected string) bool {
	for headerName, value := range headers {
		if strings.EqualFold(headerName, name) && value.String() == expected {
			return true
		}
	}
	return false
}

func waitForInstagramProfileResponse(ctx context.Context, responses <-chan instagramProfileResponse, name, profileURL string) ([]byte, error) {
	expectedUsername, err := instagramUsernameFromURL(profileURL)
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(instagramResponseWaitTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case response, ok := <-responses:
		if !ok {
			return nil, fmt.Errorf("Instagram capture stopped before receiving a response")
		}
		if response.err != nil {
			return nil, response.err
		}
		if response.statusCode == http.StatusTooManyRequests {
			return nil, &instagramRateLimitError{retryAfter: response.retryAfter}
		}
		if response.statusCode < 200 || response.statusCode >= 300 {
			return nil, fmt.Errorf("Instagram replay returned HTTP %d", response.statusCode)
		}
		responseUsername, err := instagramUsernameFromResponse(response.body)
		if err != nil {
			return nil, fmt.Errorf("decode Instagram response: %w", err)
		}
		if !strings.EqualFold(responseUsername, expectedUsername) {
			return nil, fmt.Errorf("Instagram returned a response for a different profile")
		}
		Successf("Captured Instagram profile data for %q (%d bytes)", name, len(response.body))
		return response.body, nil
	case <-timer.C:
		return nil, fmt.Errorf("Instagram profile request was not received within %s", instagramResponseWaitTimeout)
	}
}

func instagramUsernameFromURL(profileURL string) (string, error) {
	parsed, err := url.Parse(profileURL)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(parsed.Hostname(), "instagram.com") &&
		!strings.EqualFold(parsed.Hostname(), "www.instagram.com") {
		return "", fmt.Errorf("invalid Instagram profile URL %q", profileURL)
	}

	username := strings.Trim(strings.TrimSpace(parsed.Path), "/")
	if username == "" || strings.Contains(username, "/") {
		return "", fmt.Errorf("invalid Instagram profile URL %q", profileURL)
	}
	return username, nil
}

func isInstagramProfileURL(profileURL string) bool {
	_, err := instagramUsernameFromURL(profileURL)
	return err == nil
}

func instagramUsernameFromResponse(body []byte) (string, error) {
	profile, err := parseInstagramProfileResponse(body)
	if err != nil {
		return "", err
	}
	return profile.Username, nil
}

func parseInstagramProfileResponse(body []byte) (*instagramProfile, error) {
	var payload instagramProfilePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.Data.User == nil || payload.Data.User.Username == "" {
		return nil, fmt.Errorf("Instagram response does not contain a user")
	}
	return payload.Data.User, nil
}
