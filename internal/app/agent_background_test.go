package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/matt-wright86/mardi-gras/internal/agent"
)

// newBackgroundModel is a sized model with background sessions available and
// nothing known about them yet: the state just after Init, with the first scan
// still owed. Tests that need a running session fill bgAgents and call
// setBackgroundAgents themselves.
//
// The fields are set here rather than left to New because the seat running the
// tests decides what New detects, and these tests are about what the model
// does with the capability, not about detecting it.
func newBackgroundModel(t *testing.T) Model {
	t.Helper()
	m := initModel(t)
	m.bgAvail = true
	m.bgScanInFlight = false
	m.bgScanAt = time.Time{}
	m.bgScanned = false
	m.bgAgents = map[string]agent.BackgroundSession{}
	return m
}

var errBackgroundTest = errors.New("scan failed")

func bgSession(id string) agent.BackgroundSession {
	return agent.BackgroundSession{IssueID: id, ID: "bg-" + id, State: "working", Status: "busy"}
}

// A background session shows up as an agent the issue has, and is reported by
// name and state — but it is not capturable: mg has no readable output for it.
func TestBackgroundSessionIsShownAsAnAgent(t *testing.T) {
	m := newBackgroundModel(t)
	m.setBackgroundAgents(map[string]agent.BackgroundSession{"open-1": bgSession("open-1")})

	if m.capturable("open-1") {
		t.Error("a background session has no output mg can render, so it must not ask for a capture")
	}
	if _, ok := m.parade.ActiveAgents["open-1"]; !ok {
		t.Errorf("parade should show open-1 as having an agent, got %v", m.parade.ActiveAgents)
	}
	if m.header.AgentCount != 1 {
		t.Errorf("agent count = %d, want 1", m.header.AgentCount)
	}
	if got := m.detail.AgentLabels["open-1"]; got != "session bg-open-1 · working" {
		t.Errorf("detail label = %q, want the session and its state", got)
	}

	// A known session must not ask for a capture either: captureWanted would
	// start a poll of a source that does not exist.
	if m.captureWanted {
		t.Error("a background session must not request a capture")
	}
}

// What the panel says about a session: the thing it is waiting on when it is
// waiting on a person, otherwise its state, otherwise the coarser liveness.
func TestSessionNotePrefersWhatTheSessionWaitsFor(t *testing.T) {
	cases := []struct {
		name    string
		session agent.BackgroundSession
		want    string
	}{
		{"waiting on the user", agent.BackgroundSession{ID: "bg-1", Status: "idle", State: "blocked", WaitingFor: "input needed"}, "input needed"},
		{"working", agent.BackgroundSession{ID: "bg-1", Status: "busy", State: "working"}, "working"},
		{"state only", agent.BackgroundSession{ID: "bg-1", State: "done"}, "done"},
		{"status only", agent.BackgroundSession{ID: "bg-1", Status: "idle"}, "idle"},
		{"nothing known", agent.BackgroundSession{ID: "bg-1"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sessionNote(c.session); got != c.want {
				t.Errorf("sessionNote(%+v) = %q, want %q", c.session, got, c.want)
			}
		})
	}

	// A session waiting on a person must not be reported as merely "blocked":
	// that is the one line a user who is away needs to act on.
	m := newBackgroundModel(t)
	waiting := agent.BackgroundSession{IssueID: "open-1", ID: "bg-open-1", Status: "idle", State: "blocked", WaitingFor: "input needed"}
	m.setBackgroundAgents(map[string]agent.BackgroundSession{"open-1": waiting})
	if got := m.detail.AgentLabels["open-1"]; got != "session bg-open-1 · input needed" {
		t.Errorf("detail label = %q, want the session to say what it waits for", got)
	}
}

// The views read one map, so the two sources are merged for them — but a pane
// is the agent the user can see, so it wins an issue that somehow has both.
func TestPropagateMergesPanesAndBackgroundSessions(t *testing.T) {
	m := newBackgroundModel(t)
	m.inTmux = true
	m.activeAgents = map[string]string{"open-1": "%7"}
	m.bgAgents = map[string]agent.BackgroundSession{
		"open-1": bgSession("open-1"),
		"open-2": bgSession("open-2"),
	}
	m.propagateAgentState()

	if got := m.parade.ActiveAgents["open-1"]; got != "%7" {
		t.Errorf("open-1 = %q, want the pane to win the tie", got)
	}
	if got := m.parade.ActiveAgents["open-2"]; got != "bg-open-2" {
		t.Errorf("open-2 = %q, want the background session", got)
	}
	if m.header.AgentCount != 2 {
		t.Errorf("agent count = %d, want 2", m.header.AgentCount)
	}
	// The model keeps the two apart: the tmux poll installs its own map, and a
	// background session leaked into it would be dropped as a dead pane.
	if _, ok := m.activeAgents["open-2"]; ok {
		t.Errorf("background sessions leaked into activeAgents: %v", m.activeAgents)
	}
}

// A registry scan is no reason to drop what the detail panel is showing: the
// two sources are followed independently, and the pane still has an agent.
func TestBackgroundScanLeavesPaneOutputAlone(t *testing.T) {
	m := newBackgroundModel(t)
	m.inTmux = true
	m.activeAgents = map[string]string{"open-2": "%7"}
	m.detail.SetAgentOutput("open-2", []string{"working"})

	m.setBackgroundAgents(map[string]agent.BackgroundSession{"open-1": bgSession("open-1")})
	if m.detail.AgentOutputID != "open-2" || len(m.detail.AgentOutput) == 0 {
		t.Errorf("a registry scan dropped the pane's output: id=%q", m.detail.AgentOutputID)
	}
}

func TestBackgroundScanIsGated(t *testing.T) {
	m := newBackgroundModel(t)

	// The first scan is owed in every mode, so a session started before this
	// mg, or by another client, is still picked up.
	if cmd := m.gatedPollBackgroundAgents(); cmd == nil {
		t.Fatal("expected the first scan to run")
	}
	if !m.bgScanInFlight || m.bgScanAt.IsZero() {
		t.Fatal("expected the first scan to arm the gate and the TTL")
	}
	if cmd := m.gatedPollBackgroundAgents(); cmd != nil {
		t.Error("expected a second scan to be held off while one is in flight")
	}

	// An empty registry ends the watching: with nothing known and no session
	// this mg started, scans stop rather than costing a process launch forever.
	model, _ := m.Update(backgroundAgentsMsg{agents: map[string]agent.BackgroundSession{}})
	m = model.(Model)
	if m.bgScanInFlight {
		t.Error("expected the gate to clear on the result")
	}
	if cmd := m.gatedPollBackgroundAgents(); cmd != nil {
		t.Error("expected no further scans once the registry is known to be empty")
	}

	// A live session keeps it watching, on the TTL.
	m.setBackgroundAgents(map[string]agent.BackgroundSession{"open-1": bgSession("open-1")})
	if cmd := m.gatedPollBackgroundAgents(); cmd != nil {
		t.Error("expected the TTL to hold the next scan back")
	}
	m.bgScanAt = time.Now().Add(-backgroundScanInterval)
	if cmd := m.gatedPollBackgroundAgents(); cmd == nil {
		t.Error("expected a scan once the TTL elapsed")
	}
}

func TestBackgroundModeScansWithNothingKnown(t *testing.T) {
	m := newBackgroundModel(t)
	m.agentMode = agent.ModeBackground
	m.bgScanInFlight = false
	m.bgScanned = true // the first scan already ran and found nothing
	if cmd := m.gatedPollBackgroundAgents(); cmd == nil {
		t.Error("expected background mode to keep scanning: it starts sessions mg must not lose")
	}
}

// Without a runtime that has background sessions, no scan is ever attempted —
// not even the first one.
func TestBackgroundScanIsOffWhenUnsupported(t *testing.T) {
	m := newBackgroundModel(t)
	m.bgAvail = false
	m.bgScanInFlight = false
	if cmd := m.gatedPollBackgroundAgents(); cmd != nil {
		t.Error("expected no scans when the runtime has no background sessions")
	}
}

func TestHandleBackgroundAgentsKeepsSessionsOnError(t *testing.T) {
	m := newBackgroundModel(t)
	m.setBackgroundAgents(map[string]agent.BackgroundSession{"open-1": bgSession("open-1")})

	model, _ := m.Update(backgroundAgentsMsg{err: errBackgroundTest})
	m = model.(Model)
	if _, ok := m.bgAgents["open-1"]; !ok {
		t.Error("a failed scan must not clear the sessions already known")
	}
	if m.bgScanInFlight {
		t.Error("a failed scan must still clear the gate, or the poll stops for good")
	}
}

func TestHandleBackgroundLaunchedRecordsTheSession(t *testing.T) {
	m := newBackgroundModel(t)
	model, cmd := m.Update(backgroundLaunchedMsg{issueID: "open-1", session: bgSession("open-1")})
	m = model.(Model)

	if _, ok := m.bgAgents["open-1"]; !ok {
		t.Fatalf("expected the launched session to be recorded, got %v", m.bgAgents)
	}
	if m.toast.Message == "" {
		t.Error("expected a toast confirming the launch")
	}
	if cmd == nil {
		t.Error("expected the toast's dismiss timer")
	}
}

func TestHandleBackgroundStoppedDropsTheSession(t *testing.T) {
	m := newBackgroundModel(t)
	m.setBackgroundAgents(map[string]agent.BackgroundSession{
		"open-1": bgSession("open-1"),
		"open-2": bgSession("open-2"),
	})

	model, _ := m.Update(backgroundStoppedMsg{issueID: "open-1"})
	m = model.(Model)
	if _, ok := m.bgAgents["open-1"]; ok {
		t.Error("expected the stopped session to be dropped")
	}
	if _, ok := m.bgAgents["open-2"]; !ok {
		t.Error("expected the other issue's session to be left alone")
	}
	if m.header.AgentCount != 1 {
		t.Errorf("agent count = %d, want 1", m.header.AgentCount)
	}
}

// A stop that failed leaves the session in place: mg must not show an issue as
// agent-free while its agent is still running.
func TestHandleBackgroundStoppedKeepsTheSessionOnError(t *testing.T) {
	m := newBackgroundModel(t)
	m.setBackgroundAgents(map[string]agent.BackgroundSession{"open-1": bgSession("open-1")})

	model, _ := m.Update(backgroundStoppedMsg{issueID: "open-1", err: errBackgroundTest})
	m = model.(Model)
	if _, ok := m.bgAgents["open-1"]; !ok {
		t.Error("expected the session to survive a failed stop")
	}
	if m.toast.Message == "" {
		t.Error("expected the failure to be reported")
	}
}

// Leaving a session the user attached to must not wait out the scan TTL: the
// session was free to write to beads while they were in it.
func TestBackgroundDetachedRescansAtOnce(t *testing.T) {
	m := newBackgroundModel(t)
	m.bgScanAt = time.Now()
	m.lastFileMod = time.Now()

	model, cmd := m.Update(backgroundDetachedMsg{})
	m = model.(Model)
	if !m.bgScanInFlight {
		t.Error("expected a registry scan to be issued at once, not at the next TTL")
	}
	if !m.lastFileMod.IsZero() {
		t.Error("expected beads to be re-read rather than trusted")
	}
	if cmd == nil {
		t.Error("expected a refresh to be scheduled")
	}
}

// d always means "run this on the daemon", whatever the launch mode is.
func TestKeyDLaunchesABackgroundSession(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no agent runtime: the launch must fail closed
	m := newBackgroundModel(t)

	got, cmd := press(t, m, 'd')
	if cmd == nil {
		t.Fatal("expected d to return a launch command")
	}
	if got.bgScanInFlight != m.bgScanInFlight {
		t.Error("d should not disturb the scan gate")
	}
	msg := cmd()
	errMsg, ok := msg.(agentLaunchErrorMsg)
	if !ok {
		t.Fatalf("expected a failed launch message, got %T", msg)
	}
	if !strings.Contains(errMsg.err.Error(), "claude") {
		t.Errorf("error should name the runtime background sessions need, got %v", errMsg.err)
	}
}

// In background mode `a` is the same dispatch; with a session already running
// for the issue it means "take me back to it", never a second agent on the
// same work.
func TestKeyAInBackgroundMode(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(agent.AgentModeEnv, "background")
	m := newBackgroundModel(t)
	m.agentMode = agent.ModeBackground
	m.agentAvail = true

	_, cmd := press(t, m, 'a')
	if cmd == nil {
		t.Fatal("expected a to return a launch command in background mode")
	}
	if _, ok := cmd().(agentLaunchErrorMsg); !ok {
		t.Fatalf("expected the launch to fail closed with no runtime, got %T", cmd())
	}

	m.setBackgroundAgents(map[string]agent.BackgroundSession{"open-1": bgSession("open-1")})
	// With a session known, the same key resolves to attach instead of a
	// launch — a different failure here, since there is no runtime to attach
	// with either.
	_, cmd = press(t, m, 'a')
	if cmd == nil {
		t.Fatal("expected a to return a command for the running session")
	}
	msg := cmd()
	if _, ok := msg.(backgroundErrorMsg); !ok {
		t.Fatalf("expected attach to be attempted for the running session, got %T", msg)
	}
}

// A in any mode stops mg's own background session, without consulting an
// orchestrator that has never heard of it.
func TestKeyAStopsTheBackgroundSession(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	m := newBackgroundModel(t)
	m.setBackgroundAgents(map[string]agent.BackgroundSession{"open-1": bgSession("open-1")})

	got, cmd := press(t, m, 'A')
	if cmd == nil {
		t.Fatal("expected A to return a stop command")
	}
	result := cmd()
	msg, ok := result.(backgroundStoppedMsg)
	if !ok {
		t.Fatalf("expected a stop result, got %T", result)
	}
	if msg.issueID != "open-1" {
		t.Errorf("stop targeted %q, want open-1", msg.issueID)
	}
	if msg.err == nil {
		t.Error("expected the stop to fail with no runtime on PATH")
	}
	if _, ok := got.bgAgents["open-1"]; !ok {
		t.Error("the session must stay until a stop succeeds")
	}
}

// Outside tmux, with no orchestrator and no background sessions, there is
// nothing to poll and gatedPollAgentState must stay quiet.
func TestGatedPollAgentStateIsQuietWithoutSources(t *testing.T) {
	m := newBackgroundModel(t)
	m.bgAvail = false
	m.inTmux = false
	if cmd := m.gatedPollAgentState(); cmd != nil {
		t.Error("expected no polling with no pane, no orchestrator and no background sessions")
	}
}

// An orchestrator owns its own agents, but a background session is mg's: it
// must be followed under an orchestrator too, or a session started with d goes
// invisible once mg restarts and a second one gets started for the same issue.
func TestOrchestratorStillScansBackgroundSessions(t *testing.T) {
	m := newBackgroundModel(t)
	m.inTmux = false
	m.gtEnv.Available = true
	// Both of the orchestrator's own polls are held off, so the registry scan
	// is the only source the command below can have come from.
	m.gtPollInFlight = true
	m.patrolScanInFlight = true

	if cmd := m.gatedPollAgentState(); cmd == nil {
		t.Fatal("expected the registry scan to run alongside the orchestrator's polls")
	}
	if !m.bgScanInFlight {
		t.Error("expected the scan to have been issued")
	}

	// Same arrangement without the capability: nothing at all, which is what
	// proves the command above was the scan.
	m.bgAvail = false
	m.bgScanInFlight = false
	if cmd := m.gatedPollAgentState(); cmd != nil {
		t.Error("expected no polling once the registry cannot be read")
	}
}
