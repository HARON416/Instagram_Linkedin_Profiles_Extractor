package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

type instagramProfileResponse struct {
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
	// Native CDP blocking preserves Chrome's cache. Rod request hijacking would
	// disable the cache for the entire page, defeating the larger optimization.
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

func startInstagramResponseCapture(page *rod.Page) (func(), <-chan instagramProfileResponse) {
	responses := make(chan instagramProfileResponse, 4)
	matchedRequests := make(map[proto.NetworkRequestID]struct{})
	responseStatuses := make(map[proto.NetworkRequestID]int)
	capturePage, cancel := page.WithCancel()

	wait := capturePage.EachEvent(
		func(event *proto.NetworkRequestWillBeSent) {
			if !isInstagramProfileGraphQLRequest(event.Request, event.Type) {
				return
			}
			matchedRequests[event.RequestID] = struct{}{}
		},
		func(event *proto.NetworkResponseReceived) {
			if _, matched := matchedRequests[event.RequestID]; !matched {
				return
			}
			responseStatuses[event.RequestID] = event.Response.Status
		},
		func(event *proto.NetworkLoadingFinished) {
			if _, matched := matchedRequests[event.RequestID]; !matched {
				return
			}
			delete(matchedRequests, event.RequestID)
			statusCode := responseStatuses[event.RequestID]
			delete(responseStatuses, event.RequestID)

			body, err := readNetworkResponseBody(capturePage, event.RequestID)
			if err != nil {
				Warnf("Unable to capture Instagram response body: %v", err)
				return
			}

			select {
			case responses <- instagramProfileResponse{statusCode: statusCode, body: body}:
			default:
			}
		},
		func(event *proto.NetworkLoadingFailed) {
			delete(matchedRequests, event.RequestID)
			delete(responseStatuses, event.RequestID)
		},
	)
	go wait()

	return cancel, responses
}

func isInstagramProfileGraphQLRequest(request *proto.NetworkRequest, resourceType proto.NetworkResourceType) bool {
	if request == nil {
		return false
	}
	if resourceType != proto.NetworkResourceTypeXHR && resourceType != proto.NetworkResourceTypeFetch {
		return false
	}
	if !strings.HasPrefix(request.URL, "https://www.instagram.com/api/graphql") {
		return false
	}
	return hasHeaderValue(request.Headers, "x-fb-friendly-name", instagramProfileQueryName)
}

func readNetworkResponseBody(page *rod.Page, requestID proto.NetworkRequestID) ([]byte, error) {
	result, err := proto.NetworkGetResponseBody{RequestID: requestID}.Call(page)
	if err != nil {
		return nil, err
	}
	return decodeNetworkResponseBody(result.Body, result.Base64Encoded)
}

func decodeNetworkResponseBody(value string, base64Encoded bool) ([]byte, error) {
	body := []byte(value)
	var err error
	if base64Encoded {
		body, err = base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("decode base64 response: %w", err)
		}
	}
	if len(body) > instagramMaxResponseSize {
		return nil, fmt.Errorf("response exceeds %d bytes", instagramMaxResponseSize)
	}
	return body, nil
}

// startInstagramReplayInterceptor is retained as a disabled fallback. It
// duplicates Instagram's browser request through Go's HTTP stack and can be
// re-enabled from main.go if direct CDP response capture stops working.
func startInstagramReplayInterceptor(page *rod.Page) (*rod.HijackRouter, <-chan instagramProfileResponse) {
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
		go func() {
			// Fetch.requestPaused doesn't always expose Cookie. Ask Chrome for the
			// cookies that apply to this exact URL so the replay uses the active login.
			if cookies, err := page.Cookies([]string{captured.url}); err == nil {
				if cookie := cookieHeader(cookies); cookie != "" {
					captured.headers.Set("Cookie", cookie)
				}
			}
			completeInstagramHeaders(page, captured.headers)
			replayInstagramRequest(client, captured, responses)
		}()
	})

	go router.Run()
	return router, responses
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

func completeInstagramHeaders(page *rod.Page, headers http.Header) {
	setHeaderIfMissing(headers, "Priority", "u=1, i")
	setHeaderIfMissing(headers, "Sec-Fetch-Dest", "empty")
	setHeaderIfMissing(headers, "Sec-Fetch-Mode", "cors")
	setHeaderIfMissing(headers, "Sec-Fetch-Site", "same-origin")

	if headers.Get("Accept-Language") != "" {
		return
	}

	acceptLanguage := "en-US,en;q=0.9"
	if result, err := page.Eval(`() => navigator.languages.join(",")`); err == nil {
		if browserLanguages := result.Value.Str(); browserLanguages != "" {
			acceptLanguage = formatAcceptLanguage(browserLanguages)
		}
	}
	headers.Set("Accept-Language", acceptLanguage)
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
	client *http.Client,
	captured instagramGraphQLRequest,
	responses chan<- instagramProfileResponse,
) {
	request, err := http.NewRequest(captured.method, captured.url, bytes.NewReader(captured.body))
	if err != nil {
		Errorf("Instagram replay failed: prepare request: %v", err)
		return
	}
	request.Header = captured.headers.Clone()
	// These are managed by net/http. In particular, allowing Go to negotiate
	// compression avoids receiving a Brotli body that it cannot decode itself.
	request.Header.Del("Accept-Encoding")
	request.Header.Del("Content-Length")
	request.ContentLength = int64(len(captured.body))

	response, err := client.Do(request)
	if err != nil {
		Errorf("Instagram replay failed: send request: %v", err)
		return
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, instagramMaxResponseSize+1))
	if err != nil {
		Errorf("Instagram replay failed: read response: %v", err)
		return
	}
	if len(body) > instagramMaxResponseSize {
		Errorf("Instagram replay failed: response exceeds %d bytes", instagramMaxResponseSize)
		return
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		retryAfter := response.Header.Get("Retry-After")
		if retryAfter == "" {
			retryAfter = "not provided"
		}
		Errorf(
			"Instagram replay returned HTTP %d (Retry-After: %s): %s",
			response.StatusCode,
			retryAfter,
			compactInstagramError(body),
		)
	}

	result := instagramProfileResponse{
		statusCode: response.StatusCode,
		body:       body,
	}
	select {
	case responses <- result:
	default:
	}
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

func waitForInstagramProfileResponse(responses <-chan instagramProfileResponse, name, profileURL string) ([]byte, bool) {
	expectedUsername, err := instagramUsernameFromURL(profileURL)
	if err != nil {
		Warnf("Unable to identify the Instagram username for %q", name)
		return nil, false
	}

	timer := time.NewTimer(instagramResponseWaitTimeout)
	defer timer.Stop()

	for {
		select {
		case response := <-responses:
			if response.statusCode < http.StatusOK || response.statusCode >= http.StatusMultipleChoices {
				continue
			}

			responseUsername, err := instagramUsernameFromResponse(response.body)
			if err != nil || !strings.EqualFold(responseUsername, expectedUsername) {
				continue
			}

			Successf("Captured Instagram profile data for %q (%d bytes)", name, len(response.body))
			return response.body, true
		case <-timer.C:
			Warnf("Instagram profile data was not received for %q", name)
			return nil, false
		}
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
