package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

const terminalPageDataLimit = 12_000

const enrichmentCheckpointInterval = 10

const (
	linkedInPrimaryContentSelector = `section[aria-label="Primary content"]`
	linkedInSectionWaitTimeout     = 2 * time.Minute
	linkedInInitialStableTimeout   = 10 * time.Second
	linkedInStableWindow           = time.Second
	linkedInScrollPause            = time.Second
	linkedInPostScrollTimeout      = 5 * time.Second
	linkedInStableScrollRounds     = 3
	linkedInMaximumScrolls         = 40
)

type linkedInProfilePage struct {
	URL              string `json:"url"`
	FinalURL         string `json:"final_url,omitempty"`
	Title            string `json:"title,omitempty"`
	Text             string `json:"text"`
	SectionSelector  string `json:"section_selector"`
	ScrollIterations int    `json:"scroll_iterations"`
}

type linkedInScrollState struct {
	Found         bool `json:"found"`
	PageHeight    int  `json:"page_height"`
	SectionHeight int  `json:"section_height"`
	TextLength    int  `json:"text_length"`
}

func enrichProfileCandidates(outputPath string) error {
	document, found, err := readCandidateDocument(outputPath)
	if err != nil {
		return fmt.Errorf("read candidates: %w", err)
	}
	if !found {
		return fmt.Errorf("candidate file %s does not exist", outputPath)
	}

	document.InstagramDataProvider = "browser_graphql_replay"
	input := bufio.NewReader(os.Stdin)
	var page *rod.Page
	if hasProfileCandidates(document) {
		browser, loginPage := openBrowser()
		defer closeBrowserPagesAndBrowser(browser)
		page = loginPage
		enableBrowserCaching(page)
		if err := prepareBrowserLogins(page, input, document); err != nil {
			return saveEnrichmentError(outputPath, document, "browser login", err)
		}
	}

	if page != nil {
		// Reuse the login page for every platform and person; never open worker windows.
		stopBlockingResources := startBrowserResourceBlocker(page)
		defer stopBlockingResources()
		Infof("Enriching profiles sequentially in one browser window")
	}

	pacer := &instagramPacer{}
	var firstError error
	for index := range document.People {
		person := &document.People[index]
		person.EnrichmentComplete = false
		person.EnrichedAt = nil
		failed := false
		for _, platform := range platforms {
			if platform.site == "instagram.com" && len(person.InstagramCandidates) == 0 {
				continue
			}
			if platform.site == "linkedin.com/in" && len(person.LinkedInCandidates) == 0 {
				continue
			}
			if _, err := page.Activate(); err != nil {
				Warnf("Unable to activate browser window: %v", err)
			}
			startedAt := time.Now()
			printNameLogHeader(platform.name+" PAGE DATA", person.FullName, person.SourceRow, index+1, len(document.People))
			var captureErr error
			switch platform.site {
			case "instagram.com":
				captureErr = enrichInstagramCandidates(page, input, person, pacer)
			case "linkedin.com/in":
				captureErr = enrichLinkedInCandidates(page, input, person)
			}
			Infof("%s enrichment for %q finished in %s", platform.name, person.FullName, time.Since(startedAt).Round(time.Millisecond))
			if captureErr != nil {
				failed = true
				if firstError == nil {
					firstError = fmt.Errorf("enrich %s for %q: %w", platform.name, person.FullName, captureErr)
				}
			}
		}
		now := time.Now().UTC()
		if !failed {
			person.EnrichmentComplete = personHasAllPageData(*person)
			person.EnrichedAt = &now
			Successf("Enriched candidates for %q (%d/%d)", person.FullName, index+1, len(document.People))
		}
		document.UpdatedAt = now
		if (index+1)%enrichmentCheckpointInterval == 0 {
			if err := writeCandidateDocument(outputPath, document); err != nil {
				return fmt.Errorf("save enrichment for %q: %w", person.FullName, err)
			}
			Infof("Saved enrichment checkpoint for %d/%d people", index+1, len(document.People))
		}
	}
	document.UpdatedAt = time.Now().UTC()
	if err := writeCandidateDocument(outputPath, document); err != nil {
		if firstError != nil {
			return fmt.Errorf("%v; save partial data: %w", firstError, err)
		}
		return fmt.Errorf("save enrichment: %w", err)
	}
	return firstError
}

func prepareBrowserLogins(page *rod.Page, input *bufio.Reader, document candidateDocument) error {
	for platformIndex, platform := range platforms {
		profileURL := firstCandidateURL(document, platform.site)
		if profileURL == "" {
			continue
		}
		Infof("Checking %s login before capturing profiles", platform.name)
		if err := page.Navigate(profileURL); err != nil {
			return fmt.Errorf("open %s login check: %w", platform.name, err)
		}
		if err := page.WaitLoad(); err != nil {
			return fmt.Errorf("wait for %s login check: %w", platform.name, err)
		}
		if err := waitForLoginConfirmation(page, platforms[platformIndex], input); err != nil {
			return err
		}
	}
	return nil
}

func firstCandidateURL(document candidateDocument, site string) string {
	for _, person := range document.People {
		switch site {
		case "instagram.com":
			if len(person.InstagramCandidates) > 0 {
				return person.InstagramCandidates[0].URL
			}
		case "linkedin.com/in":
			if len(person.LinkedInCandidates) > 0 {
				return person.LinkedInCandidates[0].URL
			}
		}
	}
	return ""
}

func hasProfileCandidates(document candidateDocument) bool {
	for _, person := range document.People {
		if len(person.InstagramCandidates) > 0 || len(person.LinkedInCandidates) > 0 {
			return true
		}
	}
	return false
}

func hasInstagramCandidates(document candidateDocument) bool {
	for _, person := range document.People {
		if len(person.InstagramCandidates) > 0 {
			return true
		}
	}
	return false
}

func personHasAllPageData(person personCandidates) bool {
	for _, candidate := range person.InstagramCandidates {
		if candidate.InstagramProfile == nil {
			return false
		}
	}
	for _, candidate := range person.LinkedInCandidates {
		if candidate.LinkedInPage == nil {
			return false
		}
	}
	return true
}

func enrichInstagramCandidates(page *rod.Page, input *bufio.Reader, person *personCandidates, pacer *instagramPacer) error {
	if page == nil {
		return fmt.Errorf("Instagram browser was not initialized")
	}
	Infof("Instagram candidates: %d", len(person.InstagramCandidates))
	for index := range person.InstagramCandidates {
		candidate := &person.InstagramCandidates[index]
		candidate.InstagramProfile = nil
		candidate.InstagramCapturedAt = nil
		candidate.InstagramCaptureError = ""
		if err := pacer.wait(page.GetContext()); err != nil {
			return err
		}
		Infof("Instagram candidate %d: %s — %s", index+1, candidate.URL, candidate.Title)
		profile, err := captureInstagramCandidate(page, input, person.FullName, candidate.URL)
		pacer.observe(err, time.Now())
		if err != nil {
			recordInstagramCaptureError(candidate, err)
			// Stop page background traffic while the shared session cools down.
			if blankErr := page.Navigate("about:blank"); blankErr != nil {
				return fmt.Errorf("stop Instagram page after capture failure: %w", blankErr)
			}
			continue
		}
		now := time.Now().UTC()
		candidate.InstagramProfile = profile
		candidate.InstagramCapturedAt = &now
		logStructuredPageData("Instagram", profile)
	}
	return nil
}

func captureInstagramCandidate(page *rod.Page, input *bufio.Reader, name, profileURL string) (*instagramProfile, error) {
	// Scope the response channel and deduplication to this candidate. No stale
	// response or error from a previous profile can be attributed to this one.
	stopReplay, responses := startInstagramReplayInterceptor(page)
	defer stopReplay()
	if err := page.Navigate(profileURL); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return nil, fmt.Errorf("wait for load: %w", err)
	}
	if err := waitForLoginConfirmation(page, platforms[0], input); err != nil {
		return nil, err
	}
	body, err := waitForInstagramProfileResponse(page.GetContext(), responses, name, profileURL)
	if err != nil {
		return nil, err
	}
	profile, err := parseInstagramProfileResponse(body)
	if err != nil {
		return nil, fmt.Errorf("decode profile response: %w", err)
	}
	return profile, nil
}

func recordInstagramCaptureError(candidate *profileCandidate, err error) {
	candidate.InstagramCaptureError = err.Error()
	Warnf("Instagram data unavailable for %s: %v", candidate.URL, err)
}

func enrichLinkedInCandidates(page *rod.Page, input *bufio.Reader, person *personCandidates) error {
	Infof("LinkedIn candidates: %d", len(person.LinkedInCandidates))
	for index := range person.LinkedInCandidates {
		candidate := &person.LinkedInCandidates[index]
		candidate.LinkedInPage = nil
		candidate.LinkedInCaptureError = ""
		candidate.LinkedInCapturedAt = nil
		Infof("LinkedIn candidate %d: %s — %s", index+1, candidate.URL, candidate.Title)
		if page == nil {
			return fmt.Errorf("LinkedIn browser was not initialized")
		}

		profilePage, err := captureLinkedInProfilePage(page, input, candidate.URL)
		if err != nil {
			candidate.LinkedInCaptureError = err.Error()
			Warnf("LinkedIn data unavailable for %s: %v", candidate.URL, err)
			continue
		}
		now := time.Now().UTC()
		candidate.LinkedInPage = profilePage
		candidate.LinkedInCapturedAt = &now
		logLinkedInPageData(profilePage)
	}
	return nil
}

func captureLinkedInProfilePage(page *rod.Page, input *bufio.Reader, profileURL string) (*linkedInProfilePage, error) {
	navigationStartedAt := time.Now()
	if err := page.Navigate(profileURL); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return nil, fmt.Errorf("wait for load: %w", err)
	}
	Infof("LinkedIn navigation/load for %s finished in %s", profileURL, time.Since(navigationStartedAt).Round(time.Millisecond))
	loginStartedAt := time.Now()
	if err := waitForLoginConfirmation(page, platforms[1], input); err != nil {
		return nil, err
	}
	Infof("LinkedIn login check for %s finished in %s", profileURL, time.Since(loginStartedAt).Round(time.Millisecond))

	sectionStartedAt := time.Now()
	if _, err := page.Timeout(linkedInSectionWaitTimeout).Element(linkedInPrimaryContentSelector); err != nil {
		return nil, fmt.Errorf("find primary content section: %w", err)
	}
	Infof("LinkedIn primary-content wait for %s finished in %s", profileURL, time.Since(sectionStartedAt).Round(time.Millisecond))
	stableStartedAt := time.Now()
	if err := page.Timeout(linkedInInitialStableTimeout).WaitStable(linkedInStableWindow); err != nil {
		Warnf("LinkedIn initial JavaScript render did not fully stabilize within %s: %v", linkedInInitialStableTimeout, err)
	}
	Infof("LinkedIn stability wait for %s finished in %s", profileURL, time.Since(stableStartedAt).Round(time.Millisecond))

	scrollStartedAt := time.Now()
	iterations, err := scrollLinkedInPrimaryContent(page)
	if err != nil {
		return nil, err
	}
	Infof("LinkedIn scrolling for %s finished in %s after %d iteration(s)", profileURL, time.Since(scrollStartedAt).Round(time.Millisecond), iterations)
	section, err := page.Timeout(linkedInSectionWaitTimeout).Element(linkedInPrimaryContentSelector)
	if err != nil {
		return nil, fmt.Errorf("refind primary content section: %w", err)
	}
	text, err := section.Text()
	if err != nil {
		return nil, fmt.Errorf("read primary content section: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("primary content section was empty")
	}

	result := &linkedInProfilePage{
		URL:              profileURL,
		Text:             text,
		SectionSelector:  linkedInPrimaryContentSelector,
		ScrollIterations: iterations,
	}
	if info, err := page.Info(); err == nil {
		result.FinalURL = info.URL
		result.Title = info.Title
	}
	return result, nil
}

func scrollLinkedInPrimaryContent(page *rod.Page) (int, error) {
	previous := linkedInScrollState{PageHeight: -1, SectionHeight: -1, TextLength: -1}
	stableRounds := 0
	for iteration := 1; iteration <= linkedInMaximumScrolls; iteration++ {
		remote, err := page.Eval(`() => {
			const section = document.querySelector('section[aria-label="Primary content"]');
			if (!section) return JSON.stringify({found: false});
			const pageHeight = Math.max(
				document.body ? document.body.scrollHeight : 0,
				document.documentElement ? document.documentElement.scrollHeight : 0
			);
			window.scrollTo(0, pageHeight);
			section.scrollTop = section.scrollHeight;
			const last = section.lastElementChild;
			if (last) last.scrollIntoView({block: "end"});
			return JSON.stringify({
				found: true,
				page_height: pageHeight,
				section_height: section.scrollHeight,
				text_length: (section.innerText || "").length
			});
		}`)
		if err != nil {
			return iteration, fmt.Errorf("scroll primary content: %w", err)
		}
		var state linkedInScrollState
		if err := json.Unmarshal([]byte(remote.Value.Str()), &state); err != nil {
			return iteration, fmt.Errorf("decode scroll state: %w", err)
		}
		if !state.Found {
			return iteration, fmt.Errorf("primary content section disappeared while scrolling")
		}

		if state.PageHeight == previous.PageHeight &&
			state.SectionHeight == previous.SectionHeight &&
			state.TextLength == previous.TextLength {
			stableRounds++
		} else {
			stableRounds = 0
		}
		if stableRounds >= linkedInStableScrollRounds {
			return iteration, nil
		}
		previous = state
		// LinkedIn has continuously changing UI widgets, so this wait is
		// deliberately bounded. The height/text convergence check above is the
		// final stopping condition if the whole DOM never becomes perfectly idle.
		_ = page.Timeout(linkedInPostScrollTimeout).WaitDOMStable(linkedInScrollPause, 0)
	}
	Warnf("LinkedIn content was still changing after %d scrolls; capturing the current section", linkedInMaximumScrolls)
	return linkedInMaximumScrolls, nil
}

func printNameLogHeader(stage, fullName string, sourceRow, current, total int) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr)
	Infof("========== %s: %s (source row %d, %d/%d) ==========", stage, fullName, sourceRow, current, total)
}

func logStructuredPageData(platform string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		Warnf("Unable to format %s page data: %v", platform, err)
		return
	}
	Infof("%s page data:\n%s", platform, terminalDataPreview(string(data)))
}

func logLinkedInPageData(page *linkedInProfilePage) {
	metadata := *page
	metadata.Text = ""
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		Warnf("Unable to format LinkedIn page metadata: %v", err)
		return
	}
	Infof("LinkedIn rendered page metadata:\n%s", string(data))
	Infof("LinkedIn rendered primary content:\n%s", terminalDataPreview(page.Text))
}

func terminalDataPreview(value string) string {
	if len(value) <= terminalPageDataLimit {
		return value
	}
	return value[:terminalPageDataLimit] +
		fmt.Sprintf("\n... [terminal output truncated at %d bytes; full data is stored in %s]", terminalPageDataLimit, candidatesOutputPath)
}

func saveEnrichmentError(outputPath string, document candidateDocument, fullName string, cause error) error {
	document.UpdatedAt = time.Now().UTC()
	if err := writeCandidateDocument(outputPath, document); err != nil {
		return fmt.Errorf("enrich %q: %v; save partial data: %w", fullName, cause, err)
	}
	return fmt.Errorf("enrich %q: %w", fullName, cause)
}
