package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
func withFakeClaude(t *testing.T, projectDir string) (dir, logPath, registryPath string) {
	t.Helper()
	dir = t.TempDir()
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
		{"mg-", "", false},
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
	  {"id":"bg-1","name":"mg-open-1","cwd":"` + projectDir + `","kind":"background","status":"busy","state":"working"},
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

	session, err := LaunchBackground("do the thing", projectDir, "open-1")
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
	if !strings.HasSuffix(launch, " --bg --name mg-open-1 do the thing") {
		t.Errorf("launch argv = %q, want a --bg launch named mg-open-1", launch)
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

	if _, err := LaunchBackground("do the thing", projectDir, "open-1"); err != nil {
		t.Fatalf("LaunchBackground: %v", err)
	}
	calls := fakeCalls(t, logPath)
	if len(calls) < 2 {
		t.Fatalf("expected the wrapper then claude to be invoked, got %q", calls)
	}
	if !strings.HasPrefix(calls[0], wrapper+" --bg --name mg-open-1") {
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
	if _, err := LaunchBackground("do the thing", t.TempDir(), "open-1"); err == nil {
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

	if _, err := LaunchBackground("do the thing", projectDir, "open-1"); err == nil {
		t.Fatal("expected a launch with no session in the registry to fail")
	} else if !strings.Contains(err.Error(), "mg-open-1") {
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
	_, err := LaunchBackground("do the thing", projectDir, "open-1")
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
