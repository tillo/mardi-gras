package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Mode selects how a launched agent runs. It is the value of --agent-mode /
// MG_AGENT_MODE.
type Mode string

const (
	// ModeAuto keeps mg's historical behaviour: a tmux split pane when mg
	// runs inside tmux, a foreground session in this terminal otherwise.
	ModeAuto Mode = "auto"
	// ModeTmux always opens a split pane. It fails outside tmux.
	ModeTmux Mode = "tmux"
	// ModeExec always hands this terminal to a foreground session.
	ModeExec Mode = "exec"
	// ModeBackground starts the agent as a background session on Claude
	// Code's multi-agent daemon instead of in a pane: it keeps running when
	// mg exits, and mg follows it through the session registry.
	ModeBackground Mode = "background"
)

// AgentModeEnv names the launch mode, so a shell profile or a launcher script
// can set it the way --agent-mode does.
const AgentModeEnv = "MG_AGENT_MODE"

// ParseMode validates a mode value. An empty value means ModeAuto.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeTmux, ModeExec, ModeBackground:
		return m, nil
	default:
		return "", fmt.Errorf("%s=%q is not one of %s, %s, %s, %s",
			AgentModeEnv, s, ModeAuto, ModeTmux, ModeExec, ModeBackground)
	}
}

// ConfiguredMode returns the launch mode from MG_AGENT_MODE.
func ConfiguredMode() (Mode, error) {
	return ParseMode(os.Getenv(AgentModeEnv))
}

// Background reports whether this mode launches agents as background sessions.
func (m Mode) Background() bool {
	return m == ModeBackground
}

// Background sessions are Claude Code's daemon-hosted agents: `claude --bg`
// starts one and returns immediately, the session outlives the process that
// started it, and `claude agents --json` is the registry that lists them
// (interactive sessions are listed there too, with a different kind).
//
// Only Claude Code has them. For every other runtime BackgroundSupported
// reports false and the launch paths stay on panes and foreground sessions.
const (
	// backgroundKind is the registry's `kind` for a daemon-hosted session.
	backgroundKind = "background"

	// backgroundLaunchTimeout bounds one `claude --bg`, which returns as soon
	// as the daemon has claimed the session.
	backgroundLaunchTimeout = 30 * time.Second
	// backgroundListTimeout bounds one registry read.
	backgroundListTimeout = 5 * time.Second
	// backgroundStopTimeout bounds one `claude stop`.
	backgroundStopTimeout = 10 * time.Second
)

// How long a launch waits for its session to appear in the registry before
// declaring the launch failed, and how often it looks while it waits. These
// are variables so tests can shorten a wait that is otherwise indistinguishable
// from a hang.
var (
	backgroundClaimTimeout  = 10 * time.Second
	backgroundClaimInterval = 250 * time.Millisecond
)

// BackgroundSession is one live daemon-hosted agent session.
type BackgroundSession struct {
	// IssueID is the beads issue the session was started for, recovered from
	// the session's name.
	IssueID string
	// ID is the short session id `claude attach`, `logs` and `stop` take.
	ID string
	// State is the session's own state: "working", "done", "blocked".
	State string
	// Status is the coarser liveness the agent view groups by: "busy",
	// "idle", "waiting".
	Status string
	// WaitingFor says what a blocked session is blocked on, e.g. "input
	// needed". It is set only while the session is waiting, and is the one
	// field that tells a user who is not watching the session that it needs
	// them.
	WaitingFor string
}

// registrySession mirrors the fields mg reads from `claude agents --json`.
type registrySession struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Cwd        string `json:"cwd"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	State      string `json:"state"`
	WaitingFor string `json:"waitingFor"`
}

// BackgroundSupported reports whether agents can be launched as background
// sessions here: the detected runtime must be Claude Code, and its own binary
// must be resolvable.
//
// A wrapper alone is not enough, however MG_AGENT_CMD is configured. Background
// sessions are hosted by Claude Code's daemon and the registry is read through
// its CLI, so there has to be a real Claude Code underneath.
func BackgroundSupported() bool {
	if DetectRuntime() != RuntimeClaude {
		return false
	}
	_, err := runtimeBinary(RuntimeClaude)
	return err == nil
}

// runtimeBinary resolves a runtime's own binary to an absolute path, ignoring
// any MG_AGENT_CMD wrapper.
//
// The calls built on it — attach, logs, stop — address a session that already
// exists and launch no agent of their own, so there is nothing for a wrapper
// to route: it would only prepend flags its subcommand has no use for.
func runtimeBinary(rt Runtime) (string, error) {
	bin := string(rt)
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH: %w", bin, err)
	}
	return filepath.Abs(p)
}

// LaunchBackground starts issueID's agent as a daemon-hosted background
// session, and returns it once the daemon has registered it.
//
// The prompt and the flags are the launcher's, as on every other path: the
// binary is agentCommand, so MG_AGENT_CMD still decides what actually runs.
// The session is started with its working directory set to projectDir for two
// reasons: Claude Code only runs a background session in a workspace it
// trusts, and trust is per directory.
//
// It fails closed. A launch that reports success but leaves no session in the
// registry is an error, not a silently absent agent: the session id is the
// only handle attach and stop have, so an unregistered session cannot be
// managed at all.
func LaunchBackground(prompt, projectDir, issueID string) (BackgroundSession, error) {
	if !BackgroundSupported() {
		rt := DetectRuntime()
		if rt == "" {
			return BackgroundSession{}, fmt.Errorf("background sessions need %s on PATH (no agent runtime found)",
				RuntimeClaude)
		}
		return BackgroundSession{}, fmt.Errorf("background sessions need %s: the %s runtime has none",
			RuntimeClaude, rt.RuntimeLabel())
	}
	bin, err := agentCommand(RuntimeClaude)
	if err != nil {
		return BackgroundSession{}, err
	}

	// The name is the only thing tying a session back to an issue, so it
	// follows the same convention as a tmux pane: mg-<issueID>.
	name := WindowName(issueID)
	ctx, cancel := context.WithTimeout(context.Background(), backgroundLaunchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--bg", "--name", name, prompt) //nolint:gosec // bin is the resolved agent or the user's own MG_AGENT_CMD
	cmd.Dir = projectDir
	// stdout carries the launch summary and the session id in a form meant
	// for people; the registry is the interface meant for programs, so the id
	// is read from there instead (see claimBackgroundSession).
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return BackgroundSession{}, fmt.Errorf("claude --bg: %s", msg)
		}
		return BackgroundSession{}, fmt.Errorf("claude --bg: %w", err)
	}
	return claimBackgroundSession(projectDir, issueID)
}

// claimBackgroundSession finds the session a launch just created, waiting for
// the daemon to publish it.
func claimBackgroundSession(projectDir, issueID string) (BackgroundSession, error) {
	deadline := time.Now().Add(backgroundClaimTimeout)
	for {
		sessions, err := ListBackgroundAgents(projectDir)
		if err == nil {
			if session, ok := sessions[issueID]; ok {
				return session, nil
			}
		}
		if time.Now().After(deadline) {
			return BackgroundSession{}, fmt.Errorf(
				"started %s but it never appeared in `claude agents`", WindowName(issueID))
		}
		time.Sleep(backgroundClaimInterval)
	}
}

// ListBackgroundAgents returns mg's background sessions for projectDir, keyed
// by issue ID.
//
// A session counts as mg's when it is daemon-hosted, its working directory is
// projectDir, and its name carries the mg- issue prefix. That name is the only
// link back to an issue, so a session renamed in the agent view (ctrl+r) drops
// out of the map — mg stops showing it, and starts a fresh session for the
// issue if asked again.
func ListBackgroundAgents(projectDir string) (map[string]BackgroundSession, error) {
	sessions, err := listRegistrySessions(projectDir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Clean(projectDir)
	agents := make(map[string]BackgroundSession)
	for _, s := range sessions {
		if s.Kind != backgroundKind || filepath.Clean(s.Cwd) != dir {
			continue
		}
		issueID, ok := issueFromSessionName(s.Name)
		if !ok {
			continue
		}
		agents[issueID] = BackgroundSession{
			IssueID:    issueID,
			ID:         s.ID,
			State:      s.State,
			Status:     s.Status,
			WaitingFor: s.WaitingFor,
		}
	}
	return agents, nil
}

// listRegistrySessions reads the whole session registry. It is run from dir so
// the CLI resolves the same workspace mg is looking at.
func listRegistrySessions(dir string) ([]registrySession, error) {
	bin, err := runtimeBinary(RuntimeClaude)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "agents", "--json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("claude agents --json: %w", err)
	}
	var sessions []registrySession
	if err := json.Unmarshal(out, &sessions); err != nil {
		return nil, fmt.Errorf("claude agents --json: %w", err)
	}
	return sessions, nil
}

// issueFromSessionName recovers the issue ID from a session name, or reports
// that the session is not one of mg's.
func issueFromSessionName(name string) (string, bool) {
	issueID, ok := strings.CutPrefix(name, "mg-")
	if !ok || issueID == "" {
		return "", false
	}
	return issueID, true
}

// AttachBackground returns the command that opens a background session in this
// terminal, as `claude attach` does. It returns when the user detaches, which
// leaves the session running.
func AttachBackground(sessionID string) (*exec.Cmd, error) {
	bin, err := runtimeBinary(RuntimeClaude)
	if err != nil {
		return nil, err
	}
	return exec.Command(bin, "attach", sessionID), nil //nolint:gosec // bin is the resolved runtime binary, sessionID one mg read from the registry
}

// StopBackground ends a background session. Its conversation is kept, so the
// session can be attached again afterwards.
func StopBackground(sessionID string) error {
	bin, err := runtimeBinary(RuntimeClaude)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundStopTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "stop", sessionID).CombinedOutput() //nolint:gosec // see AttachBackground
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("claude stop: %s", msg)
		}
		return fmt.Errorf("claude stop: %w", err)
	}
	return nil
}
