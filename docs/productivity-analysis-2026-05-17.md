# Development Productivity Analysis: tclaude — Did the Agentic Features Move the Needle?

*Point-in-time analysis generated 2026-05-17 by a research sub-agent over the
repo's git + GitHub history (520 commits, 149 merged PRs, 2026-03-07 →
2026-05-17). It deliberately **tests** — rather than assumes — the hypothesis
that the agentic features raised productivity. The conclusion recommends a
follow-up re-run in 4–6 weeks; that later analysis would live alongside this
file as a dated sibling.*

---

## Verdict

**The hypothesis is directionally supported but the evidence is weak and heavily confounded.** Raw activity did rise sharply right around the agentic features landing — commits/day went up ~7x, PRs/day ~18x, lines/day ~19x in the post-inflection window. But almost the entire apparent jump is explained by **two confounders that have nothing to do with agentic features**:

1. **A single contributor (the maintainer) returning to near-full-time work after a 6-week near-absence.** The post-inflection window is essentially "Johan working intensely again," not "agents making everyone faster."
2. **A 9-day post-window vs a 63-day pre-window**, where the pre-window is dominated by idle gaps and one early dump-from-another-repo.

The post-inflection burst is real and is genuinely AI-assisted (91% of commits Claude co-authored vs 34% before). But the data **cannot distinguish** "agentic features made development faster" from "the maintainer started a focused sprint and used Claude Code heavily during it." It is a one-off 9-day burst, not a demonstrated sustained shift, and ~83% of post-inflection commits are the maintainer *building the agentic features themselves* — the metric is measuring the construction effort, not a productivity dividend from using them.

---

## 1. Timeline & Inflection Point

- **Project lifetime:** 2026-03-07 → 2026-05-17 (~10 weeks, 520 commits, 149 merged PRs, 90 `docs/plans/DONE/` feature docs).
- **The agentic inflection is sharp and unambiguous: PR #47, merged 2026-05-09** — "tclaude agent + agentd: cross-session coordination" (+4720 / −20, 31 files). Before that date, the strings `agentd`, `tclaude agent`, agent-coordination, and the agent dashboard **do not appear anywhere** in commit history.
- After #47, the agentic surface expanded explosively: ~70 of the 90 `DONE/` feature docs are dated 2026-05-09 onward (agent CLI, dashboard, groups, cron, permissions, spawn/clone/reincarnate, system tray).
- **The web UI is not part of the inflection** — `tclaude web` shipped 2026-04-04 and was *marked deprecated the same day*. It is a dead end, not an agentic enabler. ("web dashboard" is still a TODO.)

So the inflection date is **2026-05-09**. Everything before is "pre", everything from that day on is "post".

## 2. Activity Over Time

### Weekly breakdown

| Week | Starts | Commits | PRs | +lines | −lines | files | authors | Claude % |
|---|---|--:|--:|--:|--:|--:|--:|--:|
| W10 | 03-02 | 40 | 2 | 28,003 | 1,255 | 244 | 2 | 85% |
| W11 | 03-09 | 67 | 7 | 10,257 | 3,104 | 202 | 2 | 40% |
| W12 | 03-16 | 14 | 2 | 673 | 356 | 32 | 1 | 93% |
| W13 | 03-23 | 3 | 0 | 305 | 17 | 11 | 1 | 100% |
| W14 | 03-30 | 29 | 16 | 710 | 158 | 61 | 2 | 28% |
| W15 | 04-06 | 18 | 3 | 544 | 226 | 33 | 2 | 0% |
| W16 | 04-13 | 2 | 1 | 52 | 23 | 2 | 1 | 0% |
| W17 | 04-20 | 48 | 3 | 2,329 | 639 | 126 | 2 | 6% |
| W18 | 04-27 | 33 | 6 | 689 | 196 | 45 | 2 | 0% |
| **W19** | **05-04** | **150** | **5** | **49,000** | **5,545** | **788** | **2** | **83%** |
| **W20** | **05-11** | **116** | **104** | **67,899** | **28,586** | **956** | **2** | **97%** |

### Monthly breakdown

| Month | Commits | PRs merged | Claude co-author % |
|---|--:|--:|--:|
| 2026-03 | 138 | 24 | 57% |
| 2026-04 | 89 | 11 | 10% |
| 2026-05 | 293 | 114 | 81% |

The last two weeks (W19–W20) contain **51% of all commits and ~70% of all PRs** of the entire project. W19 itself straddles the inflection: 2 commits on 05-04, 4 on 05-07, then **49 on 05-09 and 95 on 05-10** — the burst begins *on the inflection date itself*.

## 3. Before / After Comparison

Inflection = 2026-05-09. Pre-window = 63 days; post-window = 9 days.

| Metric | PRE total | POST total | PRE/day | POST/day | Rate ratio |
|---|--:|--:|--:|--:|--:|
| Commits | 260 | 260 | 4.1 | 28.9 | **7.0x** |
| PRs merged | 41 | 108 | 0.7 | 12.0 | **18.4x** |
| Lines added | 43,678 | 116,783 | 693 | 12,976 | **18.7x** |
| Lines removed | 6,007 | 34,098 | 95 | 3,789 | **39.7x** |
| File-changes | 762 | 1,738 | 12.1 | 193 | 16.0x |
| Claude co-authored | 34% | 91% | — | — | — |

**Commit cadence:** median inter-commit gap fell from **17 min (pre)** to **8 min (post)**; mean gap from 5.7 h to 0.75 h; max gap from 142 h (a 6-day silence) to 54 h. The post window is a continuous high-tempo sprint with no idle stretches.

On every raw proxy, post-inflection activity is an order of magnitude higher per day. **If you stop here, the hypothesis looks strongly confirmed.** You should not stop here.

## 4. Confounders & Caveats (this is where the hypothesis weakens)

**A. Commits/PRs are proxies, not productivity.** They count *output volume*, not *value delivered*. Three of the biggest post-inflection PRs are pure churn or rework: #152 (+9,420/−9,302, a CSS/JS file-split refactor) and #53 (+2,386/−5,478, a test-library swap) are net-near-zero-value mechanical changes that inflate every line-count metric. The 40x "lines removed" ratio is largely refactor churn, not 40x more productivity.

**B. Contributor pattern flipped — and the two contributors never overlapped.** This is the single biggest confounder:

| | Mikael Ståldal | Johan Kjölhede |
|---|---|---|
| Pre-inflection commits | 156 | 88 |
| Post-inflection commits | 7 | 253 |
| Active weeks | W10–11, W15–18 | W10–14, then **W19–20** |

The two contributors worked in **alternating, non-overlapping bursts** — there is essentially no week where both were productive. The "pre" period is mostly *Mikael's* work plus a quiet stretch where Johan was nearly absent (W15–W18: Johan made 3 commits in 4 weeks). The "post" period is **Johan returning at full intensity** (139 commits W19, 114 W20) while Mikael drops out. The post-inflection surge is not "the team got faster" — it is "the most active contributor came back and started a sprint." A single person resuming focused work fully explains a 7x commit-rate jump without any tooling change.

**C. Window-length asymmetry is severe.** 63 days vs 9 days. The pre-window's per-day rate is dragged down by long idle gaps (a 6-day silence, multiple weeks of single-digit activity). Comparing a contributor's *peak sprint* against a *project average that includes their vacation* manufactures a large ratio almost automatically. A fairer comparison — Johan's W10–W11 burst (pre) vs his W19–W20 burst (post) — narrows the gap considerably: he did 36+18 commits in his first two active weeks and there were already 85%-Claude weeks (W10, W12, W13) **two months before any agentic feature existed**.

**D. The burst IS the agentic feature construction — circular measurement.** 215 of 258 post-inflection commits (83%) touch `agentd/`, `pkg/claude/agent/`, or `dashboard`. The post-window measures the *effort of building* the orchestration system, not a *productivity dividend from using* it. You cannot yet have been made faster by tools you are still in the middle of writing. Post-inflection PR median size is only 288 additions — lots of small commits on one feature area, a style that mechanically inflates commit counts.

**E. One-off burst, not a demonstrated sustained shift.** "Post-inflection" is **9 days**. Two weeks of intense activity from a maintainer who has previously shown two-week bursts (W10–W11) and then gone quiet is not evidence of a permanent regime change. There is no post-burst data to show the new tempo holds once the agentic features stop being the thing being built.

**F. Process/tooling changes unrelated to agentics.** The flow-test harness (PR #49, "testharness v2") and the testify migration (#53) landed in the same window and are independent productivity factors. The Claude co-authorship jump (34%→91%) shows a real change in *how* code was written — but that is "the maintainer leaned hard on Claude Code," which is a workflow choice that does **not require agentd/dashboard/agent-coordination to exist**. Note Claude co-authorship was *already 85–100%* in W10/W12/W13, long before any agentic feature.

## 5. Concluding Assessment

**Strength of evidence for "agentic features sharply raised productivity": weak.**

What the data genuinely shows:
- A real, sharp activity spike beginning exactly on the inflection date (2026-05-09).
- A real shift to heavily AI-assisted commits in that window (91% Claude co-authored).
- A real, sustained-within-the-window high tempo (8-minute median commit gap, no idle stretches).

What the data **cannot** show, and what undermines the hypothesis:
- The spike is fully explained by an ordinary confounder — **the project's most active contributor returning from a near-absence and starting a focused sprint**. Contributor-mix change alone accounts for the headline ratios.
- The post-window is **9 days** and is **83% the maintainer building the agentic features themselves** — the metrics measure construction cost, not a usage dividend. The mechanism in the hypothesis (multiple coordinated agents making development faster) cannot have produced a jump that *consists of writing that very mechanism*.
- The 18–40x line ratios are partly refactor churn (#152, #53), not delivered value.
- There is **no post-burst observation period** to confirm any lasting change.

An honest reading: the agentic features and the activity spike are **coincident in time but not shown to be causally linked**. The spike is equally consistent with "Johan came back and did a two-week Claude-assisted sprint, and what he chose to build happened to be the agentic features." To actually test the hypothesis you would need data this repo does not yet contain — sustained activity in the *weeks after* the agentic features stabilized, ideally on *non-agentic* work, and ideally with the multi-agent orchestration genuinely in use. As of 2026-05-17 that evidence does not exist.

**Recommendation:** treat the current numbers as "I had a very productive two weeks while building the orchestration system," not as "the orchestration system made me productive." Re-run this analysis in 4–6 weeks against post-W20 work to see whether the elevated tempo holds once the agentic features are *used* rather than *built* — that later window is the only one that can actually confirm or kill the hypothesis.

---

*Data sources: `git log` (520 commits, 2026-03-07→05-17), `gh pr list` (149 merged PRs), `git log --numstat`, `docs/plans/DONE/` (90 feature docs). Inflection pinned to PR #47, merge commit `05c0723`, 2026-05-09.*
