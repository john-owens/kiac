package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestLoadImagesPlatform(t *testing.T) {
	for _, tc := range []struct {
		name       string
		platform   string
		wantSave   string
		wantImport string
	}{
		{
			name:       "host platform by default",
			wantSave:   "image save example.invalid/app:v1 --output ",
			wantImport: "exec -i kiac-dev-control-plane ctr -n k8s.io image import -",
		},
		{
			name:       "explicit amd64 for Rosetta nodes",
			platform:   "linux/amd64",
			wantSave:   "image save --platform linux/amd64 example.invalid/app:v1 --output ",
			wantImport: "exec -i kiac-dev-control-plane ctr -n k8s.io image import --platform linux/amd64 -",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "container")
			logPath := filepath.Join(t.TempDir(), "commands.log")
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  "ls -a --format json")
    printf '[{"id":"kiac-dev-control-plane","status":"running"}]'
    ;;
esac
`
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
			manager := &Manager{rt: &runtime.Client{Bin: bin}}
			if err := manager.LoadImages("dev", []string{"example.invalid/app:v1"}, tc.platform); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			commands := string(raw)
			for _, want := range []string{tc.wantSave, tc.wantImport} {
				if !strings.Contains(commands, want) {
					t.Errorf("missing %q in:\n%s", want, commands)
				}
			}
		})
	}
}

func TestLoadImagesRejectsMalformedPlatform(t *testing.T) {
	manager := &Manager{rt: &runtime.Client{Bin: "/nonexistent/container"}}
	for _, platform := range []string{"amd64", "linux/", "linux/amd64 --all-platforms", "-x/amd64"} {
		err := manager.LoadImages("dev", []string{"example.invalid/app:v1"}, platform)
		if err == nil || !strings.Contains(err.Error(), "platform") {
			t.Errorf("LoadImages(platform=%q) error = %v, want platform validation error", platform, err)
		}
	}
}

func TestLoadImagesImportsIntoEveryNode(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  "ls -a --format json")
    printf '[{"id":"kiac-dev-control-plane","status":"running"},{"id":"kiac-dev-worker-1","status":"running"},{"id":"kiac-dev-worker-2","status":"running"}]'
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
	manager := &Manager{rt: &runtime.Client{Bin: bin}}
	if err := manager.LoadImages("dev", []string{"example.invalid/app:v1"}, ""); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(logPath)
	for _, node := range []string{"kiac-dev-control-plane", "kiac-dev-worker-1", "kiac-dev-worker-2"} {
		if !strings.Contains(string(raw), "exec -i "+node+" ctr -n k8s.io image import -") {
			t.Errorf("image not imported into %s:\n%s", node, raw)
		}
	}
}
