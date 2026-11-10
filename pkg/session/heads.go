package session

// resolveHead returns the head a transcript continues from: the file tail, or
// the parent a trailing cursor names when the rewind landed no entries yet.
func resolveHead(entries []Entry) string {
	if n := len(entries); n > 0 && entries[n-1].Type == TypeHead {
		return entries[n-1].ParentID
	}
	return Head(entries)
}
