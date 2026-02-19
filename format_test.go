package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name  string
		bytes int64
		want  string
	}{
		{"zero", 0, "0 B"},
		{"bytes", 512, "512 B"},
		{"kilobytes", 1536, "1.5 KB"},
		{"megabytes", 5242880, "5.0 MB"},
		{"gigabytes", 1610612736, "1.5 GB"},
		{"terabytes", 1099511627776, "1.0 TB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatSize(tt.bytes))
		})
	}
}

func TestFormatTime(t *testing.T) {
	now := time.Now()
	sameYear := time.Date(now.Year(), time.March, 15, 10, 30, 0, 0, time.UTC)
	diffYear := time.Date(2020, time.December, 25, 8, 0, 0, 0, time.UTC)

	t.Run("same year", func(t *testing.T) {
		result := formatTime(sameYear)
		assert.Contains(t, result, "Mar")
		assert.Contains(t, result, "15")
		assert.Contains(t, result, "10:30")
	})

	t.Run("different year", func(t *testing.T) {
		result := formatTime(diffYear)
		assert.Contains(t, result, "Dec")
		assert.Contains(t, result, "25")
		assert.Contains(t, result, "2020")
	})
}

func TestPrintTable(t *testing.T) {
	var buf bytes.Buffer

	headers := []string{"NAME", "SIZE", "MODIFIED"}
	rows := [][]string{
		{"file.txt", "1.2 MB", "Jan 15 10:30"},
		{"folder/", "0 B", "Feb  1 09:00"},
	}

	printTable(&buf, headers, rows)
	output := buf.String()

	assert.Contains(t, output, "NAME")
	assert.Contains(t, output, "SIZE")
	assert.Contains(t, output, "MODIFIED")
	assert.Contains(t, output, "file.txt")
	assert.Contains(t, output, "folder/")
}
