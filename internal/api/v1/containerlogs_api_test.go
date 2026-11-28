package v1_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/plugin/docker"
)

// dockerLogs serves lines the way Docker does: tail over the whole log, then
// since and until. honoursUntil false is a version 1 Agent.
type dockerLogs struct {
	lines        []docker.LogLine
	honoursUntil bool
	refusesUntil bool
}

func (d dockerLogs) logs(ctx context.Context, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error) {
	out, errCh := make(chan docker.LogLine), make(chan error, 1)
	go func() {
		defer close(errCh)
		defer close(out)
		if d.refusesUntil && !opts.Until.IsZero() {
			errCh <- fmt.Errorf("refused: %w", docker.ErrUntilUnsupported)
			return
		}
		src := d.lines
		if opts.Tail > 0 && opts.Tail < len(src) {
			src = src[len(src)-opts.Tail:]
		}
		for _, l := range src {
			if !opts.Since.IsZero() && l.Time.Before(opts.Since) {
				continue
			}
			if d.honoursUntil && !opts.Until.IsZero() && l.Time.After(opts.Until) {
				return
			}
			select {
			case out <- l:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, errCh
}

type rangeDocker struct {
	*fakeDockerClient
	src dockerLogs
}

func (r rangeDocker) Logs(ctx context.Context, _ string, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error) {
	return r.src.logs(ctx, opts)
}

type rangeRemote struct {
	src      dockerLogs
	supports bool
}

func (r rangeRemote) ContainerLogs(ctx context.Context, _, _ string, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error) {
	return r.src.logs(ctx, opts)
}
func (r rangeRemote) SupportsUntil(string) bool { return r.supports }

// recentLines is one line a minute for the last n minutes.
func recentLines(n int, now time.Time) []docker.LogLine {
	out := make([]docker.LogLine, n)
	for i := range out {
		out[i] = docker.LogLine{Time: now.Add(-time.Duration(n-i) * time.Minute), Stream: docker.StreamStdout, Message: fmt.Sprintf("line %d", i)}
	}
	return out
}

type logsFixture struct {
	base   string
	client *http.Client
}

func newLogsFixture(t *testing.T, authz roleAuthorizer, local dockerLogs, remote *rangeRemote) logsFixture {
	t.Helper()
	u := testUser(t, "log-reader", "correct horse battery staple")
	srv := newWriteEndpointTestServer(t, u, authz, nil, func(d *api.Deps) {
		d.Collection = fakeCollection{docker: rangeDocker{fakeDockerClient: &fakeDockerClient{}, src: local}}
		if remote != nil {
			d.RemoteLogs = *remote
		}
	})
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, u.Username, "correct horse battery staple").Body.Close()
	return logsFixture{base: srv.URL + "/api/v1/containers/web/logs", client: client}
}

type logsResponse struct {
	Lines []struct {
		Time      time.Time `json:"time"`
		Message   string    `json:"message"`
		Truncated bool      `json:"truncated"`
	} `json:"lines"`
	More           bool       `json:"more"`
	NextBefore     *time.Time `json:"next_before"`
	Truncated      bool       `json:"truncated"`
	ScanLimited    bool       `json:"scan_limited"`
	AvailableSince *time.Time `json:"available_since"`
	Fallback       bool       `json:"fallback"`
}

func (f logsFixture) get(t *testing.T, params url.Values) (int, logsResponse, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"?"+params.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out logsResponse
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func rangeParams(since, until time.Time, extra ...string) url.Values {
	v := url.Values{"since": {since.Format(time.RFC3339)}}
	if !until.IsZero() {
		v.Set("until", until.Format(time.RFC3339))
	}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return v
}

func TestContainerLogRangesUseTheContainerLogPermission(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	local := dockerLogs{lines: recentLines(30, now), honoursUntil: true}
	params := rangeParams(now.Add(-time.Hour), now.Add(-10*time.Minute), "limit", "5")
	for _, c := range []struct {
		name  string
		authz roleAuthorizer
		want  int
	}{
		{"viewer (no node.logs.read)", roleAuthorizer{role: user.RoleViewer}, http.StatusForbidden},
		{"operator", roleAuthorizer{role: user.RoleOperator}, http.StatusOK},
		{"operator of another node", roleAuthorizer{role: user.RoleOperator, nodeID: "node-9"}, http.StatusForbidden},
	} {
		f := newLogsFixture(t, c.authz, local, nil)
		if code, _, body := f.get(t, params); code != c.want {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	anonReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		newLogsFixture(t, roleAuthorizer{role: user.RoleOperator}, local, nil).base+"?"+params.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	anon, err := http.DefaultClient.Do(anonReq)
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", anon.StatusCode)
	}
}

func TestContainerLogRangeValidation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	f := newLogsFixture(t, roleAuthorizer{role: user.RoleOperator}, dockerLogs{honoursUntil: true}, nil)
	for _, c := range []struct {
		name   string
		params url.Values
		want   string
	}{
		{"reversed", rangeParams(now.Add(-time.Hour), now.Add(-2*time.Hour)), "since must be before until"},
		{"malformed", url.Values{"since": {"2026-13-01T00:00:00Z"}, "limit": {"5"}}, "RFC 3339"},
		{"not RFC 3339", url.Values{"since": {"1764000000"}, "limit": {"5"}}, "RFC 3339"},
		{"too wide", rangeParams(now.Add(-48*time.Hour), now.Add(-time.Hour)), "at most 24h"},
		{"future", rangeParams(now.Add(-time.Hour), now.Add(time.Hour)), "in the future"},
		{"limit", rangeParams(now.Add(-time.Hour), time.Time{}, "limit", "5000"), "limit must be"},
		{"tail with a range", rangeParams(now.Add(-time.Hour), now.Add(-time.Minute), "tail", "10"), "use limit"},
		{"until without since", url.Values{"until": {now.Format(time.RFC3339)}}, "since is required"},
	} {
		code, _, body := f.get(t, c.params)
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s, want 400 with %q", c.name, code, body, c.want)
		}
	}
	// A plain tail still works as before.
	if code, _, body := f.get(t, url.Values{"tail": {"10"}}); code != http.StatusOK {
		t.Errorf("legacy tail: %d %s", code, body)
	}
}

func TestContainerLogRangePagesAndFallsBack(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	lines := recentLines(300, now)
	since, until := now.Add(-5*time.Hour), now.Add(-100*time.Minute)
	for _, c := range []struct {
		name     string
		node     string
		remote   *rangeRemote
		fallback bool
	}{
		{"local node", "", nil, false},
		{"version 2 agent", "node-2", &rangeRemote{src: dockerLogs{lines: lines, honoursUntil: true}, supports: true}, false},
		{"known version 1 agent", "node-1", &rangeRemote{src: dockerLogs{lines: lines}, supports: false}, true},
		{"version 1 agent found out", "node-1", &rangeRemote{src: dockerLogs{lines: lines, refusesUntil: true}, supports: true}, true},
	} {
		f := newLogsFixture(t, roleAuthorizer{role: user.RoleOperator}, dockerLogs{lines: lines, honoursUntil: true}, c.remote)
		params := rangeParams(since, until, "limit", "3")
		if c.node != "" {
			params.Set("node", c.node)
		}
		code, page, body := f.get(t, params)
		if code != http.StatusOK || len(page.Lines) != 3 || page.Lines[2].Message != "line 200" || !page.More ||
			page.NextBefore == nil || page.Fallback != c.fallback {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		params.Set("before", page.NextBefore.Format(time.RFC3339Nano))
		code, page, body = f.get(t, params)
		if code != http.StatusOK || len(page.Lines) != 3 || page.Lines[2].Message != "line 197" {
			t.Errorf("%s: older page %d %s", c.name, code, body)
		}
	}
}

func TestContainerLogRangeExplainsMissingHistoryAndKeepsHostileLinesInert(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	oldest := now.Add(-30 * time.Minute)
	hostile := []docker.LogLine{
		{Time: oldest, Message: `<script>alert(1)</script>`},
		{Time: oldest.Add(time.Minute), Message: "\x1b[31mred\x1b[0m\ttabbed  spaced"},
		{Time: oldest.Add(2 * time.Minute), Message: strings.Repeat("z", 40_000)},
	}
	f := newLogsFixture(t, roleAuthorizer{role: user.RoleOperator}, dockerLogs{lines: hostile, honoursUntil: true}, nil)
	code, page, body := f.get(t, rangeParams(now.Add(-6*time.Hour), now.Add(-time.Minute)))
	if code != http.StatusOK || page.AvailableSince == nil || !page.AvailableSince.Equal(oldest) || page.More {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(body, "<script>") {
		t.Error("a log line's markup reached the response unescaped")
	}
	if page.Lines[0].Message != `<script>alert(1)</script>` || page.Lines[1].Message != "\x1b[31mred\x1b[0m\ttabbed  spaced" {
		t.Errorf("a log line was altered: %q %q", page.Lines[0].Message, page.Lines[1].Message)
	}
	if long := page.Lines[2]; !long.Truncated || len(long.Message) != 16<<10 {
		t.Errorf("long line: truncated=%v len=%d", long.Truncated, len(long.Message))
	}
}
