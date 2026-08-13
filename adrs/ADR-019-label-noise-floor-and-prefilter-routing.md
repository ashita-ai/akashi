# ADR-019: The label protocol cannot resolve a 3% base rate, so precision for this component is reported as an interval, and type-pair routing is the next candidate

## Status

Proposed, 2026-08-13

Extends ADR-017 (Accepted 2026-08-10, corrected 2026-08-12). Nothing in ADR-017 is retracted here.
Its numbers are re-scoped: they remain the best available estimates and they stop being quotable as
point values.

## Context

On 2026-08-12 the labelled corpus behind ADR-017 was published as `conflict-labels.csv` — 2,772 rows,
one per scored decision pair, every scorer feature and every gold label, no decision text — under
CC BY 4.0. That publication is what makes this ADR possible: the arithmetic in ADR-017 can now be
checked from outside the database, by anyone, against a frozen artifact. This is the first such check.

**Everything in ADR-017 reproduces.** Recomputed from the published CSV (snapshot 2026-08-12), rank AUC
with midrank ties, Hanley–McNeil intervals:

| Published claim | Recomputed | Match |
|---|---|---|
| base rate 3.35% | 93/2,772 = 3.355% (Wilson 95% CI 2.75%–4.09%) | yes |
| detector flagged 97.8% | 2,711/2,772 = 97.83% | yes |
| detector precision 3.4% | 92/2,711 = 3.394% | yes |
| detector recall 98.9% | 92/93 = 98.92% | yes |
| said "supersession" 61 times vs 627 real | 61 vs 627 | yes |
| `temporal_decay` AUC 0.728 | 0.728 (0.669–0.787) | yes |
| `topic_similarity` AUC 0.616 | 0.616 (0.554–0.678) | yes |
| `significance` AUC 0.601 | 0.601 (0.540–0.663) | yes |
| `confidence_weight` AUC 0.584 | 0.584 (0.522–0.646) | yes |
| `outcome_divergence` AUC 0.434 | 0.434 (0.378–0.491) | yes |

All five AUCs match to three decimals. The published data is honest and self-consistent, and the
2026-08-12 correction note in ADR-017 caught the one figure that was not. State this plainly: the
finding below is not that ADR-017 was careless with its arithmetic. It is that the arithmetic rests on
a base rate the labelling protocol is not strong enough to establish.

The reference for every number in ADR-017 is a single blind LLM rater. ADR-017 measured that rater
against an independent 200-pair re-rate and reported the result honestly in its Consequences section:
kappa 0.766, 88.4% raw agreement, 81.7% recall of pass-1 contradictions, 5.7% false-flag rate on
non-contradictions. What it did not do is push those constants back through the base rate they
qualify. Doing that is what this ADR records.

## Decision

**1. Precision figures for conflict detection are reported with a label-noise correction, and the
correction currently available is inadmissible.** Applying Rogan–Gladen misclassification correction —
the form Lee et al. (arXiv:2511.21140) use for LLM-as-a-judge evaluation — to the observed base rate:

    theta = (p + Sp - 1) / (Se + Sp - 1)

with p = 0.03355, Se = 0.817, Sp = 1 - 0.057 = 0.943:

    numerator   0.03355 + 0.943 - 1 = -0.0235
    denominator 0.817   + 0.943 - 1 =  0.7600
    theta = -3.09%

Negative. A prevalence cannot be negative, so the point estimate is inadmissible and no corrected
precision can be computed from it. The estimator is positive if and only if

    (1 - Sp) < p

that is, the labeller's false-flag rate on non-contradictions must be **below the base rate it is
measuring**, below 3.355%. The measured value is 5.7%. The observed 3.355% flag rate is lower than what
this labeller's false-flag rate alone would produce on a corpus containing no contradictions at all.

**2. State what that does and does not mean, every time it is quoted.** It does **not** mean there are
no contradictions in the corpus; 93 pairs were labelled as contradictions and the ones that have been
read are real. It means this labelling protocol cannot resolve a prevalence this low, so the base rate
every downstream number rests on is **not established**. The correct object is an interval over
plausible prevalence, not a point.

**3. The calibration constants are prose-derived, and the fix is human labels with the raw rows
retained.** Se = 0.817 and Sp = 0.943 come from ADR-017's Measurements section (lines 204–207). The
per-pair agreement rows behind them were never persisted — there is no table, no CSV, no artifact.
They are therefore **sensitivity-analysis inputs, not measurements**, and must be described that way
wherever they appear. Every use of them in this ADR carries that caveat.

The fix is a human-labelled calibration subset drawn from the same population as the corpus, with the
per-pair agreement rows stored in a table (a new migration), not summarised into a paragraph. Lee et
al. state that a calibration set should be allocated adaptively rather than uniformly — "it uses an
adaptive strategy to allocate calibration samples for tighter intervals". They support allocating
adaptively; they do not supply a formula for this corpus. The one we adopt is standard Neyman-style
stratified allocation, stated here on our own authority:

    m0 / m1  ≈  (1/p - 1) · sqrt(kappa)

where m1 is the positive-class (contradiction) stratum, m0 the negative-class stratum, and kappa the
relative per-label cost of the two strata. Both strata cost the same to read here, so kappa = 1 and
the ratio is 1/p - 1 = 28.8 — roughly **29 negative labels for every positive one**. That ratio sizes
the negative stratum. It does not license *sampling* the positive stratum, because there are only 93
positives in the whole corpus and the allocation is a guide to proportions, not a cap on a census.

**The subset is 480 labels: a 387-row sample of the gold negatives, plus a census of all 93 gold
contradictions.**

| Stratum | Labels | Drawn how |
|---|---|---|
| `related_not_contradicting` (plus the other non-contradiction classes) | 387 | random sample of 2,679 |
| `contradiction` | 93 | **census — every gold contradiction in the corpus** |

The specificity leg is what the budget is really buying. Precision at a 3% base rate is bounded by the
false-positive rate on a class that is 96.65% of the data, and it is the specificity leg that the
positivity condition `(1 - Sp) < p` turns on. 387 negatives settle that condition — the Wilson upper
bound on the observed false-flag rate clears 3.355% at up to 6 false flags in 387, i.e. whenever the
true rate is at or below about 1.5%.

The sensitivity leg does not get to be a sample. At the measured Se = 0.817, thirteen positives means
11 of 13 recalled, and the Wilson 95% interval on that is **57.8%–95.7%** — a 38-point band on a term
that multiplies straight through every corrected prevalence and every precision projection downstream.
That is not a measurement. Censusing all 93 gives 76 of 93 and a Wilson interval of **72.7%–88.3%**,
which is 15.6 points wide and is the narrowest this corpus can ever produce. There is no larger
positive stratum to buy; 93 is the ceiling, so take it.

If a hard 400-label cap is imposed, the census is not what gives way — the split degrades to 93 + 307,
and 307 negatives settle positivity only at up to 4 false flags, i.e. only if the true false-flag rate
is at or below about 1%. Take the 400 knowing that is the trade; do not spend it as 387/13.

**4. Until that calibration subset exists, report precision as a band over prevalence.** At the gpt-5
operating point ADR-017 fixes (s = 0.505, f = 0.0200), queue precision and the break-even
miss-to-false-alarm cost ratio move as follows:

| Assumed true prevalence pi | Queue precision | Break-even cost ratio |
|---|---|---|
| 3.35% (ADR-017's assumption) | 46.7% | 1.14 : 1 |
| 3.00% | 43.8% | 1.28 : 1 |
| 2.00% | 34.0% | 1.94 : 1 |
| 1.50% | 27.8% | 2.60 : 1 |
| 1.00% | 20.3% | 3.92 : 1 |
| 0.50% | 11.3% | 7.88 : 1 |

The 46.7% top row and ADR-017's published 41.5% are the same estimate under two false-positive rates:
this table uses the 300-pair majority-class f = 2.00%, ADR-017's corpus projection uses the
class-weighted f = 2.44% across all non-contradiction classes, which gives 41.8%. Both assume
pi = 3.35%. The published 41.5% sits inside this band; it is not pinned by it. An operator whose miss
cost is 2:1 is above break-even at pi = 3.35% and below it at pi = 1.5%, and nothing measured so far
distinguishes those two worlds.

**5. The scorer-feature line of work is closed.** Two results confirm ADR-017 decision 2 rather than
overturning it, and together they end the search for signal in the continuous features:

- `temporal_decay` is the clock. Spearman(temporal_decay, -days_apart) = +0.999999, and -days_apart
  has AUC 0.728 — identical to `temporal_decay`'s 0.728 to three decimals. The strongest feature in
  the scorer carries no information the raw timestamp difference does not already carry.
- There is no multivariate signal to recover. Pooled out-of-fold AUC from 5-fold cross-validated L2
  logistic regression: the 5 scorer features together reach 0.681, structure-only features (log gap,
  text lengths, `same_agent`/`same_project`/`same_type`) reach 0.726, everything together 0.731 —
  against 0.728 for the single best feature alone. Combining buys nothing over the clock.

Do not re-open feature engineering on the scorer without a new feature that is not a function of the
timestamp gap.

**6. Decision-type-pair routing is a provisional prioritisation candidate. It does not ship yet.**
Unordered decision-type pairs, 25 cells with >= 30 pairs tested:

`architecture` x `code_review` has a 15.5% contradiction rate (20/129, Wilson 95% CI 10.3%–22.7%),
lift 4.62x over the 3.355% corpus rate, capturing 21.5% of all contradictions in 4.7% of pairs. It
survives Bonferroni correction over the 25 tested cells and survives split-half replication (half A
10/75 = 13.3%, half B 10/54 = 18.5%). It composes with ADR-017 decision 5's temporal window:
`architecture` x `code_review` **and** cross-agent **and** gap < 72h gives 19/75 = 25.3%, lift 7.55x.

ADR-017 missed this because it tested single *continuous* features by AUC and tested deterministic
*suppression* gates. `decision_type` is categorical, was never tested as a pair interaction, and this
is a **prioritisation** signal — it routes and ranks, it does not suppress. Nothing is discarded by it.

Three caveats travel with it always, and all three are blocking conditions on shipping:

  i. **It was found by search over 25 cells on the same corpus that scores it.** Split-half is weak
     replication, not a held-out test. Pre-register the rule and measure it on pairs scored after
     2026-08-12 before it changes any queue.
  ii. **Its subgroup rates inherit the decision-1 label bias.** The effect could partly be a rater
      effect — the blind rater may call `architecture`-versus-`code_review` pairs contradictions more
      readily. That alternative explanation cannot be ruled out without the human labels in decision 3.
  iii. **`decision_type` is caller-supplied free text on the trace call.** It is a claim by the agent,
       not a verified property. Any rule keyed to it is gameable and will drift as agents change their
       type vocabulary.

Caveat (ii) means decision 3 is a prerequisite for decision 6, not a parallel track.

**7. ADR-017's headline numbers are re-scoped, not retracted.** 41.5% precision / 50.5% recall / 113-pair
queue remain the best available estimates and remain the default operating point. They are no longer
quotable without the prevalence band from decision 4 attached. A dated correction note pointing here has
been appended to ADR-017.

## Measurements

Everything below is computed from `conflict-labels.csv`, snapshot 2026-08-12, 2,772 rows, 93 positives.
Feature columns are mutable in the source system and 39 of these rows were rescored after the labelling
run, so these are properties of the snapshot, not of the pipeline in general.

**Sensitivity of the corrected base rate to the labeller's false-flag rate** (Se held at 0.817):

| Labeller false-flag rate | Sp | Corrected prevalence | 95% CI |
|---|---|---|---|
| 5.700% (measured) | 0.943 | **-3.086%** | -7.40% .. +1.23% |
| 5.000% | 0.950 | -2.145% | -6.18% .. +1.89% |
| 4.000% | 0.960 | -0.830% | -4.43% .. +2.77% |
| 3.350% (break-even) | 0.967 | 0.006% | -3.29% .. +3.30% |
| 3.000% | 0.970 | 0.451% | -2.66% .. +3.56% |
| 2.000% | 0.980 | 1.700% | -0.84% .. +4.24% |
| 1.000% | 0.990 | 2.918% | +1.05% .. +4.78% |
| 0.500% | 0.995 | 3.516% | +2.07% .. +4.96% |
| 0.000% | 1.000 | 4.106% | +3.24% .. +4.97% |

The interval covers zero until the false-flag rate is under about 1%. Variance amplification from the
correction is 1/(Se + Sp - 1)^2 = **1.73x**: correcting a weak reference labeller widens the interval
as well as moving the point. Chen et al. (arXiv:2601.05420) identify this as the reason to prefer
prediction-powered inference (Angelopoulos et al., arXiv:2301.09633) or efficient-influence-function
estimators over direct Rogan–Gladen correction — but those estimators consume a small trusted label
set alongside the cheap ones, which is exactly the artifact decision 3 buys and the current protocol
does not have.

**Multivariate feature search** (5-fold cross-validated L2 logistic regression, pooled out-of-fold AUC):

| Feature set | Pooled OOF AUC | 95% CI |
|---|---|---|
| 5 scorer features | 0.681 | 0.620–0.742 |
| structure only (log gap, lengths, `same_*`) | 0.726 | 0.667–0.786 |
| all of the above | 0.731 | 0.672–0.790 |
| *best single feature, for comparison* | *0.728* | *0.669–0.787* |

**Decision-type-pair base rates**, unordered pairs, cells with >= 30 pairs (25 cells tested,
Bonferroni z = 3.02):

| Type pair | Pairs | Contradictions | Rate | 95% CI | Lift | Survives Bonferroni |
|---|---|---|---|---|---|---|
| `architecture` x `code_review` | 129 | 20 | 15.5% | 10.3%–22.7% | 4.62x | yes |
| `assessment` x `code_review` | 46 | 4 | 8.7% | 3.4%–20.3% | 2.59x | no (weak) |
| `architecture` x `error_handling` | 33 | 2 | 6.1% | 1.7%–19.6% | 1.81x | no |
| `code_review` x `code_review` | 255 | 11 | 4.3% | 2.4%–7.6% | 1.29x | no |
| `architecture` x `investigation` | 71 | 3 | 4.2% | 1.4%–11.7% | 1.26x | no |

One cell survives. Twenty-four do not.

**Composed filters and their projection at the gpt-5 operating point** (s = 0.505, f = 0.0200). The
projections assume the subgroup label rate is the true prevalence, which is exactly the assumption
decision 1 says is unsupported — read them as relative, not absolute:

| Filter | Pairs kept | Base rate | Contradictions kept | Lift | Projected precision | Queue | Corpus recall |
|---|---|---|---|---|---|---|---|
| all scored pairs | 2,772 | 3.35% | 93 | 1.00x | 46.7% | 101 | 50.5% |
| cross-agent and gap < 24h | 726 | 7.99% | 58 | 2.38x | 68.7% | 43 | 31.5% |
| `architecture` x `code_review` | 129 | 15.50% | 20 | 4.62x | 82.2% | 12 | 10.9% |
| `architecture` x `code_review`, cross-agent | 124 | 16.13% | 20 | 4.81x | 82.9% | 12 | 10.9% |
| `architecture` x `code_review`, cross-agent, gap < 72h | 75 | 25.33% | 19 | 7.55x | 89.5% | 11 | 10.3% |
| (`architecture` x `code_review`) OR (cross-agent and gap < 12h) | 634 | 8.52% | 54 | 2.54x | 70.2% | 39 | 29.3% |

The trade is explicit: the tightest filter projects an 11-item queue at 10.3% corpus recall. That is a
routing decision — which pairs a reviewer sees first — not a suppression decision.

## Rejected alternatives

**Widening retrieval inside the high-yield window.** An earlier version of this analysis recommended
raising `AKASHI_CONFLICT_CANDIDATE_LIMIT` above 20 inside the cross-agent / gap < 72h window to attack
the ~22% funnel recall. That recommendation is withdrawn and must not be re-proposed on the current
evidence. ADR-017's own funnel measurement is the reason: blind-labelling 200 never-scored candidates
above the 0.70 similarity floor found a **1.0% contradiction rate (2/200)**, *below* the 3.355% rate of
the already-scored pool. Widening therefore draws from a lower-prevalence pool and dilutes queue
precision — it buys recall by spending the exact quantity ADR-017 establishes is scarcest. The right
response to low funnel recall is prioritised routing (decision 6) and better measurement (decision 3).
Issue #758 (a second blind-label batch of never-scored pairs) is what would change this; a wider
candidate limit is not. If funnel recall is attacked at all, the lever is a retrieval objective trained
for contradiction rather than for similarity — Xu et al.'s SparseCL (arXiv:2406.10746) is the published
form of that idea — not more results from the same similarity ranking. That is unmeasured here and is
not proposed by this ADR.

**Clamping the corrected prevalence to zero and carrying on.** A negative theta is the estimator
reporting that its inputs are outside its domain. Clamping to zero would turn a loud, informative
failure into a plausible-looking 0% base rate and an infinite break-even cost ratio, both of which are
wrong in a way nobody downstream could detect. The correction reports inadmissible and stops.

**Re-labelling more of the scored corpus with the same LLM protocol.** More labels from the same rater
shrink the sampling interval and leave the bias untouched. The binding constraint is (1 - Sp) < p, and
no volume of same-protocol labels moves Sp.

**Suppressing pairs outside the high-yield type cells.** Decision 6 is deliberately a routing rule.
Turning it into a suppressor discards 78.5% of known contradictions on a rule found by search over 25
cells on the corpus that scores it, keyed to a caller-supplied string. ADR-017's rejected-alternatives
list already records that every deterministic gate measured on this corpus sits on the diagonal.

**Judge ensembles as a route around label noise.** Kohli (arXiv:2605.29800) measures nine frontier
judges as carrying roughly two independent votes' worth of information because their errors are
correlated, which is consistent with ADR-017's own measured null result on two-model agreement
ensembles. Averaging correlated judges does not manufacture a reference standard.

## Consequences

Every precision figure this project has published for conflict detection — 3.4% for the shipped
detector, 8.1% / 26.9% / 28.7% / 41.5% by judge model, 74.2% for the cascade — is conditional on a
base rate that the labelling protocol cannot resolve. They are not wrong. They are estimates whose
interval was never computed, and the interval is wide: at the gpt-5 point, 20.3% to 46.7% over a
prevalence range this evidence cannot narrow. Anyone quoting them, including us, quotes the band.

This does not change the operating point. gpt-5 at the default remains the right choice on the
available evidence, ADR-017 decision 7's rule (do not run the single-judge point unless a miss costs
at least 1.4x a false alarm) still holds at its own stated prevalence, and conflicts remain advisory.
What changes is that the cost-ratio rule now has a prevalence argument, and operators near break-even
should treat the feature as unproven for their cost structure rather than proven.

The 480-label calibration subset in decision 3 is the unblocking work for everything else in this
document. It is the prerequisite for a defensible precision number, for distinguishing decision 6's
type-pair effect from a rater artifact, and for the efficient estimators Chen et al. describe. Scope it
as 387 sampled gold negatives plus a census of all 93 gold contradictions. Two parts of that will look
wrong to anyone who has not read decision 3 and need their justification attached wherever the work is
scoped: the negative stratum is sized 29:1 against the positive one, and the positive stratum is not
sampled at all. Sampling it is what a 400-label budget tempts you into, and 13 positives put a
57.8%–95.7% Wilson interval on the sensitivity term, which is no measurement. Under a hard 400 cap the
split is 93 + 307, and 307 negatives resolve the positivity condition only if the true false-flag rate
is at or below about 1%.

Two further consequences worth naming. First, the published CC BY 4.0 corpus is now load-bearing
infrastructure, not a marketing artifact: it is what made this check possible from outside the
database, and any future correction to it must be dated and announced the way the 2026-08-12 AUC
correction was. Second, the reproduction scripts behind this ADR read only that public CSV but are not
in this repository; they must be committed alongside the calibration work so a contributor can re-run
the check rather than trust it.

The conformal-abstention direction in issue #760 is where a tunable precision dial would come from —
SCOPE (Badshah, Emami & Sajjad, arXiv:2602.13110) calibrates an acceptance threshold so that queue
error is bounded, which is a better answer than picking a judge from a menu of four fixed operating
points. It shares decision 3's blocker: a conformal guarantee calibrated against a reference that
cannot resolve the prevalence inherits that reference's bias.

Decision 6's third caveat generalises past `decision_type`. Every structured field the detector can key
on today — `decision_type`, and the `bindings` that drive exact binding-collision detection in
`internal/conflicts/bindings.go` — is supplied by the calling agent and written verbatim; there is no
extraction from text anywhere in the pipeline. That is a deliberate design, and it means the precision
of any structural channel is the honesty of the caller. The escape is extraction at write time with an
explicit refusal path: Metropolitansky & Larson's Claimify (arXiv:2502.10855) is built around declining
to extract a claim under ambiguity rather than guessing, which is the property a second detection
channel would need to have an error profile independent of the LLM judge. Not scoped here; recorded so
the caveat is not read as being about one field.

## References

- ADR-017 — conflict detection operating point; the numbers this ADR re-scopes. See its 2026-08-13
  correction note.
- ADR-015 — separation of conflict severity from confidence scoring.
- `internal/conflicts/` — `scorer.go`, `validator.go`, `goldset.go`, `cost.go` (break-even and
  normalized expected cost arithmetic).
- `cmd/eval-conflicts --mode=gold` — gold-set evaluation; migration 107 — `conflict_gold_labels`.
- `docs/conflict-detection.md` §3.3 — operator reading of the label-noise correction flags.
- `conflict-labels.csv`, 2,772 rows, snapshot 2026-08-12, CC BY 4.0
  (<https://ashita.ai/assets/data/conflict-detection/>) — the artifact every reproduction above runs
  against. Companion to <https://ashita.ai/blog/the-detector-that-said-yes/>.
- `label_noise_analysis.py`, `followup.py` — the reproduction and follow-up scripts for this ADR. They
  read only the published CSV. **Not currently in this repository**; commit them with the decision 3
  calibration work.
- Issue #758 — second blind-label batch of never-scored high-similarity pairs.
- Issue #760 — two-stage detection: tension screen then classification; the conformal-abstention work
  extends it.
- Lee, Zeng, Jeong, Sohn & Lee, "How to Correctly Report LLM-as-a-Judge Evaluations", arXiv:2511.21140.
  <https://arxiv.org/abs/2511.21140>
- Chen, Lu, Li, Guo & Li, "Efficient Inference for Noisy LLM-as-a-Judge Evaluation", arXiv:2601.05420.
  <https://arxiv.org/abs/2601.05420>
- Angelopoulos, Bates, Fannjiang, Jordan & Zrnic, "Prediction-Powered Inference", arXiv:2301.09633.
  <https://arxiv.org/abs/2301.09633>
- Badshah, Emami & Sajjad, "SCOPE: Selective Conformal Optimized Pairwise LLM Judging", arXiv:2602.13110
  (ICML 2026). <https://arxiv.org/abs/2602.13110>
- Kohli, "Nine Judges, Two Effective Votes: Correlated Errors Undermine LLM Evaluation Panels",
  arXiv:2605.29800. <https://arxiv.org/abs/2605.29800>
- Xu, Lin, Sun, Chang & Indyk, "SparseCL: Sparse Contrastive Learning for Contradiction Retrieval",
  arXiv:2406.10746. <https://arxiv.org/abs/2406.10746>
- Metropolitansky & Larson, "Towards Effective Extraction and Evaluation of Factual Claims",
  arXiv:2502.10855. <https://arxiv.org/abs/2502.10855>
