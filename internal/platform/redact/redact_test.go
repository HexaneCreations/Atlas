package redact_test

import (
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/platform/redact"
)

func TestStringRedactsCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		in     string
		secret string
	}{
		{"long flag equals", "myapp --password=hunter2", "hunter2"},
		{"long flag space", "myapp --password hunter2", "hunter2"},
		{"passwd", "myapp --passwd=hunter2", "hunter2"},
		{"token equals", "curl --token=abc123xyz", "abc123xyz"},
		{"api-key", "svc --api-key=k-9f8e7d6c", "k-9f8e7d6c"},
		{"apikey", "svc --apikey=k-9f8e7d6c", "k-9f8e7d6c"},
		{"secret", "svc --secret=s3cr3tvalue", "s3cr3tvalue"},
		{"client-secret", "svc --client-secret=oauth-abc", "oauth-abc"},
		{"credential", "svc --credential=cred-value", "cred-value"},
		{"bare key=value", "svc password=hunter2", "hunter2"},
		{"bare token=value", "svc token=abc123xyz", "abc123xyz"},
		{"env style", "TOKEN=abc123xyz /usr/bin/svc", "abc123xyz"},
		{"colon separated", "svc --password:hunter2", "hunter2"},
		{"quoted value", `svc --password="hunter 2"`, "hunter 2"},
		{"single quoted", "svc --password='hunter2'", "hunter2"},
		{"mysql short form", "mysqldump -uroot -pSuperSecret db", "SuperSecret"},
		{"authorization bearer", `curl -H "Authorization: Bearer eyJhbGciOiJIUzI1"`, "eyJhbGciOiJIUzI1"},
		{"basic auth header", `curl -H "Authorization: Basic dXNlcjpwYXNz"`, "dXNlcjpwYXNz"},
		{"url userinfo", "psql postgres://atlas:dbpassword@db.internal:5432/atlas", "dbpassword"},
		{"private-key", "svc --private-key=MIIEvQIBADANBg", "MIIEvQIBADANBg"},
		{"aws secret", "aws --aws-secret-access-key=wJalrXUtnFEMI", "wJalrXUtnFEMI"},
		{"passphrase", "svc --passphrase=letmein", "letmein"},
		{"uppercase flag", "svc --PASSWORD=hunter2", "hunter2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redact.String(tt.in)
			if strings.Contains(got, tt.secret) {
				t.Errorf("redact.String(%q) = %q, still contains the secret %q", tt.in, got, tt.secret)
			}
			if !strings.Contains(got, redact.Placeholder) {
				t.Errorf("redact.String(%q) = %q, want it to mark the redaction with %s", tt.in, got, redact.Placeholder)
			}
		})
	}
}

// Redaction that eats ordinary arguments would destroy the diagnostic value
// command lines exist for, which is the reason they are collected at all.
func TestStringPreservesOperationalArguments(t *testing.T) {
	t.Parallel()

	tests := []string{
		"nginx -c /etc/nginx/nginx.conf",
		"java -Xmx4g -jar application.jar",
		"myapp --port 8080 --host 0.0.0.0",
		"postgres -D /var/lib/postgresql/data",
		"/usr/bin/python3 /opt/app/main.py --workers 4",
		"docker run --rm -it ubuntu:24.04 /bin/bash",
		"node server.js --max-old-space-size=4096",
		"mysqldump -u root --single-transaction db",
		"rsync -avz /srv/data/ backup@host:/srv/data/",
		"systemctl restart atlas-agent",
	}

	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			if got := redact.String(in); got != in {
				t.Errorf("redact.String(%q) = %q, want it unchanged", in, got)
			}
		})
	}
}

func TestStringKeepsFlagNameSoTheArgumentStaysIdentifiable(t *testing.T) {
	t.Parallel()

	got := redact.String("myapp --password=hunter2 --port 8080")
	if !strings.Contains(got, "--password") {
		t.Errorf("got %q, want the flag name preserved", got)
	}
	if !strings.Contains(got, "--port 8080") {
		t.Errorf("got %q, want unrelated arguments preserved", got)
	}
}

func TestStringIsIdempotent(t *testing.T) {
	t.Parallel()

	once := redact.String("myapp --password=hunter2")
	if twice := redact.String(once); twice != once {
		t.Errorf("second pass changed the result: %q -> %q", once, twice)
	}
}

func TestRedactorDefaultOnAndExplicitOff(t *testing.T) {
	t.Parallel()

	const in = "myapp --password=hunter2"

	on := redact.New(true)
	if !on.Enabled() {
		t.Error("New(true).Enabled() = false")
	}
	if got := on.String(in); strings.Contains(got, "hunter2") {
		t.Errorf("enabled Redactor left the secret in %q", got)
	}

	off := redact.New(false)
	if got := off.String(in); got != in {
		t.Errorf("disabled Redactor = %q, want the input unchanged", got)
	}
}

func TestStringHandlesEmpty(t *testing.T) {
	t.Parallel()
	if got := redact.String(""); got != "" {
		t.Errorf("redact.String(\"\") = %q", got)
	}
}
