package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/matt-wright86/mardi-gras/internal/data"
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

// sessionNameMax caps a background session's name, in runes. The agent view
// widens its name column to the longest name and truncates past about 54
// columns on its own; staying under that keeps the cut where mg puts it, after
// the issue ID, instead of wherever the view's falls.
const sessionNameMax = 48

// sessionNameSep separates the issue ID from the scope label in a session
// name. An issue ID never contains a space, so the first space ends it.
const sessionNameSep = " · "

// SessionName returns the name a background session for issue is started
// with: mg-<issueID>, as for a tmux pane, followed by a short scope label so
// sessions can be told apart in `claude agents`. The label is the issue's
// title, or its type when the title is empty.
//
// The label is cut to keep the name within sessionNameMax. The ID is never
// cut, since it is what ties the session back to the issue: an ID too long to
// leave room for a label gets no label.
func SessionName(issue data.Issue) string {
	name := WindowName(issue.ID)
	label := cleanLabel(issue.Title)
	if label == "" {
		label = cleanLabel(string(issue.IssueType))
	}
	room := sessionNameMax - utf8.RuneCountInString(name) - utf8.RuneCountInString(sessionNameSep)
	if label == "" || room < 4 {
		return name
	}
	return name + sessionNameSep + truncateLabel(label, room)
}

// cleanLabel makes issue text safe to show as one line of a session list:
// control characters (newlines, tabs, escape sequences) become spaces,
// invisible formatting characters are dropped, and runs of space collapse.
func cleanLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// truncateLabel cuts s to at most n runes, marking the cut with an ellipsis.
// The cut ends on a whole word unless that would give up more than half the
// room.
func truncateLabel(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	cut := runes[:n-1]
	if !unicode.IsSpace(runes[n-1]) {
		for i := len(cut) - 1; i >= len(cut)/2; i-- {
			if unicode.IsSpace(cut[i]) {
				cut = cut[:i]
				break
			}
		}
	}
	return strings.TrimRightFunc(string(cut), unicode.IsSpace) + "…"
}

// sessionPalette is the set of colors the agent view can draw a session in:
// the names Claude Code's /color command takes.
var sessionPalette = []string{"red", "blue", "green", "yellow", "purple", "orange", "pink", "cyan"}

// SessionColor returns the color a background session for an issue of type t
// is drawn in. It follows the type, so the agent view shows at a glance what
// kind of work each session is on; the name tells sessions of one type apart.
//
// A type mg does not know (beads allows custom ones) gets a color derived
// from its name, so every issue of that type still shares one. An empty type
// gets none, and the session keeps the view's default.
func SessionColor(t data.IssueType) string {
	switch t {
	case data.TypeBug:
		return "red"
	case data.TypeFeature:
		return "green"
	case data.TypeTask:
		return "blue"
	case data.TypeEpic:
		return "purple"
	case data.TypeChore:
		return "yellow"
	case data.TypeSpike:
		return "orange"
	case data.TypeStory:
		return "cyan"
	case data.TypeMilestone:
		return "pink"
	case "":
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(t))
	return sessionPalette[h.Sum32()%uint32(len(sessionPalette))]
}

// claudeJobsDir is where Claude Code keeps each background session's state,
// one directory per short session ID.
func claudeJobsDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "jobs"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "jobs"), nil
}

// setSessionColor colors a background session in the agent view.
//
// Claude Code has no flag or subcommand for it. The view draws each session
// in the `color` recorded in the session's state file, which /color writes
// from inside the session; mg writes the same field from outside, once,
// after the claim. The field survives the daemon's own rewrites of the file.
// The file is replaced whole (a temp file renamed over it), so a concurrent
// reader never sees it half-written, and it is never created: a session
// whose state file is missing is left alone.
func setSessionColor(sessionID, color string) error {
	if sessionID == "" || sessionID != filepath.Base(sessionID) || sessionID == "." || sessionID == ".." {
		return fmt.Errorf("session id %q is not a job directory name", sessionID)
	}
	jobs, err := claudeJobsDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(jobs, sessionID)
	path := filepath.Join(dir, "state.json")
	raw, err := os.ReadFile(path) //nolint:gosec // path is the jobs dir plus a single-segment session id, checked above
	if err != nil {
		return err
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if state == nil {
		return fmt.Errorf("%s: not a JSON object", path)
	}
	for field, value := range map[string]string{
		"color":     color,
		"updatedAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		state[field] = encoded
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(state); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state.*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LaunchBackground starts issue's agent as a daemon-hosted background
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
// managed at all. Coloring the session is cosmetic, so a color that cannot be
// set leaves the launch successful.
func LaunchBackground(prompt, projectDir string, issue data.Issue) (BackgroundSession, error) {
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
	// starts the way a tmux pane's does, mg-<issueID>; the scope label after
	// it is for the people reading the agent view.
	name := SessionName(issue)
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
	session, err := claimBackgroundSession(projectDir, issue.ID, name)
	if err != nil {
		return BackgroundSession{}, err
	}
	if color := SessionColor(issue.IssueType); color != "" {
		_ = setSessionColor(session.ID, color)
	}
	return session, nil
}

// claimBackgroundSession finds the session a launch named name just created
// for issueID, waiting for the daemon to publish it.
func claimBackgroundSession(projectDir, issueID, name string) (BackgroundSession, error) {
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
				"started %s but it never appeared in `claude agents`", name)
		}
		time.Sleep(backgroundClaimInterval)
	}
}

// ListBackgroundAgents returns mg's background sessions for projectDir, keyed
// by issue ID.
//
// A session counts as mg's when it is daemon-hosted, its working directory is
// projectDir, and its name starts mg-<issueID> (see SessionName). Only that
// first word is read, so a scope label that no longer matches the issue's
// title changes nothing. The name is still the only link back to an issue, so
// a session renamed in the agent view (ctrl+r) to something else drops out of
// the map — mg stops showing it, and starts a fresh session for the issue if
// asked again.
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
// that the session is not one of mg's. The ID runs from the mg- prefix to the
// first space, so a name with a scope label and a bare mg-<issueID> from
// before labels both resolve.
func issueFromSessionName(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "mg-")
	if !ok {
		return "", false
	}
	issueID, _, _ := strings.Cut(rest, " ")
	if issueID == "" {
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
