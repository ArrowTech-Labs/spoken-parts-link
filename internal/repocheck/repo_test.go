package repocheck

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const root = "../.."

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func glob(t *testing.T, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(root, pattern))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range m {
		rel, _ := filepath.Rel(root, p)
		if !strings.HasSuffix(rel, "_test.go") {
			out = append(out, rel)
		}
	}
	return out
}

// customerText lists every file whose words a customer can read.
func customerText(t *testing.T) []string {
	files := []string{
		"README.md",
		"SECURITY.md",
		"THREAT-MODEL.md",
		"spoken_parts_link/DOCS.md",
		"spoken_parts_link/README.md",
		"spoken_parts_link/CHANGELOG.md",
		"spoken_parts_link/config.yaml",
		"spoken_parts_link/translations/en.yaml",
	}
	files = append(files, glob(t, "internal/web/templates/*.html")...)
	files = append(files, glob(t, "internal/web/*.go")...)
	return files
}

var (
	codeFence    = regexp.MustCompile("(?s)```.*?```")
	inlineCode   = regexp.MustCompile("`[^`\n]*`")
	spacedHyphen = regexp.MustCompile(`\S - \S`)
)

func TestCustomerTextHasNoDashes(t *testing.T) {
	files := customerText(t)
	if len(files) < 10 {
		t.Fatalf("only %d files found", len(files))
	}
	for _, f := range files {
		s := read(t, f)
		for i, line := range strings.Split(s, "\n") {
			if strings.ContainsRune(line, '\u2014') {
				t.Errorf("%s:%d has an em dash", f, i+1)
			}
			if strings.ContainsRune(line, '\u2013') {
				t.Errorf("%s:%d has an en dash", f, i+1)
			}
		}
		if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "messages.go") {
			continue
		}
		prose := inlineCode.ReplaceAllString(codeFence.ReplaceAllString(s, ""), "")
		for i, line := range strings.Split(prose, "\n") {
			if spacedHyphen.MatchString(line) {
				t.Errorf("%s: spaced hyphen used as a dash: %q", f, strings.TrimSpace(line))
			}
			_ = i
		}
	}
}

// topLevel reads "key: value" lines at column 0 of a simple YAML file.
func topLevel(s string) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == ' ' || line[0] == '#' || line[0] == '-' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		m[k] = strings.TrimSpace(v)
	}
	return m
}

func TestConfigYAML(t *testing.T) {
	s := read(t, "spoken_parts_link/config.yaml")
	m := topLevel(s)
	want := map[string]string{
		"name":           `"Spoken Parts Link"`,
		"version":        `"0.2.2"`,
		"slug":           "spoken_parts_link",
		"url":            `"https://github.com/ArrowTech-Labs/spoken-parts-link"`,
		"image":          `"ghcr.io/arrowtech-labs/spoken-parts-link"`,
		"startup":        "application",
		"boot":           "auto",
		"stage":          "experimental",
		"init":           "false",
		"ingress":        "true",
		"ingress_port":   "8099",
		"ingress_stream": "false",
		"panel_icon":     "mdi:printer-3d",
		"panel_admin":    "true",
		"host_network":   "true",
		"apparmor":       "true",
		"watchdog":       `"http://[HOST]:[PORT:8099]/healthz"`,
		"backup":         "hot",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("config.yaml %s = %q, want %q", k, m[k], v)
		}
	}
	for _, k := range []string{"description", "arch", "options", "schema"} {
		if _, ok := m[k]; !ok {
			t.Errorf("config.yaml lacks %s", k)
		}
	}
	forbidden := []string{"privileged", "full_access", "docker_api", "hassio_api", "homeassistant_api", "auth_api", "ports", "devices", "map", "backup_exclude", "host_pid", "kernel_modules", "uart", "usb", "gpio"}
	for _, k := range forbidden {
		if _, ok := m[k]; ok {
			t.Errorf("config.yaml must not set %s", k)
		}
	}
	if !regexp.MustCompile(`(?m)^arch:\n  - aarch64\n  - amd64\n[^ ]`).MatchString(s) {
		t.Error("arch must be exactly aarch64 and amd64")
	}
	if !strings.Contains(s, "options:\n  log_level: info\n") || !strings.Contains(s, "schema:\n  log_level: \"list(debug|info|warning|error)\"\n") {
		t.Error("options or schema for log_level are wrong")
	}
	if strings.Contains(s, "password") {
		t.Error("no password options: access codes are typed on the link page only")
	}
	ver := strings.Trim(m["version"], `"`)
	if !strings.Contains(read(t, "spoken_parts_link/CHANGELOG.md"), "## "+ver+"\n") {
		t.Errorf("CHANGELOG.md has no entry for %s", ver)
	}
	if !strings.Contains(read(t, "spoken_parts_link/Dockerfile"), "ARG VERSION="+ver+"\n") {
		t.Errorf("Dockerfile default VERSION is not %s", ver)
	}
	for _, f := range []string{"DOCS.md", "README.md", "CHANGELOG.md", "apparmor.txt", "icon.png", "logo.png", "translations/en.yaml", "Dockerfile"} {
		if _, err := os.Stat(filepath.Join(root, "spoken_parts_link", f)); err != nil {
			t.Errorf("app folder lacks %s", f)
		}
	}
	rep := topLevel(read(t, "repository.yaml"))
	if rep["name"] != "Spoken Parts" || rep["url"] != "https://github.com/ArrowTech-Labs/spoken-parts-link" || rep["maintainer"] != "ArrowTech Labs" {
		t.Errorf("repository.yaml %v", rep)
	}
}

func TestAppArmorProfile(t *testing.T) {
	s := read(t, "spoken_parts_link/apparmor.txt")
	profiles := regexp.MustCompile(`(?m)^profile\s+(\S+)`).FindAllStringSubmatch(s, -1)
	if len(profiles) != 1 || profiles[0][1] != "spoken_parts_link" {
		t.Fatalf("want exactly one top level profile named spoken_parts_link, got %v", profiles)
	}
	for _, want := range []string{"/data/** rwk,", "/usr/bin/spoken-parts-link mr,", "deny ptrace,", "deny mount,", "deny network inet raw,", "network inet stream,"} {
		if !strings.Contains(s, want) {
			t.Errorf("apparmor.txt lacks %q", want)
		}
	}
	if strings.Contains(s, "\n  file,") {
		t.Error("apparmor.txt must not allow all files")
	}
}

func TestDockerfile(t *testing.T) {
	s := read(t, "spoken_parts_link/Dockerfile")
	if !regexp.MustCompile(`ARG GO_IMAGE=golang:1\.27-alpine@sha256:[0-9a-f]{64}\n`).MatchString(s) {
		t.Error("builder image must be pinned by digest")
	}
	for _, want := range []string{
		"CGO_ENABLED=0", "-trimpath", `-ldflags "-s -w -buildid= -X main.version=${VERSION}"`,
		`io.hass.type="addon"`, `io.hass.version="${VERSION}"`, `io.hass.arch="amd64"`, `io.hass.arch="aarch64"`,
		`io.hass.name="Spoken Parts Link"`, "io.hass.description=",
		`org.opencontainers.image.source="https://github.com/ArrowTech-Labs/spoken-parts-link"`,
		`org.opencontainers.image.licenses="PolyForm-Shield-1.0.0"`,
		"FROM scratch", "/etc/ssl/certs/ca-certificates.crt",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
}

var usesRe = regexp.MustCompile(`^\s*(?:-\s*)?uses:\s*(\S+)(.*)$`)

func TestWorkflowsPinned(t *testing.T) {
	files := glob(t, ".github/workflows/*.yml")
	if len(files) != 2 {
		t.Fatalf("workflows: %v", files)
	}
	for _, f := range files {
		s := read(t, f)
		if strings.Contains(s, "pull_request_target") {
			t.Errorf("%s uses pull_request_target", f)
		}
		n := 0
		for _, line := range strings.Split(s, "\n") {
			m := usesRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			n++
			if !regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+@[0-9a-f]{40}$`).MatchString(m[1]) {
				t.Errorf("%s: %s is not pinned to a full commit SHA", f, m[1])
			}
			if !regexp.MustCompile(`^\s*# v\d+\.\d+\.\d+\s*$`).MatchString(m[2]) {
				t.Errorf("%s: %s lacks a # vX.Y.Z comment", f, m[1])
			}
		}
		if n == 0 {
			t.Errorf("%s has no actions", f)
		}
		// No QEMU: the Dockerfile cross compiles, and binfmt runs privileged.
		if strings.Contains(s, "setup-qemu-action") {
			t.Errorf("%s sets up QEMU; it is not needed and runs an unpinned privileged image", f)
		}
		// Every buildx builder must use a BuildKit image pinned by digest.
		buildx := strings.Count(s, "docker/setup-buildx-action@")
		pinned := len(regexp.MustCompile(`(?m)^\s+driver-opts: image=moby/buildkit:v\d+\.\d+\.\d+@sha256:[0-9a-f]{64}$`).FindAllString(s, -1))
		if buildx != pinned {
			t.Errorf("%s: %d buildx setups but %d pinned BuildKit images", f, buildx, pinned)
		}
		for _, secret := range regexp.MustCompile(`secrets\.([A-Za-z_]+)`).FindAllStringSubmatch(s, -1) {
			if secret[1] != "GITHUB_TOKEN" {
				t.Errorf("%s uses secret %s", f, secret[1])
			}
		}
	}
	ci := read(t, ".github/workflows/ci.yml")
	if !strings.Contains(ci, "permissions:\n  contents: read\n") {
		t.Error("ci.yml must default to contents: read")
	}
	rel := read(t, ".github/workflows/release.yml")
	for _, want := range []string{"permissions: {}\n", "environment: release", "contents: write", "packages: write", "id-token: write", "attestations: write", "tags: [\"v*\"]", "cosign sign --yes"} {
		if !strings.Contains(rel, want) {
			t.Errorf("release.yml lacks %q", want)
		}
	}
}

func TestLicense(t *testing.T) {
	s := read(t, "LICENSE")
	if !strings.HasPrefix(s, "Required Notice: Copyright ArrowTech Labs (https://spokenparts.com)\n") {
		t.Error("LICENSE must start with the required notice")
	}
	if !strings.Contains(s, "# PolyForm Shield License 1.0.0") || !strings.Contains(s, "## Noncompete") {
		t.Error("LICENSE is not PolyForm Shield 1.0.0")
	}
}
