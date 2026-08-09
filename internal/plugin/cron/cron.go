// Package cron observes scheduled jobs.
//
// Cron is the least observable thing on a typical server. A job that has been
// failing nightly for three weeks leaves no trace anywhere an operator looks —
// there is no service to be "down", no port to be closed, and the failure mail
// goes to a mailbox nobody reads. Atlas cannot make cron report success, but it
// can at least show what is scheduled, which is the question people cannot
// otherwise answer without SSHing to the box.
//
// # Reading, never writing
//
// This plugin parses crontab files directly rather than shelling out to
// `crontab -l`. Reading is unambiguously read-only; `crontab` is a
// setuid-adjacent tool whose flags differ between implementations, and one
// mistyped argument is the difference between listing a user's jobs and
// replacing them. For a platform whose central promise is that it never
// modifies anything, the parser is worth the extra code.
package cron

import (
	"bufio"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Source is where a job was found, which determines who runs it and with what
// privileges.
type Source string

const (
	// SourceSystem is /etc/crontab: system-wide, with a user field.
	SourceSystem Source = "system"
	// SourceCronDir is /etc/cron.d: package-installed jobs, with a user field.
	SourceCronDir Source = "cron.d"
	// SourceUser is a per-user crontab from the spool, which has no user
	// field — the file's name is the user.
	SourceUser Source = "user"
	// SourcePeriodic is /etc/cron.{hourly,daily,weekly,monthly}: scripts run
	// by run-parts rather than crontab entries.
	SourcePeriodic Source = "periodic"
)

// Job is one scheduled job.
type Job struct {
	// Schedule is the raw cron expression, or the period name for a periodic
	// script.
	Schedule string
	// Command is what runs. Truncated, and never used as a metric label — a
	// command line can contain credentials.
	Command string
	// User is who it runs as. Root jobs deserve particular attention.
	User   string
	Source Source
	// File is where it was read from, for an operator who needs to edit it.
	File string
	// Line is the line number within that file.
	Line int
}

// Root reports whether the job runs with full privileges.
func (j Job) Root() bool { return j.User == "root" || j.User == "" }

// Provider supplies scheduled jobs.
type Provider interface {
	// Available reports whether any cron source is readable here.
	Available(ctx context.Context) bool
	// Jobs returns every job the caller may read.
	Jobs(ctx context.Context) ([]Job, error)
}

// Standard locations, in the order a Linux system uses them.
var (
	systemCrontab = "/etc/crontab"
	cronDDir      = "/etc/cron.d"
	// spoolDirs differ by distribution: Debian uses crontabs, Red Hat uses
	// cron. Both are checked because Atlas runs on both.
	spoolDirs = []string{
		"/var/spool/cron/crontabs",
		"/var/spool/cron",
	}
	periodicDirs = map[string]string{
		"hourly":  "/etc/cron.hourly",
		"daily":   "/etc/cron.daily",
		"weekly":  "/etc/cron.weekly",
		"monthly": "/etc/cron.monthly",
	}
)

// maxCommandLength bounds a stored command.
const maxCommandLength = 300

// fileProvider reads crontab files from disk.
type fileProvider struct {
	// Roots are injectable so tests run against a synthetic filesystem rather
	// than the host's real crontabs.
	systemCrontab string
	cronDDir      string
	spoolDirs     []string
	periodicDirs  map[string]string
}

// NewProvider returns a Provider reading the standard locations.
func NewProvider() Provider {
	return &fileProvider{
		systemCrontab: systemCrontab,
		cronDDir:      cronDDir,
		spoolDirs:     spoolDirs,
		periodicDirs:  periodicDirs,
	}
}

// Available reports whether any cron source exists and is readable.
func (p *fileProvider) Available(context.Context) bool {
	if _, err := os.Stat(p.systemCrontab); err == nil {
		return true
	}
	if entries, err := os.ReadDir(p.cronDDir); err == nil && len(entries) > 0 {
		return true
	}
	for _, dir := range p.spoolDirs {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return true
		}
	}
	for _, dir := range p.periodicDirs {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return true
		}
	}
	return false
}

func (p *fileProvider) Jobs(ctx context.Context) ([]Job, error) {
	const op = "cron.Provider.Jobs"

	var jobs []Job

	// Every source is best-effort. Atlas frequently runs unprivileged, and the
	// user spool in particular is root-only on most systems; returning what is
	// readable beats returning nothing because one directory was refused.
	if data, err := os.ReadFile(p.systemCrontab); err == nil {
		jobs = append(jobs, parseCrontab(string(data), SourceSystem, p.systemCrontab, "")...)
	}

	jobs = append(jobs, p.readDir(ctx, p.cronDDir, SourceCronDir, "")...)

	for _, dir := range p.spoolDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || ctx.Err() != nil {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			// A user crontab has no user column; the filename is the user.
			jobs = append(jobs, parseCrontab(string(data), SourceUser, path, entry.Name())...)
		}
	}

	for period, dir := range p.periodicDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !executable(dir, entry) {
				continue
			}
			jobs = append(jobs, Job{
				Schedule: "@" + period,
				Command:  filepath.Join(dir, entry.Name()),
				User:     "root", // run-parts runs these as root
				Source:   SourcePeriodic,
				File:     filepath.Join(dir, entry.Name()),
			})
		}
	}

	if jobs == nil && ctx.Err() != nil {
		return nil, errs.Wrap(ctx.Err(), errs.CodeDeadlineExceeded,
			"reading cron jobs was cancelled").WithOp(op)
	}
	return jobs, nil
}

func (p *fileProvider) readDir(ctx context.Context, dir string, source Source, user string) []Job {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var jobs []Job
	for _, entry := range entries {
		if entry.IsDir() || ctx.Err() != nil {
			continue
		}
		// run-parts and cron ignore files with dots or tildes in the name;
		// parsing them would report jobs that never run — worse than missing
		// them, because it is confidently wrong.
		if strings.ContainsAny(entry.Name(), ".~") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		jobs = append(jobs, parseCrontab(string(data), source, path, user)...)
	}
	return jobs
}

func executable(dir string, entry fs.DirEntry) bool {
	info, err := entry.Info()
	if err != nil {
		return false
	}
	return info.Mode()&0o111 != 0
}

// parseCrontab extracts jobs from crontab text.
//
// The two dialects differ in one way that matters: system crontabs and
// /etc/cron.d entries carry a user column between the schedule and the
// command, while user crontabs do not. Treating a user crontab as if it had
// one would silently drop the first word of every command.
func parseCrontab(content string, source Source, file, defaultUser string) []Job {
	hasUserField := source == SourceSystem || source == SourceCronDir

	var jobs []Job
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Environment assignments (PATH=, MAILTO=, SHELL=) are not jobs. The
		// check is for an '=' before any whitespace, so a command containing
		// '=' later is not mistaken for one.
		if eq := strings.IndexByte(line, '='); eq > 0 && !strings.ContainsAny(line[:eq], " \t") {
			continue
		}

		job, ok := parseLine(line, hasUserField)
		if !ok {
			continue
		}
		job.Source = source
		job.File = file
		job.Line = lineNo
		if job.User == "" {
			job.User = defaultUser
		}
		jobs = append(jobs, job)
	}
	return jobs
}

// parseLine splits one crontab entry.
func parseLine(line string, hasUserField bool) (Job, bool) {
	// Special schedules replace all five time fields with one token.
	if strings.HasPrefix(line, "@") {
		schedule, rest, found := strings.Cut(line, " ")
		if !found {
			return Job{}, false
		}
		rest = strings.TrimSpace(rest)

		job := Job{Schedule: schedule}
		if hasUserField {
			user, command, found := strings.Cut(rest, " ")
			if !found {
				return Job{}, false
			}
			job.User, job.Command = user, truncate(strings.TrimSpace(command))
		} else {
			job.Command = truncate(rest)
		}
		return job, job.Command != ""
	}

	// Five time fields, then optionally a user, then the command.
	fields := strings.Fields(line)
	minFields := 6
	if hasUserField {
		minFields = 7
	}
	if len(fields) < minFields {
		return Job{}, false
	}

	job := Job{Schedule: strings.Join(fields[:5], " ")}
	if hasUserField {
		job.User = fields[5]
		job.Command = truncate(strings.Join(fields[6:], " "))
	} else {
		job.Command = truncate(strings.Join(fields[5:], " "))
	}
	return job, job.Command != ""
}

func truncate(s string) string {
	if len(s) > maxCommandLength {
		return s[:maxCommandLength] + "…"
	}
	return s
}

// parsedAt is unused at runtime but documents that jobs are a point-in-time
// read rather than a subscription; cron has no change notification.
var _ = time.Now
