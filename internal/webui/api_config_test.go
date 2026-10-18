//go:build !no_webui

package webui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Exercise the shipped page script, including its actual DOM IDs and install
// selector, so HTTPS/IPv6 and scope changes are checked as generated output.
func TestAPIPageQuickConfig(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the API configuration page")
	}
	page, err := readAsset("api.html")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pagePath := filepath.Join(dir, "api.html")
	if err := os.WriteFile(pagePath, []byte(page), 0600); err != nil {
		t.Fatal(err)
	}
	probe := `
const fs = require('fs');
const vm = require('vm');
const html = fs.readFileSync(process.argv[2], 'utf8');
const ids = [...html.matchAll(/\bid="([^"]+)"/g)].map(m => m[1]);
const script = html.slice(html.lastIndexOf('<script>') + 8, html.lastIndexOf('</script>'));
const results = [];
for (const origin of [
  'http://127.0.0.1:18765', 'http://localhost:19000',
  'https://termcp.example', 'https://[2001:db8::1]:9443'
]) {
  const els = Object.fromEntries(ids.map(id => [id, {
    value: id === 'skill-agent' ? 'claude' : '', textContent: '', listeners: {},
    addEventListener(event, callback) { this.listeners[event] = callback; }
  }]));
  const sandbox = {
    location: new URL(origin),
    document: { getElementById: id => els[id] || null, querySelectorAll: () => [] }
  };
  vm.runInNewContext(script, sandbox);
  const installs = {};
  for (const target of ['claude', 'codex', 'claude-project', 'codex-project']) {
    els['skill-agent'].value = target;
    els['skill-agent'].listeners.change();
    installs[target] = {
      bash: els['cfg-skill-install'].textContent,
      powershell: els['cfg-skill-install-powershell'].textContent
    };
  }
  results.push({
    origin, installs,
    values: Object.fromEntries(Object.entries(els).map(([id, el]) => [id, el.textContent]))
  });
}
process.stdout.write(JSON.stringify(results));
`
	probePath := filepath.Join(dir, "probe.cjs")
	if err := os.WriteFile(probePath, []byte(probe), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, probePath, pagePath).CombinedOutput()
	if err != nil {
		t.Fatalf("API page script: %v\n%s", err, out)
	}
	var results []struct {
		Origin   string
		Values   map[string]string
		Installs map[string]struct{ Bash, Powershell string }
	}
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		t.Run(result.Origin, func(t *testing.T) {
			endpoint := result.Origin + "/stream"
			if got := result.Values["url-skill"]; got != result.Origin+"/SKILL.md" {
				t.Errorf("skill URL = %q", got)
			}
			var config struct {
				Servers map[string]struct {
					Command string
					Args    []string
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal([]byte(result.Values["cfg-stdio-json"]), &config); err != nil {
				t.Fatal(err)
			}
			server := config.Servers["termcp"]
			if server.Command != "termcp" || !reflect.DeepEqual(server.Args, []string{"stdio", endpoint}) {
				t.Errorf("bridge config = %+v; want a bridge to %s", server, endpoint)
			}
			for _, id := range []string{"cfg-stream-cli", "cfg-stdio-cli", "cfg-codex-cli", "cfg-codex-auth-cli", "cfg-codex-stdio-cli"} {
				if !strings.Contains(result.Values[id], "'"+endpoint+"'") {
					t.Errorf("%s lost the instance endpoint: %s", id, result.Values[id])
				}
			}
			var auth struct {
				Servers map[string]struct {
					Headers map[string]string
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal([]byte(result.Values["cfg-stream-auth-json"]), &auth); err != nil {
				t.Fatal(err)
			}
			if got := auth.Servers["termcp"].Headers["Authorization"]; got != "Bearer ${TERMCP_AUTH_TOKEN}" {
				t.Errorf("auth header must reference the agent's environment, got %q", got)
			}
			for target, install := range result.Installs {
				folder := ".claude/skills/termcp"
				if strings.HasPrefix(target, "codex") {
					folder = ".agents/skills/termcp"
				}
				wantBash := "$HOME/" + folder
				wantPS := "Join-Path $HOME '" + folder + "'"
				if strings.HasSuffix(target, "-project") {
					wantBash = "./" + folder
					wantPS = "Join-Path (Get-Location).Path '" + folder + "'"
				}
				if !strings.Contains(install.Bash, wantBash) || !strings.Contains(install.Powershell, wantPS) {
					t.Errorf("%s installed into the wrong scope:\n%s\n%s", target, install.Bash, install.Powershell)
				}
			}
		})
	}

	// Run the generated project installer with a fake curl. A partial download
	// must leave the installed skill intact and clean up its temporary file.
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Log("bash not on PATH; skipping installer execution")
		return
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful_download", true: "failed_download"}[fail], func(t *testing.T) {
			dir := t.TempDir()
			skillDir := filepath.Join(dir, ".agents", "skills", "termcp")
			if err := os.MkdirAll(skillDir, 0700); err != nil {
				t.Fatal(err)
			}
			skillPath := filepath.Join(skillDir, "SKILL.md")
			if err := os.WriteFile(skillPath, []byte("existing skill"), 0600); err != nil {
				t.Fatal(err)
			}
			curl := "curl() { while [ \"$1\" != '-o' ]; do shift; done; shift; printf 'downloaded skill' > \"$1\";"
			if fail {
				curl += " return 22;"
			}
			curl += " }\n"
			cmd := exec.Command(bash, "-c", curl+results[0].Installs["codex-project"].Bash)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if fail != (err != nil) {
				t.Fatalf("installer error = %v (failed download: %v): %s", err, fail, out)
			}
			got, err := os.ReadFile(skillPath)
			if err != nil {
				t.Fatal(err)
			}
			want := "downloaded skill"
			if fail {
				want = "existing skill"
			}
			if string(got) != want {
				t.Errorf("installed skill = %q, want %q", got, want)
			}
			entries, err := os.ReadDir(skillDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "SKILL.md" {
				t.Errorf("installer left temporary files: %v", entries)
			}
		})
	}
}
