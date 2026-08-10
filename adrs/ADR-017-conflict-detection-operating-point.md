# ADR-017: Conflict detection is a screening problem, and its operating point must be chosen against a stated cost ratio

## Status

Accepted, 2026-08-10

## Context

Akashi's conflict detection pipeline (`internal/conflicts/`) takes pairs of decisions that are
semantically close and asks whether they contradict each other. Candidates come from Qdrant top-20
retrieval during the trace pipeline (`internal/service/decisions/service.go` calls `ScoreForDecision`);
`scorer.go:527` sends any pair with topic similarity ≥ 0.70 straight to the scorer, and an LLM validator
then classifies it.

We had no measurement of whether this worked. To get one, we blind-labelled the production corpus.

The corpus is 4,389 decisions (4,127 with a project set) across 44 projects and 70 agents, spanning
2026-02-14 to 2026-08-10. Of the 2,781 scored conflict pairs, 2,772 now carry a blind four-way gold label
in `conflict_gold_labels` (migration 107), produced by methods `blind_llm_stratified_v1` (212 pairs) and
`blind_llm_fullcorpus_v1` (2,560 pairs).

The label distribution is the central fact of this document:

| Gold label | Pairs | Share |
|---|---|---|
| related_not_contradicting | 2,017 | 72.8% |
| supersession | 627 | 22.6% |
| contradiction | 93 | 3.35% |
| unrelated | 35 | 1.3% |

Against that ground truth, the shipped detector's precision was **3.4%**. It emitted "contradiction" for
97.8% of pairs (2,711 of 2,772), and emitted "supersession" only 61 times against 627 real supersessions.
It was, in effect, a constant function.

Human adjudication was not better. Of 134 conflicts a human resolved by picking a winner, only 11.2% were
real contradictions — 53% were supersessions and 35% were merely related decisions. Of 2,296 conflicts
bulk-cleared as noise, 57 were real. Operators cannot correct a detector whose output is this noisy;
they inherit its errors.

Three structural facts follow from the 3.35% base rate, and they drive every decision below.

**First, this is screening, not classification.** With prevalence π = 3.35%, sensitivity s, and
false-positive rate f on the non-contradiction classes, precision is

    precision = π·s / (π·s + (1−π)·f)

which at π = 3.35% is a near-vertical function of f and nearly flat in s:

| Majority-class FPR *f* | Resulting precision |
|---|---|
| 5.7% | 23% |
| 2.0% | 46% |
| 1.0% | 63% |
| 0.5% | 78% |
| 0.1% | 95% |

Raising recall from 30% to 80% at f = 1% moves precision only from 51% to 74%. Halving f from 2% to 1%
moves it from 46% to 63%. Effort spent on recall is worth a fraction of effort spent on the false-positive
rate against `related_not_contradicting`.

**Second, class-averaged metrics actively mislead here.** gpt-5-mini had the best sample F1 of any judge
we measured (0.704) and the worst product outcome (17.3% corpus-projected precision), because F1 cannot
see a 4x difference in majority-class FPR when non-contradictions are 96.65% of the data. Mutual
information is worse still: it is invariant to label inversion, so a detector that outputs the exact
negation of the truth scores identically. Any metric that averages over classes, or that is symmetric
under inversion, will rank the wrong system first on this corpus.

**Third, the scorer's own features carry no label signal.** `significance` has AUC 0.500 against the gold
labels — pure noise. `topic_similarity` reaches 0.587. There is no threshold on these features that
separates contradictions from the rest, so the scorer cannot be the discriminating component no matter
how it is tuned.

It is worth being clear about why systems that appear to have solved conflict detection have not solved
ours. Kubernetes server-side apply can raise a conflict cheaply and exactly because identity is a JSON
field path — two writers touching the same path is a decidable syntactic fact
(<https://kubernetes.io/docs/reference/using-api/server-side-apply/>). Open Policy Agent's notion of
conflict is a runtime per-input error ("complete rules must not produce multiple outputs") and it performs
no cross-policy semantic conflict detection at all
(<https://www.openpolicyagent.org/docs/policy-language>). Both get their precision from structure that
exists before the check runs. Akashi's decisions are prose, and as the rejected alternatives below record,
we could not recover that structure after the fact.

## Decision

**1. Treat conflict detection explicitly as screening at a ~3% base rate.** The headline metrics are
corpus-projected precision, recall against gold contradictions, absolute review-queue size, and
false-positive rate on the majority class. F1, accuracy, and mutual information are not reported as
headline numbers for this component. Evaluation runs against the gold set via
`cmd/eval-conflicts --mode=gold`, which reads `conflict_gold_labels` (added in PR #740). Any change to the
detector must be evaluated there before it ships.

**2. The judge is the discriminating component; the scorer is a recall funnel.** Because scorer features
are label-noise, the scorer's job is candidate generation and cheap prefiltering, and all discrimination
happens in the LLM validator. What made the validator work was not a model swap but a prompt rewrite: an
ordered three-test procedure plus a parser-enforced contract requiring the judge to *name the disputed
question* before it may return "contradiction". A judge that cannot state what the two decisions disagree
about is not permitted to claim they disagree.

**3. Default operating point.** With the ordered-test prompt, corpus-projected precision by judge model:

| Judge | Precision | Recall | Review queue |
|---|---|---|---|
| gpt-4o-mini | 8.1% | 63% | 726 |
| gpt-4o | 26.9% | 33% | 115 |
| gpt-4.1 | 28.7% | 52% | 168 |
| gpt-5 | 41.5% | 50.5% | 113 |

gpt-5 at 41.5% precision / 50.5% recall is the default single-judge point. Its majority-class
false-positive rate, measured on a 300-pair sample, is 2.00% (6/300, 95% CI 0.74%–4.30%); weighted FPR
across all non-contradiction classes is 2.44%, specificity 99.07%.

Deployments that need higher precision run the two-stage cascade: gpt-5-mini screens, gpt-5 confirms.
Measured on identical pairs this yields a queue of 49 at 74.2% precision and 38.7% recall. The screen is
safe because retaining `contradiction | supersession | refinement` from the mini pass keeps 89.2% of gold
contradictions.

Operators configure the judge with `AKASHI_CONFLICT_OPENAI_MODEL` (default `gpt-4o-mini`). Reasoning
models require raising `AKASHI_CONFLICT_LLM_TIMEOUT` from its 15s default: a gpt-5 run at 15s failed 159
of 200 calls, and because validation timeout is fail-safe, that presents as a silent drop in detections
rather than an error. Operators changing the model must change the timeout in the same change.

**4. Supersession is a first-class outcome, not a conflict.** 22.6% of scored pairs are supersessions —
nearly seven times the contradiction rate. A supersession is a decision correctly replacing an earlier one over
time; treating it as a conflict manufactures work and trains operators to dismiss the queue. Supersessions
route to `supersedes_suggestions` (`scorer.go:896`, shipped in #729) and never enter the conflict queue.

**5. Prefilter on the temporal window, not on agent identity.** Real contradictions have a distinct
fingerprint: 97% are cross-agent, with a median gap of 6.2 hours versus 118 hours for non-contradictions.
But "cross-agent" alone is worth only 1.01x lift, because 96% of scored pairs are already cross-agent —
the time window does all the work.

| Prefilter | Base rate | Pairs kept |
|---|---|---|
| all scored pairs | 3.35% | 100% |
| cross-agent and gap < 72h | 6.38% | 74% |
| cross-agent and gap < 24h | 7.99% | 62% |

Nearly doubling the base rate while keeping 74% of pairs is the cheapest available precision gain, and it
composes with the judge rather than competing with it.

**6. Conflicts are advisory. They never block.** Nothing in Akashi refuses a write, fails a build, or
gates a merge because of a detected conflict. At 41.5% precision, a blocking gate would be wrong more
often than not, and a gate that is wrong most of the time is disabled within a week — taking the useful
signal with it. This follows the Wikidata property-constraint model, which states plainly that
"Constraints are hints, not firm restrictions, and are meant as a help or guidance to the editor"
(<https://www.wikidata.org/wiki/Help:Property_constraints_portal>).

**7. The operating point must be chosen against a stated miss-to-false-alarm cost ratio.** At the current
point, normalized expected cost is 1.198 at a 1:1 cost ratio — that is, *worse than never flagging
anything*. Break-even is 1.39:1. Decision-curve analysis agrees exactly: net benefit is positive if and
only if precision exceeds the threshold probability, and (1 − 0.415) / 0.415 = 1.41 (Vickers & Elkin,
*Medical Decision Making* 2006;26(6):565–574; net benefit = sensitivity·prevalence −
(1 − specificity)·(1 − prevalence)·w, where w = pt/(1 − pt)).

The practical rule for operators: if a missed contradiction does not cost you at least 1.4 times what a
false alarm costs, do not run the single-judge point. Run the cascade at 74.2% precision, or turn the
feature off. At the same operating point MCC is 0.439, lift over base rate is 12.5x, and LR+ is 20.7 —
the detector carries real information; the question is only whether your cost ratio makes acting on it
worthwhile.

## Measurements

Everything below was measured on the 2,772-pair labelled corpus unless stated otherwise.

**Shape of the 93 real contradictions** (classified with gpt-4.1):

| Kind | Share |
|---|---|
| mutually_exclusive_action | 34.4% |
| incompatible_factual_claim | 30.1% |
| parameter_binding (a named parameter bound to two incompatible values) | 27% |
| incompatible_direction | 8.6% |

Only `parameter_binding` is machine-checkable without natural-language understanding. 27% is therefore a
hard ceiling on any schema, SMT, or decision-diagram approach.

**Funnel.** Top-20 retrieval generates 87,570 candidate pairs from the corpus. 2,802 reached
`scored_conflicts`. 33,151 unscored candidates sit above the 0.70 similarity floor. Blind-labelling 200 of
those unscored candidates found a 1.0% contradiction rate (2/200), projecting roughly 332 contradictions
never surfaced against 93 found — true funnel recall of about 22% (95% CI 7%–70%).

**Label reliability.** An independent 200-pair re-rate gives Cohen's kappa 0.766 for
contradiction-versus-rest, 88.4% raw agreement, 81.7% recall of pass-1 contradictions, and a 5.7%
false-flag rate on non-contradictions.

**Volume.** The corpus contains 62 distinct disputes (connected components) over 25.3 weeks — 2.45
disputes per week. This is the scale the review queue must serve, and it is why absolute queue size (113,
or 49 under the cascade) matters more than any rate.

**External calibration.** Published contradiction detection at comparable base rates lands in the same
range, which is evidence that the ceiling is the task and not our implementation. DECODE (Nie et al., ACL
2021) reports precision 23.94 / recall 74.28 for the anchored model and 17.05 / 50.13 for the
unstructured model at a 4.27% natural base rate. de Marneffe, Rafferty & Manning (ACL 2008) report RTE3
test precision 22.95 / recall 19.44.

## Rejected alternatives

Each of these was implemented and measured on this corpus. Do not re-propose them without new evidence
that invalidates the measurement.

**Threshold tuning on scorer features.** `significance` AUC 0.500, `topic_similarity` AUC 0.587. There is
nothing to threshold.

**Deterministic gates.** Every gate we tried sits on the diagonal — it suppresses true positives at
roughly the rate it suppresses false positives, which is what "no signal" looks like. Entity-disjoint
suppresses 39.1% of false positives and loses 37.4% of true positives; missing-claim-pair 53.5% versus
54.8%; finding-to-fix 19.5% versus 19.1%. An `outcome_sim ≥ 0.85` agreement gate suppresses only 0.1% of
false positives.

**Fine-tuned cross-encoder.** A fine-tuned DeBERTa cross-encoder reached held-out AUC 0.494 — worse than
chance — trained on 67 positives. At 93 total contradictions in six months of production, supervised
fine-tuning is not reachable; the data does not exist and will not exist soon.

**Embedding-pair classifier.** A classifier over 1024-dimensional pair features (|a−b| and a·b, PCA-64)
reached AUC 0.611, *worse* than metadata alone at 0.730. Embedding geometry does not encode contradiction.

**Stock NLI.** `cross-encoder/nli-deberta-v3-base` over all 2,781 pairs gave corpus AUC 0.569. Sentence-level
NLI does not transfer to decision-record pairs.

**Two-model agreement ensembles.** Requiring two models to agree performed no better than the stronger
model alone.

**Retrofitting structured identity from prose.** Four independent attempts, all failures: typed-artifact
token join gave 1.30x lift at 41% recall; rejected-alternative token join 1.03–1.13x; rejected-alternative
semantic join via embeddings AUC 0.576, best 1.90x lift at 9.7% recall; a continuous "tension" logit from
token logprobs AUC 0.669 with maximum 9.6% corpus precision. Identity that was never captured at write
time cannot be reconstructed at read time.

**"A timeline is SUPERSESSION, never CONTRADICTION" as prompt step 2.** An intermediate prompt version
used this framing and inverted the failure mode: supersession became the default sink for 54–65% of every
class and contradiction recall collapsed to 1.1%. The ordered-test wording is what survived measurement;
this is a caution against editing the validator prompt on intuition rather than against the gold set.

**Schema, SMT, or decision-diagram checking as the primary mechanism.** Capped at 27% of real
contradictions by the `parameter_binding` share. Worth building as a precise complement, never as a
replacement.

**Mutual information as a headline metric.** Invariant to label inversion; a maximally wrong detector
scores identically to a correct one.

**Blocking gates.** See decision 6.

Two results are promising but not shipped, and are recorded here so they are not confused with the
rejected list. Stacking regression over multiple judge outputs reaches AUC 0.843 / AP 0.844 versus 0.743
for the best single judge, and structure-only features reproduce AUC 0.683–0.730 — but stacking requires
running several judges per pair, and we have not justified that cost.

## Consequences

The detector now depends on a hosted reasoning model for its only discriminating component. That is a
real cost and a real dependency, and it is the honest consequence of the scorer features carrying no
signal. Deployments that leave `AKASHI_CONFLICT_OPENAI_MODEL` at its `gpt-4o-mini` default should expect
8.1% precision and a 726-pair queue on a corpus this size, and should budget review capacity accordingly
or move to gpt-5 or the cascade.

Precision is bounded by the judge's false-positive rate on `related_not_contradicting`, so that is the
number to watch in production and the number any proposed change must improve. Recall improvements move
precision far less: raising recall from 30% to 80% at f = 1% buys 23 precision points, while halving f
from 2% to 1% buys 17.

The pairwise design has a limitation we cannot fix by tuning: it provably cannot see n-ary conflicts.
Cuppens, Garcia-Alfaro & Cuppens-Boulahia give a counterexample in which rules R1 and R2 are pairwise
consistent yet jointly shadow R3, which no pairwise analysis can detect
(<https://arxiv.org/abs/1912.07283>). He, Kirschbaum & Kasiviswanathan
("Foundations of Global Consistency Checking with Noisy LLM Oracles", arXiv:2601.13600, 20 Jan 2026,
<https://arxiv.org/abs/2601.13600>) state that "pairwise checks are insufficient to guarantee global
coherence" and that "verifying global consistency requires exponentially many oracle queries in the worst
case"; they propose "an adaptive divide-and-conquer algorithm that identifies minimal inconsistent subsets
(MUSes) of facts and optionally computes minimal repairs through hitting-sets" with "low-degree polynomial
query complexity". That is the shape of any future n-ary extension; it is not what we ship today.

Funnel recall is about 22% (95% CI 7%–70%). Most contradictions in a corpus of this size are never
surfaced as candidates at all, because retrieval stops at top-20. Precision improvements to the judge do
not touch this; only retrieval changes do.

The gold labels are themselves LLM-generated, with kappa 0.766 against an independent re-rate and a 5.7%
false-flag rate on non-contradictions. Every precision figure in this document is measured against a
noisy reference and should be read as an estimate with that uncertainty attached, not as an exact value.
Human labels would be better; 2,772 human labels were not affordable, and the human adjudication we do
have (11.2% precision on 134 resolved conflicts) is not obviously more reliable.

Finally, this ADR fixes a measurement discipline, not a model choice. Models will change. The
requirements that survive them are: evaluate on `conflict_gold_labels` via `cmd/eval-conflicts
--mode=gold`, report corpus-projected precision and majority-class FPR rather than F1, state the
miss-to-false-alarm cost ratio the operating point assumes, and keep the output advisory.

A note on sample sizes, because it cost us a wrong belief once: an early 47-pair sample suggested gpt-5
had a 0% false-positive rate and 65.2% precision. The 300-pair run corrected that to 2.00% FPR and 41.5%
precision. Quote 41.5%. Do not set an operating point from a sample too small to resolve a
false-positive rate that precision is this sensitive to.

## References

- `internal/conflicts/` — `scorer.go`, `validator.go`, `goldset.go`, `severity.go`, `autoresolve.go`
- `internal/service/decisions/service.go` — calls `ScoreForDecision`; `internal/search/qdrant.go` — top-20 candidate generation
- `scorer.go:527` — `directToScorer` for topic similarity ≥ 0.70; `scorer.go:896` — supersession routing (#729)
- `cmd/eval-conflicts --mode=gold` — gold-set evaluation (PR #740); migration 107 — `conflict_gold_labels`
- ADR-015 — separation of conflict severity from confidence scoring
- Nie et al., "I like fish, especially dolphins: Addressing Contradictions in Dialogue Modeling" (DECODE),
  ACL 2021. <https://aclanthology.org/2021.acl-long.134/>
- de Marneffe, Rafferty & Manning, "Finding Contradictions in Text", ACL 2008.
  <https://aclanthology.org/P08-1118/>
- He, Kirschbaum & Kasiviswanathan, "Foundations of Global Consistency Checking with Noisy LLM Oracles",
  arXiv:2601.13600, 20 Jan 2026. <https://arxiv.org/abs/2601.13600>
- Cuppens, Garcia-Alfaro & Cuppens-Boulahia. <https://arxiv.org/abs/1912.07283>
- Vickers & Elkin, "Decision curve analysis: a novel method for evaluating prediction models",
  *Medical Decision Making* 2006;26(6):565–574.
- Wikidata property constraints portal. <https://www.wikidata.org/wiki/Help:Property_constraints_portal>
- Open Policy Agent policy language — conflict is a runtime per-input error ("complete rules must not
  produce multiple outputs"); OPA performs no cross-policy semantic conflict detection.
  <https://www.openpolicyagent.org/docs/policy-language>
- Kubernetes server-side apply raises a conflict because identity is a JSON field path.
  <https://kubernetes.io/docs/reference/using-api/server-side-apply/>
