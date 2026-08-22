
# Atlas Architecture

## Backend

- Modular Monolith
- Explicit Constructor Injection
- No DI Container
- lifecycle.Supervisor controls startup/shutdown

---

## Agent

atlas-agent

Responsibilities

- Collect metrics
- Collect inventory
- Collect events
- Push to control plane

Production

- systemd service
- install.sh
- uninstall.sh

---

## Fleet

fleetPipeline

- Enroll
- Renew
- Heartbeat
- Telemetry

Router

Envelope

Transport

---

## Storage

PostgreSQL

TimescaleDB

Hypertables

- metric_samples
- events
- alert_history

Tables

- nodes
- alert_rules
- alert_states
- incidents
- incident_members

---

## Event Pipeline

eventbus

↓

eventstore.Writer

↓

Tap

↓

Alert Engine

↓

Incident Engine

---

## Transport

Transport Interface

Send()

Close()

Implementations

- InProcess
- HTTPS + mTLS
- libp2p (plain HTTP inside a Noise-encrypted stream)

---

## Identity

HTTPS transport

- X.509
- CA
- Leaf certificates
- TOFU
- Enrollment token

libp2p transport

- Peer ID (authentication, proven by the Noise handshake)
- agent_peers allowlist (authorization, keyed by Peer ID)
- NodeID + environment read from the agent_peers record

No X.509, token or TLS is involved inside a libp2p stream. See
docs/adr/0012-connect-by-identity.md.
