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

// writeMethod reports whether one method mutates the session, and so needs the
// control seat.
func writeMethod(m string) bool {
	switch m {
	case ctl.MethodSubmit, ctl.MethodInterrupt, ctl.MethodModeCycle,
		ctl.MethodInputSet, ctl.MethodRewind, ctl.MethodResolve:
		return true
	}
	return false
}

// dispatchWrite answers one seat-gated request: observer connections are
// refused before any handler runs, so two controllers can never race a dialog
// or interleave submits invisibly. The watermark reads first, keeping the
// 34a rule that events racing the handler stay above the reply's seq.
func (c *controlConn) dispatchWrite(req ctl.Request) {
	seq := c.srv.hub.evWatermark()
	if rerr := c.srv.seatRefusal(c); rerr != nil {
		c.reply(req.ID, seq, nil, rerr)
		return
	}
	var result any
	var rerr *ctl.Error
	switch req.Method {
	case ctl.MethodSubmit:
		result, rerr = c.srv.submit(req.Params)
	case ctl.MethodInterrupt:
		result, rerr = c.srv.interrupt()
	case ctl.MethodModeCycle:
		result, rerr = c.srv.modeCycle(req.Params)
	case ctl.MethodInputSet:
		result, rerr = c.srv.inputSet(req.Params)
	case ctl.MethodRewind:
		result, rerr = c.srv.rewind(req.Params)
	case ctl.MethodResolve:
		result, rerr = c.srv.resolve(req.Params)
	default:
		rerr = ctl.Errf(ctl.CodeUnknownMethod, "unknown method %q", req.Method)
	}
	c.reply(req.ID, seq, result, rerr)
}

// attach claims or declines the control seat for c. The seat is single: a
// second claim errors naming the holder, force takes it from a live one (the
// displaced holder's writes refuse naming the new holder), and re-attaching by
// the current holder succeeds, refreshing its name. A plain attach stays an
// observer.
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
	defer s.mu.Unlock()
	switch {
	case s.seat == c:
		s.seatHolder = holder
	case s.seat != nil && !p.Force:
		return ctl.AttachResult{}, ctl.Errf(ctl.CodeSeatHeld,
			"control seat held by %s; attach again with force to take it", seatLabel(s.seatHolder))
	default:
		s.seat, s.seatHolder = c, holder
	}
	return ctl.AttachResult{Control: true}, nil
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
		return ctl.Errf(ctl.CodeObserver,
			"observer connection: the control seat is held by %s", seatLabel(name))
	}
	return ctl.Errf(ctl.CodeObserver,
		"observer connection: nobody holds the control seat; attach with control first")
}

// submit routes one line exactly as typed in the editor: prompts, /commands,
// !shell and !! all keep their existing behavior, and a turn already running
// steers or queues per the pump's rules.
func (s *controlServer) submit(params []byte) (any, *ctl.Error) {
	var p ctl.SubmitParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(p.Line) == "" {
		return nil, ctl.Errf(ctl.CodeBadRequest, "submit needs a non-empty line")
	}
	if !s.hub.Submit(p.Line, submitOriginController) {
		return nil, ctl.Errf(ctl.CodeUnavailable, "submit unavailable: the run is still starting")
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

// rewind forks onto an earlier transcript entry, the double-Esc operation. An
// active plan workflow ends first, the local editor redraws when one exists,
// and every rewind raises the head event.
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
	head, err := rewindTo(p.Head)
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
func (s *controlServer) resolve(params []byte) (any, *ctl.Error) {
	var p ctl.ResolveParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	if p.ID == 0 {
		return nil, ctl.Errf(ctl.CodeBadRequest, "interaction.resolve needs an id")
	}
	info, open, ended := s.dialogs.lookup(p.ID)
	if open {
		ans, rerr := s.resolveAnswer(p, info)
		if rerr != nil {
			return nil, rerr
		}
		if err := s.dialogs.Resolve(p.ID, ans); err != nil {
			return nil, resolveErr(p.ID, err)
		}
		return nil, nil
	}
	if ended {
		return nil, ctl.Errf(ctl.CodeSettled,
			"interaction %d already settled; a first answer won the race", p.ID)
	}
	return nil, ctl.Errf(ctl.CodeNotFound, "no such interaction %d", p.ID)
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
