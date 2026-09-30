package main

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
)

var loginConfirmationMu sync.Mutex

const (
	namesWorkbookPath = "names.xlsx"
	maximumTestNames  = 50
)

type platformSearch struct {
	name              string
	site              string
	includeDomain     string
	loggedOutSelector string
	maximumCandidates int
}

var platforms = []platformSearch{
	{
		name:              "Instagram",
		site:              "instagram.com",
		includeDomain:     "instagram.com",
		loggedOutSelector: `div[aria-modal="true"][role="dialog"]`,
		maximumCandidates: 2,
	},
	{
		name:              "LinkedIn",
		site:              "linkedin.com/in",
		includeDomain:     "linkedin.com",
		loggedOutSelector: `section[aria-modal="true"][role="dialog"][aria-labelledby="base-contextual-sign-in-modal-modal-header"]`,
		maximumCandidates: 1,
	},
}

func main() {
	installBrowserInterruptCleanup()
	ctx := context.Background()
	searchClient, err := newTinyFishSearchClientFromEnv()
	if err != nil {
		Errorf("Unable to configure TinyFish Search: %v", err)
		os.Exit(1)
	}
	matcher, err := newJevProfileMatcherFromEnv()
	if err != nil {
		Errorf("Unable to configure Jev profile matching: %v", err)
		os.Exit(1)
	}
	Infof("Jev profile matching model: %s", matcher.Model())

	records, err := readNameRecords(namesWorkbookPath)
	if err != nil {
		Errorf("Unable to read names: %v", err)
		os.Exit(1)
	}
	totalNames := len(records)
	if totalNames > maximumTestNames {
		records = records[:maximumTestNames]
		Infof("Testing cap applied: processing the first %d of %d names", maximumTestNames, totalNames)
	}
	Successf("Loaded %d names from %s", len(records), namesWorkbookPath)

	searchStartedAt := time.Now()
	if err := discoverProfileCandidates(searchClient, records, candidatesOutputPath); err != nil {
		Errorf("Unable to complete candidate discovery: %v", err)
		os.Exit(1)
	}
	Successf("Profile candidates saved to %s in %s", candidatesOutputPath, time.Since(searchStartedAt).Round(time.Millisecond))

	enrichmentStartedAt := time.Now()
	if err := enrichProfileCandidates(candidatesOutputPath); err != nil {
		Errorf("Unable to complete profile enrichment: %v", err)
		os.Exit(1)
	}
	Successf("Instagram and LinkedIn page data saved to %s in %s", candidatesOutputPath, time.Since(enrichmentStartedAt).Round(time.Millisecond))

	matchingStartedAt := time.Now()
	document, err := analyzeProfileMatches(ctx, matcher, candidatesOutputPath)
	if err != nil {
		Errorf("Unable to complete Jev profile matching: %v", err)
		os.Exit(1)
	}
	Successf("Jev profile matching results saved to %s in %s", candidatesOutputPath, time.Since(matchingStartedAt).Round(time.Millisecond))
	if err := writeMatchWorkbook(matchesWorkbookPath, document); err != nil {
		Errorf("Unable to write profile match workbook: %v", err)
		os.Exit(1)
	}
	Successf("Profile match workbook saved to %s", matchesWorkbookPath)
}

func isPlatformProfileURL(site, profileURL string) bool {
	switch site {
	case "instagram.com":
		return isInstagramProfileURL(profileURL)
	case "linkedin.com/in":
		return isLinkedInProfileURL(profileURL)
	default:
		return false
	}
}

func isLinkedInProfileURL(profileURL string) bool {
	parsed, err := url.Parse(profileURL)
	if err != nil {
		return false
	}

	hostname := strings.ToLower(parsed.Hostname())
	if hostname != "linkedin.com" && !strings.HasSuffix(hostname, ".linkedin.com") {
		return false
	}

	pathParts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	return len(pathParts) == 2 && pathParts[0] == "in" && pathParts[1] != ""
}

func waitForLoginConfirmation(page *rod.Page, platform platformSearch, input *bufio.Reader) error {
	loggedOut, _, err := page.Has(platform.loggedOutSelector)
	if err != nil {
		return fmt.Errorf("check %s login state: %w", platform.name, err)
	}
	if !loggedOut {
		return nil
	}

	// Only one tab may interact with stdin. Reload after acquiring the lock in
	// case another tab completed the shared browser-profile login while this tab
	// was waiting.
	loginConfirmationMu.Lock()
	defer loginConfirmationMu.Unlock()
	if err := page.Reload(); err == nil {
		_ = page.WaitLoad()
		loggedOut, _, err = page.Has(platform.loggedOutSelector)
		if err != nil {
			return fmt.Errorf("recheck %s login state: %w", platform.name, err)
		}
		if !loggedOut {
			return nil
		}
	}

	Promptf(
		"Sign in to %s in the browser, then press Enter to continue",
		platform.name,
	)
	if _, err := input.ReadString('\n'); err != nil {
		return err
	}

	Successf("Continuing with %s", platform.name)
	return nil
}
