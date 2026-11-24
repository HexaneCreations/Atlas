//go:build integration

// The "Add server" install-code flow against real Postgres and the real
// role_permissions seed: the three fleet.enroll endpoints, and the
// unauthenticated POST /api/v1/agent/install/redeem. That redeem is one of
// the few public routes is pinned in public_routes_integration_test.go.
package app_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/hexane/atlas/internal/api/v1"
	corefleet "github.com/hexane/atlas/internal/core/fleet"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/build"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/log"
	storagefleet "github.com/hexane/atlas/internal/storage/fleet"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

const installTestVersion = "v9.9.9"

type installFixture struct {
	t    *testing.T
	base string
	pool *pgxpool.Pool
}

// bootInstallServer boots Atlas with fleet + libp2p and the two settings
// install commands need, running as a release build.
func bootInstallServer(t *testing.T) *installFixture {
	t.Helper()
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	prev := build.Version
	build.Version = installTestVersion
	t.Cleanup(func() { build.Version = prev })

	base := bootServerWith(t, func(c *config.Config) {
		c.PublicHostname = "atlas.example.test"
		c.Fleet.Enabled = true
		c.Fleet.Host = "127.0.0.1"
		c.Fleet.Port = freeTCPPort(t)
		c.Fleet.DataDir = t.TempDir()
		c.Fleet.LibP2PEnabled = true
		c.Fleet.LibP2PListenAddrs = []string{"/ip4/127.0.0.1/tcp/0"}
		c.Fleet.LibP2PAdvertisedAddr = "/ip4/203.0.113.10/tcp/4102"
	}, log.Discard())

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return &installFixture{t: t, base: base, pool: pool}
}

func newPeerID(t *testing.T) string {
	t.Helper()
	_, pub, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, err := peer.IDFromPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func (f *installFixture) send(client *http.Client, method, path, body string, header map[string]string) (int, string) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.base+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

type createdCommand struct {
	v1.CreateInstallCommandResponse
	code string
}

var commandCodeRE = regexp.MustCompile(`--install-code '(atlas_install_[0-9a-f]{64})'`)

func (f *installFixture) create(client *http.Client, body string) createdCommand {
	f.t.Helper()
	status, raw := f.send(client, http.MethodPost, "/api/v1/fleet/install-commands", body, nil)
	if status != http.StatusCreated {
		f.t.Fatalf("create: status %d: %s", status, raw)
	}
	var out createdCommand
	if err := json.Unmarshal([]byte(raw), &out.CreateInstallCommandResponse); err != nil {
		f.t.Fatalf("decode create: %v", err)
	}
	m := commandCodeRE.FindStringSubmatch(out.Command)
	if m == nil {
		f.t.Fatalf("command carries no install code: %q", out.Command)
	}
	out.code = m[1]
	return out
}

// redeemFrom posts a redeem as the client at ip (the address the trusted
// proxy chain would attest; see httpx.ClientIP), with no session.
func (f *installFixture) redeemFrom(ip, code, peerID string) (int, map[string]any) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"code": code, "peer_id": peerID})
	status, raw := f.send(http.DefaultClient, http.MethodPost, "/api/v1/agent/install/redeem", string(body),
		map[string]string{"X-Forwarded-For": ip + ", 172.18.0.2"})
	var decoded map[string]any
	_ = json.Unmarshal([]byte(raw), &decoded)
	if strings.Contains(raw, code) && code != "" {
		f.t.Errorf("redeem response echoes the code: %s", raw)
	}
	return status, decoded
}

var ipCounter struct {
	sync.Mutex
	n int
}

// freshIP returns a distinct documentation address per call, so tests that
// are not about the rate limit never share a per-IP bucket.
func freshIP() string {
	ipCounter.Lock()
	defer ipCounter.Unlock()
	ipCounter.n++
	return fmt.Sprintf("198.18.%d.%d", ipCounter.n/250, ipCounter.n%250+1)
}

func (f *installFixture) peerRow(peerID string) (nodeID, environment, role, status string, ok bool) {
	f.t.Helper()
	err := f.pool.QueryRow(context.Background(),
		`SELECT node_id, environment, role, status FROM agent_peers WHERE peer_id = $1`, peerID).
		Scan(&nodeID, &environment, &role, &status)
	return nodeID, environment, role, status, err == nil
}

func (f *installFixture) codeRow(id string) (redeemedPeer, redeemedIP string, redeemed bool) {
	f.t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT redeemed_at, COALESCE(redeemed_peer_id, ''), COALESCE(host(redeemed_ip), '') FROM install_codes WHERE id = $1`, id).
		Scan(&at, &redeemedPeer, &redeemedIP); err != nil {
		f.t.Fatalf("read install code %s: %v", id, err)
	}
	return redeemedPeer, redeemedIP, at != nil
}

func (f *installFixture) list(client *http.Client) (v1.ListInstallCommandsResponse, string) {
	f.t.Helper()
	status, raw := f.send(client, http.MethodGet, "/api/v1/fleet/install-commands", "", nil)
	if status != http.StatusOK {
		f.t.Fatalf("list: status %d: %s", status, raw)
	}
	var out v1.ListInstallCommandsResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		f.t.Fatalf("decode list: %v", err)
	}
	return out, raw
}

func statusOf(list v1.ListInstallCommandsResponse, id string) string {
	for _, c := range list.InstallCommands {
		if c.ID == id {
			return c.Status
		}
	}
	return ""
}

// admin and superadmin may create, list and revoke; operator and viewer get
// 403 on all three and change nothing.
func TestInstallCommandsAreAdminAndSuperadminOnly(t *testing.T) {
	f := bootInstallServer(t)
	admin := authenticatedTestClient(t, f.base, coreuser.RoleAdmin)
	victim := f.create(admin, `{"environment":"production"}`)

	for _, c := range []struct {
		role  string
		allow bool
	}{
		{coreuser.RoleSuperadmin, true},
		{coreuser.RoleAdmin, true},
		{coreuser.RoleOperator, false},
		{coreuser.RoleViewer, false},
	} {
		t.Run(c.role, func(t *testing.T) {
			client := authenticatedTestClient(t, f.base, c.role)
			post, _ := f.send(client, http.MethodPost, "/api/v1/fleet/install-commands", `{"environment":"production","ttl_minutes":5}`, nil)
			get, _ := f.send(client, http.MethodGet, "/api/v1/fleet/install-commands", "", nil)
			target := victim.ID
			if c.allow {
				target = f.create(client, `{"environment":"staging"}`).ID
			}
			del, _ := f.send(client, http.MethodDelete, "/api/v1/fleet/install-commands/"+target, "", nil)

			want := [3]int{http.StatusForbidden, http.StatusForbidden, http.StatusForbidden}
			if c.allow {
				want = [3]int{http.StatusCreated, http.StatusOK, http.StatusNoContent}
			}
			if got := [3]int{post, get, del}; got != want {
				t.Fatalf("POST/GET/DELETE = %v, want %v", got, want)
			}
		})
	}

	list, _ := f.list(admin)
	if got := statusOf(list, victim.ID); got != corefleet.InstallCodePending {
		t.Errorf("a refused revoke changed the code: status %q, want pending", got)
	}
}

// Creation returns a command whose values match the server's config; the
// list never carries a code or its hash; a redeem creates the agent_peers
// row for the pre-assigned node with the default grant; the code is then
// spent for every other peer.
func TestInstallCodeRedeemAuthorizesThePeerOnceForThePreassignedNode(t *testing.T) {
	f := bootInstallServer(t)
	admin := authenticatedTestClient(t, f.base, coreuser.RoleAdmin)
	created := f.create(admin, `{"environment":"production"}`)

	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(created.NodeID) {
		t.Fatalf("node id %q is not 32 hex", created.NodeID)
	}
	if d := time.Until(created.ExpiresAt); d < 29*time.Minute || d > 31*time.Minute {
		t.Errorf("default expiry in %v, want ~30m", d)
	}
	for _, want := range []string{
		"--version '" + installTestVersion + "'",
		"--control-plane-url 'https://atlas.example.test'",
		"--server-addr '/ip4/203.0.113.10/tcp/4102/p2p/12D3KooW",
		"raw.githubusercontent.com/HexaneCreations/Atlas/" + installTestVersion + "/packaging/atlas-agent/install.sh",
	} {
		if !strings.Contains(created.Command, want) {
			t.Errorf("command lacks %q: %s", want, created.Command)
		}
	}

	var storedHash string
	if err := f.pool.QueryRow(context.Background(), `SELECT code_hash FROM install_codes WHERE id = $1`, created.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != corefleet.HashToken(created.code) {
		t.Error("the stored hash is not the SHA-256 of the code")
	}
	list, raw := f.list(admin)
	if strings.Contains(raw, created.code) || strings.Contains(raw, storedHash) || strings.Contains(raw, "atlas_install_") {
		t.Fatalf("the list exposes a code or its hash: %s", raw)
	}
	if statusOf(list, created.ID) != corefleet.InstallCodePending {
		t.Fatalf("new code is not pending: %+v", list)
	}

	peerA, peerB := newPeerID(t), newPeerID(t)
	ip := freshIP()
	status, body := f.redeemFrom(ip, created.code, peerA)
	if status != http.StatusOK || body["node_id"] != created.NodeID || body["environment"] != "production" || len(body) != 2 {
		t.Fatalf("redeem: %d %v, want 200 {node_id: %s, environment: production}", status, body, created.NodeID)
	}
	node, env, role, st, ok := f.peerRow(peerA)
	if !ok || node != created.NodeID || env != "production" || role != "agent" || st != "active" {
		t.Fatalf("agent_peers row = %q %q %q %q (found %v)", node, env, role, st, ok)
	}
	granted, err := storagefleet.NewRepository(f.pool).IsGranted(context.Background(), created.NodeID, corefleet.OperationContainerLogs)
	if err != nil || !granted {
		t.Errorf("default container_logs grant missing: %v %v", granted, err)
	}
	if gotPeer, gotIP, redeemed := f.codeRow(created.ID); !redeemed || gotPeer != peerA || gotIP != ip {
		t.Errorf("install code row: peer %q ip %q redeemed %v", gotPeer, gotIP, redeemed)
	}

	// Single use: neither another peer nor the same peer can redeem again.
	for _, p := range []string{peerB, peerA} {
		if status, _ := f.redeemFrom(freshIP(), created.code, p); status != http.StatusUnauthorized {
			t.Errorf("second redeem by %s: status %d, want 401", p, status)
		}
	}
	if _, _, _, _, ok := f.peerRow(peerB); ok {
		t.Error("a second peer was authorized from a spent code")
	}
	if list, _ := f.list(admin); statusOf(list, created.ID) != corefleet.InstallCodeRedeemed {
		t.Error("list does not report the code as redeemed")
	}
}

func TestInstallCodeConcurrentRedeemsExactlyOneWins(t *testing.T) {
	f := bootInstallServer(t)
	admin := authenticatedTestClient(t, f.base, coreuser.RoleAdmin)
	created := f.create(admin, `{"environment":"production"}`)

	const n = 8
	peers := make([]string, n)
	for i := range peers {
		peers[i] = newPeerID(t)
	}
	statuses := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		ip := freshIP()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			statuses[i], _ = f.redeemFrom(ip, created.code, peers[i])
		}()
	}
	close(start)
	wg.Wait()

	wins := 0
	for _, s := range statuses {
		switch s {
		case http.StatusOK:
			wins++
		case http.StatusUnauthorized:
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	var rows int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_peers WHERE peer_id = ANY($1) OR node_id = $2`, peers, created.NodeID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if wins != 1 || rows != 1 {
		t.Fatalf("wins = %d, agent_peers rows = %d; want exactly 1 and 1 (statuses %v)", wins, rows, statuses)
	}
}

func TestInstallCodeExpiredIsRefused(t *testing.T) {
	f := bootInstallServer(t)
	admin := authenticatedTestClient(t, f.base, coreuser.RoleAdmin)
	created := f.create(admin, `{"environment":"production","ttl_minutes":1}`)
	if d := time.Until(created.ExpiresAt); d > time.Minute+5*time.Second {
		t.Errorf("ttl_minutes=1 expires in %v", d)
	}
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE install_codes SET created_at = now() - interval '10 minutes', expires_at = now() - interval '1 second'
		WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}

	peerID := newPeerID(t)
	if status, _ := f.redeemFrom(freshIP(), created.code, peerID); status != http.StatusUnauthorized {
		t.Fatalf("expired redeem: status %d, want 401", status)
	}
	if _, _, _, _, ok := f.peerRow(peerID); ok {
		t.Error("an expired code authorized a peer")
	}
	if list, _ := f.list(admin); statusOf(list, created.ID) != corefleet.InstallCodeExpired {
		t.Error("list does not report the code as expired")
	}
	if status, _ := f.send(admin, http.MethodDelete, "/api/v1/fleet/install-commands/"+created.ID, "", nil); status != http.StatusPreconditionFailed {
		t.Errorf("revoking an expired code: status %d, want 412", status)
	}
}

func TestInstallCodeTTLIsBounded(t *testing.T) {
	f := bootInstallServer(t)
	admin := authenticatedTestClient(t, f.base, coreuser.RoleAdmin)
	for _, body := range []string{
		`{"environment":"production","ttl_minutes":61}`,
		`{"environment":"production","ttl_minutes":0}`,
		`{"environment":"prod uction"}`,
		`{"environment":"production","node_id":"0123456789abcdef0123456789abcdef"}`,
	} {
		if status, raw := f.send(admin, http.MethodPost, "/api/v1/fleet/install-commands", body, nil); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", body, status, raw)
		}
	}
}

func TestInstallCodeRevokedIsRefused(t *testing.T) {
	f := bootInstallServer(t)
	admin := authenticatedTestClient(t, f.base, coreuser.RoleAdmin)
	created := f.create(admin, `{"environment":"production"}`)

	if status, raw := f.send(admin, http.MethodDelete, "/api/v1/fleet/install-commands/"+created.ID, "", nil); status != http.StatusNoContent {
		t.Fatalf("revoke: status %d: %s", status, raw)
	}
	peerID := newPeerID(t)
	if status, _ := f.redeemFrom(freshIP(), created.code, peerID); status != http.StatusUnauthorized {
		t.Fatalf("revoked redeem: status %d, want 401", status)
	}
	if _, _, _, _, ok := f.peerRow(peerID); ok {
		t.Error("a revoked code authorized a peer")
	}
	list, _ := f.list(admin)
	if statusOf(list, created.ID) != corefleet.InstallCodeRevoked {
		t.Error("list does not report the code as revoked")
	}
	if status, _ := f.send(admin, http.MethodDelete, "/api/v1/fleet/install-commands/"+created.ID, "", nil); status != http.StatusPreconditionFailed {
		t.Errorf("second revoke: status %d, want 412", status)
	}
	if status, _ := f.send(admin, http.MethodDelete, "/api/v1/fleet/install-commands/no-such-id", "", nil); status != http.StatusNotFound {
		t.Errorf("revoke unknown: status %d, want 404", status)
	}
}

// An already-authorized Peer ID is refused and its existing binding is not
// overwritten; nothing is written, so the code stays usable for a fresh peer.
func TestInstallCodeRefusesAnAlreadyAuthorizedPeerAndWritesNothing(t *testing.T) {
	f := bootInstallServer(t)
	admin := authenticatedTestClient(t, f.base, coreuser.RoleAdmin)
	repo := storagefleet.NewRepository(f.pool)

	existing, _ := corefleet.NewNodeID()
	peerID := newPeerID(t)
	if err := repo.RegisterPeer(context.Background(), corefleet.PeerSpec{PeerID: peerID, NodeID: existing, Environment: "development"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RevokePeer(context.Background(), peerID); err != nil {
		t.Fatal(err)
	}
	created := f.create(admin, `{"environment":"production"}`)

	if status, _ := f.redeemFrom(freshIP(), created.code, peerID); status != http.StatusUnauthorized {
		t.Fatalf("redeem with a registered (revoked) peer: status %d, want 401", status)
	}
	node, env, _, st, _ := f.peerRow(peerID)
	if node != existing || env != "development" || st != "revoked" {
		t.Fatalf("existing peer row changed to %q %q %q", node, env, st)
	}
	if _, _, redeemed := f.codeRow(created.ID); redeemed {
		t.Fatal("a refused redeem spent the code")
	}
	var grants int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_operation_grants WHERE node_id = $1`, created.NodeID).Scan(&grants)
	if grants != 0 {
		t.Fatal("a refused redeem wrote a grant")
	}

	fresh := newPeerID(t)
	if status, body := f.redeemFrom(freshIP(), created.code, fresh); status != http.StatusOK || body["node_id"] != created.NodeID {
		t.Fatalf("redeem with a fresh peer after the refusal: %d %v", status, body)
	}
}

// A pre-assigned node that has meanwhile appeared (in nodes or agent_peers)
// is never bound to a new peer.
func TestInstallCodeRefusesWhenThePreassignedNodeExists(t *testing.T) {
	f := bootInstallServer(t)
	admin := authenticatedTestClient(t, f.base, coreuser.RoleAdmin)
	repo := storagefleet.NewRepository(f.pool)

	viaNodes := f.create(admin, `{"environment":"production"}`)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO nodes (node_id, hostname) VALUES ($1, 'already-here')`, viaNodes.NodeID); err != nil {
		t.Fatal(err)
	}
	viaPeers := f.create(admin, `{"environment":"production"}`)
	squatter := newPeerID(t)
	if err := repo.RegisterPeer(context.Background(), corefleet.PeerSpec{PeerID: squatter, NodeID: viaPeers.NodeID, Environment: "production"}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []createdCommand{viaNodes, viaPeers} {
		peerID := newPeerID(t)
		if status, _ := f.redeemFrom(freshIP(), c.code, peerID); status != http.StatusUnauthorized {
			t.Fatalf("redeem for existing node %s: status %d, want 401", c.NodeID, status)
		}
		if _, _, _, _, ok := f.peerRow(peerID); ok {
			t.Error("a peer was bound to a node that already exists")
		}
		if _, _, redeemed := f.codeRow(c.ID); redeemed {
			t.Error("a refused redeem spent the code")
		}
	}
	if node, _, _, _, _ := f.peerRow(squatter); node != viaPeers.NodeID {
		t.Error("the existing peer binding was changed")
	}
}

// Every failure is the same status and body, apart from the request id.
func TestInstallRedeemFailuresAreIndistinguishable(t *testing.T) {
	f := bootInstallServer(t)
	admin := authenticatedTestClient(t, f.base, coreuser.RoleAdmin)
	repo := storagefleet.NewRepository(f.pool)

	spent := f.create(admin, `{"environment":"production"}`)
	if status, _ := f.redeemFrom(freshIP(), spent.code, newPeerID(t)); status != http.StatusOK {
		t.Fatal("setup redeem failed")
	}
	revoked := f.create(admin, `{"environment":"production"}`)
	f.send(admin, http.MethodDelete, "/api/v1/fleet/install-commands/"+revoked.ID, "", nil)
	expired := f.create(admin, `{"environment":"production"}`)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE install_codes SET created_at = now() - interval '10 minutes', expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	pending := f.create(admin, `{"environment":"production"}`)
	registered := newPeerID(t)
	other, _ := corefleet.NewNodeID()
	if err := repo.RegisterPeer(context.Background(), corefleet.PeerSpec{PeerID: registered, NodeID: other, Environment: "production"}); err != nil {
		t.Fatal(err)
	}
	unknown, _ := corefleet.NewInstallCode()

	cases := map[string][2]string{
		"wrong prefix":       {corefleet.TokenPrefix + strings.Repeat("0", 64), newPeerID(t)},
		"unknown code":       {unknown.Plaintext, newPeerID(t)},
		"spent":              {spent.code, newPeerID(t)},
		"revoked":            {revoked.code, newPeerID(t)},
		"expired":            {expired.code, newPeerID(t)},
		"bad peer id":        {pending.code, "12D3KooWnope"},
		"peer already known": {pending.code, registered},
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)

	var want string
	for _, name := range names {
		status, body := f.redeemFrom(freshIP(), cases[name][0], cases[name][1])
		e, _ := body["error"].(map[string]any)
		if status != http.StatusUnauthorized || e == nil {
			t.Fatalf("%s: %d %v, want 401 envelope", name, status, body)
		}
		delete(e, "request_id")
		got, _ := json.Marshal(body)
		if want == "" {
			want = string(got)
		} else if string(got) != want {
			t.Errorf("%s: body %s differs from %s", name, got, want)
		}
	}
	if _, _, redeemed := f.codeRow(pending.ID); redeemed {
		t.Error("a refused redeem spent the pending code")
	}
}

func TestInstallRedeemIsRateLimitedPerClientIP(t *testing.T) {
	f := bootInstallServer(t)
	ip := freshIP()
	unknown, _ := corefleet.NewInstallCode()
	peerID := newPeerID(t)
	for i := range corefleet.RedeemPerIPPerMinute {
		if status, _ := f.redeemFrom(ip, unknown.Plaintext, peerID); status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, status)
		}
	}
	if status, _ := f.redeemFrom(ip, unknown.Plaintext, peerID); status != http.StatusTooManyRequests {
		t.Fatalf("attempt %d from one IP: status %d, want 429", corefleet.RedeemPerIPPerMinute+1, status)
	}
	if status, _ := f.redeemFrom(freshIP(), unknown.Plaintext, peerID); status != http.StatusUnauthorized {
		t.Fatalf("another IP: status %d, want 401 (its own budget)", status)
	}
}

// An existing node's rows (nodes, agent_peers, grants) are byte-identical
// after a full create + redeem + revoke cycle for a new machine.
func TestInstallFlowLeavesExistingNodesUntouched(t *testing.T) {
	f := bootInstallServer(t)
	admin := authenticatedTestClient(t, f.base, coreuser.RoleAdmin)
	repo := storagefleet.NewRepository(f.pool)
	ctx := context.Background()

	existing, _ := corefleet.NewNodeID()
	existingPeer := newPeerID(t)
	if _, err := f.pool.Exec(ctx, `INSERT INTO nodes (node_id, hostname) VALUES ($1, 'existing-host')`, existing); err != nil {
		t.Fatal(err)
	}
	if err := repo.RegisterPeer(ctx, corefleet.PeerSpec{PeerID: existingPeer, NodeID: existing, Environment: "production"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Grant(ctx, existing, corefleet.OperationContainerLogs, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var out string
		if err := f.pool.QueryRow(ctx, `
			SELECT concat_ws('|',
				(SELECT row_to_json(n)::text FROM nodes n WHERE node_id = $1),
				(SELECT string_agg(row_to_json(p)::text, ',' ORDER BY peer_id) FROM agent_peers p WHERE node_id = $1),
				(SELECT string_agg(row_to_json(g)::text, ',' ORDER BY operation) FROM agent_operation_grants g WHERE node_id = $1))`,
			existing).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()

	created := f.create(admin, `{"environment":"production"}`)
	if created.NodeID == existing {
		t.Fatal("an existing node id was pre-assigned")
	}
	f.redeemFrom(freshIP(), created.code, newPeerID(t))
	f.redeemFrom(freshIP(), created.code, existingPeer)
	other := f.create(admin, `{"environment":"production"}`)
	f.send(admin, http.MethodDelete, "/api/v1/fleet/install-commands/"+other.ID, "", nil)

	if after := snapshot(); after != before {
		t.Fatalf("existing node data changed:\nbefore %s\nafter  %s", before, after)
	}
}
