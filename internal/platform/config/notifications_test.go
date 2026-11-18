package config_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/secretbox"
)

func testEncryptionKey() string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i*7 + 3)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestEncryptionKeyFromEnvironment(t *testing.T) {
	t.Parallel()
	key := testEncryptionKey()
	cfg, err := config.Load(config.Options{Lookup: env(map[string]string{
		"ATLAS_NOTIFICATIONS_ENCRYPTION_KEY": key,
	})})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Notifications.EncryptionKey.Reveal() != key {
		t.Error("the key was not loaded from the environment")
	}
}

// A mounted secret almost always ends in a newline, and may carry other
// whitespace; neither is part of the key.
func TestEncryptionKeyFromSecretFileIgnoresWhitespace(t *testing.T) {
	t.Parallel()
	key := testEncryptionKey()
	for name, content := range map[string]string{
		"trailing newline": key + "\n",
		"trailing CRLF":    key + "\r\n",
		"padded":           "  " + key + " \n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "notifications-key")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.Options{Lookup: env(map[string]string{
				"ATLAS_NOTIFICATIONS_ENCRYPTION_KEY_FILE": path,
			})})
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			k, err := secretbox.ParseKey(cfg.Notifications.EncryptionKey.Reveal())
			if err != nil {
				t.Fatalf("ParseKey: %v", err)
			}
			want, _ := secretbox.ParseKey(key)
			if k.ID() != want.ID() {
				t.Errorf("file-loaded key id = %s, want %s", k.ID(), want.ID())
			}
		})
	}
}

func TestInvalidEncryptionKeyIsAConfigErrorWithoutTheKey(t *testing.T) {
	t.Parallel()
	for name, key := range map[string]string{
		"not base64":     "definitely not base64 !!!",
		"16 bytes":       base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")),
		"33 bytes":       base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdefX")),
		"raw 32 chars":   "0123456789abcdef0123456789abcdef",
		"url-safe alpha": strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xfb", 32)))),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(config.Options{Lookup: env(map[string]string{
				"ATLAS_NOTIFICATIONS_ENCRYPTION_KEY": key,
			})})
			if err == nil {
				t.Fatal("Load() accepted an invalid key")
			}
			if !strings.Contains(err.Error(), "notifications.encryption_key") {
				t.Errorf("error does not name the setting: %v", err)
			}
			if strings.Contains(err.Error(), key) {
				t.Errorf("error contains the key: %v", err)
			}
		})
	}
}

// Unset is valid: notifications are disabled, the server still starts.
func TestMissingEncryptionKeyIsValid(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(config.Options{Lookup: env(nil)})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Notifications.EncryptionKey != "" {
		t.Error("the key has a default")
	}
}

func TestEncryptionKeyCannotBeSetFromYAML(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "atlas.yaml")
	body := "notifications:\n  encryption_key: " + testEncryptionKey() + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(config.Options{Path: path, Lookup: env(nil)}); err == nil {
		t.Fatal("Load() accepted an encryption key from YAML")
	}
}

// `atlas-server config` shows whether the key is set, never its value; no
// other way of printing the configuration reveals it either.
func TestRenderRedactsTheEncryptionKey(t *testing.T) {
	t.Parallel()
	key := testEncryptionKey()

	cfg := config.Default()
	if got := cfg.Render()["notifications"].(map[string]any)["encryption_key"]; got != "" {
		t.Errorf("unset key renders as %q, want empty", got)
	}

	cfg.Notifications.EncryptionKey = config.Secret(key)
	section, ok := cfg.Render()["notifications"].(map[string]any)
	if !ok {
		t.Fatal("rendered config has no notifications section")
	}
	if got := section["encryption_key"]; got != "[REDACTED]" {
		t.Errorf("set key renders as %q, want [REDACTED]", got)
	}

	rendered, _ := json.Marshal(cfg.Render())
	direct, _ := json.Marshal(cfg)
	for name, out := range map[string]string{
		"Render":       string(rendered),
		"json.Marshal": string(direct),
		"%v":           fmt.Sprintf("%v", cfg),
		"%+v":          fmt.Sprintf("%+v", cfg),
		"%#v":          fmt.Sprintf("%#v", cfg),
		"%s on field":  fmt.Sprintf("%s", cfg.Notifications.EncryptionKey),
	} {
		if strings.Contains(out, key) {
			t.Errorf("%s leaked the encryption key", name)
		}
	}
}

func TestEnvVarsIncludesTheEncryptionKey(t *testing.T) {
	t.Parallel()
	if !slices.Contains(config.EnvVars(), "ATLAS_NOTIFICATIONS_ENCRYPTION_KEY") {
		t.Error("EnvVars() is missing ATLAS_NOTIFICATIONS_ENCRYPTION_KEY")
	}
}
