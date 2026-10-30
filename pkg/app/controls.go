package app

import "time"

// doublePressWindow is how long the first ctrl+c holds the quit gesture armed.
const doublePressWindow = 10 * time.Second

// hintNoticeTTL is how long a one-shot control notice (a cancelled survey or
// staged command, a queue-drop outcome) holds the hint line before releasing it
// to whatever claimed it before.
const hintNoticeTTL = 6 * time.Second

// quitArm is the ctrl+c double-press window both front ends' control loops
// share: the first press that stopped nothing arms and holds the hint line for
// exactly the window, a second press inside it commits. Labels name the
// outcome, quit for the host loop and detach for attach, so the gesture is one
// implementation serving both.
type quitArm struct {
	hints *hintBoard
	again string // the armed hint: "ctrl+c again to <verb>"
	going string // the committed hint: "<verb>ing…"
	at    time.Time
}

// fire consumes one ctrl+c that stopped nothing: true when a second press
// landed inside the window, false when this press armed one. The window is
// measured from the arm, not from the hint's expiry, so a press arriving as
// the hint expires cannot lose the arm.
func (q *quitArm) fire() bool {
	if !q.at.IsZero() && time.Since(q.at) < doublePressWindow {
		q.hints.Request(q.going, "")
		return true
	}
	q.at = time.Now()
	q.hints.ShowFor(q.again, "", doublePressWindow)
	return false
}

// interruptNotice names what an interrupt stopped, the one hint both loops
// show for a keypress that did real work. Empty paints nothing, a stopped turn
// included: its own stream is the visible news.
func interruptNotice(stopped string) string {
	switch stopped {
	case stopSurvey.String():
		return "cancelled project survey"
	case stopShell.String():
		return "cancelled shell command"
	default:
		return ""
	}
}
