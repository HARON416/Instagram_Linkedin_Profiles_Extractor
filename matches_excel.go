package main

import (
	"fmt"

	"github.com/xuri/excelize/v2"
)

const (
	matchesWorkbookPath = "profile_matches.xlsx"
	matchesSheet        = "AI Matches"
	runDetailsSheet     = "Run Details"
)

var matchHeaders = []interface{}{
	"NAME",
	"LINKEDIN PROFILE",
	"INSTAGRAM PROFILE",
	"SCORE",
	"RESULT",
	"WHY",
	"NEEDS REVIEW",
}

var runDetailsHeaders = []interface{}{
	"NAME",
	"STATUS",
	"GEMINI MODEL",
	"ANALYZED AT",
	"ANALYSIS ERROR",
}

func writeMatchWorkbook(path string, document candidateDocument) error {
	file := excelize.NewFile()
	defer file.Close()
	defaultSheet := file.GetSheetName(0)
	matchesIndex, err := file.NewSheet(matchesSheet)
	if err != nil {
		return err
	}
	file.SetActiveSheet(matchesIndex)
	if err := file.DeleteSheet(defaultSheet); err != nil {
		return err
	}
	if _, err := file.NewSheet(runDetailsSheet); err != nil {
		return err
	}
	if err := file.SetSheetRow(matchesSheet, "A1", &matchHeaders); err != nil {
		return err
	}
	if err := file.SetSheetRow(runDetailsSheet, "A1", &runDetailsHeaders); err != nil {
		return err
	}

	headerStyle, err := file.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Size: 12},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"#E0E0E0"}, Pattern: 1},
		Border: []excelize.Border{
			{Type: "bottom", Color: "#000000", Style: 1},
		},
	})
	if err != nil {
		return err
	}
	percentageStyle, err := file.NewStyle(&excelize.Style{NumFmt: 9})
	if err != nil {
		return err
	}
	for _, sheet := range []string{matchesSheet, runDetailsSheet} {
		// A row style fills the header through Excel's final column instead of
		// ending at the final populated header cell.
		if err := file.SetRowStyle(sheet, 1, 1, headerStyle); err != nil {
			return err
		}
	}

	for index, person := range document.People {
		row := index + 2
		linkedInURL := firstLinkedInURL(person)
		bestScore := any("")
		if person.BestMatchScore != nil {
			bestScore = float64(*person.BestMatchScore) / 100
		}

		matchValues := []interface{}{
			person.FullName,
			linkedInURL,
			person.BestInstagramURL,
			bestScore,
			friendlyMatchDecision(person.MatchDecision),
			person.MatchSummary,
			yesNo(person.ManualReview),
		}
		if err := file.SetSheetRow(matchesSheet, fmt.Sprintf("A%d", row), &matchValues); err != nil {
			return err
		}
		if err := addBlankOverflowBlockers(file, matchesSheet, row, matchValues); err != nil {
			return err
		}
		for column, target := range map[string]string{
			"B": linkedInURL,
			"C": person.BestInstagramURL,
		} {
			if target == "" {
				continue
			}
			if err := file.SetCellHyperLink(matchesSheet, fmt.Sprintf("%s%d", column, row), target, "External"); err != nil {
				return err
			}
		}

		analyzedAt := ""
		if person.AnalyzedAt != nil {
			analyzedAt = person.AnalyzedAt.Format("2006-01-02 15:04:05 MST")
		}
		runDetails := []interface{}{
			person.FullName,
			friendlyMatchStatus(person.MatchStatus),
			person.GeminiModel,
			analyzedAt,
			person.GeminiError,
		}
		if err := file.SetSheetRow(runDetailsSheet, fmt.Sprintf("A%d", row), &runDetails); err != nil {
			return err
		}
		if err := addBlankOverflowBlockers(file, runDetailsSheet, row, runDetails); err != nil {
			return err
		}
	}
	if len(document.People) > 0 {
		if err := file.SetCellStyle(matchesSheet, "D2", fmt.Sprintf("D%d", len(document.People)+1), percentageStyle); err != nil {
			return err
		}
	}

	for _, width := range []struct {
		sheet, start, end string
		value             float64
	}{
		{matchesSheet, "A", "A", 24},
		{matchesSheet, "B", "C", 38},
		{matchesSheet, "D", "D", 18},
		{matchesSheet, "E", "E", 18},
		{matchesSheet, "F", "F", 52},
		{matchesSheet, "G", "G", 16},
		{runDetailsSheet, "A", "A", 24},
		{runDetailsSheet, "B", "B", 30},
		{runDetailsSheet, "C", "C", 22},
		{runDetailsSheet, "D", "D", 28},
		{runDetailsSheet, "E", "E", 52},
	} {
		if err := file.SetColWidth(width.sheet, width.start, width.end, width.value); err != nil {
			return err
		}
	}
	return file.SaveAs(path)
}

func addBlankOverflowBlockers(file *excelize.File, sheet string, row int, values []interface{}) error {
	for index, value := range values {
		if fmt.Sprint(value) != "" {
			continue
		}
		cell, err := excelize.CoordinatesToCellName(index+1, row)
		if err != nil {
			return err
		}
		// XLSX has no native "clip text" setting. A formula returning an empty
		// string remains visually blank but occupies the cell, preventing text
		// in the cell to its left from overflowing into it.
		if err := file.SetCellFormula(sheet, cell, `=""`); err != nil {
			return err
		}
	}
	return nil
}

func firstLinkedInURL(person personCandidates) string {
	if len(person.LinkedInCandidates) == 0 {
		return ""
	}
	return person.LinkedInCandidates[0].URL
}

func friendlyMatchDecision(value string) string {
	switch value {
	case "likely_match":
		return "Likely Match"
	case "possible_match":
		return "Possible Match"
	case "no_match_selected":
		return "No Match"
	case "unlikely_match":
		return "Unlikely Match"
	case "insufficient_evidence":
		return "Insufficient Evidence"
	case "baseline_unverified":
		return "LinkedIn Not Verified"
	default:
		return value
	}
}

func friendlyMatchStatus(value string) string {
	switch value {
	case "complete":
		return "Complete"
	case "error":
		return "Error"
	case "skipped_no_linkedin_baseline":
		return "Skipped: No LinkedIn Profile"
	case "skipped_no_instagram_data":
		return "Skipped: No Instagram Data"
	default:
		return value
	}
}

func yesNo(value bool) string {
	if value {
		return "Yes"
	}
	return "No"
}
