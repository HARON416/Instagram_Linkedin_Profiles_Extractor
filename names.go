package main

import (
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

const (
	firstNameHeader = "first name"
	lastNameHeader  = "last name"
)

type nameRecord struct {
	SourceRow int
	FirstName string
	LastName  string
	FullName  string
}

func readNameRecords(path string) ([]nameRecord, error) {
	workbook, err := excelize.OpenFile(path)
	if err != nil {
		return nil, err
	}
	defer workbook.Close()

	sheet, rows, firstNameColumn, lastNameColumn, err := findNameColumns(workbook)
	if err != nil {
		return nil, err
	}

	records := make([]nameRecord, 0, len(rows)-1)
	for rowIndex, row := range rows[1:] {
		firstName := cellValue(row, firstNameColumn)
		lastName := cellValue(row, lastNameColumn)
		fullName := strings.Join(strings.Fields(firstName+" "+lastName), " ")
		if fullName == "" {
			continue
		}
		records = append(records, nameRecord{
			SourceRow: rowIndex + 2,
			FirstName: firstName,
			LastName:  lastName,
			FullName:  fullName,
		})
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("sheet %q contains no names", sheet)
	}
	return records, nil
}

func readNames(path string) ([]string, error) {
	records, err := readNameRecords(path)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.FullName)
	}
	return names, nil
}

func findNameColumns(workbook *excelize.File) (string, [][]string, int, int, error) {
	for _, sheet := range workbook.GetSheetList() {
		rows, err := workbook.GetRows(sheet)
		if err != nil {
			return "", nil, 0, 0, fmt.Errorf("read sheet %q: %w", sheet, err)
		}
		if len(rows) == 0 {
			continue
		}

		firstNameColumn := -1
		lastNameColumn := -1
		for column, value := range rows[0] {
			switch normalizeNameHeader(value) {
			case firstNameHeader:
				firstNameColumn = column
			case lastNameHeader:
				lastNameColumn = column
			}
		}
		if firstNameColumn >= 0 && lastNameColumn >= 0 {
			return sheet, rows, firstNameColumn, lastNameColumn, nil
		}
	}

	return "", nil, 0, 0, fmt.Errorf(
		"no sheet contains both %q and %q columns",
		"First Name",
		"Last Name",
	)
}

func normalizeNameHeader(value string) string {
	value = strings.TrimPrefix(value, "\ufeff")
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func cellValue(row []string, column int) string {
	if column < 0 || column >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[column])
}
