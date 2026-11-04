package config

import (
	"os"
	"path/filepath"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"SP_LINK_DATA": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.String() != "https://spokenparts.com" || c.Listen != "172.30.32.1:8099" || c.SSDP != "0.0.0.0:2021" {
		t.Fatalf("%+v", c)
	}
	if len(c.Allow) != 1 || c.Allow[0].String() != "172.30.32.2" || c.LogLevel != "info" {
		t.Fatalf("%+v", c)
	}
}

func TestOverridesAndOptions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "options.json"), []byte(`{"log_level":"debug"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env(map[string]string{
		"SP_LINK_DATA":   dir,
		"SP_LINK_SERVER": "http://localhost:9000",
		"SP_LINK_LISTEN": "0.0.0.0:8099",
		"SP_LINK_ALLOW":  "172.17.0.1, 127.0.0.1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Host != "localhost:9000" || len(c.Allow) != 2 || c.LogLevel != "debug" || c.Listen != "0.0.0.0:8099" {
		t.Fatalf("%+v", c)
	}
}

func TestRejectsBadValues(t *testing.T) {
	for _, m := range []map[string]string{
		{"SP_LINK_SERVER": "http://spokenparts.com"},
		{"SP_LINK_ALLOW": "everyone"},
		{"SP_LINK_ALLOW": " , "},
	} {
		m["SP_LINK_DATA"] = t.TempDir()
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%v accepted", m)
		}
	}
}
