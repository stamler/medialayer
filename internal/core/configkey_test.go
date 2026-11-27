package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs/config"
)

func TestConfigKeyEncryptsSignIns(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "rclone.conf")
	t.Cleanup(config.ClearConfigPassword)
	// A plain config from before the key.
	write(t, conf, []byte("[proton]\ntype = protondrive\nusername = a@proton.me\nclient_access_token = secret\n"))
	notice, err := setupConfig(conf, "key-one")
	if err != nil || notice != "" {
		t.Fatalf("setupConfig = %q, %v", notice, err)
	}
	b, _ := os.ReadFile(conf)
	if !strings.Contains(string(b), encryptedHeader) || strings.Contains(string(b), "secret") {
		t.Fatalf("config not encrypted:\n%s", b)
	}
	if got := config.GetValue("proton", "client_access_token"); got != "secret" {
		t.Errorf("value after encrypting = %q", got)
	}

	// The same key reads it back.
	if notice, err := setupConfig(conf, "key-one"); err != nil || notice != "" {
		t.Fatalf("reopen = %q, %v", notice, err)
	}
	if got := config.GetValue("proton", "username"); got != "a@proton.me" {
		t.Errorf("value after reopening = %q", got)
	}

	// Another key, or none, sets the file aside instead of stopping.
	for _, key := range []string{"key-two", ""} {
		notice, err := setupConfig(conf, key)
		if err != nil || notice == "" {
			t.Fatalf("setupConfig with key %q = %q, %v", key, notice, err)
		}
		if config.GetValue("proton", "type") != "" {
			t.Errorf("key %q: an unreadable config's remotes are still there", key)
		}
		if _, err := os.Stat(conf); !os.IsNotExist(err) {
			t.Errorf("key %q: unreadable config left in place: %v", key, err)
		}
		aside, _ := filepath.Glob(conf + ".unreadable-*")
		if len(aside) == 0 {
			t.Errorf("key %q: nothing set aside", key)
		}
		for _, p := range aside {
			_ = os.Remove(p)
		}
		// Put an encrypted config back for the next round.
		if _, err := setupConfig(conf, "key-one"); err != nil {
			t.Fatal(err)
		}
		config.FileSetValue("proton", "type", protonType)
		config.SaveConfig()
	}

	// A new config written with a key is encrypted from the start.
	conf2 := filepath.Join(t.TempDir(), "rclone.conf")
	if _, err := setupConfig(conf2, "key-one"); err != nil {
		t.Fatal(err)
	}
	config.FileSetValue("gdrive", "type", "drive")
	config.SaveConfig()
	if enc, err := isEncryptedConfig(conf2); err != nil || !enc {
		t.Errorf("new config encrypted = %v, %v", enc, err)
	}
}

func TestUnparsableConfigSetAside(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	t.Cleanup(config.ClearConfigPassword)
	write(t, conf, []byte("[unclosed\nkey = value\n"))
	for _, key := range []string{"", "key-one"} {
		notice, err := setupConfig(conf, key)
		if err != nil || notice == "" {
			t.Fatalf("key %q: setupConfig = %q, %v", key, notice, err)
		}
		config.FileSetValue("gdrive", "type", "drive") // loads without stopping
		write(t, conf, []byte("[unclosed\nkey = value\n"))
	}
}
