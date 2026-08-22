<!-- claude-db:start -->
## Project memory

This project has persistent memory of past sessions, served by the `memory`
MCP server. Session summaries are injected at startup and the best matching
observation is injected above each prompt, but that is only ever a slice.

Search it *before* re-deriving something this project already knows. That
means before grepping or reading git history to reconstruct why something is
the way it is, before saying you lack context, and before asking the user to
re-explain a past decision or a failed approach: call `search`, then
`get_observations` for the ids worth reading.

Verifying against the code afterwards is right — memory records what was
done, the source shows what is. Starting there is what wastes the memory.

`search` covers *why*: past decisions, bugs, reasoning. `find_usages` is
the default the moment a question is about a symbol or identifier — before
editing or removing a shared or exported name, or to look one up at all —
since it tags the likely definition and re-derives the answer from the
current source on every call, so it is never stale. grep still wins for
everything that is not a code identifier: plain text, log files, comments,
strings, and scoping with grep flags `find_usages` does not expose.

When the user states a standing rule or preference, record it with
`remember` so it survives this session.
<!-- claude-db:end -->
