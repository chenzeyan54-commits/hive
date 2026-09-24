- [Admin MCP: server skeleton and refusal contract](admin-mcp.md) — **partly shipped (v6).**
  Foundation for the operator-facing admin MCP (#8699, part of #8697 phase 1):
  the design record, a `cmd/` binary speaking MCP over stdio via the official
  Go SDK sharing `pkg/hivectl`'s client rather than carrying a second one. A
  hive roster with exactly one active selection — changed only by an explicit
  selection call, never as a side effect of another tool — that verifies
  reachability and credential auth before any operation, diagnosing a
  direct-route spoke distinctly from a wrong token. Read-only by default with
  writes explicitly enabled, and the refusal contract: no `X-Hive-User` /
  `X-Hive-Role` / `X-Hive-Owner-Role-Verified` ever sent, the credential in
  the `Authorization` header only and never a URL, never a result. Cross-
  referenced from [task-mcp.md](task-mcp.md); that page remains the agent-
  facing surface.