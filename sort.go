package main

import (
	"strings"

	"github.com/pyed/rtapi"
)

// sort changes torrents sorting
func sort(tokens []string) {
	if len(tokens) == 0 {
		send(`sort takes one of:
			(*name, downrate, uprate, size, ratio, age, upload*)
			optionally start with (*rev*) for reversed order
			e.g. "*sort rev size*" to get biggest torrents first.`, true)
		return
	}

	var reversed bool
	if strings.ToLower(tokens[0]) == "rev" {
		reversed = true
		tokens = tokens[1:]
	}

	switch strings.ToLower(tokens[0]) {
	case "name":
		if reversed {
			rtapi.CurrentSorting = rtapi.ByNameRev
			send("sort: by `reversed name`", true)
			break
		}
		rtapi.CurrentSorting = rtapi.ByName
		send("sort: by `name`", true)

	case "downrate":
		if reversed {
			rtapi.CurrentSorting = rtapi.ByDownRateRev
			send("sort: by `reversed down rate`", true)
			break
		}
		rtapi.CurrentSorting = rtapi.ByDownRate
		send("sort: by `down rate`", true)

	case "uprate":
		if reversed {
			rtapi.CurrentSorting = rtapi.ByUpRateRev
			send("sort: by `reversed up rate`", true)
			break
		}
		rtapi.CurrentSorting = rtapi.ByUpRate
		send("sort: by `up rate`", true)
	case "size":
		if reversed {
			rtapi.CurrentSorting = rtapi.BySizeRev
			send("sort: by `reversed size`", true)
			break
		}
		rtapi.CurrentSorting = rtapi.BySize
		send("sort: by `size`", true)
	case "ratio":
		if reversed {
			rtapi.CurrentSorting = rtapi.ByRatioRev
			send("sort: by `reversed ratio`", true)
			break
		}
		rtapi.CurrentSorting = rtapi.ByRatio
		send("sort: by `ratio`", true)

	case "age":
		if reversed {
			rtapi.CurrentSorting = rtapi.ByAgeRev
			send("sort: by `reversed age`", true)
			break
		}
		rtapi.CurrentSorting = rtapi.ByAge
		send("sort: by `age`", true)
	case "upload":
		if reversed {
			rtapi.CurrentSorting = rtapi.ByUpTotalRev
			send("sort: by `reversed up total`", true)
			break
		}
		rtapi.CurrentSorting = rtapi.ByUpTotalRev
		send("sort: by `up total`", true)
	default:
		send("unkown sorting method", false)
		return
	}
}
