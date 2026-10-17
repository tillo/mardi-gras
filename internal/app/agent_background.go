package app

import (
	"fmt"
	"maps"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/matt-wright86/mardi-gras/internal/agent"
	"github.com/matt-wright86/mardi-gras/internal/components"
	"github.com/matt-wright86/mardi-gras/internal/data"
)

// Background agents are Claude Code sessions hosted by its multi-agent daemon:
// mg starts one with `claude --bg` and then follows the session registry,
// instead of owning a pane. The session outlives mg, which is the point — a
// client on a phone can start work, drop the connection, and find the agent
// still running later, from mg or from `claude agents`.
//
// They are kept in their own map rather than in activeAgents because the two
// are polled independently: tmux panes on every refresh tick, the registry on
// a TTL (it costs a process launch). Neither poll may clear the other's
// entries, so each installs its own map and propagateAgentState merges them
// for the views.

// configuredAgentMode reads MG_AGENT_MODE, which cmd/mg sets from
// --agent-mode and validates before the TUI starts. A library caller that
// leaves an invalid value there gets mg's historical behaviour rather than a
// model that refuses to launch anything.
func configuredAgentMode() agent.Mode {
	mode, err := agent.ConfiguredMode()
	if err != nil {
		return agent.ModeAuto
	}
	return mode
}

// backgroundScanInterval is how often mg re-reads the session registry.
// `claude agents --json` is a process launch, so it runs on a TTL rather than
// on every refresh tick.
const backgroundScanInterval = 5 * time.Second

// backgroundAgentsMsg carries a registry scan.
type backgroundAgentsMsg struct {
	agents map[string]agent.BackgroundSession
	err    error
}

// backgroundLaunchedMsg reports a background session mg just started.
type backgroundLaunchedMsg struct {
	issueID string
	session agent.BackgroundSession
}

// backgroundStoppedMsg reports the outcome of stopping one.
type backgroundStoppedMsg struct {
	issueID string
	err     error
}

// backgroundDetachedMsg is sent when the user comes back from a background
// session they had attached to.
type backgroundDetachedMsg struct{ err error }

// backgroundErrorMsg surfaces a background-agent failure that is not tied to a
// launch, as a toast.
type backgroundErrorMsg struct{ err error }

// backgroundAgentCmd returns the Cmd for asking issue's agent to run in the
// background: attaching to the session already running for it, or starting
// one. It is what `d` does, and what `a` does when the launch mode is
// background.
func (m Model) backgroundAgentCmd(issue *data.Issue) tea.Cmd {
	// One session per issue: a second agent on the same work is never what
	// the user meant, and attaching is how they get back to the first.
	if session, ok := m.bgAgents[issue.ID]; ok {
		return attachBackgroundCmd(session.ID)
	}
	deps := issue.EvaluateDependencies(m.detail.IssueMap, m.blockingTypes)
	prompt := agent.BuildPrompt(*issue, deps, m.detail.IssueMap)
	// A copy, taken now: the Cmd runs off the update loop, and the launch
	// reads the title and type to name and color the session.
	target, issueID, dir := *issue, issue.ID, m.projectDir
	return func() tea.Msg {
		session, err := agent.LaunchBackground(prompt, dir, target)
		if err != nil {
			return agentLaunchErrorMsg{issueID: issueID, err: err}
		}
		return backgroundLaunchedMsg{issueID: issueID, session: session}
	}
}

// attachBackgroundCmd hands the terminal to a background session, so the user
// can talk to it without leaving mg and without a second client.
func attachBackgroundCmd(sessionID string) tea.Cmd {
	c, err := agent.AttachBackground(sessionID)
	if err != nil {
		return func() tea.Msg { return backgroundErrorMsg{err: err} }
	}
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return backgroundDetachedMsg{err: err}
	})
}

// stopBackgroundCmd ends a background session on issue.
func stopBackgroundCmd(issueID, sessionID string) tea.Cmd {
	return func() tea.Msg {
		return backgroundStoppedMsg{issueID: issueID, err: agent.StopBackground(sessionID)}
	}
}

// setBackgroundAgents installs a registry scan, whether it came from the poll
// or from a launch. Unlike setActiveAgents it touches nothing the pane poll
// owns: the two are followed independently and neither may drop the other's
// agent from the views. It installs no agent output either, since a session has
// none to read — the detail panel names it and its state instead.
func (m *Model) setBackgroundAgents(agents map[string]agent.BackgroundSession) {
	m.bgAgents = agents
	m.propagateAgentState()
}

// handleBackgroundAgents absorbs a registry scan.
func (m Model) handleBackgroundAgents(msg backgroundAgentsMsg) (tea.Model, tea.Cmd) {
	m.bgScanInFlight = false
	m.bgScanned = true
	if msg.err != nil {
		// A failed scan says nothing about the sessions already known, so
		// they are kept until a scan that succeeds replaces them.
		return m, nil
	}
	m.setBackgroundAgents(msg.agents)
	return m, nil
}

// handleBackgroundLaunched records a session mg just started, so the issue
// shows its agent at once instead of on the next scan.
func (m Model) handleBackgroundLaunched(msg backgroundLaunchedMsg) (tea.Model, tea.Cmd) {
	agents := make(map[string]agent.BackgroundSession, len(m.bgAgents)+1)
	maps.Copy(agents, m.bgAgents)
	agents[msg.issueID] = msg.session
	m.setBackgroundAgents(agents)
	toast, cmd := components.ShowToast(
		fmt.Sprintf("Background agent %s started for %s", msg.session.ID, msg.issueID),
		components.ToastSuccess, toastDuration,
	)
	m.toast = toast
	return m, cmd
}

// handleBackgroundStopped drops a stopped session from the map. The daemon
// keeps it in the registry for a while, so mg does not wait for a scan to
// agree.
func (m Model) handleBackgroundStopped(msg backgroundStoppedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		toast, cmd := components.ShowToast(
			fmt.Sprintf("Stop failed: %s", msg.err),
			components.ToastError, toastDuration,
		)
		m.toast = toast
		return m, cmd
	}
	if _, ok := m.bgAgents[msg.issueID]; ok {
		agents := make(map[string]agent.BackgroundSession, len(m.bgAgents))
		maps.Copy(agents, m.bgAgents)
		delete(agents, msg.issueID)
		m.setBackgroundAgents(agents)
	}
	return m, nil
}

// handleBackgroundDetached continues after the user leaves a session they had
// attached to. It was free to change anything while they were in it, so the
// next scan must not wait out the TTL, and anything it wrote to beads should
// show up now rather than at the next poll.
func (m Model) handleBackgroundDetached(msg backgroundDetachedMsg) (tea.Model, tea.Cmd) {
	m.lastFileMod = time.Time{}
	refresh := m.requestRefresh()
	m.bgScanAt = time.Time{}
	return m, tea.Batch(refresh, m.gatedPollAgentState())
}

// handleBackgroundError surfaces a background-agent failure as a toast.
func (m Model) handleBackgroundError(msg backgroundErrorMsg) (tea.Model, tea.Cmd) {
	toast, cmd := components.ShowToast(
		fmt.Sprintf("Background agent: %s", msg.err),
		components.ToastError, toastDuration,
	)
	m.toast = toast
	return m, cmd
}

// gatedPollBackgroundAgents returns a Cmd that re-reads the session registry,
// or nil when there is nothing to watch, a scan is in flight, or the TTL has
// not elapsed.
//
// Watching is opt-in, since a scan costs a process launch. mg scans while the
// launch mode runs agents in the background, while it still knows of a live
// session, and once at startup — so a session started before this mg, or by
// another client, is still picked up — and stops as soon as a scan finds
// nothing to follow.
func (m *Model) gatedPollBackgroundAgents() tea.Cmd {
	if !m.bgAvail || m.bgScanInFlight || !m.watchBackground() {
		return nil
	}
	if !m.bgScanAt.IsZero() && time.Since(m.bgScanAt) < backgroundScanInterval {
		return nil
	}
	m.bgScanInFlight = true
	m.bgScanAt = time.Now()
	return pollBackgroundAgents(m.projectDir)
}

// sessionLabels describes each background session for the detail panel.
//
// A pane can be captured as text; a session cannot. `claude logs` prints the
// session's raw terminal stream, which is a differential repaint — cursor
// moves and erases, no line breaks at all — so what arrives is one long line of
// redraw noise rather than the conversation. mg therefore reports what the
// registry does say: which session is on the issue, and what it is doing. `a`
// on the issue attaches to it, which is how to watch it or talk to it.
func (m *Model) sessionLabels() map[string]string {
	if len(m.bgAgents) == 0 {
		return nil
	}
	labels := make(map[string]string, len(m.bgAgents))
	for issueID, session := range m.bgAgents {
		label := "session " + session.ID
		if note := sessionNote(session); note != "" {
			label += " · " + note
		}
		labels[issueID] = label
	}
	return labels
}

// sessionNote is the one thing worth saying about a session beyond its id.
//
// A session waiting on the user says so in `waitingFor` while its state stays
// the blunt "blocked", so that field is preferred: for someone reading this on
// a phone, "input needed" is the whole point of looking. Everything else falls
// back to the state, then to the coarser liveness.
func sessionNote(session agent.BackgroundSession) string {
	if session.WaitingFor != "" {
		return session.WaitingFor
	}
	if session.State != "" {
		return session.State
	}
	return session.Status
}

// watchBackground reports whether a background session could still exist: the
// mode starts them, mg has seen one, or mg has not looked yet. That last case
// is what makes the first scan run in every mode.
func (m *Model) watchBackground() bool {
	return m.agentMode.Background() || len(m.bgAgents) > 0 || !m.bgScanned
}

// pollBackgroundAgents reads the registry off the UI goroutine.
func pollBackgroundAgents(projectDir string) tea.Cmd {
	return func() tea.Msg {
		agents, err := agent.ListBackgroundAgents(projectDir)
		return backgroundAgentsMsg{agents: agents, err: err}
	}
}
