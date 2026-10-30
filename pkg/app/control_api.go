package app

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/jentfoo/ajent/pkg/ctl"
)

// submitOriginController is the provenance origin a control-socket submit
// carries onto the transcript's message entries.
const submitOriginController = "controller"

// controlMethod routes one controller method. lane marks a mutating verb: its
// handler runs on the control executor, where guards revalidate at execution
// time and replies follow the effects they acknowledge. priority jumps the
// lane's queue (interrupt). seat marks a call that needs the control seat.
// Off-lane seat calls check at dispatch.
type controlMethod struct {
	handle   func(c *controlConn, params []byte) (any, uint64, *ctl.Error)
	seat     bool
	lane     bool
	priority bool
}

// seqResult wraps a handler result whose reply must carry the seq the result
// was taken at, not the watermark at reply time.
type seqResult struct {
	val any
	seq uint64
}

// controlMethods is every method a controller may call. Adding one means adding
// its entry here; the seat and lane flags sit beside the handler that obeys
// them.
var controlMethods = map[string]controlMethod{
	ctl.MethodState:  {handle: (*controlConn).stateGet},
	ctl.MethodAttach: {handle: (*controlConn).attachCall}, // callable before any seat exists

	ctl.MethodSubmit: {seat: true, lane: true, handle: func(c *controlConn, p []byte) (any, uint64, *ctl.Error) {
		res, rerr := c.srv.submit(p)
		return res, 0, rerr
	}},
	ctl.MethodInterrupt: {seat: true, lane: true, priority: true, handle: func(c *controlConn, _ []byte) (any, uint64, *ctl.Error) {
		res, rerr := c.srv.interrupt()
		return res, 0, rerr
	}},
	ctl.MethodModeCycle: {seat: true, lane: true, handle: func(c *controlConn, p []byte) (any, uint64, *ctl.Error) {
		res, rerr := c.srv.modeCycle(p)
		return res, 0, rerr
	}},
	ctl.MethodInputSet: {seat: true, lane: true, handle: func(c *controlConn, p []byte) (any, uint64, *ctl.Error) {
		res, rerr := c.srv.inputSet(p)
		return res, 0, rerr
	}},
	ctl.MethodQueueRecall: {seat: true, lane: true, handle: func(c *controlConn, _ []byte) (any, uint64, *ctl.Error) {
		res, rerr := c.srv.queueRecall()
		return res, 0, rerr
	}},
	ctl.MethodRewind: {seat: true, lane: true, handle: func(c *controlConn, p []byte) (any, uint64, *ctl.Error) {
		res, rerr := c.srv.rewind(p)
		return res, 0, rerr
	}},
	ctl.MethodResolve: {seat: true, handle: (*controlConn).resolveCall}, // answers stay off the lane
	ctl.MethodSeatYield: {seat: true, handle: func(c *controlConn, _ []byte) (any, uint64, *ctl.Error) {
		c.srv.yield(c)
		return nil, 0, nil
	}},
}

// stateGet answers one snapshot read with the seat section from this
// connection's view. The reply's seq is the snapshot's own: head and ephemera
// were read under it.
func (c *controlConn) stateGet([]byte) (any, uint64, *ctl.Error) {
	snap, seq := c.srv.hub.SnapshotAt()
	st := ctlStateFrom(snap)
	st.Seat = c.srv.seatState(c)
	return seqResult{val: st, seq: seq}, seq, nil
}

// attach claims or declines the control seat for c. The seat is single: a
// second claim errors naming the holder, force takes it from a live one (the
// displaced holder learns through seat.lost), and re-attaching by the current
// holder succeeds, refreshing its name. A plain attach stays an observer.
func (s *controlServer) attach(c *controlConn, params []byte) (ctl.AttachResult, *ctl.Error) {
	var p ctl.AttachParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return ctl.AttachResult{}, rerr
	}
	if !p.Control {
		return ctl.AttachResult{}, nil
	}
	holder := strings.TrimSpace(p.Holder)
	if holder == "" {
		holder = "controller"
	}
	s.mu.Lock()
	displaced := "" // the live holder force-displaced, for its seat.lost event
	var refusal *ctl.Error
	switch {
	case s.seat == c:
		s.seatHolder = holder
	case s.seat != nil && !p.Force:
		refusal = seatHeldError(s.seatHolder)
	default:
		if s.seat != nil {
			displaced = s.seatHolder
		}
		s.seat, s.seatHolder = c, holder
	}
	s.mu.Unlock()
	if refusal != nil {
		return ctl.AttachResult{}, refusal
	}
	if displaced != "" {
		// off the server lock: publishing takes the hub lock, and no path may
		// hold both at once
		s.hub.publish(ctl.EventSeatLost, ctl.SeatLostData{From: displaced, To: holder})
	}
	s.writeRegistry()
	return ctl.AttachResult{Control: true}, nil
}

// yield releases the control seat this connection holds, so a script can hand
// the session back without dropping its socket. Releasing a seat it does not
// hold is a benign no-op. Like disconnect release, this spends no event number:
// the holder already knows, the registry rewrite is what tells pickers, and a
// seat.lost with no successor would read as a takeover to anyone matching From.
func (s *controlServer) yield(c *controlConn) {
	s.mu.Lock()
	if s.seat != c {
		s.mu.Unlock()
		return
	}
	s.seat = nil
	s.seatHolder = ""
	s.mu.Unlock()
	s.writeRegistry()
}

// seatHeldError builds the takeover-capable refusal: the code is dedicated and
// the holder rides its own field, so a script can decide between force
// reattach, waiting and exiting without parsing prose.
func seatHeldError(holder string) *ctl.Error {
	e := ctl.Errf(ctl.CodeSeatHeld,
		"control seat held by %s; attach again with force to take it", seatLabel(holder))
	e.Holder = holder
	return e
}

// seatLabel renders a holder's name for refusal errors.
func seatLabel(holder string) string {
	if holder == "" {
		return "another controller"
	}
	return `"` + holder + `"`
}

// seatRefusal reports whether c may mutate, else the error every write method
// answers with, naming whoever holds the seat.
func (s *controlServer) seatRefusal(c *controlConn) *ctl.Error {
	s.mu.Lock()
	mine := s.seat == c
	held := s.seat != nil
	name := s.seatHolder
	s.mu.Unlock()
	if mine {
		return nil
	}
	if held {
		e := ctl.Errf(ctl.CodeObserver,
			"observer connection: the control seat is held by %s", seatLabel(name))
		e.Holder = name
		return e
	}
	return ctl.Errf(ctl.CodeObserver,
		"observer connection: nobody holds the control seat; attach with control first")
}

// seatState reports the control seat from this connection's side of it.
func (s *controlServer) seatState(c *controlConn) ctl.SeatState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ctl.SeatState{Held: s.seat != nil, Holder: s.seatHolder, Yours: s.seat == c}
}

// attachCall adapts the attach method onto the registry shape.
func (c *controlConn) attachCall(params []byte) (any, uint64, *ctl.Error) {
	res, rerr := c.srv.attach(c, params)
	return res, 0, rerr
}

// resolveCall adapts the resolve method onto the registry shape.
func (c *controlConn) resolveCall(params []byte) (any, uint64, *ctl.Error) {
	return nil, 0, c.srv.resolve(params)
}

// submit routes one line exactly as typed in the editor: prompts, /commands,
// !shell and !! all keep their existing behavior, and a turn already running
// steers or queues per the pump's rules. A dialog holding the editor refuses,
// the same rule input.set applies.
func (s *controlServer) submit(params []byte) (any, *ctl.Error) {
	var p ctl.SubmitParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(p.Line) == "" {
		return nil, ctl.Errf(ctl.CodeBadRequest, "submit needs a non-empty line")
	}
	if held, ok := s.hub.ui.dialogs.holding(); ok {
		return nil, ctl.Errf(ctl.CodeUnavailable,
			"input held by an open %s dialog; resolve it first", held.Kind)
	}
	if err := s.hub.Submit(p.Line, p.PendingID, submitOriginController); err != nil {
		if errors.Is(err, errPromptQueueFull) {
			return nil, ctl.Errf(ctl.CodeBusy,
				"prompt queue full; retry once the running command or turn finishes")
		}
		return nil, ctl.Errf(ctl.CodeUnavailable, "submit unavailable: %s", err)
	}
	return nil, nil
}

// interrupt runs the body behind the Esc keypress: the running turn aborts,
// the staged shell beside it dies, and queued prompts recover to the local
// editor or drop in a serve run.
func (s *controlServer) interrupt() (any, *ctl.Error) {
	return ctl.InterruptResult{Stopped: s.hub.Interrupt().String()}, nil
}

// modeCycle steps the permission mode like Shift+Tab (or its back direction),
// re-evaluating open approval dialogs under the new mode.
func (s *controlServer) modeCycle(params []byte) (any, *ctl.Error) {
	var p ctl.ModeCycleParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	if s.hub.barrier == nil {
		return ctl.ModeResult{}, nil // no gate exists: nothing to cycle
	}
	return ctl.ModeResult{Mode: s.hub.ModeCycle(p.Back).String()}, nil
}

// inputSet prefills the editor. A dialog holding the editor refuses naming its
// kind, and a serve run has no editor at all. During an active turn the text
// simply sits in the editor, matching local typing.
func (s *controlServer) inputSet(params []byte) (any, *ctl.Error) {
	var p ctl.InputSetParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	if !s.hub.Interactive() {
		return nil, ctl.Errf(ctl.CodeUnavailable, "serve mode has no editor to prefill")
	}
	if held, ok := s.hub.ui.dialogs.holding(); ok {
		return nil, ctl.Errf(ctl.CodeUnavailable,
			"input held by an open %s dialog; resolve it first", held.Kind)
	}
	s.hub.ui.SetInput(p.Text)
	return nil, nil
}

// queueRecall runs the body behind Alt+Up on one lane: the newest queued
// prompt pops from the session's queue and its text returns to the calling
// surface's editor rather than the host's, so editing there resubmits the edit.
// The reply names the row id, since the watermark it carries supersedes the
// drop event that resolved the same row. An empty queue answers an empty result
// like interrupt naming nothing stopped; an item whose payload cannot cross the
// wire refuses in place and stays queued.
func (s *controlServer) queueRecall() (any, *ctl.Error) {
	if s.hub.q == nil {
		return nil, ctl.Errf(ctl.CodeUnavailable, "recall unavailable: the run is still starting")
	}
	id, text, err := s.hub.q.recallInto()
	if err != nil {
		return nil, ctl.Errf(ctl.CodeUnavailable, "recall refused: %v", err)
	}
	return ctl.RecallResult{ID: id, Text: text}, nil
}

// rewind forks onto an earlier transcript entry through the same transaction
// the keyboard's double-Esc runs, so live state and the writer head cannot
// diverge and the last rewind wins. An active plan workflow ends first.
func (s *controlServer) rewind(params []byte) (any, *ctl.Error) {
	var p ctl.RewindParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(p.Head) == "" {
		return nil, ctl.Errf(ctl.CodeBadRequest, "session.rewind needs a head entry id")
	}
	rewindTo := s.hub.rewindEntry()
	if rewindTo == nil {
		return nil, ctl.Errf(ctl.CodeUnavailable, "rewind unavailable: the run is still starting")
	}
	head, err := rewindTo(p.Head, rewindCauseRewind)
	if err != nil {
		return nil, ctl.Errf(rewindCode(err), "rewind: %v", err)
	}
	return ctl.RewindResult{Head: head}, nil
}

// rewindCode maps a rewind failure onto its reply code: an unknown head is
// not-found, every other refusal names what happened.
func rewindCode(err error) string {
	if errors.Is(err, errUnknownHead) {
		return ctl.CodeNotFound
	}
	return ctl.CodeUnavailable
}

// resolve answers one pending interaction: {index} for choice kinds,
// {indexes} for multi pick, {text} for input, {cancel} for Esc everywhere.
// First resolution wins against the local surface, and a stale id errors
// legibly after End.
func (s *controlServer) resolve(params []byte) *ctl.Error {
	var p ctl.ResolveParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return rerr
	}
	if p.ID == 0 {
		return ctl.Errf(ctl.CodeBadRequest, "interaction.resolve needs an id")
	}
	info, open, ended := s.dialogs.lookup(p.ID)
	if open {
		ans, rerr := s.resolveAnswer(p, info)
		if rerr != nil {
			return rerr
		}
		if err := s.dialogs.Resolve(p.ID, ans); err != nil {
			return resolveErr(p.ID, err)
		}
		return nil
	}
	if ended {
		return ctl.Errf(ctl.CodeSettled,
			"interaction %d already settled; a first answer won the race", p.ID)
	}
	return ctl.Errf(ctl.CodeNotFound, "no such interaction %d", p.ID)
}

// resolveErr maps a broker refusal onto its reply error.
func resolveErr(id uint64, err error) *ctl.Error {
	if errors.Is(err, ErrInteractionSettled) {
		return ctl.Errf(ctl.CodeSettled,
			"interaction %d already settled; a first answer won the race", id)
	}
	return ctl.Errf(ctl.CodeNotFound, "no such interaction %d", id)
}

// resolveAnswer maps one resolve request onto a broker answer, validating the
// shape against what the dialog announced rather than per-kind rules: exactly
// one shape applies, a controller cannot answer a choice dialog with text, and
// an index never passes its options.
func (s *controlServer) resolveAnswer(p ctl.ResolveParams, info interactionInfo) (interactionAnswer, *ctl.Error) {
	shapes := info.Shapes
	badShape := func(shape string) *ctl.Error {
		return ctl.Errf(ctl.CodeBadRequest,
			"a %s dialog is not answered with %s", info.Kind, shape)
	}
	// several shapes together is a client bug, never a priority order to
	// resolve silently
	given := 0
	for _, ok := range []bool{p.Cancel, p.Index != nil, p.Indexes != nil, p.Text != nil} {
		if ok {
			given++
		}
	}
	if given > 1 {
		return interactionAnswer{}, ctl.Errf(ctl.CodeBadRequest,
			"interaction.resolve takes exactly one of index, indexes, text or cancel")
	}
	switch {
	case p.Cancel:
		return interactionAnswer{Declined: true}, nil
	case p.Index != nil:
		if len(shapes.options) == 0 {
			// no options announced means nothing to choose, never by index
			return interactionAnswer{}, badShape("an index")
		}
		if !indexInRange(*p.Index, shapes.options) {
			return interactionAnswer{}, ctl.Errf(ctl.CodeBadRequest,
				"index %d out of range for a %s dialog with %d options",
				*p.Index, info.Kind, len(shapes.options))
		}
		return interactionAnswer{Index: *p.Index}, nil
	case p.Indexes != nil:
		if !shapes.multi || len(shapes.options) == 0 {
			// no options announced means nothing to choose, never by index;
			// an empty list is a real answer once the dialog announces any
			return interactionAnswer{}, badShape("indexes")
		}
		for _, i := range *p.Indexes {
			if !indexInRange(i, shapes.options) {
				return interactionAnswer{}, ctl.Errf(ctl.CodeBadRequest,
					"index %d out of range for a %s dialog with %d options",
					i, info.Kind, len(shapes.options))
			}
		}
		return interactionAnswer{Indexes: *p.Indexes}, nil
	case p.Text != nil:
		if !shapes.text {
			return interactionAnswer{}, badShape("text")
		}
		if p.Chat && !shapes.chat {
			return interactionAnswer{}, ctl.Errf(ctl.CodeBadRequest,
				"a %s dialog does not take chat answers", info.Kind)
		}
		return interactionAnswer{Text: *p.Text, Chat: p.Chat}, nil
	}
	return interactionAnswer{}, ctl.Errf(ctl.CodeBadRequest,
		"interaction.resolve needs index, indexes, text or cancel")
}

// indexInRange reports whether one choice index selects from a dialog's options.
func indexInRange(i int, options []string) bool {
	return i >= 0 && i < len(options)
}

// decodeParams unmarshals one request's params, tolerating an empty body.
func decodeParams(raw []byte, v any) *ctl.Error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return ctl.Errf(ctl.CodeBadRequest, "params: %v", err)
	}
	return nil
}
