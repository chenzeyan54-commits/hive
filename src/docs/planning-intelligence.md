# Planning Intelligence

**Planning intelligence** turns a big, vague GitHub issue into an ordered,
reviewable plan of small sub-tasks — and gates that plan behind a human approval
before any agent starts work. It is how Hive handles work that is too large for a
single agent kick: instead of one agent thrashing on an epic, the **architect**
lane decomposes it into a DAG of child beads, a human reviews the plan, and only
then are the children released to the fleet.

The feature has four moving parts:

1. **Decompose** — the architect breaks an epic into ordered, dependency-linked,
   execution-tagged sub-tasks (child beads).
2. **Plan-review gate** — a decomposed plan starts as a **draft**; its children
   are withheld from `Ready()` (no agent can claim them) until a human approves.
3. **Stall-replan** — if an approved plan stops progressing, the governor
   re-kicks the architect to revise the unfinished work, bounded by a replan cap.
4. **The issue entry point** (this page's focus) — how a GitHub issue *becomes*
   an epic to plan, by label or by a dashboard click.

## Planning a GitHub issue

There are two ways to ask Hive to plan an issue. **The `plan` label is the
primary, first-class trigger** — maintainers think in labels, and it needs no
dashboard at all. The dashboard button is a convenience for the same flow.

### 1. The `plan` label (recommended)

Add the label **`plan`** (or **`epic`**) to any issue in a repo Hive watches. On
the next eval cycle Hive:

- mints an **epic bead** from the issue (idempotently — labeling the same issue
  twice never creates a second epic),
- marks it `plan_status=draft` and `decompose_pending`, so it shows up
  immediately in the dashboard as a queued plan, and
- hands it to the architect to decompose.

When the architect finishes, the plan appears in the plan-review view for your
approval. Remove/re-add the label at will — the epic is keyed to the issue.

The label trigger is **OFF by default** and must be enabled explicitly. The
label path feeds a raw issue body into the architect's kick prompt with no
per-kick review, so a maintainer merely labeling an attacker's issue would
otherwise auto-fire attacker-controlled text into the highest-autonomy agent.
Making it opt-in forces an operator to consciously accept that. When enabled, it
still only fires at ACMM **L5+** (where the decomposing architect is scheduled),
so enabling it below L5 is inert.

```yaml
planning:
  plan_from_label: true   # opt in — fires at ACMM L5+; omit/false = off (default)
```

### 2. The **⧉ Plan** button (dashboard)

Every actionable issue pill on the dashboard has a **⧉ Plan** button. Click it to
run the exact same flow as the label — mint the epic and request decomposition —
without leaving the dashboard. The button is always available (it does not depend
on ACMM level); the label path is what the ACMM gate governs.

## The architect is a shared agent — and its pause is respected

Decomposition is driven by the **architect** agent, which is a *general-purpose*
lane with its own pre-existing job (RFCs, refactor and performance scans).
Decomposition is just one more thing it does, so:

> **If you have paused the architect, Hive does NOT auto-unpause it and does NOT
> silently drop your plan request.** The epic is minted and queued
> (`decompose_pending`), and it waits until you resume the architect.

When a plan is queued because the architect is paused, Hive tells you clearly —
in the **⧉ Plan** toast, and on the governor **PLANNING** tile:

> ⏸ Architect is paused — this plan won't be built until you resume the architect agent.

You paused it deliberately; this message makes sure you know that is *why*
nothing is happening, and points you at the fix. Resume the architect (or let its
cadence fire) and it picks up every queued plan on the next cycle, clears the
`decompose_pending` marker as it materializes children, and the normal
plan-review flow takes over.

Everything here drives the architect through the same out-of-band kick path the
governor already uses (`SendKick` / `IsPaused`); it never touches the
agent-launch mutex.

## Reviewing and approving a plan

A decomposed epic is a **draft**: its child beads are hidden from `Ready()`, so no
agent can start the work yet. In the plan-review view you can:

- inspect the ordered children with their execution tags (`agent_suitable` /
  `human_required`), dependency edges, **who has each task** (the child bead's
  actor), **its pull request** when a working agent recorded one (`pr_url`, or
  `pr_repo` + `pr_number`, or an `external_ref` that is itself a PR URL), and a
  status colour per child,
- **retag** or **remove** a child before approving,
- **approve** — releasing the children through `Ready()` so the fleet can claim
  them, or
- **reject** — returning the plan to draft (re-gating the children) so the
  architect can revise it.

High-maturity ACMM packs may enable `plan_auto_approve`, which approves a plan at
decomposition time (no review gate); by default the gate is on.

## The PLANNING governor tile

The governor's **PLANNING** metric summarizes plan state across all bead stores:

- **active** — epics that have been decomposed (draft or approved),
- **review** — drafts awaiting human approval,
- **queued** — issue-sourced epics not yet built by the architect
  (`decompose_pending`) and still inside the stuck threshold,
- **⚠ stuck** — epics that have been `decompose_pending` for longer than
  `planning.DecomposeStuckAfter` (12h, three missed architect cycles at ACMM
  L5). They are excluded from the *queued* suffix on purpose: a queue nothing
  is draining is not work in flight, and reporting it as such is what hid
  [#8010](https://github.com/hivecommons/hive/issues/8010) behind a reassuring
  count,
- and a **⏸** marker plus a warning tooltip when queued work is blocked on a
  paused architect.

Whenever something is waiting on a *person* — a task list awaiting approval, or
a stuck epic — the tile turns amber and its tooltip **names** each one instead
of leaving a bare count to be decoded. The same list leads the **Plans** modal.

When there is no planning activity at all, the tile's tooltip nudges you toward
the feature: *click ⧉ Plan on any issue, or add the `plan` label on GitHub.*

## Plan state on the issue pill

Once an issue has an epic, the 📋 **Plan this issue** button on its
Repositories-card pill is replaced by a **state chip** for that epic — the
linkage and the plan's progress, without opening anything:

| Chip | Meaning |
|---|---|
| `⧗ planning` | An epic exists and is queued for the architect to decompose. |
| `● N tasks · review` | The plan is drafted; its N tasks are withheld from the fleet until you approve it. |
| `✓ done/total` | Approved and executing — how many of its tasks are finished. |
| `⚠ stuck` | Queued for the architect past the stuck threshold with nothing built. |

Clicking the chip opens that epic's plan-review view. The chip is drawn from
the plan listing that rides the `/api/status` payload (`planning.plans`, the
same set `GET /api/plans` returns), joined onto the pills by
`issueRepo#issueNumber` — so a card with forty issue pills still makes zero
extra requests.

## Configuration reference

```yaml
# Auto-plan issues carrying the `plan`/`epic` label. Omit to use the ACMM gate
# (on at L5+); set explicitly to force on/off.
planning:
  plan_from_label: true

# Tier-classification keywords (used to pick a model per issue) are config-driven
# and shown in the dashboard governor-config view. Empty/absent keeps the
# built-in defaults, so behavior is unchanged when unset.
classifier:
  simple_keywords: [typo, i18n, rename, const, label, badge, tooltip, placeholder, aria, "alt text"]
  complex_signals: ["race condition", deadlock, "memory leak", performance, "api change"]

# Stall-replan lane (Phase 3): re-kicks the architect on stalled plans.
governor:
  replan:
    enabled: true
    interval_s: 1800        # scan cadence (default 30m)
    stall_threshold_s: 21600 # no child progress for 6h → stalled
    max_replans: 5          # cap before escalating to a human
```

## Safety properties

- **Idempotent**: an issue maps to exactly one epic (keyed by a stable issue
  ref); the label trigger and repeated clicks never duplicate it.
- **Respects pause**: a paused architect queues plans, it is never force-resumed.
- **Gated**: draft-plan children are unclaimable until a human approves.
- **No launch-path mutex**: all architect interaction is via `SendKick`/`IsPaused`
  from the governor tick or an HTTP handler — never the agent-launch path.
