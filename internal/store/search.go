package store

import "strings"

// Hit is one file found by Search.
type Hit struct {
	Name      string
	NameMatch bool
	Lines     []LineHit
	// More counts matching lines beyond the ones kept in Lines.
	More int
}

// LineHit is a matching line of a text file.
type LineHit struct {
	N    int
	Text string
}

// Search finds needle, ignoring case, in the names of all live files and in
// the content of the text files held on this node. At most maxFiles hits
// are returned and maxLines lines kept per file (0 means no limit); cut
// reports whether more files matched. Hits are sorted by name.
func (s *Store) Search(needle string, maxFiles, maxLines int) (hits []Hit, cut bool) {
	needle = strings.ToLower(strings.TrimSpace(needle))
	if needle == "" {
		return nil, false
	}
	for _, m := range s.List(false) {
		h := Hit{Name: m.Name, NameMatch: strings.Contains(strings.ToLower(m.Name), needle)}
		if data, ok := s.ReadText(m.Name); ok && strings.Contains(strings.ToLower(string(data)), needle) {
			for i, line := range strings.Split(string(data), "\n") {
				if !strings.Contains(strings.ToLower(line), needle) {
					continue
				}
				if maxLines > 0 && len(h.Lines) >= maxLines {
					h.More++
					continue
				}
				h.Lines = append(h.Lines, LineHit{N: i + 1, Text: strings.TrimSpace(line)})
			}
		}
		if !h.NameMatch && len(h.Lines) == 0 {
			continue
		}
		if maxFiles > 0 && len(hits) >= maxFiles {
			return hits, true
		}
		hits = append(hits, h)
	}
	return hits, false
}
