
# Architectural Constraints

These decisions are stable.

Do not redesign without explicit approval.

---

## Platform

- Atlas is read-only.
- Backend is Modular Monolith.
- UI follows backend.

---

## Transport

- ADR-0005
- Transport abstraction is permanent.
- HTTPS + mTLS today.
- libp2p later.

---

## Storage

Metrics

- Historical

Inventory

- Latest Only

Events

- Durable

Alerts

- Built on Events

Incidents

- Built on Alerts + Events

---

## Engineering

- Reuse abstractions.
- No duplicate models.
- Backward compatible APIs.
- Truth over convenience.
