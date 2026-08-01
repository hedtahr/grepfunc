package patchedit

import (
	"bytes"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxSubstringWindow     = 20
	longSubstringThreshold = 200
	maxPreviewLineLen      = 80
	previewLineLen         = 60

	strategyExact           = "exact"
	strategyWhitespaceFuzzy = "whitespace_fuzzy"
	strategyLineFuzzy       = "line_fuzzy"
	strategySubstringFuzzy  = "substring_fuzzy"
	strategyInsert          = "insert"
)

func findAllMatches(content []byte, oldText string) []MatchLoc {
	lineTable := buildLineTable(content)
	if locs := exactMatch(content, oldText, lineTable); len(locs) > 0 {
		return locs
	}

	if locs := wsFuzzyMatch(content, oldText, lineTable); len(locs) > 0 {
		return locs
	}

	if locs := lineFuzzyMatch(content, oldText); len(locs) > 0 {
		return locs
	}

	return subFuzzyMatch(content, oldText, lineTable)
}

func exactMatch(content []byte, oldText string, lineTable []int) []MatchLoc {
	if oldText == "" {
		return nil
	}

	var locs []MatchLoc

	search := []byte(oldText)

	idx := 0

	for {
		ofs := bytes.Index(content[idx:], search)
		if ofs < 0 {
			break
		}

		abs := idx + ofs
		ls, le := lineOffsetsToLines(lineTable, abs, abs+len(search))
		locs = append(locs, MatchLoc{
			Offset: abs, EndOffset: abs + len(search),
			LineStart: ls, LineEnd: le, Strategy: strategyExact,
		})
		idx = abs + 1
	}

	return locs
}

func wsFuzzyMatch(content []byte, oldText string, lineTable []int) []MatchLoc {
	normContent, origForNorm := normalizeContent(content)

	normOld := collapse(oldText)
	if normOld == "" || normContent == "" {
		return nil
	}

	byteToRune := buildByteToRuneIndex(normContent)

	var locs []MatchLoc

	off := 0

	for {
		idx := strings.Index(normContent[off:], normOld)
		if idx < 0 {
			break
		}

		locs = append(locs, wsMatchLoc(content, lineTable, byteToRune, origForNorm, off+idx, len(normOld)))

		off += idx + len(normOld)
		if off >= len(normContent) {
			break
		}
	}

	return locs
}

func wsMatchLoc(content []byte, lineTable []int, byteToRune, origForNorm []int, abs, normLen int) MatchLoc {
	origOfs := origForNorm[byteToRune[abs]]

	endByte := abs + normLen - 1
	if endByte >= len(byteToRune) {
		endByte = len(byteToRune) - 1
	}

	origEnd := origForNorm[byteToRune[endByte]]
	if origEnd < len(content) {
		origEnd++ // exclusive end byte
	} else {
		origEnd = len(content)
	}

	ls, le := lineOffsetsToLines(lineTable, origOfs, origEnd)

	return MatchLoc{Offset: origOfs, EndOffset: origEnd, LineStart: ls, LineEnd: le, Strategy: strategyWhitespaceFuzzy}
}

// normalizeContent collapses runs of whitespace to single spaces and records the
// original byte offset of each normalized rune.
func normalizeContent(content []byte) (string, []int) {
	var normBuf bytes.Buffer

	origForNorm := make([]int, 0, len(content))
	inWS := false

	for byteIdx, char := range string(content) {
		if isWhitespace(char) {
			if !inWS && normBuf.Len() > 0 {
				normBuf.WriteByte(' ')

				origForNorm = append(origForNorm, byteIdx)
				inWS = true
			}
		} else {
			normBuf.WriteRune(char)

			origForNorm = append(origForNorm, byteIdx)
			inWS = false
		}
	}

	normContent := strings.TrimSpace(normBuf.String())
	trailingTrim := normBuf.Len() - len(normContent)

	// Trim trailing whitespace entries; leading whitespace was never recorded.
	if trailingTrim > 0 && len(origForNorm) > trailingTrim {
		origForNorm = origForNorm[:len(origForNorm)-trailingTrim]
	}

	return normContent, origForNorm
}

// buildByteToRuneIndex maps byte offsets in normContent to rune indexes into origForNorm.
func buildByteToRuneIndex(normContent string) []int {
	byteToRune := make([]int, len(normContent))
	runeIdx := 0

	for byteIdx := 0; byteIdx < len(normContent); {
		_, sz := utf8.DecodeRuneInString(normContent[byteIdx:])
		for j := range sz {
			byteToRune[byteIdx+j] = runeIdx
		}

		byteIdx += sz
		runeIdx++
	}

	return byteToRune
}

func isWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func collapse(s string) string {
	var buf bytes.Buffer

	inWS := false

	for _, char := range s {
		if isWhitespace(char) {
			if !inWS {
				buf.WriteByte(' ')

				inWS = true
			}
		} else {
			buf.WriteRune(char)

			inWS = false
		}
	}

	return strings.TrimSpace(buf.String())
}

func lineFuzzyMatch(content []byte, oldText string) []MatchLoc {
	nonEmpty := nonEmptyLines(oldText)
	if len(nonEmpty) == 0 {
		return nil
	}

	lines, lineOfs := splitLinesWithOffsets(content)
	first := strings.TrimSpace(nonEmpty[0])

	var locs []MatchLoc

	for i := range lines {
		if lineMatchesAt(lines, nonEmpty, i, first) {
			locs = append(locs, lineMatchLoc(lines, lineOfs, nonEmpty, i))
		}
	}

	return locs
}

func nonEmptyLines(text string) []string {
	var nonEmpty []string

	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}

	return nonEmpty
}

func splitLinesWithOffsets(content []byte) ([]string, []int) {
	lines := strings.SplitAfter(string(content), "\n")
	lineOfs := make([]int, len(lines))

	ofs := 0
	for i, l := range lines {
		lineOfs[i] = ofs
		ofs += len(l)
	}

	return lines, lineOfs
}

func lineMatchesAt(lines, nonEmpty []string, lineIdx int, first string) bool {
	trimLine := strings.TrimSpace(lines[lineIdx])
	if trimLine == "" || first == "" {
		return false
	}

	if !strings.Contains(trimLine, first) && !strings.Contains(first, trimLine) {
		return false
	}

	return restLinesMatch(lines, nonEmpty, lineIdx)
}

func restLinesMatch(lines, nonEmpty []string, lineIdx int) bool {
	for j := 1; j < len(nonEmpty) && (lineIdx+j) < len(lines); j++ {
		trimFile := strings.TrimSpace(lines[lineIdx+j])

		trimOld := strings.TrimSpace(nonEmpty[j])
		if trimFile == "" || trimOld == "" {
			return false
		}

		if !strings.Contains(trimFile, trimOld) && !strings.Contains(trimOld, trimFile) {
			return false
		}
	}

	return len(nonEmpty) <= len(lines)-lineIdx
}

func lineMatchLoc(lines []string, lineOfs []int, nonEmpty []string, lineIdx int) MatchLoc {
	lineEnd := lineIdx + len(nonEmpty) - 1
	if lineEnd >= len(lines) {
		lineEnd = len(lines) - 1
	}

	return MatchLoc{
		Offset: lineOfs[lineIdx], EndOffset: lineOfs[lineEnd] + len(lines[lineEnd]),
		LineStart: lineIdx + 1, LineEnd: lineEnd + 1, Strategy: strategyLineFuzzy,
	}
}

func subFuzzyMatch(content []byte, oldText string, lineTable []int) []MatchLoc {
	longest := longestCommonSubstring(content, oldText)
	if longest == "" {
		return nil
	}

	needle := []byte(longest)

	var locs []MatchLoc

	off := 0

	for {
		idx := bytes.Index(content[off:], needle)
		if idx < 0 {
			break
		}

		abs := off + idx
		ls, le := lineOffsetsToLines(lineTable, abs, abs+len(longest))
		locs = append(locs, MatchLoc{
			Offset: abs, EndOffset: abs + len(longest),
			LineStart: ls, LineEnd: le, Strategy: strategySubstringFuzzy,
		})
		off = abs + 1
	}

	return locs
}

func longestCommonSubstring(content []byte, oldText string) string {
	if len(oldText) > longSubstringThreshold {
		return longestCommonLine(content, oldText)
	}

	return longestCommonWindow(content, oldText, min(len(oldText), maxSubstringWindow))
}

func longestCommonLine(content []byte, oldText string) string {
	longest := ""

	for line := range strings.SplitSeq(oldText, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > len(longest) && bytes.Contains(content, []byte(trimmed)) {
			longest = trimmed
		}
	}

	return longest
}

func longestCommonWindow(content []byte, oldText string, minLen int) string {
	longest := ""

	for start := range len(oldText) {
		for end := start + minLen; end <= len(oldText); end++ {
			sub := oldText[start:end]
			if bytes.Contains(content, []byte(sub)) {
				if len(sub) > len(longest) {
					longest = sub
				}
			} else {
				break
			}
		}
	}

	return longest
}

func buildLineTable(content []byte) []int {
	lineStarts := []int{0}

	for byteIdx, b := range content {
		if b == '\n' {
			lineStarts = append(lineStarts, byteIdx+1)
		}
	}

	return lineStarts
}

func lineOffsetsToLines(lineStarts []int, start, end int) (int, int) {
	lineStart := sort.SearchInts(lineStarts, start+1)
	lineEnd := sort.SearchInts(lineStarts, end)

	return lineStart, lineEnd + 1
}

type nearestMatch struct {
	Line    int
	Preview string
}

func findNearest(content []byte, oldText string) nearestMatch {
	searchText := oldText

	if strings.Contains(oldText, "\n") {
		for l := range strings.SplitSeq(oldText, "\n") {
			trimmed := strings.TrimSpace(l)
			if trimmed != "" {
				searchText = trimmed

				break
			}
		}
	}

	lines := strings.Split(string(content), "\n")
	best := nearestMatch{Line: 0, Preview: ""}
	bestLen := 0

	for i, line := range lines {
		l := commonPrefixLen(line, searchText)
		if l > bestLen {
			bestLen = l

			best.Line = i + 1
			if len(line) > maxPreviewLineLen {
				best.Preview = line[:maxPreviewLineLen] + "..."
			} else {
				best.Preview = line
			}
		}
	}

	if best.Line == 0 {
		best.Line = 1
		if len(lines) > 0 && len(lines[0]) > 0 {
			best.Preview = "(no similar lines; first line: " + lines[0][:min(len(lines[0]), previewLineLen)] + ")"
		} else {
			best.Preview = "(empty file)"
		}
	}

	return best
}

func commonPrefixLen(a, b string) int {
	commonLen := min(len(a), len(b))
	for i := range commonLen {
		if a[i] != b[i] {
			return i
		}
	}

	return commonLen
}
