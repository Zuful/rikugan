package escalation

import (
	"slices"
	"strings"
)

// Context is what the critical knows: the targets an action may name.
type Context struct {
	// Services are the incident's services, as namespace/name.
	Services []string
	// Revisions are the revisions named in the incident (the one rolled
	// out, the one before), by service.
	Revisions map[string][]string
	// Signals are the incident's signal names (error_rate, latency_p95…).
	Signals []string
	// Teams are the teams an escalation may name; none configured, any
	// escalation is kept without a team.
	Teams []string
}

// ToolCall is a tool call the agent made, as the agent's server
// recorded it.
type ToolCall struct {
	// Tool is the tool's name, without the MCP server prefix.
	Tool   string
	Output string
}

// Checked is a Finding after rikugan's checks.
type Checked struct {
	Cause      string
	Evidence   []CheckedQuote
	RuledOut   []CheckedRuledOut
	Action     Action
	Confidence Confidence
	// ActionRejected is set when the agent's action named a target
	// outside the incident, or a kind outside the vocabulary.
	ActionRejected bool
	// Downgraded is set when high confidence came without any verified
	// evidence.
	Downgraded bool
}

// CheckedQuote is a piece of evidence, verified or not.
type CheckedQuote struct {
	Evidence
	Verified bool
}

// CheckedRuledOut is a ruled-out hypothesis; Verified is meaningful when
// it cites a tool.
type CheckedRuledOut struct {
	RuledOut
	Verified bool
}

// Check holds the finding against the incident and the agent's real
// tool calls:
//
//   - an action outside the vocabulary, or whose target is not part of
//     ctx, becomes none, marked rejected;
//   - each quote is verified against the outputs of the calls of the
//     tool it cites (see Quoted);
//   - high confidence without a single verified quote drops to medium.
func Check(f Finding, ctx Context, calls []ToolCall) Checked {
	c := Checked{Cause: f.Cause, Confidence: f.Confidence}
	for i, e := range f.Evidence {
		if i == maxEvidence {
			break
		}
		c.Evidence = append(c.Evidence, CheckedQuote{Evidence: e, Verified: e.Quote != "" && Quoted(e.Quote, outputs(calls, e.Tool))})
	}
	for i, r := range f.RuledOut {
		if i == maxRuledOut {
			break
		}
		ok := r.Tool == "" || called(calls, r.Tool)
		if ok && r.Quote != "" {
			ok = Quoted(r.Quote, outputs(calls, r.Tool))
		}
		c.RuledOut = append(c.RuledOut, CheckedRuledOut{RuledOut: r, Verified: ok})
	}
	c.Action, c.ActionRejected = checkAction(f.Action, ctx)
	switch c.Confidence {
	case ConfidenceLow, ConfidenceMedium:
	case ConfidenceHigh:
		if !slices.ContainsFunc(c.Evidence, func(q CheckedQuote) bool { return q.Verified }) {
			c.Confidence, c.Downgraded = ConfidenceMedium, true
		}
	default:
		c.Confidence = ConfidenceLow
	}
	return c
}

func checkAction(a Action, ctx Context) (Action, bool) {
	reject := Action{Kind: ActionNone}
	hasService := slices.Contains(ctx.Services, a.Service)
	switch a.Kind {
	case ActionNone:
		return Action{Kind: ActionNone}, false
	case ActionRestart, ActionScale:
		if !hasService {
			return reject, true
		}
		return Action{Kind: a.Kind, Service: a.Service}, false
	case ActionRollback:
		if !hasService || !slices.Contains(ctx.Revisions[a.Service], a.Revision) {
			return reject, true
		}
		return Action{Kind: a.Kind, Service: a.Service, Revision: a.Revision}, false
	case ActionInspect:
		if !slices.Contains(ctx.Signals, a.Target) && !isTool(a.Target) {
			return reject, true
		}
		return Action{Kind: a.Kind, Target: a.Target}, false
	case ActionEscalate:
		if a.Target == "" || len(ctx.Teams) == 0 {
			return Action{Kind: a.Kind}, false
		}
		if !slices.Contains(ctx.Teams, a.Target) {
			return reject, true
		}
		return Action{Kind: a.Kind, Target: a.Target}, false
	}
	return reject, true
}

// isTool reports whether name looks like one of rikugan's tool names:
// lower-case words joined by underscores. Inspect only names it, so the
// exact list need not be known here.
func isTool(name string) bool {
	if name == "" || len(name) > 40 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

func called(calls []ToolCall, tool string) bool {
	return slices.ContainsFunc(calls, func(c ToolCall) bool { return c.Tool == tool })
}

func outputs(calls []ToolCall, tool string) []string {
	var out []string
	for _, c := range calls {
		if c.Tool == tool {
			out = append(out, c.Output)
		}
	}
	return out
}

// Quoted reports whether quote is found in one of the outputs. Runs of
// white space count as one space on both sides; a quote cut by "…" or
// "..." is split into fragments (quotation marks around it are dropped), each of which must appear, in order, in
// the same output.
func Quoted(quote string, outputs []string) bool {
	quote = strings.Trim(strings.TrimSpace(quote), `"'“”«»`+"`")
	var frags []string
	for _, f := range strings.Split(strings.ReplaceAll(quote, "...", "…"), "…") {
		if f = squash(f); f != "" {
			frags = append(frags, f)
		}
	}
	if len(frags) == 0 {
		return false
	}
	for _, out := range outputs {
		rest := squash(out)
		ok := true
		for _, f := range frags {
			i := strings.Index(rest, f)
			if i < 0 {
				ok = false
				break
			}
			rest = rest[i+len(f):]
		}
		if ok {
			return true
		}
	}
	return false
}

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }
