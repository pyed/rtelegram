package main

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// parseSize reads a size in binary units: 1024, 500K, 5M, 1.5G, 2T, with an
// optional trailing B or iB such as 5MB or 5MiB.
func parseSize(text string) (uint64, error) {
	number := strings.ToUpper(strings.TrimSpace(text))
	number = strings.TrimSuffix(number, "IB")
	number = strings.TrimSuffix(number, "B")
	multiplier := 1.0
	if number != "" {
		if shift := strings.IndexByte("KMGT", number[len(number)-1]); shift >= 0 {
			multiplier = math.Pow(1024, float64(shift+1))
			number = number[:len(number)-1]
		}
	}
	value, err := strconv.ParseFloat(number, 64)
	if err != nil || value < 0 || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, errors.New("sizes look like 500K, 5M, or 1.5G")
	}
	bytes := value * multiplier
	if bytes >= math.MaxUint64 {
		return 0, errors.New("size is too large")
	}
	return uint64(bytes), nil
}

// formatMinutes writes a duration to the minute: 5m, 1h30m, or 2h.
func formatMinutes(d time.Duration) string {
	minutes := int(d.Round(time.Minute) / time.Minute)
	switch {
	case minutes < 60:
		return strconv.Itoa(minutes) + "m"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "h"
	}
	return strconv.Itoa(minutes/60) + "h" + strconv.Itoa(minutes%60) + "m"
}
