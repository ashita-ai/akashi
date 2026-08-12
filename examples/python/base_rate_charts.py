#!/usr/bin/env python3
"""Generate the operating-point figures as inline SVG.

Every chart in the "detector that said yes to everything" writeup comes out of
this script, from the blind gold-label counts. Nothing is hand-drawn, so you can
change the counts to your own corpus and see whether the argument still holds.

Emits SVG rather than raster images because the figures are embedded inline in
the post. No dependencies: standard library only, same as base_rate.py.

Run:
    python examples/python/base_rate_charts.py            # writes ./charts/*.svg
    python examples/python/base_rate_charts.py --out _posts/img
"""

from __future__ import annotations

import argparse
import os

# Blind four-way labels over 2,772 scored pairs (migration 107).
LABELS = [
    ("related, not contradicting", 2017),
    ("supersession", 627),
    ("contradiction", 93),
    ("unrelated", 35),
]
TOTAL = sum(n for _, n in LABELS)
PREVALENCE = 93 / TOTAL  # 0.0335

# Corpus-projected precision by judge, ordered-test prompt (ADR-017).
JUDGES = [("gpt-4o-mini", 8.1), ("gpt-4o", 26.9), ("gpt-4.1", 28.7), ("gpt-5", 41.5)]

# House chart style, taken from the existing figures on the site.
SERIES, INK, GRID = "#2c4a6e", "#1a1a1a", "#ddd"
AXIS_LABEL, DATA_LABEL = "#666", "#444"
FONT = "font-family: Inter, sans-serif;"


def precision(prev: float, sens: float, fpr: float) -> float:
    tp, fp = prev * sens, (1 - prev) * fpr
    return tp / (tp + fp) if tp + fp else 0.0


def svg(body: str, alt: str, w: int = 660, h: int = 340) -> str:
    """A standalone SVG document that is also safe to paste inline.

    The xmlns makes the file valid on its own, so it renders in a browser or a
    previewer and can be checked before it goes anywhere. Inline in HTML5 the
    attribute is simply ignored. The <figure> wrapper belongs to the page, not
    to the figure, so it lives in the post rather than here.
    """
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" '
        f'width="100%" role="img" aria-label="{alt}" style="{FONT}">\n{body}</svg>'
    )


def composition() -> str:
    """Horizontal bars. The point is that one bar is a sliver."""
    x0, top, row, bar_h, width = 210, 60, 62, 30, 380
    out = [
        f'  <text x="20" y="26" font-size="15" font-weight="600" fill="{INK}">'
        f"93 real contradictions in 2,772 pairs</text>\n"
    ]
    # Vertical gridlines run ACROSS the bars, never through them.
    for pct in (0, 25, 50, 75, 100):
        x = x0 + width * pct / 100
        out.append(
            f'  <line x1="{x:.0f}" y1="{top - 12}" x2="{x:.0f}" y2="{top + row * 4 - 26}" '
            f'stroke="{GRID}" stroke-width="1"/>\n'
        )
        out.append(
            f'  <text x="{x:.0f}" y="{top + row * 4 - 10}" font-size="11" '
            f'fill="{AXIS_LABEL}" text-anchor="middle">{pct}%</text>\n'
        )
    for i, (name, count) in enumerate(LABELS):
        share = count / TOTAL * 100
        y = top + row * i
        hero = name == "contradiction"
        w = max(width * share / 100, 2)
        out.append(
            f'  <text x="{x0 - 14}" y="{y + 20}" font-size="12.5" '
            f'fill="{INK if hero else DATA_LABEL}" text-anchor="end">{name}</text>\n'
        )
        out.append(
            f'  <rect x="{x0}" y="{y}" width="{w:.1f}" height="{bar_h}" fill="{SERIES}"'
            + ("" if hero else ' opacity="0.35"')
            + "/>\n"
        )
        # The hero bar gets two decimals: 3.4% would collide with the shipped
        # detector's 3.4% precision, which is a different number.
        share_label = f"{share:.2f}" if hero else f"{share:.1f}"
        out.append(
            f'  <text x="{x0 + w + 10:.0f}" y="{y + 20}" font-size="12" '
            f'fill="{SERIES if hero else DATA_LABEL}"'
            + (' font-weight="600"' if hero else "")
            + f">{share_label}%  ({count:,})</text>\n"
        )
    return svg(
        "".join(out),
        "Horizontal bar chart of blind labels over 2,772 candidate pairs: "
        "72.8 percent related but not contradicting, 22.6 percent supersession, "
        "3.35 percent contradiction, 1.3 percent unrelated",
        h=top + row * 4 + 4,
    )


def operating_point() -> str:
    """Precision against false-positive rate, one line per recall. One x-axis."""
    x0, y0, w, h = 70, 40, 520, 250
    fprs = [i / 1000 for i in range(1, 61)]  # 0.1% .. 6.0%
    out = [
        f'  <text x="20" y="24" font-size="15" font-weight="600" fill="{INK}">'
        f"Precision is a story about false positives</text>\n"
    ]
    for pct in (0, 25, 50, 75, 100):
        y = y0 + h - h * pct / 100
        out.append(
            f'  <line x1="{x0}" y1="{y:.0f}" x2="{x0 + w}" y2="{y:.0f}" '
            f'stroke="{GRID if pct else INK}" stroke-width="1"/>\n'
        )
        out.append(
            f'  <text x="{x0 - 10}" y="{y + 4:.0f}" font-size="11" '
            f'fill="{AXIS_LABEL}" text-anchor="end">{pct}%</text>\n'
        )
    for pct in (0, 2, 4, 6):
        x = x0 + w * pct / 6
        out.append(
            f'  <text x="{x:.0f}" y="{y0 + h + 20}" font-size="11" '
            f'fill="{AXIS_LABEL}" text-anchor="middle">{pct}%</text>\n'
        )
    # Ordered lines: heavier and darker as recall rises.
    for sens, opacity in ((0.30, 0.35), (0.50, 0.6), (0.80, 1.0)):
        pts = " ".join(
            f"{x0 + w * f / 0.06:.1f},{y0 + h - h * precision(PREVALENCE, sens, f):.1f}"
            for f in fprs
        )
        out.append(
            f'  <polyline points="{pts}" fill="none" stroke="{SERIES}" '
            f'stroke-width="2" opacity="{opacity}"/>\n'
        )
        end_y = y0 + h - h * precision(PREVALENCE, sens, fprs[-1])
        out.append(
            f'  <text x="{x0 + w + 8}" y="{end_y + 4:.0f}" font-size="11.5" '
            f'fill="{SERIES}" opacity="{opacity}">recall {sens:.0%}</text>\n'
        )
    out.append(
        f'  <text x="{x0 + w / 2:.0f}" y="{y0 + h + 44}" font-size="11.5" '
        f'fill="{AXIS_LABEL}" text-anchor="middle">'
        f"false-positive rate on the majority class</text>\n"
    )
    return svg(
        "".join(out),
        "Line chart: precision against false-positive rate at a 3.35 percent base "
        "rate, three lines for 30, 50 and 80 percent recall, all falling steeply "
        "as the false-positive rate rises",
        h=y0 + h + 58,
    )


def judges() -> str:
    """What actually moved the number."""
    x0, y0, h, bw, gap = 70, 46, 210, 78, 52
    out = [
        f'  <text x="20" y="26" font-size="15" font-weight="600" fill="{INK}">'
        f"Same prompt, same corpus. Only the judge changed.</text>\n"
    ]
    for pct in (0, 20, 40):
        y = y0 + h - h * pct / 50
        out.append(
            f'  <line x1="{x0}" y1="{y:.0f}" x2="{x0 + 4 * (bw + gap)}" y2="{y:.0f}" '
            f'stroke="{GRID if pct else INK}" stroke-width="1"/>\n'
        )
        out.append(
            f'  <text x="{x0 - 10}" y="{y + 4:.0f}" font-size="11" '
            f'fill="{AXIS_LABEL}" text-anchor="end">{pct}%</text>\n'
        )
    for i, (name, val) in enumerate(JUDGES):
        x = x0 + 20 + i * (bw + gap)
        bh = h * val / 50
        hero = name == "gpt-5"
        out.append(
            f'  <rect x="{x}" y="{y0 + h - bh:.1f}" width="{bw}" height="{bh:.1f}" '
            f'fill="{SERIES}"' + ("" if hero else ' opacity="0.35"') + "/>\n"
        )
        out.append(
            f'  <text x="{x + bw / 2:.0f}" y="{y0 + h - bh - 8:.0f}" font-size="12.5" '
            f'fill="{SERIES}"'
            + (' font-weight="600"' if hero else "")
            + f' text-anchor="middle">{val}%</text>\n'
        )
        out.append(
            f'  <text x="{x + bw / 2:.0f}" y="{y0 + h + 20}" font-size="12" '
            f'fill="{DATA_LABEL}" text-anchor="middle">{name}</text>\n'
        )
    out.append(
        f'  <text x="{x0 - 10}" y="{y0 + h + 44}" font-size="11.5" '
        f'fill="{AXIS_LABEL}">corpus-projected precision</text>\n'
    )
    return svg(
        "".join(out),
        "Bar chart of corpus-projected precision by judge model: gpt-4o-mini 8.1 "
        "percent, gpt-4o 26.9 percent, gpt-4.1 28.7 percent, gpt-5 41.5 percent",
        h=y0 + h + 58,
    )


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("--out", default="charts")
    args = p.parse_args()
    os.makedirs(args.out, exist_ok=True)

    for name, fn in (
        ("corpus-composition", composition),
        ("operating-point", operating_point),
        ("judges", judges),
    ):
        path = os.path.join(args.out, f"{name}.svg")
        with open(path, "w") as fh:
            fh.write(fn() + "\n")
        print(f"  {path}")

    print(f"\nbase rate: {PREVALENCE:.2%} (93/{TOTAL:,})")


if __name__ == "__main__":
    main()
