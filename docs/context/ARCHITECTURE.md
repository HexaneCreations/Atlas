
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

Future

- libp2p

---

## Identity

Today

- X.509
- CA
- Leaf certificates
- TOFU

Future

- Peer ID
- libp2p
