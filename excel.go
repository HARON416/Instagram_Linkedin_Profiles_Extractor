package main

import (
	"fmt"
	"time"

	"github.com/xuri/excelize/v2"
)

const (
	workbookPath      = "profiles.xlsx"
	profilesSheet     = "Profiles"
	bioLinksSheet     = "Instagram Bio Links"
	firstDataRow      = 2
	linkedinURLCol    = "R"
	lastUpdatedCol    = "S"
	instagramURLCol   = "B"
	profilePictureCol = "O"
	externalURLCol    = "P"
)

var profileHeaders = []interface{}{
	"Search Name",
	"Instagram URL",
	"Instagram ID",
	"Instagram Username",
	"Instagram Full Name",
	"Instagram Biography",
	"Instagram Category",
	"Instagram Followers",
	"Instagram Following",
	"Instagram Posts",
	"Instagram Private",
	"Instagram Verified",
	"Instagram Business",
	"Instagram Account Type",
	"Instagram Profile Picture",
	"Instagram External URL",
	"Instagram Threads Username",
	"LinkedIn URL",
	"Updated At",
}

var bioLinkHeaders = []interface{}{
	"Search Name",
	"Instagram Username",
	"Title",
	"URL",
	"Link Type",
	"Pinned",
}

type profileWorkbook struct {
	file        *excelize.File
	path        string
	names       []string
	nextBioRow  int
	headerStyle int
}

func newProfileWorkbook(path string, names []string) (*profileWorkbook, error) {
	file := excelize.NewFile()
	store := &profileWorkbook{
		file:       file,
		path:       path,
		names:      append([]string(nil), names...),
		nextBioRow: firstDataRow,
	}

	if err := file.SetSheetName(file.GetSheetName(0), profilesSheet); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.NewSheet(bioLinksSheet); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := store.initializeStyles(); err != nil {
		_ = file.Close()
		return nil, err
	}

	if err := store.initializeSheets(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.SaveAs(path); err != nil {
		_ = file.Close()
		return nil, err
	}

	return store, nil
}

func (store *profileWorkbook) initializeStyles() error {
	style, err := store.file.NewStyle(&excelize.Style{
		Font: &excelize.Font{
			Bold:   true,
			Family: "Cambria",
			Size:   12,
		},
		Fill: excelize.Fill{
			Type:    "pattern",
			Pattern: 1,
			Color:   []string{"D9D9D9"},
		},
		Border: []excelize.Border{
			{Type: "bottom", Color: "A6A6A6", Style: 1},
		},
		Alignment: &excelize.Alignment{
			Horizontal: "left",
			Vertical:   "center",
		},
	})
	if err != nil {
		return err
	}
	store.headerStyle = style
	return nil
}

func (store *profileWorkbook) initializeSheets() error {
	if err := store.file.SetSheetRow(profilesSheet, "A1", &profileHeaders); err != nil {
		return err
	}
	if err := store.file.SetSheetRow(bioLinksSheet, "A1", &bioLinkHeaders); err != nil {
		return err
	}
	// Apply the style to the row itself so the header background continues
	// through Excel's final column, rather than stopping at the last header.
	if err := store.file.SetRowStyle(profilesSheet, 1, 1, store.headerStyle); err != nil {
		return err
	}
	if err := store.file.SetRowStyle(bioLinksSheet, 1, 1, store.headerStyle); err != nil {
		return err
	}
	if err := store.file.SetRowHeight(profilesSheet, 1, 22); err != nil {
		return err
	}
	if err := store.file.SetRowHeight(bioLinksSheet, 1, 22); err != nil {
		return err
	}
	for index, name := range store.names {
		rowNumber := firstDataRow + index
		cell := fmt.Sprintf("A%d", rowNumber)
		row := []interface{}{name}
		if err := store.file.SetSheetRow(profilesSheet, cell, &row); err != nil {
			return err
		}
	}

	for _, width := range []struct {
		sheet, start, end string
		value             float64
	}{
		{profilesSheet, "A", "A", 22},
		{profilesSheet, "B", "B", 36},
		{profilesSheet, "C", "C", 18},
		{profilesSheet, "D", "D", 24},
		{profilesSheet, "E", "E", 26},
		{profilesSheet, "F", "F", 48},
		{profilesSheet, "G", "G", 22},
		{profilesSheet, "H", "N", 18},
		{profilesSheet, "O", "P", 36},
		{profilesSheet, "Q", "Q", 26},
		{profilesSheet, "R", "R", 36},
		{profilesSheet, "S", "S", 28},
		{bioLinksSheet, "A", "A", 22},
		{bioLinksSheet, "B", "C", 24},
		{bioLinksSheet, "D", "D", 44},
		{bioLinksSheet, "E", "E", 18},
		{bioLinksSheet, "F", "F", 12},
	} {
		if err := store.file.SetColWidth(width.sheet, width.start, width.end, width.value); err != nil {
			return err
		}
	}

	for _, sheet := range []string{profilesSheet, bioLinksSheet} {
		if err := store.file.SetPanes(sheet, &excelize.Panes{
			Freeze:      true,
			YSplit:      1,
			TopLeftCell: "A2",
			ActivePane:  "bottomLeft",
		}); err != nil {
			return err
		}
	}

	return nil
}

func (store *profileWorkbook) setProfileURL(index int, site, profileURL string) error {
	column := linkedinURLCol
	if site == "instagram.com" {
		column = instagramURLCol
	}

	row, err := store.profileRow(index)
	if err != nil {
		return err
	}
	cell := fmt.Sprintf("%s%d", column, row)
	if err := store.setHyperlink(profilesSheet, cell, profileURL); err != nil {
		return err
	}
	if err := store.setUpdatedAt(row); err != nil {
		return err
	}
	return store.file.Save()
}

func (store *profileWorkbook) setInstagramData(index int, profile *instagramProfile) error {
	row, err := store.profileRow(index)
	if err != nil {
		return err
	}

	values := []interface{}{
		profile.identifier(),
		profile.Username,
		profile.FullName,
		profile.Biography,
		profile.Category,
		profile.FollowerCount,
		profile.FollowingCount,
		profile.MediaCount,
		profile.IsPrivate,
		profile.IsVerified,
		profile.IsBusiness,
		profile.AccountType,
		profile.profilePictureURL(),
		profile.ExternalURL,
		profile.ThreadsUsername,
	}
	if err := store.file.SetSheetRow(profilesSheet, fmt.Sprintf("C%d", row), &values); err != nil {
		return err
	}

	for _, link := range []struct {
		column string
		url    string
	}{
		{profilePictureCol, profile.profilePictureURL()},
		{externalURLCol, profile.ExternalURL},
	} {
		if link.url != "" {
			if err := store.setHyperlink(profilesSheet, fmt.Sprintf("%s%d", link.column, row), link.url); err != nil {
				return err
			}
		}
	}

	for _, link := range profile.BioLinks {
		bioRowNumber := store.nextBioRow
		bioRow := []interface{}{
			store.names[index],
			profile.Username,
			link.Title,
			link.URL,
			link.LinkType,
			link.IsPinned,
		}
		if err := store.file.SetSheetRow(bioLinksSheet, fmt.Sprintf("A%d", bioRowNumber), &bioRow); err != nil {
			return err
		}
		if link.URL != "" {
			if err := store.setHyperlink(bioLinksSheet, fmt.Sprintf("D%d", bioRowNumber), link.URL); err != nil {
				return err
			}
		}
		store.nextBioRow++
	}
	if err := store.setUpdatedAt(row); err != nil {
		return err
	}
	return store.file.Save()
}

func (store *profileWorkbook) setHyperlink(sheet, cell, target string) error {
	if err := store.file.SetCellValue(sheet, cell, target); err != nil {
		return err
	}
	if err := store.file.SetCellHyperLink(sheet, cell, target, "External"); err != nil {
		return err
	}
	return nil
}

func (store *profileWorkbook) setUpdatedAt(row int) error {
	return store.file.SetCellValue(
		profilesSheet,
		fmt.Sprintf("%s%d", lastUpdatedCol, row),
		time.Now().Format(time.RFC3339),
	)
}

func (store *profileWorkbook) profileRow(index int) (int, error) {
	if index < 0 || index >= len(store.names) {
		return 0, fmt.Errorf("profile index %d is out of range", index)
	}
	return firstDataRow + index, nil
}

func (store *profileWorkbook) close() error {
	return store.file.Close()
}
