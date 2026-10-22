// Package escalation turns an agent's investigation of a critical into
// a message for the team. The agent answers in a fixed JSON shape
// (Finding, its schema given to the agent); rikugan then checks what it
// can and writes the message itself:
//
//   - the suggested action comes from a closed vocabulary, and its
//     target must be part of the critical's context;
//   - each piece of evidence must quote the output of a tool call the
//     agent really made;
//   - free text loses URLs, anything that looks like a command, and
//     secrets.
//
// Everything the agent read is untrusted (logs, commits, alert titles
// can carry injected instructions), so its answer is too: the message
// says it is a hypothesis to verify.
package escalation

// Finding is the agent's answer, decoded from the JSON it returns.
type Finding struct {
	// Cause is the probable cause, one sentence.
	Cause    string     `json:"cause"`
	Evidence []Evidence `json:"evidence"`
	// RuledOut are the hypotheses the agent excluded, and why.
	RuledOut   []RuledOut `json:"ruled_out"`
	Action     Action     `json:"action"`
	Confidence Confidence `json:"confidence"`
}

// Evidence is a fact behind the cause: an excerpt of a tool's output,
// quoted as is.
type Evidence struct {
	// Tool is the tool whose output holds Quote.
	Tool string `json:"tool"`
	// Quote is copied from the tool's output; "…" may stand for a
	// skipped part.
	Quote string `json:"quote"`
	// Note says what the quote shows, when it is not obvious.
	Note string `json:"note,omitempty"`
}

// RuledOut is a hypothesis the agent excluded.
type RuledOut struct {
	Hypothesis string `json:"hypothesis"`
	Reason     string `json:"reason"`
	// Tool and Quote, when set, are checked like Evidence.
	Tool  string `json:"tool,omitempty"`
	Quote string `json:"quote,omitempty"`
}

// ActionKind is the closed vocabulary of suggested actions.
type ActionKind string

// The actions an agent may suggest. None of them is ever carried out by
// rikugan.
const (
	ActionRollback ActionKind = "rollback"
	ActionRestart  ActionKind = "restart"
	ActionScale    ActionKind = "scale"
	ActionInspect  ActionKind = "inspect"
	ActionEscalate ActionKind = "escalate"
	ActionNone     ActionKind = "none"
)

// actionKinds lists the vocabulary, in the schema's order.
var actionKinds = []ActionKind{ActionRollback, ActionRestart, ActionScale, ActionInspect, ActionEscalate, ActionNone}

// Action is the one action the agent suggests.
type Action struct {
	Kind ActionKind `json:"kind"`
	// Service is the target of rollback, restart and scale, as
	// namespace/name.
	Service string `json:"service,omitempty"`
	// Revision is where a rollback goes back to.
	Revision string `json:"revision,omitempty"`
	// Target is the signal or tool to inspect, or the team to
	// escalate to.
	Target string `json:"target,omitempty"`
}

// Confidence is how sure the agent says it is.
type Confidence string

// Confidence levels. High requires direct evidence: tool data showing
// the cause, not a coincidence in time.
const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// Limits on what the message carries: it must stay readable in a chat.
const (
	maxEvidence = 5
	maxRuledOut = 3
	maxText     = 300 // runes per free-text field
	maxQuote    = 240
)

// Schema is the JSON Schema of Finding, given to the agent as the
// required shape of its answer. The action kind is an enumeration: the
// closed vocabulary holds at the source, and Check holds it again.
func Schema() map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	kinds := make([]string, len(actionKinds))
	for i, k := range actionKinds {
		kinds[i] = string(k)
	}
	quoted := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tool":  str("Name of the tool whose output holds the quote."),
			"quote": str("Excerpt copied exactly from that tool's output; '…' may replace a skipped part."),
			"note":  str("What the quote shows, if not obvious."),
		},
		"required":             []string{"tool", "quote"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cause":    str("The probable cause, in one sentence. No commands, no URLs."),
			"evidence": map[string]any{"type": "array", "maxItems": maxEvidence, "items": quoted},
			"ruled_out": map[string]any{
				"type":     "array",
				"maxItems": maxRuledOut,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"hypothesis": str("A cause considered and excluded."),
						"reason":     str("Why, from tool data."),
						"tool":       str("Tool whose output shows it, if any."),
						"quote":      str("Excerpt copied exactly from that output, if any."),
					},
					"required":             []string{"hypothesis", "reason"},
					"additionalProperties": false,
				},
			},
			"action": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":     map[string]any{"type": "string", "enum": kinds},
					"service":  str("namespace/name, for rollback, restart and scale: a service of the incident."),
					"revision": str("For rollback: a revision named in the incident."),
					"target":   str("For inspect: a signal or tool. For escalate: a team."),
				},
				"required":             []string{"kind"},
				"additionalProperties": false,
			},
			"confidence": map[string]any{
				"type":        "string",
				"enum":        []string{string(ConfidenceLow), string(ConfidenceMedium), string(ConfidenceHigh)},
				"description": "high only with direct evidence of the cause, not a coincidence in time.",
			},
		},
		"required":             []string{"cause", "evidence", "ruled_out", "action", "confidence"},
		"additionalProperties": false,
	}
}
