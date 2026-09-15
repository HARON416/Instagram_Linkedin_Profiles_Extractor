package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestProfileSearchQueryUsesFullNameAndSite(t *testing.T) {
	if got, want := profileSearchQuery("  John   Nyakawa Ondari "), `"John Nyakawa Ondari"`; got != want {
		t.Fatalf("query = %q, want %q", got, want)
	}
}

func TestProfileSearchPurposeIsPlatformSpecific(t *testing.T) {
	instagram := profileSearchPurpose("Stuart O'Donnell", "instagram.com")
	if !strings.Contains(instagram, "https://www.instagram.com/<username>/") || !strings.Contains(instagram, "may be private") {
		t.Fatalf("unexpected Instagram purpose: %q", instagram)
	}

	linkedIn := profileSearchPurpose("Stuart O'Donnell", "linkedin.com/in")
	if !strings.Contains(linkedIn, "https://www.linkedin.com/in/<profile-slug>/") {
		t.Fatalf("unexpected LinkedIn purpose: %q", linkedIn)
	}
}

func TestReadNamesFindsColumnsByHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.xlsx")
	workbook := excelize.NewFile()
	if err := workbook.SetSheetName(workbook.GetSheetName(0), "All Names"); err != nil {
		t.Fatal(err)
	}
	headers := []interface{}{"Last Name", "Ignored", " First   Name "}
	if err := workbook.SetSheetRow("All Names", "A1", &headers); err != nil {
		t.Fatal(err)
	}
	firstRow := []interface{}{"O'Donnell", "x", "  Stuart "}
	if err := workbook.SetSheetRow("All Names", "A2", &firstRow); err != nil {
		t.Fatal(err)
	}
	secondRow := []interface{}{"", "x", "Madonna"}
	if err := workbook.SetSheetRow("All Names", "A3", &secondRow); err != nil {
		t.Fatal(err)
	}
	if err := workbook.SaveAs(path); err != nil {
		t.Fatal(err)
	}
	if err := workbook.Close(); err != nil {
		t.Fatal(err)
	}

	names, err := readNames(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("read %d names, want 2", len(names))
	}
	if names[0] != "Stuart O'Donnell" {
		t.Fatalf("first name = %q, want %q", names[0], "Stuart O'Donnell")
	}
	if names[1] != "Madonna" {
		t.Fatalf("second name = %q, want %q", names[1], "Madonna")
	}

	records, err := readNameRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	if records[0].SourceRow != 2 || records[0].FirstName != "Stuart" || records[0].LastName != "O'Donnell" {
		t.Fatalf("unexpected first name record: %#v", records[0])
	}
}

func TestLinkedInProfileURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url  string
		want bool
	}{
		{url: "https://www.linkedin.com/in/john-nyakawa", want: true},
		{url: "https://ke.linkedin.com/in/john-nyakawa/", want: true},
		{url: "https://www.linkedin.com/company/example", want: false},
		{url: "https://example.com/in/john-nyakawa", want: false},
	}

	for _, test := range tests {
		if got := isLinkedInProfileURL(test.url); got != test.want {
			t.Errorf("isLinkedInProfileURL(%q) = %t, want %t", test.url, got, test.want)
		}
	}
}

func TestInstagramUsernameFromURL(t *testing.T) {
	got, err := instagramUsernameFromURL("https://www.instagram.com/John.Nyakawa/")
	if err != nil {
		t.Fatal(err)
	}
	if want := "John.Nyakawa"; got != want {
		t.Fatalf("username = %q, want %q", got, want)
	}
}

func TestInstagramProfileURLRejectsReels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want bool
	}{
		{
			name: "profile result",
			url:  "https://www.instagram.com/williamsamoeiruto/",
			want: true,
		},
		{
			name: "reel result",
			url:  "https://www.instagram.com/reel/DdGzlfAo6eQ/",
			want: false,
		},
		{
			name: "different site",
			url:  "https://example.com/williamsamoeiruto/",
			want: false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isInstagramProfileURL(test.url); got != test.want {
				t.Fatalf("isInstagramProfileURL(%q) = %t, want %t", test.url, got, test.want)
			}
		})
	}
}

func TestInstagramUsernameFromResponse(t *testing.T) {
	body := []byte(`{"data":{"user":{"username":"john.nyakawa"}}}`)
	got, err := instagramUsernameFromResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if want := "john.nyakawa"; got != want {
		t.Fatalf("username = %q, want %q", got, want)
	}
}

func TestProfileWorkbookStoresProfileData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.xlsx")
	store, err := newProfileWorkbook(path, []string{"John Nyakawa", "Jane Doe"})
	if err != nil {
		t.Fatal(err)
	}

	instagramURL := "https://www.instagram.com/john.nyakawa/"
	linkedinURL := "https://ke.linkedin.com/in/john-nyakawa/"
	profile := &instagramProfile{
		ID:                "123",
		Username:          "john.nyakawa",
		FullName:          "John Nyakawa",
		Biography:         "Test biography",
		FollowerCount:     100,
		FollowingCount:    25,
		MediaCount:        10,
		ProfilePictureURL: "https://example.com/picture.jpg",
		BioLinks: []instagramBioLink{
			{Title: "Website", URL: "https://example.com", LinkType: "external"},
		},
	}

	if err := store.setProfileURL(0, "instagram.com", instagramURL); err != nil {
		t.Fatal(err)
	}
	if err := store.setInstagramData(0, profile); err != nil {
		t.Fatal(err)
	}
	if err := store.setProfileURL(0, "linkedin.com/in", linkedinURL); err != nil {
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}

	workbook, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer workbook.Close()

	for cell, want := range map[string]string{
		"A2": "John Nyakawa",
		"B2": instagramURL,
		"C2": "123",
		"D2": "john.nyakawa",
		"R2": linkedinURL,
	} {
		got, err := workbook.GetCellValue(profilesSheet, cell)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", cell, got, want)
		}
	}

	linked, target, err := workbook.GetCellHyperLink(profilesSheet, "B2")
	if err != nil {
		t.Fatal(err)
	}
	if !linked || target != instagramURL {
		t.Fatalf("Instagram hyperlink = (%t, %q), want (true, %q)", linked, target, instagramURL)
	}

	gotBioLink, err := workbook.GetCellValue(bioLinksSheet, "D2")
	if err != nil {
		t.Fatal(err)
	}
	if gotBioLink != "https://example.com" {
		t.Fatalf("bio link = %q, want %q", gotBioLink, "https://example.com")
	}

	headerStyleID, err := workbook.GetCellStyle(profilesSheet, "A1")
	if err != nil {
		t.Fatal(err)
	}
	if headerStyleID == 0 {
		t.Fatal("expected a styled header")
	}
	headerStyle, err := workbook.GetStyle(headerStyleID)
	if err != nil {
		t.Fatal(err)
	}
	if headerStyle.Font == nil || !headerStyle.Font.Bold || headerStyle.Font.Family != "Cambria" || headerStyle.Font.Size != 12 {
		t.Fatalf("unexpected header font: %#v", headerStyle.Font)
	}
	if len(headerStyle.Fill.Color) == 0 || headerStyle.Fill.Color[0] != "D9D9D9" {
		t.Fatalf("unexpected header fill: %v", headerStyle.Fill.Color)
	}
	if len(headerStyle.Border) != 1 || headerStyle.Border[0].Type != "bottom" {
		t.Fatalf("unexpected header border: %#v", headerStyle.Border)
	}

	farRightHeaderStyleID, err := workbook.GetCellStyle(profilesSheet, "XFD1")
	if err != nil {
		t.Fatal(err)
	}
	if farRightHeaderStyleID != headerStyleID {
		t.Fatalf("far-right header style = %d, want %d", farRightHeaderStyleID, headerStyleID)
	}

	for _, test := range []struct {
		sheet string
		cell  string
	}{
		{profilesSheet, "A2"},
		{profilesSheet, "B2"},
		{profilesSheet, "S2"},
		{bioLinksSheet, "D2"},
	} {
		styleID, err := workbook.GetCellStyle(test.sheet, test.cell)
		if err != nil {
			t.Fatal(err)
		}
		if styleID != 0 {
			t.Fatalf("%s!%s style = %d, want default style 0", test.sheet, test.cell, styleID)
		}
	}

	height, err := workbook.GetRowHeight(profilesSheet, 1)
	if err != nil {
		t.Fatal(err)
	}
	if height != 22 {
		t.Fatalf("header row height = %v, want 22", height)
	}
}
