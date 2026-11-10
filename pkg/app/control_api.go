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
	handle   func(c *controlConn, params []byte) (any, *ctl.Error)
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
// them. guardMethods wraps every handler, so a faulting verb answers its
// caller wherever it runs.
var controlMethods = guardMethods(map[string]controlMethod{
	ctl.MethodState:  {handle: (*controlConn).stateGet},
	ctl.MethodAttach: {handle: (*controlConn).attachCall}, // callable before any seat exists

	ctl.MethodSubmit:      {seat: true, lane: true, handle: (*controlConn).submitCall},
	ctl.MethodInterrupt:   {seat: true, lane: true, priority: true, handle: (*controlConn).interruptCall},
	ctl.MethodModeCycle:   {seat: true, lane: true, handle: (*controlConn).modeCycleCall},
	ctl.MethodModeSet:     {seat: true, lane: true, handle: (*controlConn).modeSetCall},
	ctl.MethodQueueRecall: {seat: true, lane: true, handle: (*controlConn).queueRecallCall},
	ctl.MethodRewind:      {seat: true, lane: true, handle: (*controlConn).rewindCall},
	ctl.MethodResolve:     {seat: true, handle: (*controlConn).resolveCall}, // answers stay off the lane
	ctl.MethodToolsSet:    {seat: true, lane: true, handle: (*controlConn).toolsSetCall},
	ctl.MethodModelSet:    {seat: true, lane: true, handle: (*controlConn).modelSetCall},
	ctl.MethodSettingsSet: {seat: true, lane: true, handle: (*controlConn).settingsSetCall},

	// stop serializes inside the sub-agent manager and touches no session state,
	// so like resolve it stays off the lane
	ctl.MethodAgentsStop: {seat: true, handle: (*controlConn).agentsStopCall},

	ctl.MethodModelsGet:   {handle: (*controlConn).modelsGetCall}, // reads answer any connection
	ctl.MethodSettingsGet: {handle: (*controlConn).settingsGetCall},
	ctl.MethodAgentsList:  {handle: (*controlConn).agentsListCall},
})

// guardMethods wraps every registered handler once, converting a panic into
// an unexpected refusal. No single execution point exists: lane verbs run on
// the executor goroutine, off-lane reads on the conn's read loop, so the
// wrap travels with the handler and a fault never kills either goroutine.
func guardMethods(methods map[string]controlMethod) map[string]controlMethod {
	wrapped := make(map[string]controlMethod, len(methods))
	for name, m := range methods {
		h := m.handle
		m.handle = func(c *controlConn, params []byte) (result any, rerr *ctl.Error) {
			defer func() {
				if p := recover(); p != nil {
					result, rerr = nil, ctl.Errf(ctl.CodeUnexpected, "%s faulted: %v", name, p)
				}
			}()
			return h(c, params)
		}
		wrapped[name] = m
	}
	return wrapped
}

// stateGet answers one snapshot read with the seat section from this
// connection's view. The reply's seq is the snapshot's own: head and ephemera
// were read under it.
func (c *controlConn) stateGet([]byte) (any, *ctl.Error) {
	snap, seq := c.srv.hub.SnapshotAt()
	st := ctlStateFrom(snap)
	st.Seat = c.srv.seatState(c)
	return seqResult{val: st, seq: seq}, nil
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
	var displaced string // the live holder force-displaced, for its seat.lost event
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
func (c *controlConn) attachCall(params []byte) (any, *ctl.Error) {
	return c.srv.attach(c, params)
}

func (c *controlConn) submitCall(params []byte) (any, *ctl.Error) {
	return c.srv.submit(params)
}

func (c *controlConn) interruptCall([]byte) (any, *ctl.Error) {
	return c.srv.interrupt()
}

func (c *controlConn) modeCycleCall(params []byte) (any, *ctl.Error) {
	return c.srv.modeCycle(params)
}

func (c *controlConn) queueRecallCall([]byte) (any, *ctl.Error) {
	return c.srv.queueRecall()
}

func (c *controlConn) rewindCall(params []byte) (any, *ctl.Error) {
	return c.srv.rewind(params)
}

// resolveCall adapts the resolve method onto the registry shape.
func (c *controlConn) resolveCall(params []byte) (any, *ctl.Error) {
	return nil, c.srv.resolve(params)
}

// submit routes one line exactly as typed in the editor: prompts, /commands,
// !shell and !! all keep their existing behavior, and a turn already running
// steers or queues per the pump's rules. A dialog holding the editor refuses.
func (s *controlServer) submit(params []byte) (any, *ctl.Error) {
	var p ctl.SubmitParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(p.Line) == "" {
		return nil, ctl.Errf(ctl.CodeBadRequest, "submit needs a non-empty line")
	}
	if rerr := s.inputHeld(); rerr != nil {
		return nil, rerr
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

// queueRecall runs the body behind Alt+Up on one lane: the newest queued
// prompt pops from the session's queue and its text returns to the calling
// surface's editor rather than the host's, so editing there resubmits the edit.
// The reply names the row id, since the watermark it carries supersedes the
// drop event that resolved the same row. An empty queue answers an empty result
// like interrupt naming nothing stopped; an item whose payload cannot cross the
// wire refuses in place and stays queued.
func (s *controlServer) queueRecall() (any, *ctl.Error) {
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
	head, err := s.hub.rewindEntry()(p.Head, rewindCauseRewind)
	if err != nil {
		// an unknown head is not-found, every other refusal names what happened
		code := ctl.CodeUnavailable
		if errors.Is(err, errUnknownHead) {
			code = ctl.CodeNotFound
		}
		return nil, ctl.Errf(code, "rewind: %v", err)
	}
	return ctl.RewindResult{Head: head}, nil
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
			// a lost resolve race reads exactly like answering after End
			return settleRefused(p.ID, errors.Is(err, ErrInteractionSettled))
		}
		return nil
	}
	return settleRefused(p.ID, ended)
}

// settleRefused maps a stale or resolved interaction id onto its reply error.
func settleRefused(id uint64, ended bool) *ctl.Error {
	if ended {
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
	var given int
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

// inputHeld refuses while a dialog holds the editor; submit applies the rule.
func (s *controlServer) inputHeld() *ctl.Error {
	if held, ok := s.hub.ui.dialogs.holding(); ok {
		return ctl.Errf(ctl.CodeUnavailable,
			"input held by an open %s dialog; resolve it first", held.Kind)
	}
	return nil
}
