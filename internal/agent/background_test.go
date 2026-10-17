package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/matt-wright86/mardi-gras/internal/data"
)

// openIssue is the issue the launch tests start an agent for.
var openIssue = data.Issue{ID: "open-1", Title: "Fix the thing", IssueType: data.TypeBug}

// openSessionName is the name a launch for openIssue must use.
const openSessionName = "mg-open-1 · Fix the thing"

// fakeClaudeScript is a stand-in for the claude CLI. It records every
// invocation to $FAKE_CLAUDE_LOG, answers `agents --json` with the registry
// file the test wrote, and — the part that makes a launch testable — registers
// a `--bg` launch in the registry the way the daemon would, so the claim that
// follows finds it.
//
// It is deliberately written with shell builtins and absolute utility paths:
// PATH inside these tests holds the fake and nothing else.
const fakeClaudeScript = `#!/bin/sh
printf '%s\n' "$0 $*" >> "$FAKE_CLAUDE_LOG"
case "$1" in
agents)
  /bin/cat "$FAKE_CLAUDE_SESSIONS"
  ;;
--bg)
  if [ "$FAKE_CLAUDE_REGISTER" = "0" ]; then
    exit 0
  fi
  printf '[{"id":"%s","name":"%s","cwd":"%s","kind":"background","status":"busy","state":"working"}]\n' \
    "$FAKE_CLAUDE_ID" "$3" "$FAKE_CLAUDE_CWD" > "$FAKE_CLAUDE_SESSIONS"
  ;;
stop)
  if [ "$FAKE_CLAUDE_FAIL" = "stop" ]; then
    printf 'no such session\n' >&2
    exit 1
  fi
  ;;
esac
exit 0
`

// withFakeClaude rewrites PATH to a temp dir holding fakeClaudeScript as
// `claude`, and pins detection to that runtime, so the ambient seat cannot
// decide the result. It returns the dir, the call log, and the registry file
// the fake answers `agents --json` from (initially empty). The registry and
// call log are plain files: a test may overwrite either before calling in.
//
// CLAUDE_CONFIG_DIR points into the same dir, so a launch's color write lands
// there, never in the seat's own ~/.claude.
func withFakeClaude(t *testing.T, projectDir string) (dir, logPath, registryPath string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "config"))
	logPath = filepath.Join(dir, "calls.log")
	registryPath = filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(registryPath, []byte("[]\n"), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("MG_AGENT_RUNTIME", string(RuntimeClaude))
	t.Setenv(AgentCommandEnv, "")
	t.Setenv("FAKE_CLAUDE_LOG", logPath)
	t.Setenv("FAKE_CLAUDE_SESSIONS", registryPath)
	t.Setenv("FAKE_CLAUDE_BIN", bin)
	t.Setenv("FAKE_CLAUDE_ID", "bg-1")
	t.Setenv("FAKE_CLAUDE_CWD", projectDir)
	return dir, logPath, registryPath
}

// fakeCalls returns the invocations the fake recorded, one per line, as
// "<argv0> <args...>".
func fakeCalls(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read call log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
	}{
		{"", ModeAuto},
		{"auto", ModeAuto},
		{" AUTO ", ModeAuto},
		{"tmux", ModeTmux},
		{"exec", ModeExec},
		{"background", ModeBackground},
		{"Background", ModeBackground},
	}
	for _, c := range cases {
		got, err := ParseMode(c.in)
		if err != nil {
			t.Errorf("ParseMode(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A typo must be refused, not folded into a default: a launcher script that
	// misspells `background` would otherwise silently open panes instead.
	if _, err := ParseMode("backgroud"); err == nil { //nolint:misspell // the misspelling is the case under test
		t.Error("expected an unknown mode to be an error")
	} else if !strings.Contains(err.Error(), AgentModeEnv) {
		t.Errorf("error should name the variable it read, got %v", err)
	}
}

func TestConfiguredMode(t *testing.T) {
	t.Setenv(AgentModeEnv, "")
	if got, err := ConfiguredMode(); err != nil || got != ModeAuto {
		t.Errorf("unset mode = %q, %v; want auto, nil", got, err)
	}
	t.Setenv(AgentModeEnv, "background")
	if got, err := ConfiguredMode(); err != nil || got != ModeBackground {
		t.Errorf("configured mode = %q, %v; want background, nil", got, err)
	}
}

func TestModeBackground(t *testing.T) {
	if ModeBackground.Background() != true {
		t.Error("ModeBackground should report itself as background")
	}
	for _, m := range []Mode{ModeAuto, ModeTmux, ModeExec} {
		if m.Background() {
			t.Errorf("%q should not report itself as background", m)
		}
	}
}

func TestIssueFromSessionName(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"mg-open-1", "open-1", true},
		{"mg-bd-42", "bd-42", true},
		{"mg-open-1 · Fix the thing", "open-1", true},
		{"mg-infra-tq2m.3 · bug", "infra-tq2m.3", true},
		{"mg-", "", false},
		{"mg- · Fix the thing", "", false},
		{"backlog-82", "", false},
		{"", "", false},
		{"xmg-open-1", "", false},
	}
	for _, c := range cases {
		got, ok := issueFromSessionName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("issueFromSessionName(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestBackgroundSupportedNeedsClaude(t *testing.T) {
	withFakePath(t, "codex")
	if BackgroundSupported() {
		t.Error("expected background sessions to be unsupported on the codex runtime")
	}

	withFakePath(t, "claude")
	if !BackgroundSupported() {
		t.Error("expected background sessions to be supported with claude on PATH")
	}
}

// A wrapper is not a stand-in for Claude Code: the daemon hosts the session and
// the CLI reads the registry, so a seat configured with MG_AGENT_CMD but no
// claude underneath has no background sessions to offer.
func TestBackgroundSupportedNeedsClaudeEvenWithAWrapper(t *testing.T) {
	dir := withFakePath(t)
	t.Setenv(AgentCommandEnv, writeShim(t, dir, "agent-shim"))
	if DetectRuntime() != RuntimeClaude {
		t.Fatalf("precondition: a wrapper should detect as claude, got %q", DetectRuntime())
	}
	if BackgroundSupported() {
		t.Error("expected background sessions to be unsupported without a real claude binary")
	}
}

func TestListBackgroundAgentsFiltersTheRegistry(t *testing.T) {
	projectDir := t.TempDir()
	other := t.TempDir()
	_, _, registryPath := withFakeClaude(t, projectDir)
	registry := `[
	  {"id":"bg-1","name":"mg-open-1 · Fix the thing","cwd":"` + projectDir + `","kind":"background","status":"busy","state":"working"},
	  {"id":"bg-2","name":"mg-open-2","cwd":"` + projectDir + `","kind":"background","status":"idle","state":"blocked","waitingFor":"input needed"},
	  {"id":"i-1","name":"mg-open-3","cwd":"` + projectDir + `","kind":"interactive","status":"busy","state":"working"},
	  {"id":"bg-3","name":"mg-open-4","cwd":"` + other + `","kind":"background","status":"busy","state":"working"},
	  {"id":"bg-4","name":"backlog-82","cwd":"` + projectDir + `","kind":"background","status":"busy","state":"working"}
	]`
	if err := os.WriteFile(registryPath, []byte(registry), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	agents, err := ListBackgroundAgents(projectDir)
	if err != nil {
		t.Fatalf("ListBackgroundAgents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected only this project's mg- sessions, got %+v", agents)
	}
	if got := agents["open-1"]; got.ID != "bg-1" || got.State != "working" || got.Status != "busy" {
		t.Errorf("open-1 = %+v, want the bg-1 row's fields", got)
	}
	if got := agents["open-2"]; got.ID != "bg-2" || got.WaitingFor != "input needed" {
		t.Errorf("open-2 = %+v, want bg-2 and what it waits for", got)
	}
}

func TestListBackgroundAgentsReportsAFailedRead(t *testing.T) {
	projectDir := t.TempDir()
	_, _, registryPath := withFakeClaude(t, projectDir)
	// A registry that is not JSON stands in for any CLI failure: the error must
	// reach the caller rather than be read as "no sessions".
	if err := os.WriteFile(registryPath, []byte("not json\n"), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	if _, err := ListBackgroundAgents(projectDir); err == nil {
		t.Error("expected a malformed registry to be an error")
	}
}

func TestLaunchBackgroundNamesTheSessionForTheIssue(t *testing.T) {
	projectDir := t.TempDir()
	_, logPath, _ := withFakeClaude(t, projectDir)

	session, err := LaunchBackground("do the thing", projectDir, openIssue)
	if err != nil {
		t.Fatalf("LaunchBackground: %v", err)
	}
	if session.ID != "bg-1" || session.IssueID != "open-1" {
		t.Errorf("session = %+v, want the registered bg-1 session claimed for open-1", session)
	}

	calls := fakeCalls(t, logPath)
	if len(calls) == 0 {
		t.Fatal("expected the fake claude to have been invoked")
	}
	launch := calls[0]
	if !strings.HasSuffix(launch, " --bg --name "+openSessionName+" do the thing") {
		t.Errorf("launch argv = %q, want a --bg launch named %q", launch, openSessionName)
	}
}

// writeJobState creates the state file the daemon keeps for sessionID under
// the fake's CLAUDE_CONFIG_DIR, and returns its path.
func writeJobState(t *testing.T, sessionID, body string) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "jobs", sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	return path
}

// readJobState decodes a state file.
func readJobState(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return state
}

func TestLaunchBackgroundColorsTheSessionByType(t *testing.T) {
	projectDir := t.TempDir()
	withFakeClaude(t, projectDir)
	path := writeJobState(t, "bg-1", `{"id":"bg-1","name":"`+openSessionName+`","updatedAt":"2026-01-01T00:00:00.000Z"}`)

	if _, err := LaunchBackground("do the thing", projectDir, openIssue); err != nil {
		t.Fatalf("LaunchBackground: %v", err)
	}
	state := readJobState(t, path)
	if state["color"] != "red" {
		t.Errorf("color = %v, want red for a bug", state["color"])
	}
	if state["name"] != openSessionName {
		t.Errorf("name = %v, want the daemon's own field kept", state["name"])
	}
}

// Coloring is cosmetic: a session whose state file cannot be written is still
// a session mg started, and the launch must say so.
func TestLaunchBackgroundSucceedsWhenTheColorCannotBeSet(t *testing.T) {
	projectDir := t.TempDir()
	withFakeClaude(t, projectDir)
	writeJobState(t, "bg-1", "not json\n")

	session, err := LaunchBackground("do the thing", projectDir, openIssue)
	if err != nil {
		t.Fatalf("LaunchBackground: %v", err)
	}
	if session.ID != "bg-1" {
		t.Errorf("session = %+v, want the registered bg-1 session", session)
	}
}

func TestSessionName(t *testing.T) {
	cases := []struct {
		name  string
		issue data.Issue
		want  string
	}{
		{"title", openIssue, openSessionName},
		{"type when untitled", data.Issue{ID: "open-1", IssueType: data.TypeChore}, "mg-open-1 · chore"},
		{"custom type", data.Issue{ID: "open-1", IssueType: "decision"}, "mg-open-1 · decision"},
		{"id alone", data.Issue{ID: "open-1"}, "mg-open-1"},
		{"blank title", data.Issue{ID: "open-1", Title: " \t\n", IssueType: data.TypeTask}, "mg-open-1 · task"},
		{"one line", data.Issue{ID: "open-1", Title: "Fix\nthe\t\tthing\x1b[31m "}, "mg-open-1 · Fix the thing [31m"},
		{
			"long title",
			data.Issue{ID: "infra-tq2m", Title: "Rename the local groups after the slot they fill rather than the model"},
			"mg-infra-tq2m · Rename the local groups after…",
		},
		{
			"id too long for a label",
			data.Issue{ID: "an-issue-id-long-enough-to-leave-no-room-x", Title: "Fix the thing"},
			"mg-an-issue-id-long-enough-to-leave-no-room-x",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SessionName(c.issue); got != c.want {
				t.Errorf("SessionName = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSessionNameStaysWithinTheCap(t *testing.T) {
	titles := []string{
		strings.Repeat("x", 200),
		strings.Repeat("é", 200),
		strings.Repeat("word ", 40),
	}
	for _, title := range titles {
		issue := data.Issue{ID: "infra-tq2m.3", Title: title}
		name := SessionName(issue)
		if n := utf8.RuneCountInString(name); n > sessionNameMax {
			t.Errorf("SessionName(%.10q…) has %d runes, want at most %d", title, n, sessionNameMax)
		}
		if !strings.HasSuffix(name, "…") {
			t.Errorf("SessionName(%.10q…) = %q, want the cut marked", title, name)
		}
		if got, ok := issueFromSessionName(name); !ok || got != issue.ID {
			t.Errorf("issueFromSessionName(%q) = %q, %v; want the issue back", name, got, ok)
		}
	}
}

func TestSessionColor(t *testing.T) {
	known := map[data.IssueType]string{
		data.TypeBug:       "red",
		data.TypeFeature:   "green",
		data.TypeTask:      "blue",
		data.TypeEpic:      "purple",
		data.TypeChore:     "yellow",
		data.TypeSpike:     "orange",
		data.TypeStory:     "cyan",
		data.TypeMilestone: "pink",
	}
	for issueType, want := range known {
		if got := SessionColor(issueType); got != want {
			t.Errorf("SessionColor(%q) = %q, want %q", issueType, got, want)
		}
	}
	if got := SessionColor(""); got != "" {
		t.Errorf("SessionColor(\"\") = %q, want no color", got)
	}
}

// A custom type still gets one color for all its issues, from the palette the
// agent view can draw.
func TestSessionColorCustomType(t *testing.T) {
	for _, issueType := range []data.IssueType{"decision", "ledger", "incident"} {
		got := SessionColor(issueType)
		if !slices.Contains(sessionPalette, got) {
			t.Errorf("SessionColor(%q) = %q, not in the palette", issueType, got)
		}
		if again := SessionColor(issueType); again != got {
			t.Errorf("SessionColor(%q) = %q then %q, want it stable", issueType, got, again)
		}
	}
}

func TestSetSessionColor(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := writeJobState(t, "bg-1", `{"id":"bg-1","name":"mg-open-1","color":"blue","updatedAt":"2026-01-01T00:00:00.000Z","nested":{"a":[1,2]}}`)

	before := time.Now().UTC().Truncate(time.Millisecond)
	if err := setSessionColor("bg-1", "red"); err != nil {
		t.Fatalf("setSessionColor: %v", err)
	}
	state := readJobState(t, path)
	if state["color"] != "red" {
		t.Errorf("color = %v, want red", state["color"])
	}
	if state["name"] != "mg-open-1" || state["id"] != "bg-1" {
		t.Errorf("state = %v, want the other fields kept", state)
	}
	if nested, _ := json.Marshal(state["nested"]); string(nested) != `{"a":[1,2]}` {
		t.Errorf("nested = %s, want it kept as it was", nested)
	}
	updated, err := time.Parse("2006-01-02T15:04:05.000Z", state["updatedAt"].(string))
	if err != nil || updated.Before(before) {
		t.Errorf("updatedAt = %v, want a fresh timestamp in the daemon's format", state["updatedAt"])
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("state mode = %o, want 600", mode)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read job dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("job dir holds %d entries, want only state.json (no temp file left)", len(entries))
	}
}

// The state file is the daemon's: mg edits one it finds and never creates one.
func TestSetSessionColorLeavesAMissingStateAlone(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	if err := setSessionColor("bg-1", "red"); err == nil {
		t.Error("expected a missing state file to be an error")
	}
	if _, err := os.Stat(filepath.Join(config, "jobs")); !os.IsNotExist(err) {
		t.Errorf("expected nothing created under the config dir, stat = %v", err)
	}
}

func TestSetSessionColorRefusesAPathAsTheID(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	outside := filepath.Join(config, "state.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	for _, id := range []string{"", ".", "..", "../bg-1", "bg-1/../.."} {
		if err := setSessionColor(id, "red"); err == nil {
			t.Errorf("setSessionColor(%q) = nil, want it refused", id)
		}
	}
	if raw, _ := os.ReadFile(outside); string(raw) != `{}` {
		t.Errorf("decoy outside the jobs dir was rewritten: %s", raw)
	}
}

// The launch is the one background call that runs a model, so it is the one
// that must go through MG_AGENT_CMD: the wrapper is what decides which door and
// which credentials the agent gets.
func TestLaunchBackgroundRunsThroughTheWrapper(t *testing.T) {
	projectDir := t.TempDir()
	dir, logPath, _ := withFakeClaude(t, projectDir)
	wrapper := filepath.Join(dir, "wrapper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"$FAKE_CLAUDE_LOG\"\nexec \"$FAKE_CLAUDE_BIN\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	t.Setenv(AgentCommandEnv, wrapper)

	if _, err := LaunchBackground("do the thing", projectDir, openIssue); err != nil {
		t.Fatalf("LaunchBackground: %v", err)
	}
	calls := fakeCalls(t, logPath)
	if len(calls) < 2 {
		t.Fatalf("expected the wrapper then claude to be invoked, got %q", calls)
	}
	if !strings.HasPrefix(calls[0], wrapper+" --bg --name "+openSessionName) {
		t.Errorf("first invocation = %q, want the wrapper running the launch", calls[0])
	}
}

// Attach, logs and stop address a session that already exists and start no
// model, so they go to the runtime binary itself: a wrapper would only be
// handed flags its subcommand has no use for.
func TestAttachAndStopBypassTheWrapper(t *testing.T) {
	projectDir := t.TempDir()
	dir, logPath, _ := withFakeClaude(t, projectDir)
	wrapper := filepath.Join(dir, "wrapper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	t.Setenv(AgentCommandEnv, wrapper)

	cmd, err := AttachBackground("bg-1")
	if err != nil {
		t.Fatalf("AttachBackground: %v", err)
	}
	if cmd.Args[0] == wrapper {
		t.Errorf("attach went through the wrapper: %q", cmd.Args)
	}
	if err := StopBackground("bg-1"); err != nil {
		t.Fatalf("StopBackground: %v", err)
	}
	calls := fakeCalls(t, logPath)
	if len(calls) != 1 || !strings.HasSuffix(calls[0], " stop bg-1") {
		t.Fatalf("expected one bare stop call, got %q", calls)
	}
}

func TestLaunchBackgroundRefusesWithoutClaude(t *testing.T) {
	withFakePath(t, "codex")
	if _, err := LaunchBackground("do the thing", t.TempDir(), openIssue); err == nil {
		t.Fatal("expected a launch without claude to fail")
	} else if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should name the runtime it needs, got %v", err)
	}
}

func TestLaunchBackgroundFailsClosedWithoutARegistryEntry(t *testing.T) {
	projectDir := t.TempDir()
	_, logPath, _ := withFakeClaude(t, projectDir)
	// The launch exits 0 but registers nothing, which is what a daemon that
	// dropped the session looks like from here.
	t.Setenv("FAKE_CLAUDE_REGISTER", "0")
	restore := backgroundClaimTimeout
	backgroundClaimTimeout = 100 * time.Millisecond
	defer func() { backgroundClaimTimeout = restore }()

	if _, err := LaunchBackground("do the thing", projectDir, openIssue); err == nil {
		t.Fatal("expected a launch with no session in the registry to fail")
	} else if !strings.Contains(err.Error(), openSessionName) {
		t.Errorf("error should name the session that never appeared, got %v", err)
	}
	if calls := fakeCalls(t, logPath); len(calls) < 1 {
		t.Error("expected the launch to have been attempted")
	}
}

func TestLaunchBackgroundSurfacesStderr(t *testing.T) {
	projectDir := t.TempDir()
	dir, _, _ := withFakeClaude(t, projectDir)
	// A launch that fails outright: claude refuses an untrusted workspace on
	// stderr, and that message is the whole diagnosis.
	failing := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf 'Workspace not trusted\\n' >&2\nexit 1\n"
	if err := os.WriteFile(failing, []byte(script), 0o755); err != nil {
		t.Fatalf("write failing claude: %v", err)
	}
	_, err := LaunchBackground("do the thing", projectDir, openIssue)
	if err == nil {
		t.Fatal("expected a failing launch to be an error")
	}
	if !strings.Contains(err.Error(), "Workspace not trusted") {
		t.Errorf("error should carry the CLI's own message, got %v", err)
	}
}

func TestStopBackgroundSurfacesTheCLIMessage(t *testing.T) {
	projectDir := t.TempDir()
	withFakeClaude(t, projectDir)
	t.Setenv("FAKE_CLAUDE_FAIL", "stop")
	err := StopBackground("bg-1")
	if err == nil {
		t.Fatal("expected a failing stop to be an error")
	}
	if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("error should carry the CLI's own message, got %v", err)
	}
}
