package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The parser is the whole plugin. Everything downstream — the counts, the
// inventory, the "who runs as root" answer — is a projection of what these
// functions decide a line means, and the two crontab dialects differ in a way
// that fails silently: reading a user crontab as if it had a user column drops
// the first word of every command, producing jobs that look plausible and are
// wrong.

func TestParseSystemCrontabHasUserField(t *testing.T) {
	const content = `
# /etc/crontab
SHELL=/bin/sh
PATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin

17 *	* * *	root	cd / && run-parts --report /etc/cron.hourly
25 6	* * *	backup	/usr/local/bin/backup.sh --full
`
	jobs := parseCrontab(content, SourceSystem, "/etc/crontab", "")

	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2: %+v", len(jobs), jobs)
	}

	if got, want := jobs[0].Schedule, "17 * * * *"; got != want {
		t.Errorf("schedule = %q, want %q", got, want)
	}
	if got, want := jobs[0].User, "root"; got != want {
		t.Errorf("user = %q, want %q", got, want)
	}
	if got, want := jobs[0].Command, "cd / && run-parts --report /etc/cron.hourly"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
	if !jobs[0].Root() {
		t.Error("job running as root not reported as root")
	}

	if got, want := jobs[1].User, "backup"; got != want {
		t.Errorf("user = %q, want %q", got, want)
	}
	if jobs[1].Root() {
		t.Error("job running as backup reported as root")
	}
	// Line numbers are what an operator uses to find the entry again.
	if got, want := jobs[1].Line, 7; got != want {
		t.Errorf("line = %d, want %d", got, want)
	}
}

func TestParseUserCrontabHasNoUserField(t *testing.T) {
	// The trap: "rsync" here is the command, not a username. A parser that
	// assumes a user column returns command "-a /data /backup" and attributes
	// the job to a user named rsync — both wrong, neither obviously so.
	const content = "30 2 * * * rsync -a /data /backup\n"

	jobs := parseCrontab(content, SourceUser, "/var/spool/cron/crontabs/deploy", "deploy")

	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if got, want := jobs[0].Command, "rsync -a /data /backup"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
	// The filename supplies the user a user crontab does not carry.
	if got, want := jobs[0].User, "deploy"; got != want {
		t.Errorf("user = %q, want %q", got, want)
	}
	if jobs[0].Root() {
		t.Error("deploy's job reported as root")
	}
}

func TestParseSpecialSchedules(t *testing.T) {
	tests := []struct {
		name         string
		line         string
		hasUserField bool
		wantSchedule string
		wantUser     string
		wantCommand  string
	}{
		{
			name: "reboot with user field", line: "@reboot root /usr/local/bin/warm-cache",
			hasUserField: true,
			wantSchedule: "@reboot", wantUser: "root", wantCommand: "/usr/local/bin/warm-cache",
		},
		{
			name: "daily without user field", line: "@daily /usr/local/bin/rotate --quiet",
			hasUserField: false,
			wantSchedule: "@daily", wantUser: "", wantCommand: "/usr/local/bin/rotate --quiet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, ok := parseLine(tc.line, tc.hasUserField)
			if !ok {
				t.Fatal("line rejected")
			}
			if job.Schedule != tc.wantSchedule {
				t.Errorf("schedule = %q, want %q", job.Schedule, tc.wantSchedule)
			}
			if job.User != tc.wantUser {
				t.Errorf("user = %q, want %q", job.User, tc.wantUser)
			}
			if job.Command != tc.wantCommand {
				t.Errorf("command = %q, want %q", job.Command, tc.wantCommand)
			}
		})
	}
}

func TestParseSkipsNonJobs(t *testing.T) {
	// Environment assignments, comments, and blank lines are not jobs, and a
	// truncated entry is not one either — reporting a job whose command is a
	// fragment is worse than not reporting it.
	const content = `
# a comment
MAILTO=""
PATH=/usr/bin:/bin
CRON_TZ=UTC

*/5 * * *
`
	if jobs := parseCrontab(content, SourceUser, "crontab", "app"); len(jobs) != 0 {
		t.Fatalf("got %d jobs, want 0: %+v", len(jobs), jobs)
	}
}

func TestParseDoesNotMistakeCommandsContainingEqualsForAssignments(t *testing.T) {
	// The assignment check looks for '=' before any whitespace, so a command
	// with an '=' in a later argument stays a job.
	const content = "0 3 * * * /usr/bin/psql --set=ON_ERROR_STOP=1 -f /opt/vacuum.sql\n"

	jobs := parseCrontab(content, SourceUser, "crontab", "postgres")
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if !strings.Contains(jobs[0].Command, "ON_ERROR_STOP=1") {
		t.Errorf("command = %q, want the --set argument preserved", jobs[0].Command)
	}
}

func TestParseTruncatesLongCommands(t *testing.T) {
	long := strings.Repeat("x", maxCommandLength+50)
	jobs := parseCrontab("0 0 * * * "+long+"\n", SourceUser, "crontab", "app")

	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	// Bounded, and visibly so — an operator seeing the ellipsis knows to look
	// at the file rather than trusting a command that was silently cut.
	if got := len([]rune(jobs[0].Command)); got != maxCommandLength+1 {
		t.Errorf("command length = %d runes, want %d", got, maxCommandLength+1)
	}
	if !strings.HasSuffix(jobs[0].Command, "…") {
		t.Error("truncated command not marked with an ellipsis")
	}
}

func TestProviderReadsEveryStandardLocation(t *testing.T) {
	root := t.TempDir()
	p := providerRootedAt(t, root)

	writeFile(t, filepath.Join(root, "etc/crontab"),
		"17 * * * * root cd / && run-parts /etc/cron.hourly\n", 0o644)
	writeFile(t, filepath.Join(root, "etc/cron.d/certbot"),
		"0 */12 * * * root certbot renew --quiet\n", 0o644)
	// cron itself ignores files with a dot or tilde in the name, so parsing
	// them would report jobs that never actually run.
	writeFile(t, filepath.Join(root, "etc/cron.d/certbot.dpkg-old"),
		"0 1 * * * root /bin/false\n", 0o644)
	writeFile(t, filepath.Join(root, "etc/cron.d/backup~"),
		"0 2 * * * root /bin/false\n", 0o644)
	writeFile(t, filepath.Join(root, "var/spool/cron/crontabs/deploy"),
		"30 2 * * * rsync -a /data /backup\n", 0o600)
	writeFile(t, filepath.Join(root, "etc/cron.daily/logrotate"),
		"#!/bin/sh\n", 0o755)
	// run-parts only runs executables, so a non-executable file in a periodic
	// directory is a leftover, not a job.
	writeFile(t, filepath.Join(root, "etc/cron.daily/README"),
		"notes\n", 0o644)

	jobs, err := p.Jobs(context.Background())
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}

	bySource := map[Source]int{}
	for _, j := range jobs {
		bySource[j.Source]++
	}

	want := map[Source]int{
		SourceSystem:   1,
		SourceCronDir:  1, // the .dpkg-old and ~ files are correctly skipped
		SourceUser:     1,
		SourcePeriodic: 1, // README is not executable
	}
	for source, n := range want {
		if bySource[source] != n {
			t.Errorf("%s: got %d jobs, want %d", source, bySource[source], n)
		}
	}
	if len(jobs) != 4 {
		t.Errorf("got %d jobs total, want 4: %+v", len(jobs), jobs)
	}
}

func TestProviderAvailableRequiresSomethingReadable(t *testing.T) {
	root := t.TempDir()
	p := providerRootedAt(t, root)

	// Nothing readable means the plugin must report absence rather than
	// activating and showing "0 jobs" — which reads as "nothing is scheduled"
	// rather than the truth, "I cannot see what is scheduled".
	if p.Available(context.Background()) {
		t.Fatal("Available on an empty host, want false")
	}

	writeFile(t, filepath.Join(root, "etc/crontab"), "# empty\n", 0o644)

	if !p.Available(context.Background()) {
		t.Fatal("Available with a readable /etc/crontab, want true")
	}
}

func TestProviderToleratesUnreadableSources(t *testing.T) {
	root := t.TempDir()
	p := providerRootedAt(t, root)

	writeFile(t, filepath.Join(root, "etc/crontab"),
		"17 * * * * root /usr/local/bin/hourly\n", 0o644)

	// The user spool is root-only on most systems and Atlas frequently runs
	// unprivileged. Returning what is readable beats returning nothing.
	spool := filepath.Join(root, "var/spool/cron/crontabs")
	writeFile(t, filepath.Join(spool, "deploy"), "30 2 * * * /bin/true\n", 0o600)
	if err := os.Chmod(spool, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(spool, 0o755) })

	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions are not enforced")
	}

	jobs, err := p.Jobs(context.Background())
	if err != nil {
		t.Fatalf("Jobs returned an error for one unreadable directory: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Source != SourceSystem {
		t.Errorf("got %+v, want the one readable system job", jobs)
	}
}

func TestCollectorCountsOnlyAndNeverLabelsCommands(t *testing.T) {
	// A command routinely contains credentials passed as arguments. If one
	// ever became a metric label it would live in the datastore forever, be
	// visible to anyone with read access, and survive rotation of the secret.
	const secret = "--password=hunter2"

	provider := &fakeProvider{jobs: []Job{
		{Schedule: "@daily", Command: "/usr/bin/mysqldump " + secret, User: "root", Source: SourceSystem},
		{Schedule: "0 * * * *", Command: "/usr/local/bin/sync", User: "app", Source: SourceUser},
		{Schedule: "@weekly", Command: "/etc/cron.weekly/man-db", User: "root", Source: SourcePeriodic},
	}}

	samples, err := newCronCollector(provider, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, s := range samples {
		for key, value := range s.Labels {
			if key != "source" {
				t.Errorf("metric %s carries label %q, want only 'source'", s.Metric, key)
			}
			if strings.Contains(value, secret) || strings.Contains(value, "/usr/bin") {
				t.Errorf("metric %s leaks a command into label %s=%q", s.Metric, key, value)
			}
		}
	}

	values := map[string]float64{}
	for _, s := range samples {
		key := s.Metric
		if source, ok := s.Labels["source"]; ok {
			key += "{" + source + "}"
		}
		values[key] = s.Value
	}

	want := map[string]float64{
		"cron.jobs.total":           3,
		"cron.jobs.root":            2,
		"cron.jobs.count{system}":   1,
		"cron.jobs.count{user}":     1,
		"cron.jobs.count{periodic}": 1,
		"cron.jobs.count{cron.d}":   0, // emitted at zero, so a chart shows a fall
	}
	for metric, wantValue := range want {
		got, ok := values[metric]
		if !ok {
			t.Errorf("%s not emitted", metric)
			continue
		}
		if got != wantValue {
			t.Errorf("%s = %v, want %v", metric, got, wantValue)
		}
	}
}

// providerRootedAt returns a fileProvider reading from a synthetic filesystem,
// so tests never depend on — or read — the host's real crontabs.
func providerRootedAt(t *testing.T, root string) *fileProvider {
	t.Helper()
	return &fileProvider{
		systemCrontab: filepath.Join(root, "etc/crontab"),
		cronDDir:      filepath.Join(root, "etc/cron.d"),
		spoolDirs: []string{
			filepath.Join(root, "var/spool/cron/crontabs"),
			filepath.Join(root, "var/spool/cron"),
		},
		periodicDirs: map[string]string{
			"hourly":  filepath.Join(root, "etc/cron.hourly"),
			"daily":   filepath.Join(root, "etc/cron.daily"),
			"weekly":  filepath.Join(root, "etc/cron.weekly"),
			"monthly": filepath.Join(root, "etc/cron.monthly"),
		},
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fakeProvider returns jobs a test set, so collector behaviour can be asserted
// without a filesystem.
type fakeProvider struct {
	jobs []Job
	err  error
}

func (f *fakeProvider) Available(context.Context) bool { return true }

func (f *fakeProvider) Jobs(context.Context) ([]Job, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.jobs, nil
}

var _ Provider = (*fakeProvider)(nil)

func TestInventoryRedactsJobCommandSecretsByDefault(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{jobs: []Job{
		{Schedule: "0 3 * * *", Command: `curl -H "Authorization: Bearer eyJhbGciOiJIUzI1" https://api.example.com/backup`},
		{Schedule: "*/5 * * * *", Command: "/usr/local/bin/healthcheck --port 8080"},
	}}
	p := New(Options{Provider: provider})

	got, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	if strings.Contains(got[0].Command, "eyJhbGciOiJIUzI1") {
		t.Errorf("command = %q, still carries the bearer token", got[0].Command)
	}
	if !strings.Contains(got[0].Command, "curl") || !strings.Contains(got[0].Command, "https://api.example.com/backup") {
		t.Errorf("command = %q, want the job still identifiable", got[0].Command)
	}
	if got[1].Command != "/usr/local/bin/healthcheck --port 8080" {
		t.Errorf("command = %q, want ordinary arguments untouched", got[1].Command)
	}
}

func TestInventoryRedactionCanBeDisabledExplicitly(t *testing.T) {
	t.Parallel()

	const raw = "mysql -uroot -pSuperSecret -e 'select 1'"
	provider := &fakeProvider{jobs: []Job{{Schedule: "0 3 * * *", Command: raw}}}
	p := New(Options{Provider: provider, DisableSecretRedaction: true})

	got, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if got[0].Command != raw {
		t.Errorf("command = %q, want %q when redaction is explicitly disabled", got[0].Command, raw)
	}
}
