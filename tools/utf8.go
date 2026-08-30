package tools

import "unicode/utf8"

// UTF-8 safe byte truncation. Tool results are capped in bytes (32KB command
// output, 64KB file reads, 100KB line ranges) so a single oversized result can
// never blow the context budget. A plain byte slice (s[:max]) can cut a
// multi-byte rune in half — the model would then read U+FFFD at every
// truncation boundary of a Chinese/Japanese/Korean or emoji text. Both
// helpers back the cut off to a rune boundary instead, moving it by at most
// utf8.UTFMax-1 bytes, so the byte cap still holds (within 3 bytes) and the
// returned text is always valid UTF-8.

// truncateUTF8Prefix returns the longest prefix of s that is at most maxBytes
// long and ends on a rune boundary. It returns s unchanged when it already
// fits, and the empty string for a non-positive maxBytes.
func truncateUTF8Prefix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	// Back the cut off while it lands inside a multi-byte rune: the byte at
	// the cut is then a continuation byte (10xxxxxx), not a rune start.
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// truncateUTF8Suffix returns the longest suffix of s that is at most maxBytes
// long and starts on a rune boundary — the counterpart of truncateUTF8Prefix
// for truncations that keep the tail of a stream. It returns "" for a
// non-positive maxBytes.
func truncateUTF8Suffix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	// Advance the start while it lands inside a multi-byte rune.
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
