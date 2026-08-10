package tui

const noticeMarker = "!"

// Level is the severity of a notice.
type Level int

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
)

// Notify commits a marked line to history at the given level.
func (u *UI) Notify(msg string, level Level) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.flushNotice()
	u.commit(u.noticeLine(msg, level), flowWrap)
	u.repaint()
}

// NotifyKeyed replaces the previous notice carrying the same key while it is
// still the newest thing on screen, so repeated progress notices collapse
// rather than scrolling. Anything else being committed first flushes it to
// history, since a committed line is never rewritten.
func (u *UI) NotifyKeyed(key, msg string, level Level) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.noticeKey != "" && u.noticeKey != key {
		u.flushNotice()
	}
	u.noticeKey = key
	u.noticeText = u.noticeLine(msg, level)
	u.repaint()
}

// noticeLine renders a notice with its level styling.
func (u *UI) noticeLine(msg string, level Level) string {
	style := u.theme.Dim
	switch level {
	case LevelWarn:
		style = u.theme.Warn
	case LevelError:
		style = u.theme.Error
	}
	return style.Wrap(noticeMarker + " " + msg)
}

// flushNotice moves a live keyed notice into history. Caller holds the lock.
func (u *UI) flushNotice() {
	if u.noticeText == "" {
		return
	}
	text := u.noticeText
	u.noticeText, u.noticeKey = "", ""
	u.commit(text, flowWrap)
}
