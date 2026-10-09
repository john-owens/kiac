package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

// The registry cache is one zot pull-through cache shared by every kiac
// cluster. Its name deliberately does not start with "kiac-": cluster
// commands own every container under that prefix, and the cache must
// outlive `kiac delete cluster`. Images live on a named volume, so they
// also survive deleting and recreating the cache container itself.
const (
	RegistryCacheName   = "kiac.registry-cache"
	RegistryCacheVolume = "kiac.registry-cache"
	// zot v2.1.22, pinned to its multi-arch index (linux/arm64 included).
	RegistryCacheImage = "ghcr.io/project-zot/zot:v2.1.22@sha256:96cda11459ce6f8c60da3b03f03e9f67fd11b3ae2d82c4c04c92b3c608996080"
	registryCachePort  = "5000"
)

// registryUpstream is one registry the cache proxies. Host is the name
// image references use; URL is where the content actually lives.
type registryUpstream struct {
	Host string
	URL  string
}

// registryUpstreams covers where Kubernetes, addon, and most workload
// images come from. Anything else is pulled directly, as before.
var registryUpstreams = []registryUpstream{
	{Host: "docker.io", URL: "https://registry-1.docker.io"},
	{Host: "registry.k8s.io", URL: "https://registry.k8s.io"},
	{Host: "ghcr.io", URL: "https://ghcr.io"},
	{Host: "quay.io", URL: "https://quay.io"},
}

// zotCacheConfig stores each upstream under /<host>/..., so one cache
// serves several registries without repository name clashes.
func zotCacheConfig() string {
	type content struct {
		Prefix      string `json:"prefix"`
		Destination string `json:"destination"`
	}
	type upstream struct {
		URLs      []string  `json:"urls"`
		OnDemand  bool      `json:"onDemand"`
		TLSVerify bool      `json:"tlsVerify"`
		Content   []content `json:"content"`
	}
	var ups []upstream
	for _, u := range registryUpstreams {
		ups = append(ups, upstream{
			URLs: []string{u.URL}, OnDemand: true, TLSVerify: true,
			Content: []content{{Prefix: "**", Destination: "/" + u.Host}},
		})
	}
	cfg := map[string]any{
		"distSpecVersion": "1.1.1",
		// dedupe hard-links blobs; keep the store simple and portable.
		"storage": map[string]any{"rootDirectory": "/var/lib/registry", "gc": true, "dedupe": false},
		// docker2s2: most docker.io/quay.io images still use Docker
		// manifest lists, which zot otherwise refuses to sync.
		"http":       map[string]any{"address": "0.0.0.0", "port": registryCachePort, "compat": []string{"docker2s2"}},
		"log":        map[string]any{"level": "info"},
		"extensions": map[string]any{"sync": map[string]any{"enable": true, "registries": ups}},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	return string(raw) + "\n"
}

// registryCacheDir holds the cache's config and the CAs it trusts for
// upstream pulls; it is bind-mounted read-only at /etc/zot.
func registryCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kiac", "registry-cache"), nil
}

// writeRegistryCacheFiles refreshes config.json and, when caCerts is
// non-empty, replaces ca/*.crt so the set matches the latest request.
// Nil keeps the CAs from an earlier start (resume and heal pass nil).
func writeRegistryCacheFiles(dir string, caCerts []string) error {
	caDir := filepath.Join(dir, "ca")
	if len(caCerts) > 0 {
		if err := os.RemoveAll(caDir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(caDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(zotCacheConfig()), 0o644); err != nil {
		return err
	}
	for i, c := range caCerts {
		if err := os.WriteFile(filepath.Join(caDir, fmt.Sprintf("kiac-%d.crt", i+1)), []byte(c), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// RegistryCacheStatus reports the cache container's state and address.
// An empty state means it does not exist.
func (m *Manager) RegistryCacheStatus() (state, ip string, err error) {
	infos, err := m.rt.List(RegistryCacheName)
	if err != nil {
		return "", "", err
	}
	for _, info := range infos {
		if info.Name == RegistryCacheName {
			return info.Status, info.IP, nil
		}
	}
	return "", "", nil
}

// EnsureRegistryCache starts the shared cache if needed and returns its
// address once it answers. caCerts are trusted for upstream pulls.
func (m *Manager) EnsureRegistryCache(caCerts []string) (string, error) {
	return m.ensureRegistryCache(caCerts, probeRegistryCache)
}

func (m *Manager) ensureRegistryCache(caCerts []string, healthy func(ip string) error) (string, error) {
	dir, err := registryCacheDir()
	if err != nil {
		return "", err
	}
	state, ip, err := m.RegistryCacheStatus()
	if err != nil {
		return "", err
	}
	// Refresh config and CAs before any (re)start; a running cache keeps
	// what it loaded until it is restarted.
	if err := writeRegistryCacheFiles(dir, caCerts); err != nil {
		return "", fmt.Errorf("writing registry cache config: %w", err)
	}
	running := strings.EqualFold(state, "running")
	switch {
	case running && len(caCerts) > 0:
		// zot reads its trust store at startup; a restart is the only
		// way new CAs take effect.
		if err := m.rt.Stop(RegistryCacheName); err != nil {
			return "", err
		}
		if err := m.rt.Start(RegistryCacheName); err != nil {
			return "", err
		}
	case running && ip != "":
		return ip, healthy(ip)
	case running:
		// Still booting: wait for its address below.
	case state != "":
		if err := m.rt.Start(RegistryCacheName); err != nil {
			return "", err
		}
	default:
		if err := m.rt.ImagePull(RegistryCacheImage); err != nil {
			return "", err
		}
		if err := m.rt.RunDetached(runtime.RunOpts{
			Name:    RegistryCacheName,
			Image:   RegistryCacheImage,
			CPUs:    "2",
			Memory:  "1G",
			Volumes: []string{RegistryCacheVolume + ":/var/lib/registry"},
			Mounts:  runtime.Mounts{{Source: dir, Target: "/etc/zot", ReadOnly: true}},
			// The image pins SSL_CERT_FILE to the system bundle; Go
			// still reads every file in SSL_CERT_DIR on top of it.
			Env: []string{"SSL_CERT_DIR=/etc/ssl/certs:/etc/zot/ca"},
		}); err != nil {
			return "", err
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, ip, err = m.RegistryCacheStatus(); err == nil && ip != "" {
			if err = healthy(ip); err == nil {
				return ip, nil
			}
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("no IP address assigned")
			}
			return "", fmt.Errorf("registry cache %s did not become ready: %w", RegistryCacheName, err)
		}
		time.Sleep(time.Second)
	}
}

// probeRegistryCache checks the distribution API from the host. Node
// VMs reach the cache on the same vmnet network.
func probeRegistryCache(ip string) error {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + ip + ":" + registryCachePort + "/v2/")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry cache answered %s", resp.Status)
	}
	return nil
}

// DeleteRegistryCache removes the cache container; purge also deletes
// the volume holding every cached image.
func (m *Manager) DeleteRegistryCache(purge bool) error {
	state, _, err := m.RegistryCacheStatus()
	if err != nil {
		return err
	}
	if state != "" {
		if err := m.rt.Remove(RegistryCacheName); err != nil {
			return err
		}
	}
	if purge {
		if m.container == nil {
			return fmt.Errorf("deleting volume %s: no apple/container client", RegistryCacheVolume)
		}
		return m.container.VolumeDelete(RegistryCacheVolume)
	}
	return nil
}

// Nodes reach the cache by name, not IP: container IPs change across
// restarts, and re-pointing one /etc/hosts line is cheap, whereas
// mirror config is only read when containerd or k3s starts.
const registryCacheEndpoint = "http://" + RegistryCacheName + ":" + registryCachePort

// registryCacheHostsScript (re)points the cache name at ip in
// /etc/hosts. It rewrites in place with cat rather than sed -i, which
// fails when /etc/hosts is a bind mount.
func registryCacheHostsScript(ip string) string {
	return `grep -v ' ` + RegistryCacheName + `$' /etc/hosts > /tmp/kiac-hosts; ` +
		`echo '` + ip + ` ` + RegistryCacheName + `' >> /tmp/kiac-hosts; cat /tmp/kiac-hosts > /etc/hosts; rm -f /tmp/kiac-hosts; `
}

// registryMirrorMarker matches the mirror config kiac writes on either
// distro, so healing only touches nodes created with --registry-cache.
const registryMirrorMarker = `grep -qs '` + RegistryCacheName + `' /etc/containerd/certs.d/docker.io/hosts.toml /etc/rancher/k3s/registries.yaml`

// kubeadmMirrorScript points kindest/node's containerd at the cache.
// kindest/node ships no registry config_path, so it is added once; a
// config that already has a CRI registry table is left alone and
// reported, since appending a second table would break containerd.
// override_path keeps zot's /<host>/ repository namespace.
func kubeadmMirrorScript(ip string) string {
	var b strings.Builder
	b.WriteString(`set -e; C=/etc/containerd/config.toml; ` +
		`if ! grep -q 'config_path = "/etc/containerd/certs.d"' "$C"; then ` +
		`if grep -q 'io.containerd.grpc.v1.cri".registry]' "$C"; then echo "containerd config already has a registry table; not adding the kiac mirror" >&2; exit 1; fi; ` +
		`printf '\n[plugins."io.containerd.grpc.v1.cri".registry]\n  config_path = "/etc/containerd/certs.d"\n' >> "$C"; fi; `)
	for _, u := range registryUpstreams {
		d := "/etc/containerd/certs.d/" + u.Host
		hosts := fmt.Sprintf("server = %q\n\n[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n  override_path = true\n",
			u.URL, registryCacheEndpoint+"/v2/"+u.Host)
		fmt.Fprintf(&b, "mkdir -p %s; printf '%%s' %s > %s/hosts.toml; ", d, shQuote(hosts), d)
	}
	b.WriteString(registryCacheHostsScript(ip))
	return b.String()
}

// k3sRegistriesYAML is the k3s equivalent: an endpoint per upstream and
// a rewrite into zot's /<host>/ namespace.
func k3sRegistriesYAML() string {
	var b strings.Builder
	b.WriteString("mirrors:\n")
	for _, u := range registryUpstreams {
		fmt.Fprintf(&b, "  %q:\n    endpoint:\n      - %q\n    rewrite:\n      \"^(.*)$\": %q\n",
			u.Host, registryCacheEndpoint, u.Host+"/$1")
	}
	return b.String()
}

const k3sRegistryCacheEnv = "KIAC_REGISTRY_CACHE_IP"

// k3sRegistriesPrep writes registries.yaml and the cache's hosts entry
// before k3s starts; k3s only reads registries.yaml at startup. The
// address is the one recorded at create; kiac cache start and resume
// re-point it if the cache has moved since.
var k3sRegistriesPrep = `if [ -n "$` + k3sRegistryCacheEnv + `" ]; then mkdir -p /etc/rancher/k3s; ` +
	`printf '%s' ` + shQuote(k3sRegistriesYAML()) + ` > /etc/rancher/k3s/registries.yaml; ` +
	strings.ReplaceAll(registryCacheHostsScript("__IP__"), "'__IP__ ", `"$`+k3sRegistryCacheEnv+`"' `) + `fi; `

func k3sRegistriesEnvFor(cfg Config) []string {
	if cfg.RegistryCacheIP == "" {
		return nil
	}
	return []string{k3sRegistryCacheEnv + "=" + cfg.RegistryCacheIP}
}

// HealRegistryCache re-points every running node of cluster name that
// was created with --registry-cache at the cache's current address,
// starting the cache first if it is stopped. Clusters without the
// mirror are left untouched, and so is a cache that was deleted.
func (m *Manager) HealRegistryCache(name string) error {
	return m.healRegistryCache(name, probeRegistryCache)
}

func (m *Manager) healRegistryCache(name string, healthy func(ip string) error) error {
	state, _, err := m.RegistryCacheStatus()
	if err != nil || state == "" {
		return err
	}
	infos, err := m.rt.List(prefix(name))
	if err != nil {
		return err
	}
	var nodes []string
	for _, info := range infos {
		if strings.EqualFold(info.Status, "running") && info.Backend == runtime.BackendContainer {
			nodes = append(nodes, info.Name)
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	// Find mirrored nodes first, so a cluster that never used the cache
	// does not start a cache someone stopped on purpose.
	marked := make([]bool, len(nodes))
	_ = inParallel(len(nodes), func(i int) error {
		_, err := m.rt.Exec(nodes[i], "sh", "-c", registryMirrorMarker)
		marked[i] = err == nil
		return nil
	})
	var mirrored []string
	for i, n := range nodes {
		if marked[i] {
			mirrored = append(mirrored, n)
		}
	}
	if len(mirrored) == 0 {
		return nil
	}
	ip, err := m.ensureRegistryCache(nil, healthy)
	if err != nil {
		return err
	}
	return inParallel(len(mirrored), func(i int) error {
		_, err := m.rt.Exec(mirrored[i], "sh", "-c", registryCacheHostsScript(ip))
		return err
	})
}
