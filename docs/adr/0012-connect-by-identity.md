# ADR-0012: Connect by identity, never by address

- **Status:** Accepted
- **Date:** 2026-08-08
- **Phase:** 4

## Context

The agent milestone shipped with a production-ready installer and a real
end-to-end verification: a systemd-managed `atlas-agent` enrolling to an
`atlas-server` over HTTPS with mTLS, surviving a simulated reboot, and
resuming monitoring without re-enrolling. That verification assumed the
control plane is reachable at a fixed URL.

It is not, in the deployment this product is meant for. `atlas-server` may run
on a laptop or workstation behind NAT with only a private IP — no forwarded
port, no public IP, and deliberately no dependency on a third-party tunnel or
overlay network (Cloudflare Tunnel, Tailscale). Address-based dialing
(`ATLAS_AGENT_CONTROL_PLANE_URL=https://host:port`) cannot work against a
control plane with no stable, reachable address, regardless of which protocol
rides on top of it. This is a reachability problem, not a protocol problem:
swapping HTTP for another request/response protocol at the same address does
not change whether that address can be dialed.

The deeper issue is what an agent is actually configured to trust. A URL
names a location, not the thing that should be running there. Every time the
control plane moves — laptop to workstation, workstation to a cloud host — the
address changes and every agent's configuration must change with it, even
though the entity the agent actually needs to trust (the control plane's
identity) has not changed at all.

## Decision

**Atlas agents connect to the control plane's cryptographic identity, not to
its network address.**

The control plane and each agent hold a long-term Peer ID. Discovery, NAT
traversal, and relaying — getting two identified peers into a connected state
at all — become the job of a dedicated bootstrap/relay component (Atlas
Relay). Once a libp2p-encrypted stream exists between two peers, everything
built on top of it is unchanged: the existing X.509 enrollment flow, telemetry,
inventory, spool, batching, backpressure, replay protection, and clock-skew
handling all run exactly as they do today, inside that stream instead of
inside a plain TCP connection.

```
 Atlas Relay
 bootstrap + discovery
        │
   ┌────┴────┐
   │         │
Atlas    Atlas
Server   Agent
PeerID   PeerID
   └────┬────┘
        │  libp2p encrypted stream
        │
  existing Transport interface (ADR-0005)
        │
  enrollment · telemetry · inventory · spool · fleet model
  (unchanged)
```

Peer identity and X.509 identity are kept as two separate keypairs, not one
unified identity:

| Concern | Owner |
| --- | --- |
| Discovery, routing, peer connectivity, NAT traversal, relay | libp2p Peer ID |
| Enrollment, authorization, certificate lifecycle, machine trust | X.509 (existing CA/leaf model) |

This is a deliberate boundary, not a shortcut. libp2p answers "how do I reach
this peer"; X.509 answers "should I trust what I'm now connected to, and for
what." Collapsing them into one identity would make a routing-layer concern
(how peers find each other) the same object as an authorization-layer concern
(what a peer is allowed to do), which is the wrong coupling — it is the same
mistake as authenticating an HTTP request by trusting its source IP. Keeping
them separate also means a compromised or misbehaving relay can see that two
peers are talking, but not authenticate as either of them, and the existing
TOFU-pinned CA and cert-issuance logic in `internal/agent/credentials.go`
needs no redesign to sit on top of a different underlying connection.

This also strengthens the trust story at first contact. Today, first
enrollment trusts whatever CA is presented by the server answering at a
configured URL (TOFU by network location). Once dialing is identity-based, an
operator hands an agent the control plane's Peer ID up front, alongside its
enrollment token — trust begins at identity, before the first byte, rather
than at whatever happens to be listening at an address.

## Why the existing seam makes this possible

ADR-0005 put a `Transport` interface between collection and storage
specifically so that changing how observations travel would never require
changing what produces or stores them. This decision is that seam being used
for exactly the case it was built for: identity-based transport is a new
`Transport` implementation, not a new Agent architecture. The Agent
composition root, the fleet model, the inventory and telemetry payload
models, and the storage layer do not change. Only the thing underneath
`Transport.Send` — and the enrollment/renewal dial in `internal/agent` —
changes what it dials.

## Alternatives considered

**Keep HTTPS and solve reachability with a tunnel/overlay (Cloudflare Tunnel,
Tailscale).** Rejected by explicit product requirement: no dependency on
third-party network infrastructure, and no operator-managed port forwarding.

**Unify libp2p Peer ID and X.509 identity into one keypair now.** More
conceptually pure — one identity, not two. Rejected for the initial
implementation: it requires converting between `crypto/x509` and libp2p's key
types and touches the CA/TOFU-pinning logic that was only just hardened.
Deferred as a possible later refinement once the separate-identity version is
proven; nothing in this decision forecloses it.

**Replace HTTPS outright rather than adding a second `Transport`
implementation.** Rejected: HTTPS + mTLS is verified, hardened, and correct
for the common case of a control plane on a normally reachable host (most
production deployments). The problem being solved is specific to one
deployment topology; the fix should be additive, not a rewrite of a system
just proven correct end to end.

## Consequences

**Good.**

- The control plane can move between a laptop, a workstation, a home lab, and
  a cloud host without reconfiguring a single agent — the thing agents trust
  did not change, only where it happens to be running.
- Deployment flexibility improves without weakening `Observe Everything,
  Control Nothing.`: Atlas Relay only ever helps peers find and reach each
  other. It never terminates the enrollment, authorization, or telemetry
  protocol, and never sees anything Atlas collects — it sits below the
  encrypted stream, not inside it.
- First-contact trust improves: pinning a Peer ID up front is a stronger
  starting point than TOFU-by-URL.
- No change to the Agent architecture, the backend, the telemetry model, the
  inventory model, or the fleet model. The blast radius is the transport
  layer and the enrollment dial, exactly as ADR-0005 intended.

**Costs.**

- A new operational component (Atlas Relay) is required somewhere reachable;
  this trades a third-party dependency for a self-operated one, not for zero
  infrastructure.
- Two cryptographic identity systems to reason about per peer instead of one,
  until/unless a future ADR unifies them.
- Relay nodes observe connection metadata — which peers are trying to reach
  each other, and when — even though they never see payload. Acceptable
  because Atlas Relay is self-hosted, not a third party, but worth stating
  plainly rather than implying zero metadata exposure.
- Hole-punch failure against symmetric NAT still falls back to relaying
  traffic, which is a real bandwidth and availability dependency on the relay
  component in that case.

**Revisit if** the two-identity boundary between libp2p and X.509 proves to be
friction rather than protection in practice, or if the product requirement
driving this (control plane with no stable public address) is dropped from
the roadmap.

## Implementation order

This ADR records the decision and the target shape only. Implementation is
deliberately sequenced after the platform is feature-complete, not
concurrent with it:

1. Complete the remaining backend milestones.
2. Complete the core observability platform.
3. Once the platform is feature-complete, add libp2p as a second `Transport`
   implementation behind the interface ADR-0005 already established, and
   validate that identity-based networking drops in without changing the
   Agent architecture.
