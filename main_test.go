package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkMessageLongUTF8WithoutNewline(t *testing.T) {
	const repeats = telegramMessageLimit*2 + 123

	var builder strings.Builder
	builder.Grow(repeats * len("你"))
	for i := 0; i < repeats; i++ {
		builder.WriteString("你")
	}
	text := builder.String()

	chunks := chunkMessage(text, telegramMessageLimit)

	if len(chunks) == 0 {
		t.Fatalf("expected chunks, got none")
	}

	expectedChunks := (repeats + telegramMessageLimit - 1) / telegramMessageLimit
	if len(chunks) != expectedChunks {
		t.Fatalf("expected %d chunks, got %d", expectedChunks, len(chunks))
	}

	var combined strings.Builder
	totalRunes := 0
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid UTF-8", i)
		}

		runeCount := utf8.RuneCountInString(chunk)
		if runeCount == 0 {
			t.Fatalf("chunk %d was empty", i)
		}
		if runeCount > telegramMessageLimit {
			t.Fatalf("chunk %d exceeded limit: %d", i, runeCount)
		}
		if i < len(chunks)-1 && runeCount != telegramMessageLimit {
			t.Fatalf("chunk %d expected %d runes, got %d", i, telegramMessageLimit, runeCount)
		}

		combined.WriteString(chunk)
		totalRunes += runeCount
	}

	if totalRunes != repeats {
		t.Fatalf("expected %d runes total, got %d", repeats, totalRunes)
	}

	if combined.String() != text {
		t.Fatalf("combined chunks do not match original text")
	}
}
