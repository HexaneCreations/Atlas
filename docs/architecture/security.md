
# Atlas Security Architecture

## Status

- Status: Proposed
- Scope: Atlas Control Plane, Atlas Agent, Fleet/libp2p communication
- Production and Development environments
- Future User Authentication and RBAC
- Last Updated: 2026-08-12

---

# 1. Purpose

This document defines the security architecture for Atlas.

The primary security requirement is that an Atlas Agent must only accept
control operations from a Control Plane that is both:

1. Cryptographically authenticated.
2. Explicitly authorized to control that Agent.

Authentication alone is not authorization.

The architecture must support the same physical Agent being connected to
multiple independent Control Planes, such as:

- Production Atlas
- Development Atlas

without requiring separate testing Agents.

---

# 2. Core Security Principle

Every privileged operation must establish:

    Who is requesting the operation?
        +
    Which Agent is being targeted?
        +
    Is the requester authorized to control that Agent?
        +
    Is the requested operation permitted?

The system must never treat the following as sufficient authorization:

- IP address
- hostname
- container name
- Docker network membership
- HTTP headers
- static shared secrets
- source IP
- libp2p connectivity alone
- successful TCP connection
- successful TLS connection alone

These mechanisms may provide transport security or authentication,
but authorization must be explicitly enforced.

---

# 3. High-Level Architecture

The target architecture is:

```text
                         ┌──────────────────────────┐
                         │      User / Operator     │
                         └────────────┬─────────────┘
                                      │
                              Future Authentication
                                      │
                                      ▼
                         ┌──────────────────────────┐
                         │      User RBAC           │
                         │      (Future)            │
                         └────────────┬─────────────┘
                                      │
                                      ▼
                         ┌──────────────────────────┐
                         │     Atlas Control Plane  │
                         │                          │
                         │ Production / Development │
                         └────────────┬─────────────┘
                                      │
                            Control Plane Identity
                                      │
                         Explicit Agent Authorization
                                      │
                                      ▼
                              ┌──────────────┐
                              │    Agent     │
                              │              │
                              │ agent-001    │
                              └──────────────┘
```
