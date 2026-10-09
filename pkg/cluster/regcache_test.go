package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestRegistryCacheNameIsOutsideClusterNamespace(t *testing.T) {
	// Cluster commands own every container under "kiac-"; the shared
	// cache must never be listed, resumed, or deleted as a cluster node.
	if strings.HasPrefix(RegistryCacheName, "kiac-") {
		t.Fatalf("RegistryCacheName %q collides with cluster container prefix kiac-", RegistryCacheName)
	}
}

func TestZotCacheConfig(t *testing.T) {
	var cfg struct {
		HTTP struct {
			Port   string   `json:"port"`
			Compat []string `json:"compat"`
		} `json:"http"`
		Storage struct {
			RootDirectory string `json:"rootDirectory"`
			Dedupe        bool   `json:"dedupe"`
		} `json:"storage"`
		Extensions struct {
			Sync struct {
				Enable     bool `json:"enable"`
				Registries []struct {
					URLs     []string `json:"urls"`
					OnDemand bool     `json:"onDemand"`
					Content  []struct {
						Prefix      string `json:"prefix"`
						Destination string `json:"destination"`
					} `json:"content"`
				} `json:"registries"`
			} `json:"sync"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal([]byte(zotCacheConfig()), &cfg); err != nil {
		t.Fatalf("zot config is not valid JSON: %v", err)
	}
	if cfg.HTTP.Port != "5000" {
		t.Errorf("port = %q, want 5000", cfg.HTTP.Port)
	}
	// Without docker2s2, zot refuses Docker-format manifest lists, which
	// most docker.io and quay.io images still use.
	if !slices.Contains(cfg.HTTP.Compat, "docker2s2") {
		t.Errorf("http.compat = %q, want docker2s2", cfg.HTTP.Compat)
	}
	if !cfg.Extensions.Sync.Enable {
		t.Fatal("sync extension disabled")
	}
	got := map[string]string{}
	for _, r := range cfg.Extensions.Sync.Registries {
		if !r.OnDemand || len(r.URLs) != 1 || len(r.Content) != 1 || r.Content[0].Prefix != "**" {
			t.Errorf("registry %+v is not a single on-demand catch-all upstream", r)
			continue
		}
		got[strings.TrimPrefix(r.Content[0].Destination, "/")] = r.URLs[0]
	}
	want := map[string]string{
		"docker.io":       "https://registry-1.docker.io",
		"registry.k8s.io": "https://registry.k8s.io",
		"ghcr.io":         "https://ghcr.io",
		"quay.io":         "https://quay.io",
	}
	for host, url := range want {
		if got[host] != url {
			t.Errorf("upstream %s = %q, want %q", host, got[host], url)
		}
	}
}

func TestKubeadmMirrorScript(t *testing.T) {
	script := kubeadmMirrorScript("192.168.64.9")
	for _, want := range []string{
		`config_path = "/etc/containerd/certs.d"`,
		"/etc/containerd/certs.d/docker.io/hosts.toml",
		`server = "https://registry-1.docker.io"`,
		`[host."http://kiac.registry-cache:5000/v2/docker.io"]`,
		`[host."http://kiac.registry-cache:5000/v2/registry.k8s.io"]`,
		`[host."http://kiac.registry-cache:5000/v2/quay.io"]`,
		`[host."http://kiac.registry-cache:5000/v2/ghcr.io"]`,
		"192.168.64.9 kiac.registry-cache",
		"override_path = true",
		`capabilities = ["pull", "resolve"]`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("mirror script missing %q:\n%s", want, script)
		}
	}
}

func TestK3sRegistriesYAML(t *testing.T) {
	y := k3sRegistriesYAML()
	for _, want := range []string{
		"mirrors:",
		`  "docker.io":`,
		`      - "http://kiac.registry-cache:5000"`,
		`      "^(.*)$": "docker.io/$1"`,
		`  "registry.k8s.io":`,
		`      "^(.*)$": "registry.k8s.io/$1"`,
	} {
		if !strings.Contains(y, want) {
			t.Errorf("registries.yaml missing %q:\n%s", want, y)
		}
	}
}

func TestK3sRunOptsCarryRegistryMirror(t *testing.T) {
	cfg := Config{Name: "dev", Image: "example.invalid/k3s:v1", RegistryCacheIP: "192.168.64.9"}
	want := "KIAC_REGISTRY_CACHE_IP=192.168.64.9"
	for name, o := range map[string]runtime.RunOpts{
		"server": k3sServerRunOpts(cfg, "kiac-dev-control-plane", "token", nil),
		"agent":  k3sAgentRunOpts(cfg, "kiac-dev-worker-1", nil, nil),
	} {
		if !slices.Contains(o.Env, want) {
			t.Errorf("%s env = %q, want registries.yaml payload", name, o.Env)
		}
		boot := strings.Join(o.Args, " ")
		at := strings.Index(boot, "/etc/rancher/k3s/registries.yaml")
		if at < 0 || at > strings.Index(boot, "exec sh -c") {
			t.Errorf("%s boot script does not write registries.yaml before starting k3s", name)
		}
	}
	plain := k3sServerRunOpts(Config{Name: "dev"}, "kiac-dev-control-plane", "token", nil)
	for _, e := range plain.Env {
		if strings.HasPrefix(e, "KIAC_REGISTRY_CACHE_IP=") {
			t.Fatalf("env carries a mirror without --registry-cache: %q", plain.Env)
		}
	}
}

func TestEnsureRegistryCacheCreatesContainer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(t.TempDir(), "container")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  "ls -a --format json")
    if [ -f "$KIAC_TEST_COMMAND_LOG.ran" ]; then
      printf '[{"id":"kiac.registry-cache","status":"running","networks":[{"address":"192.168.64.9/24"}]}]'
    else
      printf '[]'
    fi
    ;;
  "run --help") printf -- '--cap-add\n' ;;
  run\ -d*) touch "$KIAC_TEST_COMMAND_LOG.ran" ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
	manager := &Manager{rt: &runtime.Client{Bin: bin}}
	ca := string(testCAPEM(t, "Corp Root"))

	ip, err := manager.ensureRegistryCache([]string{ca}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.168.64.9" {
		t.Errorf("ip = %q, want 192.168.64.9", ip)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commands := string(raw)
	dir := filepath.Join(home, ".kiac", "registry-cache")
	for _, want := range []string{
		"run -d --name kiac.registry-cache",
		"-v kiac.registry-cache:/var/lib/registry",
		"--mount type=bind,source=" + dir + ",target=/etc/zot,readonly",
		"SSL_CERT_DIR=/etc/ssl/certs:/etc/zot/ca",
		RegistryCacheImage,
	} {
		if !strings.Contains(commands, want) {
			t.Errorf("missing %q in:\n%s", want, commands)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Errorf("config.json not written: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "ca", "kiac-1.crt")); err != nil || string(got) != ca {
		t.Errorf("CA for upstream pulls not written: %v", err)
	}
}

func TestEnsureRegistryCacheReusesRunningContainer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin := filepath.Join(t.TempDir(), "container")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  "ls -a --format json") printf '[{"id":"kiac.registry-cache","status":"running","networks":[{"address":"192.168.64.7/24"}]}]' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
	manager := &Manager{rt: &runtime.Client{Bin: bin}}
	ip, err := manager.ensureRegistryCache(nil, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.168.64.7" {
		t.Errorf("ip = %q, want 192.168.64.7", ip)
	}
	raw, _ := os.ReadFile(logPath)
	if strings.Contains(string(raw), "run -d") || strings.Contains(string(raw), "start ") {
		t.Errorf("running cache was recreated or restarted:\n%s", raw)
	}
}

func TestHealRegistryCacheRepointsMirroredNodes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin := filepath.Join(t.TempDir(), "container")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  "ls -a --format json") printf '[{"id":"kiac.registry-cache","status":"running","networks":[{"address":"192.168.64.20/24"}]},{"id":"kiac-dev-control-plane","status":"running"},{"id":"kiac-dev-worker-1","status":"stopped"},{"id":"kiac-other-control-plane","status":"running"}]' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
	m := &Manager{rt: &runtime.Client{Bin: bin}}
	// Skip the host HTTP probe: the fake cache has no listener.
	if _, err := m.ensureRegistryCache(nil, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := m.healRegistryCache("dev", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(logPath)
	commands := string(raw)
	if !strings.Contains(commands, "exec kiac-dev-control-plane sh -c grep -qs 'kiac.registry-cache'") ||
		!strings.Contains(commands, "echo '192.168.64.20 kiac.registry-cache'") {
		t.Errorf("running dev node not re-pointed at the cache:\n%s", commands)
	}
	for _, untouched := range []string{"exec kiac-dev-worker-1", "exec kiac-other-control-plane"} {
		if strings.Contains(commands, untouched) {
			t.Errorf("%q: heal touched a stopped node or another cluster:\n%s", untouched, commands)
		}
	}
}

func TestHealRegistryCacheSkipsClustersWithoutMirror(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin := filepath.Join(t.TempDir(), "container")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	// The cache is stopped and no node carries the mirror marker.
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  "ls -a --format json") printf '[{"id":"kiac.registry-cache","status":"stopped"},{"id":"kiac-dev-control-plane","status":"running"}]' ;;
  *"grep -qs"*) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
	m := &Manager{rt: &runtime.Client{Bin: bin}}
	if err := m.healRegistryCache("dev", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(logPath)
	if strings.Contains(string(raw), "start kiac.registry-cache") {
		t.Fatalf("heal started a stopped cache for a cluster without the mirror:\n%s", raw)
	}
}

func TestEnsureRegistryCacheRestartsForNewCAs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin := filepath.Join(t.TempDir(), "container")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  "ls -a --format json") printf '[{"id":"kiac.registry-cache","status":"running","networks":[{"address":"192.168.64.7/24"}]}]' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
	m := &Manager{rt: &runtime.Client{Bin: bin}}
	if _, err := m.ensureRegistryCache([]string{string(testCAPEM(t, "Corp"))}, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(logPath)
	commands := string(raw)
	stop, start := strings.Index(commands, "stop kiac.registry-cache"), strings.Index(commands, "start kiac.registry-cache")
	if stop < 0 || start < stop {
		t.Fatalf("running cache was not restarted to load new CAs:\n%s", commands)
	}
}

func TestDeleteRegistryCachePurgeWithoutClient(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '[]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{rt: &runtime.Client{Bin: bin}}
	if err := m.DeleteRegistryCache(true); err == nil {
		t.Fatal("purge without an apple/container client succeeded, want error")
	}
}
