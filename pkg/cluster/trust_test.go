package cluster

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

// testCAPEM returns a freshly generated self-signed CA certificate.
func testCAPEM(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeTestFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadCABundle(t *testing.T) {
	corp := testCAPEM(t, "Corp Root")
	proxy := testCAPEM(t, "Proxy Intercept")
	// One file holding two certs plus a stray key, one file with one cert.
	keyBlock := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("not-a-real-key")})
	multi := writeTestFile(t, "corp.pem", append(append(append([]byte("comment line\n"), corp...), keyBlock...), proxy...))
	single := writeTestFile(t, "single.crt", corp)

	certs, err := LoadCABundle([]string{multi, single})
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 3 {
		t.Fatalf("got %d certs, want 3", len(certs))
	}
	for i, c := range certs {
		if !strings.HasPrefix(c, "-----BEGIN CERTIFICATE-----") || strings.Contains(c, "PRIVATE KEY") {
			t.Errorf("cert %d is not a lone PEM certificate:\n%s", i, c)
		}
	}
	if certs[0] != string(corp) || certs[1] != string(proxy) {
		t.Errorf("certificates not returned in file order")
	}
}

func TestLoadCABundleErrors(t *testing.T) {
	notPEM := writeTestFile(t, "junk.crt", []byte("hello"))
	badDER := writeTestFile(t, "bad.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("garbage")}))
	for name, paths := range map[string][]string{
		"missing file":    {filepath.Join(t.TempDir(), "absent.crt")},
		"no certificates": {notPEM},
		"unparseable DER": {badDER},
	} {
		if _, err := LoadCABundle(paths); err == nil {
			t.Errorf("%s: LoadCABundle succeeded, want error", name)
		}
	}
	if certs, err := LoadCABundle(nil); err != nil || certs != nil {
		t.Errorf("LoadCABundle(nil) = %v, %v; want nil, nil", certs, err)
	}
}

func TestK3sRunOptsCarryExtraCA(t *testing.T) {
	ca := string(testCAPEM(t, "Corp Root"))
	cfg := Config{Name: "dev", Image: "example.invalid/k3s:v1", CACerts: []string{ca}}
	want := "KIAC_EXTRA_CA_B64=" + base64.StdEncoding.EncodeToString([]byte(ca))
	for name, o := range map[string]runtime.RunOpts{
		"server": k3sServerRunOpts(cfg, "kiac-dev-control-plane", "token", nil),
		"agent":  k3sAgentRunOpts(cfg, "kiac-dev-worker-1", []string{"K3S_URL=https://x:6443"}, nil),
	} {
		if !slices.Contains(o.Env, want) {
			t.Errorf("%s env = %q, want it to contain the CA bundle", name, o.Env)
		}
		boot := strings.Join(o.Args, " ")
		if !strings.Contains(boot, "KIAC_EXTRA_CA_B64") || !strings.Contains(boot, "/etc/ssl/certs/ca-certificates.crt") {
			t.Errorf("%s boot script does not install the CA bundle:\n%s", name, boot)
		}
		// The CA install must happen before k3s starts pulling images.
		if strings.Index(boot, "KIAC_EXTRA_CA_B64") > strings.Index(boot, "exec sh -c") {
			t.Errorf("%s boot script installs the CA bundle after starting k3s", name)
		}
	}

	plain := Config{Name: "dev", Image: "example.invalid/k3s:v1"}
	o := k3sServerRunOpts(plain, "kiac-dev-control-plane", "token", nil)
	for _, e := range o.Env {
		if strings.HasPrefix(e, "KIAC_EXTRA_CA_B64=") {
			t.Fatalf("server env carries a CA bundle without --ca-cert: %q", o.Env)
		}
	}
}

func TestConfigureKubeadmContainerdExtraCA(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	stdinPath := filepath.Join(t.TempDir(), "stdin")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
if [ "$2" = "-i" ]; then cat >> "$KIAC_TEST_STDIN"; fi
case "$*" in *"systemctl is-active containerd"*) echo active ;; esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
	t.Setenv("KIAC_TEST_STDIN", stdinPath)
	manager := &Manager{rt: &runtime.Client{Bin: bin}}

	certs := []string{string(testCAPEM(t, "A")), string(testCAPEM(t, "B"))}
	if err := manager.configureKubeadmContainerd("kiac-dev-worker-1", certs, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commands := string(raw)
	for _, want := range []string{
		"exec -i kiac-dev-worker-1 sh -c",
		"/usr/local/share/ca-certificates/kiac/kiac-1.crt",
		"/usr/local/share/ca-certificates/kiac/kiac-2.crt",
		"update-ca-certificates",
		"restart containerd",
	} {
		if !strings.Contains(commands, want) {
			t.Errorf("missing %q in:\n%s", want, commands)
		}
	}
	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != certs[0]+certs[1] {
		t.Errorf("certificates written = %q, want both PEMs in order", stdin)
	}
	restart := strings.Index(commands, "systemctl restart containerd")
	if restart < 0 {
		t.Fatalf("containerd was not restarted:\n%s", commands)
	}
	if !strings.Contains(commands[restart:], "systemctl is-active containerd") {
		t.Errorf("containerd readiness was not re-checked after restart:\n%s", commands)
	}
}
