package helperproto

import (
	"strings"
	"time"
)

// Pinned by the contract owner at launch 14978a13. Unlike RFC3339Nano,
// this layout retains zeroes and always writes exactly nine fractional digits.
const scanTimeLayout = "2006-01-02T15:04:05.000000000Z"

func validScanTime(value string) bool {
	if len(value) != len(scanTimeLayout) {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && t.UTC().Format(scanTimeLayout) == value
}

// formatScanTime accepts standard UTC RFC 3339 input for the writer to pad.
// Fixed-position checks reject Go's lenient comma and one-digit-hour forms.
// No raw parse error or rejected timestamp escapes this metadata boundary.
func formatScanTime(value string) (string, error) {
	if len(value) < 20 || len(value) > 30 || !strings.HasSuffix(value, "Z") {
		return "", ErrInvalidScanRecord
	}
	if value[4] != '-' || value[7] != '-' || value[10] != 'T' || value[13] != ':' || value[16] != ':' || (len(value) > 20 && value[19] != '.') {
		return "", ErrInvalidScanRecord
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", ErrInvalidScanRecord
	}
	return t.UTC().Format(scanTimeLayout), nil
}
