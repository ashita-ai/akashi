#!/usr/bin/env python3
"""Why conflict detection is a screening problem, not a classification problem.

At a low base rate, precision is governed almost entirely by the false-positive
rate on the majority class — not by recall. This is the single most useful thing
to understand before tuning any rare-event detector, and it is why F1 pointed us
at the wrong judge for months.

No server, no dependencies, no network. Runs anywhere Python does.

Run:
    python examples/python/base_rate.py
"""

PREVALENCE = 0.0335  # 93 real contradictions in 2,772 blind-labelled pairs


def precision(prevalence: float, sensitivity: float, fpr: float) -> float:
    """P(real | flagged). fpr is the false-positive rate on the majority class."""
    tp = prevalence * sensitivity
    fp = (1 - prevalence) * fpr
    return tp / (tp + fp) if tp + fp else 0.0


def main() -> None:
    print(f"Base rate: {PREVALENCE:.2%} of candidate pairs are real contradictions\n")

    print("Hold recall at 50%. Move only the false-positive rate:")
    print(f"  {'majority-class FPR':>20}  {'precision':>10}")
    for fpr in (0.057, 0.020, 0.010, 0.005, 0.001):
        print(f"  {fpr:>19.1%}  {precision(PREVALENCE, 0.50, fpr):>10.1%}")

    print("\nNow hold FPR at 1%. Move only recall:")
    print(f"  {'recall':>20}  {'precision':>10}")
    for s in (0.30, 0.50, 0.80, 0.95):
        print(f"  {s:>19.0%}  {precision(PREVALENCE, s, 0.01):>10.1%}")

    lo = precision(PREVALENCE, 0.30, 0.01)
    hi = precision(PREVALENCE, 0.80, 0.01)
    tight = precision(PREVALENCE, 0.50, 0.01)
    loose = precision(PREVALENCE, 0.50, 0.02)

    print(
        f"\nRecall 30% -> 80% buys {(hi - lo) * 100:.0f} precision points."
        f"\nHalving FPR 2% -> 1% buys {(tight - loose) * 100:.0f}, for far less work."
        "\n\nSpend your effort on the false-positive rate against the boring majority."
    )


if __name__ == "__main__":
    main()
