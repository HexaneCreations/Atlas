package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// `atlas-server config` runs inside production containers: it reports
// whether the notifications encryption key is set, never the key.
func TestPrintConfigRedactsTheNotificationsEncryptionKey(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	t.Setenv("ATLAS_NOTIFICATIONS_ENCRYPTION_KEY", key)
	t.Setenv("ATLAS_CONFIG_FILE", "")

	var buf bytes.Buffer
	if err := printConfig(&buf, ""); err != nil {
		t.Fatalf("printConfig: %v", err)
	}
	if strings.Contains(buf.String(), key) {
		t.Fatal("atlas-server config printed the encryption key")
	}

	var out struct {
		Notifications struct {
			EncryptionKey string `json:"encryption_key"`
		} `json:"notifications"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if out.Notifications.EncryptionKey != "[REDACTED]" {
		t.Errorf("notifications.encryption_key = %q, want [REDACTED]", out.Notifications.EncryptionKey)
	}
}
