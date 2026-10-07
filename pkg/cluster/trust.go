package cluster

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"
)

// kubeadmExtraCADir is where kindest/node's update-ca-certificates picks
// up local CAs; kiac owns the subdirectory so it can rewrite it wholesale.
const kubeadmExtraCADir = "/usr/local/share/ca-certificates/kiac"

// k3sExtraCAEnv carries the extra CA bundle into k3s node VMs. The k3s
// image has no update-ca-certificates, and k3s starts from the boot
// preamble, so the bundle must be in the environment before boot.
const k3sExtraCAEnv = "KIAC_EXTRA_CA_B64"

// LoadCABundle reads PEM files and returns each certificate as its own
// PEM block, in file order. Non-certificate blocks (keys, comments) are
// skipped; a file without any certificate, or one that does not parse,
// is an error so a wrong path cannot silently leave pulls untrusted.
func LoadCABundle(paths []string) ([]string, error) {
	var certs []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading --ca-cert: %w", err)
		}
		found := 0
		for rest := raw; ; {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				continue
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return nil, fmt.Errorf("--ca-cert %s: invalid certificate: %w", path, err)
			}
			certs = append(certs, string(pem.EncodeToMemory(block)))
			found++
		}
		if found == 0 {
			return nil, fmt.Errorf("--ca-cert %s: no PEM CERTIFICATE blocks found", path)
		}
	}
	return certs, nil
}

// k3sExtraCAEnvFor returns the env entry for k3s node VMs, or nil.
func k3sExtraCAEnvFor(cfg Config) []string {
	if len(cfg.CACerts) == 0 {
		return nil
	}
	bundle := strings.Join(cfg.CACerts, "")
	return []string{k3sExtraCAEnv + "=" + base64.StdEncoding.EncodeToString([]byte(bundle))}
}

// k3sExtraCAPrep rebuilds the system bundle from a pristine copy plus
// the extra CAs, so a node restart re-running the preamble does not
// append the same certificates again. k3s's embedded containerd (Go)
// reads this bundle when it first verifies a registry.
const k3sExtraCAPrep = `if [ -n "$` + k3sExtraCAEnv + `" ]; then ` +
	`KIAC_CA=/etc/ssl/certs/ca-certificates.crt; ` +
	`[ -f "$KIAC_CA.kiac-orig" ] || cp "$KIAC_CA" "$KIAC_CA.kiac-orig"; ` +
	`{ cat "$KIAC_CA.kiac-orig"; printf '%s' "$` + k3sExtraCAEnv + `" | base64 -d; } > "$KIAC_CA.kiac-new" && mv "$KIAC_CA.kiac-new" "$KIAC_CA"; ` +
	`fi; `

// installKubeadmExtraCA trusts certs in a booted kindest/node VM:
// one file per certificate (update-ca-certificates links by first cert),
// then a containerd restart so its cached system pool is reloaded. It
// runs before kubeadm init, so every image pull already trusts them.
func (m *Manager) installKubeadmExtraCA(node string, certs []string, wait time.Duration) error {
	if _, err := m.rt.Exec(node, "sh", "-c", "rm -rf "+kubeadmExtraCADir+" && mkdir -p "+kubeadmExtraCADir); err != nil {
		return fmt.Errorf("preparing extra CA directory on %s: %w", node, err)
	}
	for i, c := range certs {
		dest := fmt.Sprintf("%s/kiac-%d.crt", kubeadmExtraCADir, i+1)
		if err := m.rt.ExecStdinTimeout(node, transferBudget(wait), strings.NewReader(c), "sh", "-c", "cat > "+dest); err != nil {
			return fmt.Errorf("installing extra CA certificate on %s: %w", node, err)
		}
	}
	if _, err := m.rt.Exec(node, "sh", "-c", "update-ca-certificates >/dev/null && systemctl restart containerd"); err != nil {
		return fmt.Errorf("trusting extra CA certificates on %s: %w", node, err)
	}
	return m.rt.WaitReady(node, wait)
}
