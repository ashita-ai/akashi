# Conflict Detection: Operator and Contributor Reference

> `conflict_gold_labels` (migration 107), `--mode=gold`, `AKASHI_CONFLICT_OPENAI_MODEL` and
> `AKASHI_CONFLICT_LLM_TIMEOUT` arrived in PR #740 and are on `main`. On an older checkout,
> none of this page's evaluation instructions apply.

This document is about **tuning and evaluating** conflict detection quality. For the
mechanism reference — significance formula, claim extraction, metrics, admin endpoints —
see [conflicts.md](conflicts.md).

Everything numeric here was measured on the akashi production corpus: 4,389 decisions
(4,127 with a project set), 44 projects, 70 agents, 2026-02-14 to 2026-08-10. Where a
number is a projection rather than a direct count, it says so.

**The one-paragraph summary.** Contradictions are rare — 3.35% of scored pairs. At that
base rate, precision is governed almost entirely by the judge's false-positive rate on the
majority class, not by its recall and not by any threshold you can tune. The shipped
default (`gpt-4o-mini`) runs at roughly 8% queue precision. A stronger judge gets you to
~41%. Nothing else measured on this corpus moved the number.

---

## 1. What the pipeline does, end to end

### 1.1 Trigger and candidate generation

`POST /v1/trace` persists the decision, then fires conflict scoring asynchronously —
`internal/service/decisions/service.go` calls `ScoreForDecision`, implemented in
`internal/conflicts/scorer.go` (`ScoreForDecision` → `scoreForDecision`). Candidates come from a Qdrant vector
search over decision embeddings (`internal/search/qdrant.go`), capped at the top 20 nearest
neighbours (`AKASHI_CONFLICT_CANDIDATE_LIMIT`, default `20`).

**This cap is the single largest recall limit in the system.** Top-20 retrieval generates
87,570 candidate pairs across the corpus. Only 2,802 ever reached `scored_conflicts`;
33,151 unscored candidates sit above the 0.70 similarity floor and were never examined.
Blind-labelling 200 of those unscored candidates found a 1.0% contradiction rate (2/200),
which projects to roughly 332 contradictions never surfaced against 93 found — true funnel
recall of about 22% (95% CI 7%–70%, wide because the sample found only two positives).

### 1.2 Significance and the 0.70 bypass

Each candidate gets a significance score (topic similarity × outcome divergence ×
confidence weight × temporal decay). Candidates below `AKASHI_CONFLICT_EARLY_EXIT_FLOOR`
(default `0.25`) are pruned — *unless* they qualify for the direct-to-scorer bypass:

```go
// internal/conflicts/scorer.go — grep for directToScorer
directToScorer := hasScorer && sc.topicSim >= s.decisionTopicSimFloor
```

`decisionTopicSimFloor` is `AKASHI_CONFLICT_DECISION_TOPIC_SIM_FLOOR`, default `0.70`. The
bypass exists because bi-encoders cannot represent stance opposition: "X is correct" and
"X is wrong" embed close together, so cosine divergence is near zero for exactly the pairs
that matter most. Any pair above the floor goes to the judge regardless of significance.

Do not try to recover precision by raising the significance threshold. Against the gold
labels (2,772 pairs, snapshot 2026-08-12), significance has AUC **0.601** (95% CI
0.54–0.66) and topic similarity **0.616** (0.55–0.68). Their intervals overlap, so neither
is measurably better than the other, and neither has a threshold that separates
contradictions from non-contradictions. `outcome_divergence` is inverted at 0.434
(0.38–0.49); the strongest feature in the scorer is `temporal_decay` at 0.728 (0.67–0.79),
which was never intended as a signal. See ADR-017 §2 for the full table and the correction
note — figures of 0.500/0.587 published before 2026-08-12 do not reproduce.

### 1.3 Structural suppressors (pre-LLM)

Before any LLM call, `scoreForDecision` applies a family of deterministic filters, each
incrementing its own counter so you can see which one is doing the work. The counters are Go
field names on `s.metrics`, all declared in `internal/conflicts/metrics.go`; the predicates
themselves live in the per-filter files alongside `scorer.go` (`branch_filters.go`,
`cross_agent_precedent.go`, `disjoint_resource.go`, `disjoint_work_item.go`,
`operational_progression.go`, `temporal_filter.go`, `workflow_filters.go`):

| Filter | Counter | What it kills |
|---|---|---|
| FP-pattern suppression | `fpPatternFiltered` | Pair shapes with a known historical FP rate |
| Complementary workflow | `workflowFiltered` | Directional workflow pairs (finding → fix) |
| Coordinated change | `coordinatedFiltered` | Decisions sharing a coordinated change unit |
| Cross-branch mechanical | `crossBranchFiltered` | Same mechanical operation on two branches |
| Same-branch self-correction | `selfCorrectionFiltered` | One agent revising itself on one branch |
| Same-agent same-ticket refinement | `supersedesCandidateFiltered` | Iterative refinement within one ticket |
| Cross-agent precedent refinement | `crossAgentPrecedentFiltered` | Refinement of an explicitly cited precedent |
| Temporal re-assessment | `temporalReassessmentFiltered` | Two review decisions ≥7 days apart |
| Operational state progression | `operationalProgressionFiltered` | Operational state that legitimately mutates |
| Disjoint work item | `disjointWorkItemFiltered` | Different PR/ticket references |
| Disjoint resource | `disjointResourceFiltered` | Different `connector_`/`org_` resources |
| Outcome-similarity agreement | `outcomeSimFiltered` | `AKASHI_CONFLICT_OUTCOME_SIM_FLOOR` (default `0.85`) |

Measured on the live candidate stream (2026-08-11), these rules together suppress **~56%**
of candidate pairs before any judge sees them — 990 of 1,779 in the trailing window. Their
false-negative rate is what migration 111's `suppressed_pair_samples` table exists to
measure, via the deterministic sample enabled by `AKASHI_CONFLICT_SUPPRESSION_SAMPLE_RATE`
(default `0` — off; `0.05` is the intended dogfood setting). Until that sample accumulates
labels, the 56% is a throughput fact, not a safety one.

These are cheap and they cut LLM cost, but **do not expect them to buy precision.** Every
deterministic gate measured against the gold labels sits on the diagonal — it removes
false positives and true positives in the same proportion:

| Gate | FPs suppressed | TPs lost |
|---|---|---|
| Entity-disjoint | 39.1% | 37.4% |
| Missing claim pair | 53.5% | 54.8% |
| Finding-to-fix | 19.5% | 19.1% |
| `outcome_sim >= 0.85` agreement | 0.1% | — |

A gate that is on the diagonal changes throughput and cost, never the precision of what
survives it.

### 1.4 The LLM judge and its taxonomy

`internal/conflicts/validator.go` builds the prompt and parses the response. The judge
emits one of five relationship tokens — `contradiction`, `supersession`, `complementary`,
`refinement`, `unrelated` — which collapse to the four-way gold taxonomy
(`complementary` and `refinement` both map to `related_not_contradicting`; the split is
not reliably separable by human or model raters and no routing decision depends on it).

Two properties of the current prompt are load-bearing and were the only intervention that
moved measured precision:

1. **An ordered three-test procedure.** (1) Name the single specific question both
   decisions answer; different work items, subsystems, incidents, tickets or scopes mean
   no shared question. (2) Timeline test. (3) Are both positions still live and
   incompatible?
2. **A parser-enforced disputed-question contract.** A `contradiction` verdict that names
   no question is downgraded to `complementary` in `ParseValidatorResponse`
   (`internal/conflicts/validator.go`). The verdict is not trusted unless the model can state
   what is in dispute.

The wording is fragile in a specific, measured way. An intermediate version framed step 2
as "a timeline is SUPERSESSION, never CONTRADICTION" and **inverted the failure**:
supersession became the default sink for 54–65% of every class and contradiction recall
collapsed to 1.1%. If you edit this prompt, run the gold eval (§3). Do not eyeball it.

`IsConflict()` returns true for `contradiction` and `supersession` only.

### 1.5 Severity, grouping, supersession routing

- **Severity** (`internal/conflicts/severity.go`) is computed from metadata that is
  independent of the significance score — decision-type impact tiers 1–4, promoted or
  demoted by other signals. `ComputeSeverity` never returns `critical`; that level is
  reserved for precedent escalation, when a conflict reopens the winning side of a
  previously resolved conflict (`FindReopenedResolution`, similarity threshold 0.80).
- **Grouping** assigns each conflict to a topic group via `FindOrCreateTopicGroup`, then
  applies transitive dedup: if both decisions already participate in conflicts inside the
  same group, the pair is dropped (`transitiveGroupFiltered`). This is what prevents the
  O(n²) explosion from supersession chains.
- **Supersession routing.** A `supersession` verdict is recorded as a row in
  `supersedes_suggestions` instead of opening a conflict — the scorer intercepts the verdict
  before `IsConflict()` and hands it to `internal/conflicts/supersedes_suggestions.go`
  (shipped in #729; direction taken from the judge rather than the clock since #749). The
  validator still *returns* `supersession`, so precision/recall evals are unaffected — only
  the scorer's handling changes.

Auto-resolution policy lives in `internal/conflicts/autoresolve.go` (`ClassifyTier`,
`ShouldAutoResolve`, `DetermineWinner`).

---

## 2. Choosing a judge model

**Judge capability dominates precision.** This is the finding, and it is not close.
Measured on the blind gold set and projected onto the full corpus:

| Judge | Corpus-projected precision | Recall | Projected queue size |
|---|---|---|---|
| `gpt-4o-mini` (shipped default) | 8.1% | 63% | 726 pairs |
| `gpt-4o` | 26.9% | 33% | 115 pairs |
| `gpt-4.1` | 28.7% | 52% | 168 pairs |
| `gpt-5` | **41.5%** | 50.5% | 113 pairs |
| `gpt-5-mini` screen → `gpt-5` confirm | **74.2%** | 38.7% | 49 pairs |

The two-stage cascade was measured on identical pairs. A `gpt-5-mini` screen that retains
`contradiction | supersession | refinement` keeps 89.2% of gold contradictions, so the
screen is cheap insurance rather than a recall cliff.

`gpt-5`'s majority-class false-positive rate, measured on 300 pairs, is **2.00%** (6/300,
95% CI 0.74%–4.30%). Weighted FPR across all non-contradiction classes is 2.44%;
specificity 97.56%. An earlier 47-pair sample suggested 0% FPR and 65.2% precision — that
was small-sample noise and was corrected by the 300-pair run. **Quote 41.5%.**

### Do not rank judges by F1

`gpt-5-mini` had the *best* sample F1 (0.704) and the *worst* product outcome (17.3%
corpus precision). F1 on a stratified sample cannot see a 4× difference in majority-class
false-positive rate, and at a 3.35% base rate that difference is the entire story. Rank
judges by corpus-projected precision and queue size. §4 explains why.

### Configuration

```bash
AKASHI_CONFLICT_OPENAI_MODEL=gpt-5     # default: gpt-4o-mini
AKASHI_CONFLICT_LLM_TIMEOUT=120s       # default: 15s  <-- RAISE THIS FOR REASONING MODELS
```

**Timeout warning — read this before switching to a reasoning model.** The default
`AKASHI_CONFLICT_LLM_TIMEOUT` is 15s. A `gpt-5` run at 15s failed **159 of 200 calls**. A
validation error is fail-safe: the candidate is skipped, not flagged. So a timeout that is
too low does not error, does not alert, and does not appear in the conflict queue — it
presents as a **silent drop in detections**, which looks exactly like "the new model is
more precise". If you change the judge model and detections fall, check
`akashi.conflicts.llm_calls{result="timeout"}` before you conclude anything about quality.

Related knobs: `AKASHI_CONFLICT_LLM_MODEL` (Ollama text model), `AKASHI_CONFLICT_LLM_THREADS`
(default `floor(NumCPU/3)`), `AKASHI_CONFLICT_PROFILE` (`balanced` | `high_precision` |
`high_recall`; individual env vars override the profile).

---

## 3. How to evaluate a change

> **Read this before you start: the gold corpus is not distributed, and you cannot run
> `--mode=gold` without building your own.**
>
> `conflict_gold_labels` (migration 107) has exactly one write path in this repository, and it
> is inside a test (`internal/conflicts/goldset_protection_test.go`). There is no endpoint, no
> CLI subcommand, no seed script, and no fixture that populates it. The 2,772 labelled pairs
> behind every number in this document are real decision texts from the maintainer's own
> deployment — they carry ticket references, service names, and customer-adjacent identifiers,
> and they are not published.
>
> So on a fresh clone, `--mode=gold` connects, finds an empty table, and reports nothing. It
> does this *after* you have provisioned a Postgres, set `AKASHI_DB_DSN`, and supplied an
> `OPENAI_API_KEY`. That is a bad way to find out, which is why it is written here first.
>
> **What an outside contributor can offer instead**, in descending order of strength:
>
> 1. **Your own labelled corpus.** Run the detector against your own decision trail, label the
>    scored pairs blind against the four-way taxonomy in §3.2, and report precision the way §3.3
>    describes — re-weighted onto true class sizes, with the sample size and interval attached.
>    This is the strongest evidence available and it does not require our data.
> 2. **A counter-example with a mechanism.** A concrete pair the detector gets wrong, plus which
>    stage produced the error (structural suppressor, significance threshold, judge verdict, or
>    parser contract) and the reasoning for the fix. Structural suppressors are individually
>    unit-testable with no database at all — see the per-filter tests in `internal/conflicts/`
>    (`branch_filters_test.go`, `disjoint_resource_test.go`, `disjoint_work_item_test.go`,
>    `operational_progression_test.go`, `temporal_filter_test.go`, `cross_agent_precedent_test.go`).
> 3. **A pure-code change with tests.** Parser contracts, prompt structure, and the suppressors
>    are all testable offline. `go test ./internal/conflicts/...` needs no Docker.
>
> What is *not* useful, and has been tried and measured: threshold tuning, new regex
> suppressors, and hand-written eval sets. §3.2 explains why a hand-written set cannot falsify
> the prompt it was written alongside, and §5 records the alternatives already rejected with
> their numbers. Read both before proposing one.

### 3.1 Run the gold eval

```bash
AKASHI_DB_DSN='postgres://...' \
OPENAI_API_KEY=sk-... \
go run ./cmd/eval-conflicts --mode=gold \
  --gold-model=gpt-5 \
  --gold-timeout=120s \
  --gold-limit=0 \
  --gold-conc=8 \
  --save
```

| Flag | Default | Meaning |
|---|---|---|
| `--mode` | `validator` | `validator`, `scorer`, `benchmark`, `gold` |
| `--gold-limit` | `200` | Max labeled pairs; `0` = all |
| `--gold-conc` | `8` | Concurrent validator calls |
| `--gold-model` | `gpt-4o-mini` | OpenAI model under test |
| `--gold-timeout` | `15s` | Per-call deadline — raise for reasoning models |
| `--gold-classes` | *(all)* | Comma-separated gold classes to evaluate |
| `--save` | off | Write JSON to `./eval-results/` |

Gold mode talks to Postgres directly (`AKASHI_DB_DSN`, `AKASHI_ORG_ID`, `OPENAI_API_KEY`),
not to a running server.

### 3.2 What `conflict_gold_labels` is, and why the hand-written eval set was useless

The table (migration `107_conflict_gold_labels.sql`) holds **blind** four-way labels on
2,772 of the 2,781 scored pairs — method `blind_llm_stratified_v1` (212) and
`blind_llm_fullcorpus_v1` (2,560). Blind means the rater saw only the two decision texts
and their structural metadata, never the pipeline's verdict.

Gold distribution:

| Label | Count | Share |
|---|---|---|
| `related_not_contradicting` | 2,017 | 72.8% |
| `supersession` | 627 | 22.6% |
| `contradiction` | 93 | **3.35%** |
| `unrelated` | 35 | 1.3% |

This exists because the hand-written `DefaultEvalDataset` scored **1.000 precision / 1.000
recall** while the shipped detector was running at **3.4% precision** in production. An
eval set written from the same intuitions as the prompt cannot falsify that prompt: if you
add examples to a fixture file and the fixture passes, you have measured nothing.

The scale of what the gold set exposed: the shipped detector emitted `contradiction` for
**97.8% of pairs** (2,711 of 2,772) and `supersession` only 61 times against 627 real
supersessions. Human adjudication was no safety net either — of 134 conflicts a human
resolved with a winner, only **11.2%** were real contradictions (53% supersessions, 35%
merely related); of 2,296 bulk-cleared, 57 were real.

**Label ceiling.** An independent 200-pair re-rate gives Cohen's kappa **0.766** for
contradiction-vs-rest, 88.4% raw agreement, 81.7% recall of pass-1 contradictions, 5.7%
false-flag rate on non-contradictions. No judge can be meaningfully "better" than the label
noise, so treat differences inside that band as unresolved.

### 3.3 Reading the output — and why the sample number flatters everything

`reportGold` prints two precision figures. They are not interchangeable.

```
relationship accuracy  : NN.N%
contradiction precision: NN.N%  recall: NN.N%  F1: N.NNN  (stratified sample)
corpus-projected queue : NNN pairs, precision NN.N% (base rate 3.4%, lift NN.Nx)
```

The **stratified sample** number is computed over a sample that deliberately oversamples
contradictions so there are enough positives to measure recall. That inflates the apparent
base rate, and therefore inflates precision, for *every* judge. It is useful only for
comparing per-class behaviour.

The **corpus-projected** number re-weights each class's flag rate by its true corpus size
(`goldCorpusSizes` in `cmd/eval-conflicts/main.go:558`: contradiction 93, supersession 627,
complementary 2,017, unrelated 35), then recomputes precision on the reweighted totals.
That is the queue an operator would actually see. **Report this one** — for `gpt-5` it
reads 113 pairs at 41.5% precision, lift 12.4×.

To measure the number that actually bounds precision — the false-positive rate on the
majority class — run the judge against non-contradictions only:

```bash
go run ./cmd/eval-conflicts --mode=gold \
  --gold-classes=related_not_contradicting \
  --gold-limit=300 --gold-model=gpt-5 --gold-timeout=120s
```

300 pairs is the sample size behind the 2.00% figure in §2; smaller samples produce
confidence intervals too wide to act on (the 47-pair run read 0%).

Also available: `POST /v1/admin/conflicts/validate-pair` for a single ad-hoc pair, and
`--mode=benchmark` for local scorer changes with no server or API key.

#### Correcting for label noise: `--label-sensitivity` and `--label-specificity`

Added with ADR-019. If `--help` on your build does not list them, you are on an older build and every
precision figure it prints is uncorrected.

The gold labels are LLM-generated, so the 3.35% base rate that every projection above divides by is
itself an estimate from a noisy rater. These two flags supply that rater's reliability and apply the
Rogan–Gladen correction to the base rate before precision is projected:

```bash
go run ./cmd/eval-conflicts --mode=gold --gold-model=gpt-5 --gold-timeout=120s \
  --label-sensitivity=0.817 \
  --label-specificity=0.943
```

`--label-sensitivity` is the rater's recall of true contradictions; `--label-specificity` is
`1 - (the rater's false-flag rate on non-contradictions)`. Both flags are required together and both
default to unset, in which case no correction is applied and the printed base rate is the raw 3.35%.

The 0.817 / 0.943 above come from the 200-pair re-rate reported in ADR-017 §Measurements. **The
per-pair agreement rows behind them were never persisted**, so treat them as sensitivity-analysis
inputs and sweep them rather than trusting either digit.

**Reading an inadmissible result.** The correction is `theta = (p + Sp - 1) / (Se + Sp - 1)`, and it
returns a positive prevalence only when `(1 - Sp) < p` — the rater's false-flag rate must be below the
base rate it is measuring. At the constants above it is not (5.7% against a 3.35% base rate), so
`theta` comes out at **-3.09%**. A negative prevalence is not a number the run can use, and it is not
clamped to zero: the correction reports the failure, names the positivity condition it violated, and
the run exits non-zero.

Three things this does **not** mean:

- It does not mean the corpus has no contradictions. It means this labelling protocol cannot resolve a
  prevalence this low, so the base rate is not established.
- It is not a bug in the run, and it is not fixed by supplying different constants. Only a better
  reference labeller moves `Sp`.
- It does not invalidate the operating point. gpt-5 at 41.5% is still the default.

**What to do with it.** Report precision as a band over prevalence rather than a point. At the gpt-5
operating point (s = 0.505, f = 2.00%):

| Assumed prevalence | Queue precision | Break-even miss:false-alarm ratio |
|---|---|---|
| 3.35% | 46.7% | 1.14 : 1 |
| 2.00% | 34.0% | 1.94 : 1 |
| 1.00% | 20.3% | 3.92 : 1 |

The 46.7% top row and the 41.5% default above are the same operating point at the same 3.35%
prevalence, differing only in the false-positive rate: this table uses the majority-class f = 2.00%
measured on 300 pairs, while 41.5% comes from ADR-017's class-weighted f = 2.44% across all
non-contradiction classes.

If your cost ratio sits inside that band, the feature is unproven for your cost structure — treat §4's
break-even arithmetic as unresolved rather than passed. ADR-019 has the full sweep, the positivity
condition, and the calibration work that would close it.

---

## 4. Choosing an operating point from a cost ratio

At base rate π and judge sensitivity s with majority-class false-positive rate f:

```
precision = π·s / (π·s + (1-π)·f)
```

With π = 3.35%, precision is a near-vertical function of `f`:

| `f` | Resulting precision |
|---|---|
| 5.7% | 23% |
| 2.0% | 46% |
| 1.0% | 63% |
| 0.5% | 78% |
| 0.1% | 95% |

Recall barely participates. At f = 1%, raising recall from 30% to 80% moves precision only
51% → 74%. **Buy false-positive rate, not recall.**

### The break-even arithmetic

Decision-curve analysis (Vickers & Elkin 2006) defines net benefit as

```
NB = sensitivity·prevalence − (1 − specificity)·(1 − prevalence)·w,   w = pt/(1 − pt)
```

where `pt` is the threshold probability — the precision at which you are indifferent
between investigating a flag and ignoring it. Net benefit is positive exactly when
precision > `pt`. At the `gpt-5` operating point (precision 41.5%):

```
(1 − 0.415) / 0.415 = 1.41
```

**The queue pays for itself only if one missed contradiction costs at least ~1.4× one
false alarm.** Independently computed: normalized expected cost is **1.198** at a 1:1
miss:false-alarm ratio — *worse than never flagging at all* — with break-even at
**1.39:1**. The two methods agree.

The decision procedure:

1. Estimate your own miss:false-alarm cost ratio.
2. If it is below ~1.4:1, do not run an open queue at 41.5% precision. Use the
   `gpt-5-mini` → `gpt-5` cascade — 74.2% precision, so break-even is
   `(1 − 0.742)/0.742 ≈ 0.35:1` — and accept 38.7% recall.
3. If it is well above 1.4:1, the single-judge configuration is defensible and you keep
   the higher recall.

Supporting figures at the 41.5% point: MCC 0.439, lift 12.4×, LR+ 20.7.

**Metrics to distrust.** Mutual information is unsuitable as a headline number: it is
invariant to label inversion, so a detector that outputs the exact negation of the truth
scores identically. Class-averaged F1 is unsuitable for the reason given in §2.

### Cheap prefilters that raise the base rate

The contradiction fingerprint is 97% cross-agent with a median gap of 6.2h, versus 118h
for non-contradictions.

| Prefilter | Base rate | Pairs kept |
|---|---|---|
| All scored pairs | 3.35% | 100% |
| Cross-agent + gap < 72h | 6.38% | 74% |
| Cross-agent + gap < 24h | 7.99% | 62% |

Note that "cross-agent" *alone* is worth only 1.01× lift — 96% of scored pairs are already
cross-agent. The time window does all the work.

### Volume

62 distinct disputes (connected components) over 25.3 weeks = **2.45 disputes/week**. Size
your triage rota against that, not against the raw conflict count.

---

## 5. Known limitations, and how to measure them yourself

**Funnel recall is ~22%.** Top-20 retrieval never shows the judge most of the corpus.
To re-measure: sample unscored candidate pairs above the 0.70 similarity floor, label them
blind, and compare the projected contradiction count against the 93 found. Raising
`AKASHI_CONFLICT_CANDIDATE_LIMIT` increases LLM cost linearly and has not been evaluated
for precision impact.

**Pairwise checking cannot certify global consistency.** Akashi is pairwise-only. He,
Kirschbaum and Kasiviswanathan ([arXiv:2601.13600](https://arxiv.org/abs/2601.13600),
20 Jan 2026) state that "pairwise checks are insufficient to guarantee global coherence"
and that verifying global consistency "requires exponentially many oracle queries in the
worst case"; they propose an adaptive divide-and-conquer algorithm identifying minimal
inconsistent subsets (MUSes) with optional minimal repairs via hitting-sets, at low-degree
polynomial query complexity. Cuppens et al.
([arXiv:1912.07283](https://arxiv.org/abs/1912.07283)) give the concrete counterexample:
rules R1 and R2 can be pairwise-consistent yet jointly shadow R3.

**At most 27% of real contradictions are machine-checkable.** Classifying the 93 real
contradictions (with `gpt-4.1`) by shape:

| Shape | Share |
|---|---|
| `mutually_exclusive_action` | 34.4% |
| `incompatible_factual_claim` | 30.1% |
| `parameter_binding` (a named parameter bound to two incompatible values) | 27% |
| `incompatible_direction` | 8.6% |

Only `parameter_binding` is decidable by a schema, SMT solver or decision diagram. **27% is
the hard ceiling on any such approach** — which is roughly where declarative systems land
in practice: Kubernetes server-side apply raises conflicts because identity is a
[JSON field path](https://kubernetes.io/docs/reference/using-api/server-side-apply/); Open
Policy Agent's conflict is a runtime per-input error ("complete rules must not produce
multiple outputs") with
[no cross-policy semantic detection](https://www.openpolicyagent.org/docs/policy-language);
and Wikidata property constraints are explicitly advisory — "Constraints are hints, not
firm restrictions, and are meant as a help or guidance to the editor"
([portal](https://www.wikidata.org/wiki/Help:Property_constraints_portal)).

**Do not re-propose these — all measured on this corpus and all failed:**

| Approach | Result |
|---|---|
| Threshold tuning on scorer features | no separating threshold; significance AUC 0.601 (0.54–0.66), topic similarity 0.616 (0.55–0.68) — see §1.2 |
| Deterministic gates | all on the diagonal (§1.3) |
| Fine-tuned DeBERTa cross-encoder | held-out AUC 0.494 at 67 training positives |
| Embedding-pair classifier (1024-d, \|a−b\| and a·b, PCA-64) | AUC 0.611 — worse than metadata alone (0.730) |
| Stock NLI (`cross-encoder/nli-deberta-v3-base`, 2,781 pairs) | corpus AUC 0.569 |
| Two-model ensemble requiring agreement | no better than the stronger model alone |
| Retrofitting identity from prose | 4 independent failures: typed-artifact token join 1.30× lift at 41% recall; rejected-alternative token join 1.03–1.13×; rejected-alternative semantic join AUC 0.576 (best 1.90× at 9.7% recall); continuous "tension" logit AUC 0.669, max 9.6% corpus precision |

**What did move the number:** the prompt rewrite (§1.4), a stronger judge (§2), the
two-stage cascade (§2), and stacking a regression over multiple judges' outputs — AUC
0.843 / AP 0.844 versus 0.743 for the best single judge, with structure-only features
reproducing AUC 0.683–0.730. Stacking is not shipped.

**Calibrate your expectations against the literature.** Contradiction detection is hard
everywhere, not just here. DECODE (Nie et al., ACL 2021,
[paper](https://aclanthology.org/2021.acl-long.134/)) reports precision 23.94 / recall
74.28 for its anchored model at a 4.27% natural base rate, and 17.05 / 50.13 unstructured.
de Marneffe, Rafferty and Manning (ACL 2008,
[paper](https://aclanthology.org/P08-1118/)) report RTE3 test precision 22.95 / recall
19.44. A 41.5%-precision queue at a 3.35% base rate is above that bar; a 3.4% queue was far
below it.

### Re-measuring after any change

1. Full gold run for headline numbers:
   `go run ./cmd/eval-conflicts --mode=gold --gold-limit=0 --gold-model=<m> --gold-timeout=120s --save`
2. Majority-class FPR on ≥300 pairs: add `--gold-classes=related_not_contradicting`.
3. Read the **corpus-projected** line, never the stratified-sample line.
4. Recompute break-even as `(1 − precision) / precision` and compare against your cost ratio.
5. Confirm the run was not silently truncated by timeouts: the `errors=` field in the eval
   header and `akashi.conflicts.llm_calls{result="timeout"}` in production must both be near zero.
