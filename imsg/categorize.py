"""Keyword-category review: find messages matching topic categories, group them
into conversations, score them, and write a Markdown report plus CSVs.

Categories, weights, and bonus combinations live in a JSON config (see
categories.example.json). Put your real one in categories.local.json, which is
git-ignored.

    python -m imsg.categorize --config categories.local.json --chat 5
    python -m imsg.categorize --config categories.local.json --min-score 20 --context 15
"""

from __future__ import annotations

import argparse
import csv
import json
import re
import sqlite3
from collections import Counter
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DEFAULT_INDEX = ROOT / "data" / "index.sqlite"
DEFAULT_OUT = ROOT / "output"


def load_config(path: Path) -> dict:
    cfg = json.loads(path.read_text())
    cfg["_compiled"] = {
        name: [re.compile(p, re.IGNORECASE) for p in spec["patterns"]]
        for name, spec in cfg["categories"].items()
    }
    return cfg


def score_categories(cats: set[str], cfg: dict) -> int:
    weights = {n: s.get("weight", 1) for n, s in cfg["categories"].items()}
    score = sum(weights.get(c, 1) for c in cats)
    for threshold, bonus in cfg.get("overlap_bonus", {"2": 3, "3": 5, "5": 8}).items():
        if len(cats) >= int(threshold):
            score += bonus
    for combo in cfg.get("combos", []):
        need_all = set(combo.get("all", []))
        need_any = set(combo.get("any", []))
        if need_all <= cats and (not need_any or cats & need_any):
            score += combo["bonus"]
    low = set(cfg.get("low_signal_only", []))
    if low and cats <= low:
        score -= cfg.get("low_signal_penalty", 8)
    return score


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--config", type=Path, default=ROOT / "categories.example.json")
    ap.add_argument("--index", type=Path, default=DEFAULT_INDEX)
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT)
    ap.add_argument("--chat", type=int, action="append", default=[], help="limit to chat_id (repeatable)")
    ap.add_argument("--context", type=int, default=15, help="messages of context before/after each hit")
    ap.add_argument("--min-score", type=int, default=0, help="only include conversations scoring at least this")
    ap.add_argument("--title", default="Message Review")
    args = ap.parse_args(argv)

    cfg = load_config(args.config)
    conn = sqlite3.connect(f"file:{args.index}?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row

    where, params = "WHERE is_reaction = 0", []
    if args.chat:
        where += f" AND chat_id IN ({','.join('?' * len(args.chat))})"
        params = args.chat
    all_rows = [dict(r) for r in conn.execute(
        f"SELECT m.*, c.label AS chat FROM messages m LEFT JOIN chats c USING(chat_id) {where} "
        "ORDER BY chat_id, ts, id", params)]

    # Group rows per chat so context windows never cross conversations.
    by_chat: dict[int, list[dict]] = {}
    for r in all_rows:
        by_chat.setdefault(r["chat_id"], []).append(r)

    hits, groups = [], []
    for chat_id, rows in by_chat.items():
        chat_hits = []
        for i, r in enumerate(rows):
            if not r["text"]:
                continue
            cats = [n for n, pats in cfg["_compiled"].items() if any(p.search(r["text"]) for p in pats)]
            if cats:
                h = {"i": i, "row": r, "categories": cats,
                     "start": max(0, i - args.context), "end": min(len(rows), i + args.context + 1)}
                chat_hits.append(h)
                hits.append(h)
        # Merge overlapping context windows into one conversation.
        for h in chat_hits:
            if groups and groups[-1]["chat_id"] == chat_id and h["start"] <= groups[-1]["end"]:
                groups[-1]["end"] = max(groups[-1]["end"], h["end"])
                groups[-1]["hits"].append(h)
            else:
                groups.append({"chat_id": chat_id, "rows": rows, "start": h["start"], "end": h["end"], "hits": [h]})

    for g in groups:
        g["categories"] = set().union(*(set(h["categories"]) for h in g["hits"]))
        g["score"] = score_categories(g["categories"], cfg)
        g["senders"] = Counter(h["row"]["sender"] for h in g["hits"])

    selected = sorted((g for g in groups if g["score"] >= args.min_score),
                      key=lambda g: (-g["score"], g["rows"][g["start"]]["ts"] or 0))

    args.out.mkdir(parents=True, exist_ok=True)

    with (args.out / "hits.csv").open("w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow(["message_id", "date", "chat", "sender", "categories", "text"])
        for h in hits:
            r = h["row"]
            w.writerow([r["id"], r["date"], r["chat"], r["sender"], " | ".join(h["categories"]), r["text"]])

    with (args.out / "conversations.csv").open("w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow(["rank", "score", "chat", "start_date", "end_date", "hit_count", "categories",
                    "senders", "first_message_id", "last_message_id"])
        for n, g in enumerate(selected, 1):
            first, last = g["rows"][g["start"]], g["rows"][g["end"] - 1]
            w.writerow([n, g["score"], first["chat"], first["date"], last["date"], len(g["hits"]),
                        " | ".join(sorted(g["categories"])),
                        " | ".join(f"{s}: {c}" for s, c in g["senders"].items()),
                        first["id"], last["id"]])

    report = args.out / "review.md"
    with report.open("w", encoding="utf-8") as f:
        f.write(f"# {args.title}\n\n")
        f.write("Conversations identified by keyword categories, ranked by score. "
                "This is a search index to guide reading, not a conclusion about what the messages mean.\n\n")
        for n, g in enumerate(selected, 1):
            rows = g["rows"]
            first, last = rows[g["start"]], rows[g["end"] - 1]
            hit_idx = {h["i"] for h in g["hits"]}
            f.write(f"## {n}. Score {g['score']} · {first['date'][:10]} · {first['chat']}\n\n")
            f.write(f"**Time range:** {first['date']} → {last['date']}  \n")
            f.write(f"**Categories:** {' | '.join(sorted(g['categories']))}  \n")
            f.write(f"**Keyword hits by sender:** {', '.join(f'{s} ({c})' for s, c in g['senders'].items())}  \n")
            f.write(f"**Message IDs:** {first['id']} → {last['id']}\n\n")
            f.write("### Matched messages\n\n```text\n")
            for h in g["hits"]:
                r = h["row"]
                f.write(f"[{r['date']}] {r['sender']} [{' | '.join(h['categories'])}] id={r['id']}\n{r['text']}\n\n")
            f.write("```\n\n### Full conversation\n\n```text\n")
            for j in range(g["start"], g["end"]):
                r = rows[j]
                mark = ">>> " if j in hit_idx else "    "
                f.write(f"{mark}[{r['date']}] {r['sender']}: {r['text']}\n")
            f.write("```\n\n---\n\n")

    counts = Counter(c for h in hits for c in h["categories"])
    print(f"Messages scanned: {len(all_rows)}")
    print(f"Keyword hits:     {len(hits)}")
    print(f"Conversations:    {len(groups)} ({len(selected)} with score >= {args.min_score})")
    print(f"Wrote: {report}, {args.out / 'hits.csv'}, {args.out / 'conversations.csv'}\n")
    for name in cfg["categories"]:
        print(f"  {name:36} {counts[name]}")
    print("\nTop conversations:")
    for g in selected[:15]:
        print(f"  score {g['score']:3} | {g['rows'][g['start']]['date'][:10]} | {len(g['hits']):3} hits | "
              f"{' | '.join(sorted(g['categories']))}")


if __name__ == "__main__":
    main()
