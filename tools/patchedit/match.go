package patchedit

import (
	"bytes"
	"sort"
	"strings"
	"unicode/utf8"
)

func findAllMatches(content []byte, oldText string) []MatchLoc {
	lt := buildLineTable(content)
	if locs := exactMatch(content, oldText, lt); len(locs) > 0 {
		return locs
	}
	if locs := wsFuzzyMatch(content, oldText, lt); len(locs) > 0 {
		return locs
	}
	if locs := lineFuzzyMatch(content, oldText); len(locs) > 0 {
		return locs
	}
	return subFuzzyMatch(content, oldText, lt)
}

func exactMatch(content []byte, oldText string, lt []int) []MatchLoc {
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
		ls, le := lineOffsetsToLines(lt, abs, abs+len(search))
		locs = append(locs, MatchLoc{Offset: abs, EndOffset: abs + len(search), LineStart: ls, LineEnd: le, Strategy: "exact"})
		idx = abs + 1
	}
	return locs
}

func wsFuzzyMatch(content []byte, oldText string, lt []int) []MatchLoc {
	// Build normalized string + orig→norm mapping in single pass
	var normBuf bytes.Buffer
	inWS := false
	origForNorm := make([]int, 0)
	for i, r := range string(content) {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !inWS && normBuf.Len() > 0 {
				normBuf.WriteByte(' ')
				origForNorm = append(origForNorm, i)
				inWS = true
			}
		} else {
			normBuf.WriteRune(r)
			origForNorm = append(origForNorm, i)
			inWS = false
		}
	}
	normContent := strings.TrimSpace(normBuf.String())
	// Trim trailing whitespace entries from origForNorm (leading WS never recorded by normBuf.Len()>0 guard)
	trailingTrim := normBuf.Len() - len(normContent)
	if trailingTrim > 0 && len(origForNorm) > trailingTrim {
		origForNorm = origForNorm[:len(origForNorm)-trailingTrim]
	}

	normOld := collapse(oldText)
	if normOld == "" || normContent == "" {
		return nil
	}

	// Build byte→rune index map: normBuf.WriteRune writes multi-byte UTF-8,
	// but origForNorm has one entry per rune. strings.Index returns byte positions.
	byteToRune := make([]int, len(normContent))
	ri := 0
	for bi := 0; bi < len(normContent); {
		_, sz := utf8.DecodeRuneInString(normContent[bi:])
		for j := range sz {
			byteToRune[bi+j] = ri
		}
		bi += sz
		ri++
	}

	origStr := string(content)

	var locs []MatchLoc
	off := 0
	for {
		idx := strings.Index(normContent[off:], normOld)
		if idx < 0 {
			break
		}
		abs := off + idx
		origOfs := origForNorm[byteToRune[abs]]
		endByte := abs + len(normOld) - 1
		if endByte >= len(byteToRune) {
			endByte = len(byteToRune) - 1
		}
		origEnd := origForNorm[byteToRune[endByte]]
		if origEnd < len(origStr) {
			origEnd++ // exclusive end byte
		} else {
			origEnd = len(origStr)
		}
		ls, le := lineOffsetsToLines(lt, origOfs, origEnd)
		locs = append(locs, MatchLoc{Offset: origOfs, EndOffset: origEnd, LineStart: ls, LineEnd: le, Strategy: "whitespace_fuzzy"})
		off = abs + len(normOld)
		if off >= len(normContent) {
			break
		}
	}
	return locs
}

func collapse(s string) string {
	var buf bytes.Buffer
	inWS := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !inWS {
				buf.WriteByte(' ')
				inWS = true
			}
		} else {
			buf.WriteRune(r)
			inWS = false
		}
	}
	return strings.TrimSpace(buf.String())
}

func lineFuzzyMatch(content []byte, oldText string) []MatchLoc {
	oldLines := strings.Split(oldText, "\n")
	var nonEmpty []string
	for _, l := range oldLines {
		t := strings.TrimRight(l, " \t\r")
		if t != "" {
			nonEmpty = append(nonEmpty, t)
		}
	}
	if len(nonEmpty) == 0 {
		return nil
	}

	lines := strings.SplitAfter(string(content), "\n")
	lineOfs := make([]int, len(lines))
	ofs := 0
	for i, l := range lines {
		lineOfs[i] = ofs
		ofs += len(l)
	}

	first := strings.TrimSpace(nonEmpty[0])
	var locs []MatchLoc
	for i, line := range lines {
		trimLine := strings.TrimSpace(line)
		if trimLine == "" || first == "" {
			continue
		}
		if !strings.Contains(trimLine, first) && !strings.Contains(first, trimLine) {
			continue
		}
		match := true
		for j := 1; j < len(nonEmpty) && (i+j) < len(lines); j++ {
			trimFile := strings.TrimSpace(lines[i+j])
			trimOld := strings.TrimSpace(nonEmpty[j])
			if trimFile == "" || trimOld == "" {
				match = false
				break
			}
			if !strings.Contains(trimFile, trimOld) && !strings.Contains(trimOld, trimFile) {
				match = false
				break
			}
		}
		if match && len(nonEmpty) <= len(lines)-i {
			le := i + len(nonEmpty) - 1
			if le >= len(lines) {
				le = len(lines) - 1
			}
			locs = append(locs, MatchLoc{Offset: lineOfs[i], EndOffset: lineOfs[le] + len(lines[le]), LineStart: i + 1, LineEnd: le + 1, Strategy: "line_fuzzy"})
		}
	}
	return locs
}

func subFuzzyMatch(content []byte, oldText string, lt []int) []MatchLoc {
	minLen := min(len(oldText), 20)
	longest := ""

	// Fast heuristic for long old_text: use longest non-empty trimmed line
	if len(oldText) > 200 {
		for line := range strings.SplitSeq(oldText, "\n") {
			t := strings.TrimSpace(line)
			if len(t) > len(longest) && bytes.Contains(content, []byte(t)) {
				longest = t
			}
		}
	} else {
		for start := 0; start < len(oldText); start++ {
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
	}
	if longest == "" {
		return nil
	}
	// Find all occurrences of the longest common substring
	needle := []byte(longest)
	var locs []MatchLoc
	off := 0
	for {
		idx := bytes.Index(content[off:], needle)
		if idx < 0 {
			break
		}
		abs := off + idx
		ls, le := lineOffsetsToLines(lt, abs, abs+len(longest))
		locs = append(locs, MatchLoc{Offset: abs, EndOffset: abs + len(longest), LineStart: ls, LineEnd: le, Strategy: "substring_fuzzy"})
		off = abs + 1
	}
	return locs
}

func buildLineTable(content []byte) []int {
	t := []int{0}
	for i, b := range content {
		if b == '\n' {
			t = append(t, i+1)
		}
	}
	return t
}

func lineOffsetsToLines(t []int, start, end int) (int, int) {
	ls := sort.SearchInts(t, start+1)
	le := sort.SearchInts(t, end+1)
	return ls, le + 1
}

type nearestMatch struct {
	Line    int
	Preview string
}

func findNearest(content []byte, oldText string) nearestMatch {
	searchText := oldText
	if strings.Contains(oldText, "\n") {
		for l := range strings.SplitSeq(oldText, "\n") {
			t := strings.TrimSpace(l)
			if t != "" {
				searchText = t
				break
			}
		}
	}
	lines := strings.Split(string(content), "\n")
	best := nearestMatch{}
	bestLen := 0
	for i, line := range lines {
		l := commonPrefixLen(line, searchText)
		if l > bestLen {
			bestLen = l
			best.Line = i + 1
			if len(line) > 80 {
				best.Preview = line[:80] + "..."
			} else {
				best.Preview = line
			}
		}
	}
	if best.Line == 0 {
		best.Line = 1
		if len(lines) > 0 && len(lines[0]) > 0 {
			best.Preview = "(no similar lines; first line: " + lines[0][:min(len(lines[0]), 60)] + ")"
		} else {
			best.Preview = "(empty file)"
		}
	}
	return best
}

func commonPrefixLen(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
