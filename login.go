package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

const browserCleanupTimeout = 10 * time.Second

var (
	activeBrowserMu sync.Mutex
	activeBrowser   *rod.Browser
)

func installBrowserInterruptCleanup() {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		<-interrupts
		Warn("Interrupt received; closing automated browser windows")
		closeActiveBrowser()
		os.Exit(130)
	}()
}

func openBrowser() (*rod.Browser, *rod.Page) {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	accountName := filepath.Base(cwd)
	profileDir := filepath.Join(cwd, "browser_profile")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		panic(err)
	}
	if err := disableBrowserProfileSessionRestore(profileDir); err != nil {
		panic(fmt.Errorf("disable browser session restore: %w", err))
	}
	if err := clearBrowserProfileWindowSession(profileDir); err != nil {
		panic(fmt.Errorf("clear browser window session: %w", err))
	}

	port, err := accountDebugPort(cwd)
	if err != nil {
		panic(err)
	}

	Infof("Opening browser for %s", accountName)

	u := launcher.NewUserMode().
		Leakless(true).
		NoSandbox(true).
		Headless(false).
		Devtools(false).
		UserDataDir(profileDir).
		Set("remote-debugging-port", fmt.Sprintf("%d", port)).
		Set("disable-notifications").
		Set("disable-background-timer-throttling").
		Set("disable-backgrounding-occluded-windows").
		Set("disable-renderer-backgrounding").
		MustLaunch()

	browser := rod.New().ControlURL(u).MustConnect().NoDefaultDevice()
	activeBrowserMu.Lock()
	activeBrowser = browser
	activeBrowserMu.Unlock()

	page := browser.MustPage("about:blank").MustWindowMaximize()
	closeRestoredAutomationPages(browser, page)

	//time.Sleep(5000 * time.Minute)

	return browser, page
}

// disableBrowserProfileSessionRestore prevents Chrome from reopening every
// window recorded in the persistent automation profile. Cookies and login
// state remain in the profile; only the startup behavior is changed.
func disableBrowserProfileSessionRestore(profileDir string) error {
	preferencesPath := filepath.Join(profileDir, "Default", "Preferences")
	body, err := os.ReadFile(preferencesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var preferences map[string]any
	if err := json.Unmarshal(body, &preferences); err != nil {
		return fmt.Errorf("decode %s: %w", preferencesPath, err)
	}
	session, ok := preferences["session"].(map[string]any)
	if !ok {
		session = make(map[string]any)
		preferences["session"] = session
	}
	session["restore_on_startup"] = 5
	profile, ok := preferences["profile"].(map[string]any)
	if !ok {
		profile = make(map[string]any)
		preferences["profile"] = profile
	}
	profile["exit_type"] = "Normal"
	profile["exited_cleanly"] = true

	updated, err := json.Marshal(preferences)
	if err != nil {
		return fmt.Errorf("encode %s: %w", preferencesPath, err)
	}
	info, err := os.Stat(preferencesPath)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(preferencesPath), ".preferences-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(updated); err != nil {
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
	if err := os.Rename(temporaryPath, preferencesPath); err != nil {
		return fmt.Errorf("replace %s: %w", preferencesPath, err)
	}
	return nil
}

// clearBrowserProfileWindowSession removes only Chrome's saved tabs/windows.
// Authentication lives in Cookies, Login Data, IndexedDB, and Local Storage,
// none of which are touched here.
func clearBrowserProfileWindowSession(profileDir string) error {
	defaultDir := filepath.Join(profileDir, "Default")
	targets := []string{
		filepath.Join(defaultDir, "Sessions"),
		filepath.Join(defaultDir, "Current Session"),
		filepath.Join(defaultDir, "Current Tabs"),
		filepath.Join(defaultDir, "Last Session"),
		filepath.Join(defaultDir, "Last Tabs"),
	}
	for _, target := range targets {
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("remove %s: %w", target, err)
		}
	}
	return nil
}

func closeRestoredAutomationPages(browser *rod.Browser, keep *rod.Page) {
	pages, err := browser.Timeout(browserCleanupTimeout).Pages()
	if err != nil {
		Warnf("Unable to inspect restored automation pages: %v", err)
		return
	}
	closed := 0
	for _, page := range pages {
		if page.TargetID == keep.TargetID {
			continue
		}
		if err := page.Timeout(browserCleanupTimeout).Close(); err != nil {
			Warnf("Unable to close restored automation page: %v", err)
			continue
		}
		closed++
	}
	if closed > 0 {
		Infof("Closed %d restored automation window(s) before starting the Rod pool", closed)
	}
}

func closeBrowserPagesAndBrowser(browser *rod.Browser) {
	if browser == nil {
		return
	}

	pages, err := browser.Timeout(browserCleanupTimeout).Pages()
	if err != nil {
		Warnf("Unable to list browser pages during cleanup: %v", err)
	} else {
		for _, page := range pages {
			if err := page.Timeout(browserCleanupTimeout).Close(); err != nil {
				Warnf("Unable to close browser page during cleanup: %v", err)
			}
		}
	}

	if err := browser.Timeout(browserCleanupTimeout).Close(); err != nil {
		Warnf("Unable to close browser during cleanup: %v", err)
	}
	activeBrowserMu.Lock()
	if activeBrowser == browser {
		activeBrowser = nil
	}
	activeBrowserMu.Unlock()
}

func closeActiveBrowser() {
	activeBrowserMu.Lock()
	browser := activeBrowser
	activeBrowser = nil
	activeBrowserMu.Unlock()
	closeBrowserPagesAndBrowser(browser)
}

func accountDebugPort(accountPath string) (int, error) {
	const (
		minPort = 9222
		maxPort = 65535
	)

	portRange := maxPort - minPort + 1
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(accountPath))
	port := minPort + int(hash.Sum32()%uint32(portRange))
	if !portAvailable(port) {
		return 0, fmt.Errorf(
			"browser debugging port %d is already in use; another extractor instance or automated Chrome session is still running",
			port,
		)
	}
	return port, nil
}

func portAvailable(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	return listener.Close() == nil
}
