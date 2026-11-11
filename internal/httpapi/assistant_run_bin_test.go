package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/runtime"
)

// A chat turn on Claude Code installs the CLI on first use instead of failing
// with "服务器未安装 Claude Code CLI".
func TestResolveAssistantRunBinInstallsClaudeCode(t *testing.T) {
	if fileExecutable("/usr/local/bin/claude") || fileExecutable("/usr/bin/claude") {
		t.Skip("a system-wide claude is installed")
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "npm.log")
	npm := `#!/bin/sh
printf '%s\n' "$*" >> "$NPM_TEST_LOG"
prefix=$3
mkdir -p "$prefix/node_modules/@anthropic-ai/claude-code/bin" "$prefix/node_modules/.bin"
printf '#!/bin/sh\necho ok\n' > "$prefix/node_modules/@anthropic-ai/claude-code/bin/claude.exe"
chmod 755 "$prefix/node_modules/@anthropic-ai/claude-code/bin/claude.exe"
ln -s ../@anthropic-ai/claude-code/bin/claude.exe "$prefix/node_modules/.bin/claude"
`
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(npm), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENTFLOW_CLAUDE_BIN", "")
	t.Setenv("AGENTFLOW_MANAGED_CLI_ROOT", "")
	t.Setenv("NPM_TEST_LOG", log)
	t.Setenv("AGENTFLOW_RUNTIME_CLI_DIR", "")
	if _, err := resolveAssistantRunBin(context.Background(), assistantEngineClaude); !errors.As(err, new(assistantCLIMissingError)) {
		t.Fatalf("without an install dir = %v", err)
	}
	t.Setenv("AGENTFLOW_RUNTIME_CLI_DIR", t.TempDir())
	path, err := resolveAssistantRunBin(context.Background(), assistantEngineClaude)
	if err != nil || !strings.HasSuffix(path, "claude.exe") {
		t.Fatalf("install = %q, %v", path, err)
	}
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "@anthropic-ai/claude-code@"+runtime.ClaudeCodeVersion) {
		t.Fatalf("npm calls: %s", calls)
	}
	if _, err := resolveAssistantRunBin(context.Background(), assistantEngineCodex); err == nil {
		t.Fatal("codex must not be installed implicitly")
	}
}
