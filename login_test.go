package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDisableBrowserProfileSessionRestorePreservesOtherPreferences(t *testing.T) {
	profileDir := t.TempDir()
	defaultDir := filepath.Join(profileDir, "Default")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	preferencesPath := filepath.Join(defaultDir, "Preferences")
	if err := os.WriteFile(preferencesPath, []byte(`{
		"session":{"restore_on_startup":1,"startup_urls":["https://example.com"]},
		"unrelated":{"preserve_me":true}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := disableBrowserProfileSessionRestore(profileDir); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(preferencesPath)
	if err != nil {
		t.Fatal(err)
	}
	var preferences map[string]any
	if err := json.Unmarshal(body, &preferences); err != nil {
		t.Fatal(err)
	}
	session := preferences["session"].(map[string]any)
	if got := session["restore_on_startup"]; got != float64(5) {
		t.Fatalf("restore_on_startup = %#v, want 5", got)
	}
	if got := preferences["unrelated"].(map[string]any)["preserve_me"]; got != true {
		t.Fatalf("unrelated preference changed: %#v", got)
	}
	profile := preferences["profile"].(map[string]any)
	if got := profile["exit_type"]; got != "Normal" {
		t.Fatalf("exit_type = %#v, want Normal", got)
	}
	if got := profile["exited_cleanly"]; got != true {
		t.Fatalf("exited_cleanly = %#v, want true", got)
	}
}

func TestClearBrowserProfileWindowSessionPreservesAuthenticationData(t *testing.T) {
	profileDir := t.TempDir()
	defaultDir := filepath.Join(profileDir, "Default")
	sessionsDir := filepath.Join(defaultDir, "Sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "Session_1"), []byte("70 restored windows"), 0o600); err != nil {
		t.Fatal(err)
	}
	cookiesPath := filepath.Join(defaultDir, "Cookies")
	if err := os.WriteFile(cookiesPath, []byte("authentication data"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := clearBrowserProfileWindowSession(profileDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionsDir); !os.IsNotExist(err) {
		t.Fatalf("Sessions directory still exists or returned unexpected error: %v", err)
	}
	body, err := os.ReadFile(cookiesPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "authentication data" {
		t.Fatalf("Cookies data = %q", got)
	}
}
