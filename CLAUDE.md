
# Atlas

Before writing any code, always read:

- docs/context/IMPLEMENTATION_CONTEXT.md
- docs/context/ARCHITECTURE.md
- docs/context/ARCHITECTURAL_CONSTRAINTS.md
- docs/context/ENGINEERING_GUIDE.md

These documents are the authoritative project context.

# Development Rules

- Backend first.
- Production-quality code only.
- Prefer integration tests over mocks whenever practical.
- Prefer code over documentation.
- Keep comments minimal (exported APIs and non-obvious logic only).
- Reuse existing abstractions before introducing new ones.
- Do not redesign completed architecture without explicit approval.
- Do not create ADRs, documentation, or roadmap updates unless explicitly requested.
- Continue milestone-by-milestone until blocked by:
  - an architectural decision,
  - a security concern,
  - a genuine implementation blocker,
  - or explicit user approval.

# Implementation Responses

Keep implementation responses concise.

Return only:

- What was implemented
- What was verified
- Blocker (if any)
- Next recommended task

Avoid lengthy explanations unless explicitly requested.

## General Principles

- Never assume missing requirements.
- Ask before making architectural decisions.
- Reuse existing code before creating new abstractions.
- Preserve backward compatibility whenever practical.

## Token Efficiency

- Keep implementation responses under 200 words unless requested.
- Avoid repeating previously established architecture.
- Avoid rewriting project summaries.
- Do not restate completed milestones.
- Focus responses on the current implementation only.
