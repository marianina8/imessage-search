"""Build a printable report (HTML → Save as PDF) and a CSV of selected messages.

The report is laid out for someone reviewing the messages as a record, such as
a lawyer: cover page, source and method, an optional AI summary that cites
numbered messages (M-0001…), an index of matching messages, and the full
transcript with surrounding context.
"""

from __future__ import annotations

import csv
import html
import io
import re
import sqlite3
from collections import Counter
from datetime import datetime

MAX_ROWS = 5000
MAX_SUMMARY_CHARS = 120_000

REPORT_SYSTEM_PROMPT = """You are helping someone prepare their text messages for review by their lawyer.
Write a neutral, precise summary of the messages provided.

Rules:
- Every factual statement must cite the message reference(s) that support it, in square brackets, e.g. [M-0012] or [M-0012, M-0015].
- Quote short phrases exactly (in quotation marks) when the precise wording matters.
- Keep what someone said separate from what actually happened, e.g. "Jordan wrote that she paid the invoice [M-0040]", not "Jordan paid the invoice".
- Do not speculate, characterize motives or emotions, or draw legal conclusions.
- Point out ambiguities, contradictions, and gaps (for example, references to calls or conversations that aren't in the messages).

Use exactly these Markdown sections:
## Overview
(2–4 sentences)
## Key points
(bullets, most important first)
## Timeline
(bullets in date order: "**Mon DD, YYYY** — event [refs]")
## Statements by participant
(a ### heading per person, with bullets)
## Ambiguities and gaps
(bullets)"""


# --------------------------------------------------------------------------- #
# Collecting the rows
# --------------------------------------------------------------------------- #
def collect(conn: sqlite3.Connection, ids: list[int], context_n: int) -> list[dict]:
    """Matched messages plus `context_n` messages either side, grouped into excerpts.

    Returns rows in report order, each with ref ("M-0001"), matched flag, and excerpt number.
    """
    cols = {r[1] for r in conn.execute("PRAGMA table_info(messages)")}
    att = "m.attachments" if "attachments" in cols else "'' AS attachments"
    ids = list(dict.fromkeys(int(i) for i in ids))
    matched = set(ids)

    # Locate each match's position inside its conversation.
    windows: dict[int, list[tuple[float, float]]] = {}
    for i in ids:
        row = conn.execute("SELECT chat_id, ts FROM messages WHERE id=?", (i,)).fetchone()
        if row:
            windows.setdefault(row[0], []).append(row[1])

    picked: dict[int, dict] = {}
    for chat_id, stamps in windows.items():
        for ts in stamps:
            q = (f"SELECT m.id, m.guid, m.chat_id, c.label AS chat, m.ts, m.date, m.sender, m.is_from_me, "
                 f"m.text, m.has_attachment, {att} FROM messages m LEFT JOIN chats c USING(chat_id) "
                 "WHERE m.chat_id=? AND m.is_reaction=0 AND m.ts {op} ? ORDER BY m.ts {order}, m.id {order} LIMIT ?")
            before = conn.execute(q.format(op="<", order="DESC"), (chat_id, ts, context_n)).fetchall()
            after = conn.execute(q.format(op=">=", order="ASC"), (chat_id, ts, context_n + 1)).fetchall()
            for r in list(before) + list(after):
                picked.setdefault(r["id"], dict(r))
            if len(picked) > MAX_ROWS:
                break

    rows = sorted(picked.values(), key=lambda r: (r["chat_id"], r["ts"] or 0, r["id"]))

    # Split into excerpts: a new excerpt starts when the conversation changes or
    # there's a gap in the message sequence (i.e. unselected messages between).
    seq = {}
    for chat_id in {r["chat_id"] for r in rows}:
        ordered = [x[0] for x in conn.execute(
            "SELECT id FROM messages WHERE chat_id=? AND is_reaction=0 ORDER BY ts, id", (chat_id,))]
        seq.update({mid: n for n, mid in enumerate(ordered)})
    excerpts: list[list[dict]] = []
    for r in rows:
        prev = excerpts[-1][-1] if excerpts else None
        if prev and prev["chat_id"] == r["chat_id"] and seq.get(r["id"], 0) - seq.get(prev["id"], 0) <= 1:
            excerpts[-1].append(r)
        else:
            excerpts.append([r])
    excerpts.sort(key=lambda ex: (ex[0]["ts"] or 0))

    out, n = [], 0
    for e_num, ex in enumerate(excerpts, 1):
        for r in ex:
            n += 1
            r.update(ref=f"M-{n:04d}", matched=r["id"] in matched, excerpt=e_num)
            out.append(r)
    return out[:MAX_ROWS]


# --------------------------------------------------------------------------- #
# Formatting helpers
# --------------------------------------------------------------------------- #
def fmt_dt(iso: str, seconds=True) -> str:
    if not iso:
        return ""
    d = datetime.fromisoformat(iso)
    return d.strftime(f"%b %-d, %Y, %-I:%M{':%S' if seconds else ''} %p")


def fmt_day(iso: str) -> str:
    return datetime.fromisoformat(iso).strftime("%B %-d, %Y") if iso else ""


def meta(conn) -> dict:
    try:
        return dict(conn.execute("SELECT key, value FROM meta").fetchall())
    except sqlite3.Error:
        return {}


def describe_filters(f: dict, chats: dict[int, str]) -> list[tuple[str, str]]:
    out = []
    if f.get("q"):
        mode = {"all": "all of the words", "any": "any of the words", "phrase": "the exact phrase"}.get(f.get("mode"), "")
        out.append(("Words", f"“{f['q']}” ({mode})"))
    senders = f.get("sender") or []
    if senders:
        out.append(("Sender", ", ".join(senders)))
    if f.get("chat"):
        out.append(("Conversation", chats.get(int(f["chat"]), f["chat"])))
    if f.get("from") or f.get("to"):
        out.append(("Dates", f"{f.get('from') or 'beginning'} to {f.get('to') or 'latest'}"))
    return out or [("Criteria", "All messages")]


def summary_input(rows: list[dict]) -> tuple[str, bool]:
    lines, used = [], 0
    for r in rows:
        text = (r["text"] or "") + (f" [attachment: {r['attachments']}]" if r.get("attachments") else "")
        line = f"[{r['ref']}] {r['date'][:16].replace('T', ' ')} | {r['chat']} | {r['sender']}: {text}"
        if used + len(line) > MAX_SUMMARY_CHARS:
            return "\n".join(lines), True
        lines.append(line)
        used += len(line) + 1
    return "\n".join(lines), False


def md_to_html(md: str, valid_refs: set[str]) -> str:
    """Small Markdown renderer for the summary; turns [M-0001] into links."""
    def inline(s: str) -> str:
        s = html.escape(s)
        s = re.sub(r"\*\*(.+?)\*\*", r"<strong>\1</strong>", s)
        s = re.sub(r"(?<![\w*])\*(?!\s)(.+?)(?<!\s)\*(?![\w*])", r"<em>\1</em>", s)

        def refs(m):
            parts = re.findall(r"M-\d{4}", m.group(0))
            links = [f'<a class="ref" href="#{p}">{p}</a>' if p in valid_refs else p for p in parts]
            return "[" + ", ".join(links) + "]"
        return re.sub(r"\[(?:M-\d{4}(?:\s*[,;–-]\s*)?)+\]", refs, s)

    out, in_list = [], False
    for line in md.splitlines():
        li = re.match(r"^\s*[-*]\s+(.*)", line)
        if li:
            if not in_list:
                out.append("<ul>")
                in_list = True
            out.append(f"<li>{inline(li.group(1))}</li>")
            continue
        if in_list:
            out.append("</ul>")
            in_list = False
        h = re.match(r"^(#{1,4})\s+(.*)", line)
        if h:
            level = 3 if len(h.group(1)) <= 2 else 4
            out.append(f"<h{level}>{inline(h.group(2))}</h{level}>")
        elif line.strip():
            out.append(f"<p>{inline(line)}</p>")
    if in_list:
        out.append("</ul>")
    return "\n".join(out)


# --------------------------------------------------------------------------- #
# Outputs
# --------------------------------------------------------------------------- #
def to_csv(rows: list[dict]) -> bytes:
    buf = io.StringIO()
    buf.write("﻿")  # so Excel opens it as UTF-8
    w = csv.writer(buf)
    w.writerow(["Ref", "Matched search", "Excerpt", "Date and time", "Sender", "Sent by me",
                "Conversation", "Message", "Attachments", "Message ROWID", "Message GUID"])
    for r in rows:
        w.writerow([r["ref"], "yes" if r["matched"] else "context", r["excerpt"], r["date"], r["sender"],
                    "yes" if r["is_from_me"] else "no", r["chat"], r["text"], r.get("attachments", ""),
                    r["id"], r["guid"]])
    return buf.getvalue().encode("utf-8")


CSS = """
:root { --ink:#1b1b1b; --muted:#666; --line:#d9d9d9; --soft:#f4f4f2; --accent:#1f4fa8; --mark:#fff3b0; }
* { box-sizing: border-box; }
body { margin:0; background:#e9e9e6; color:var(--ink); font: 11pt/1.45 Georgia, "Times New Roman", serif; }
.toolbar { position:sticky; top:0; background:#1f1f21; color:#fff; padding:10px 16px; display:flex; gap:10px;
  align-items:center; flex-wrap:wrap; font: 14px -apple-system, system-ui, sans-serif; z-index:2; }
.toolbar button, .toolbar a { font:inherit; font-weight:600; border:0; border-radius:8px; padding:8px 14px;
  background:#fff; color:#1f1f21; cursor:pointer; text-decoration:none; }
.toolbar button.primary { background:#6d9cff; color:#0b1020; }
.toolbar span { color:#bbb; font-size:13px; }
.page { background:#fff; max-width:8.5in; margin:16px auto; padding:0.8in 0.8in; box-shadow:0 1px 4px rgba(0,0,0,.15); }
h1 { font-size:24pt; margin:0 0 4pt; line-height:1.15; }
h2 { font-size:14pt; margin:22pt 0 8pt; padding-bottom:4pt; border-bottom:1.5px solid var(--ink); break-after:avoid; }
h3 { font-size:12pt; margin:14pt 0 4pt; break-after:avoid; }
h4 { font-size:11pt; margin:10pt 0 2pt; break-after:avoid; }
.sub { color:var(--muted); font-size:12pt; margin:0 0 24pt; }
.sans { font-family: -apple-system, "Helvetica Neue", Arial, sans-serif; }
table { width:100%; border-collapse:collapse; }
.facts td { padding:5pt 0; border-bottom:1px solid var(--line); vertical-align:top; }
.facts td:first-child { width:30%; color:var(--muted); font: 9.5pt -apple-system, "Helvetica Neue", Arial, sans-serif;
  text-transform:uppercase; letter-spacing:.04em; padding-top:7pt; }
.mono { font-family: ui-monospace, Menlo, monospace; font-size:8.5pt; word-break:break-all; }
.note { font-size:10pt; color:var(--muted); }
.ai { border:1.5px solid var(--ink); padding:10pt 14pt; margin-top:6pt; }
.ai-label { font: 700 9pt -apple-system, "Helvetica Neue", Arial, sans-serif; text-transform:uppercase; letter-spacing:.06em;
  border-bottom:1px solid var(--line); padding-bottom:6pt; margin-bottom:4pt; }
.ai-label span { display:block; font-weight:400; text-transform:none; letter-spacing:0; color:var(--muted); margin-top:3pt; }
.ai ul { padding-left:16pt; margin:4pt 0; } .ai li { margin:3pt 0; }
a.ref { color:var(--accent); text-decoration:none; font: 600 9pt -apple-system, "Helvetica Neue", Arial, sans-serif; }
.index th, .index td { text-align:left; padding:4pt 6pt 4pt 0; border-bottom:1px solid var(--line); vertical-align:top;
  font-size:9.5pt; }
.index th { font: 700 8.5pt -apple-system, "Helvetica Neue", Arial, sans-serif; text-transform:uppercase; color:var(--muted); }
.index td:nth-child(1) { white-space:nowrap; } .index td:nth-child(2) { white-space:nowrap; width:1%; }
.excerpt { margin-top:16pt; }
.excerpt-head { font: 600 9.5pt -apple-system, "Helvetica Neue", Arial, sans-serif; background:var(--soft);
  padding:5pt 8pt; border-left:3px solid var(--ink); break-after:avoid; }
.msg { display:grid; grid-template-columns: 0.72in 1.45in 1fr; gap:0 8pt; padding:4pt 0 4pt 8pt;
  border-bottom:1px solid #eee; break-inside:avoid; font-size:10pt; }
.msg .r { font: 600 8.5pt -apple-system, "Helvetica Neue", Arial, sans-serif; color:var(--muted); padding-top:1pt; }
.msg .t { font: 8.5pt -apple-system, "Helvetica Neue", Arial, sans-serif; color:var(--muted); padding-top:1pt; }
.msg .who { font-weight:700; }
.msg .body { white-space:pre-wrap; overflow-wrap:anywhere; }
.msg .att { font: italic 9pt Georgia, serif; color:var(--muted); }
.msg.match { background:var(--mark); border-left:3px solid #b08800; padding-left:5pt; }
.msg.match .r::after { content:" ★"; color:#8a6a00; }
.legend { font: 9pt -apple-system, "Helvetica Neue", Arial, sans-serif; color:var(--muted); margin:4pt 0 0; }
.legend b { background:var(--mark); border-left:3px solid #b08800; padding:0 4pt; font-weight:600; color:var(--ink); }
.cover { min-height:8.4in; display:flex; flex-direction:column; }
.cover .end { margin-top:auto; font-size:9.5pt; color:var(--muted); }
.pb { break-before:page; }
@page { size: letter; margin: 0.7in 0.75in;
  @bottom-center { content: "Page " counter(page) " of " counter(pages); font: 8.5pt Georgia, serif; color:#666; }
  @top-right { content: "REPORT_TITLE"; font: 8.5pt Georgia, serif; color:#666; } }
@page :first { @top-right { content: none; } }
@media print {
  body { background:#fff; }
  .toolbar { display:none; }
  .page { margin:0; padding:0; box-shadow:none; max-width:none; }
  a.ref { color:var(--ink); }
  .msg.match { -webkit-print-color-adjust:exact; print-color-adjust:exact; }
  .excerpt-head, .legend b { -webkit-print-color-adjust:exact; print-color-adjust:exact; }
}
"""


def to_html(rows: list[dict], info: dict, token: str) -> str:
    e = html.escape
    title = info["title"]
    m = info["meta"]
    matched = [r for r in rows if r["matched"]]
    senders = Counter(r["sender"] for r in rows)
    dates = [r["date"] for r in rows if r["date"]]
    date_range = f"{fmt_day(min(dates))} – {fmt_day(max(dates))}" if dates else "—"
    tz = m.get("timezone", "")

    facts = [
        ("Prepared by", e(info["prepared_by"] or "—")),
        ("Prepared on", e(datetime.now().astimezone().strftime("%B %-d, %Y at %-I:%M %p %Z"))),
        ("Messages in this report",
         f"{len(rows):,} total: {len(matched):,} matching the search, {len(rows) - len(matched):,} shown for context"
         + (f" ({info['context_n']} before and after each match)" if info["context_n"] else "")),
        ("Date range", e(date_range)),
        ("Participants", e(", ".join(f"{s} ({n:,})" for s, n in senders.most_common()))),
    ] + [(k, e(v)) for k, v in describe_filters(info["filters"], info["chats"])]

    out = [f"""<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{e(title)} – {datetime.now().strftime('%Y-%m-%d')}</title>
<style>{CSS.replace('REPORT_TITLE', title.replace('"', "'").replace(chr(92), ''))}</style></head><body>
<div class="toolbar"><button class="primary" onclick="window.print()">Save as PDF</button>
<a href="/report/{token}.csv">Download spreadsheet (CSV)</a>
<span>In the print window, choose <b>Save as PDF</b> and turn off “Headers and footers”. Page numbers are added automatically in Chrome.</span></div>
<div class="page">
<section class="cover">
  <h1>{e(title)}</h1>
  <p class="sub">Text message report</p>
  <table class="facts">{''.join(f'<tr><td>{k}</td><td>{v}</td></tr>' for k, v in facts)}</table>
  <p class="end">Messages are reproduced as stored on the device. Reference numbers (M-0001…) identify each message in this
  report and match the accompanying spreadsheet.</p>
</section>

<section class="pb">
  <h2>1. Source and method</h2>
  <table class="facts">
    <tr><td>Source</td><td>{e(m.get('source_desc', 'Not recorded'))}</td></tr>
    <tr><td>Database fingerprint</td><td><span class="mono">SHA-256 {e(m.get('source_sha256', 'not recorded'))}</span><br>
      <span class="note">A fingerprint of the exact copy of the Messages database this report was made from. Any change to that file changes the fingerprint.</span></td></tr>
    <tr><td>Search index built</td><td>{e(fmt_dt(m.get('built_at', ''), seconds=False) or '—')}</td></tr>
    <tr><td>Times</td><td>Shown in the Mac's local time zone{f' ({e(tz)})' if tz else ''}, adjusted for daylight saving time.</td></tr>
    <tr><td>What's included</td><td>Message text exactly as stored. Tapback reactions are left out. Attachments are listed
      by file name; their contents aren't included. Messages marked ★ matched the search; the others are the surrounding
      conversation, included for context.</td></tr>
    <tr><td>Prepared with</td><td>{e(m.get('tool', 'iMessage Search'))}</td></tr>
  </table>
</section>
"""]

    sec = 2
    if info.get("summary"):
        out.append(f"""<section class="pb"><h2>{sec}. Summary</h2>
<div class="ai"><div class="ai-label">AI-generated summary
<span>Written by Claude ({e(info['model'])}) on {e(datetime.now().strftime('%B %-d, %Y'))} from the {e(f"{info['summary_count']:,}")} messages
in this report{' (the earliest ones, because the full set was too long)' if info['summary_truncated'] else ''}{f', focusing on: “{e(info["question"])}”' if info.get('question') else ''}.
It's a reading aid, not evidence or legal advice. Check each point against the cited messages.</span></div>
{md_to_html(info['summary'], {r['ref'] for r in rows})}
</div></section>""")
        sec += 1

    out.append(f"""<section class="pb"><h2>{sec}. Index of matching messages</h2>
<table class="index"><thead><tr><th>Ref</th><th>Date</th><th>Sender</th><th>Message</th></tr></thead><tbody>""")
    for r in matched:
        text = (r["text"] or "").replace("\n", " ")
        text = text[:160] + ("…" if len(text) > 160 else "")
        out.append(f'<tr><td><a class="ref" href="#{r["ref"]}">{r["ref"]}</a></td><td>{e(fmt_dt(r["date"], False))}</td>'
                   f'<td>{e(r["sender"])}</td><td>{e(text)}</td></tr>')
    out.append("</tbody></table></section>")
    sec += 1

    out.append(f"""<section class="pb"><h2>{sec}. Messages</h2>
<p class="legend"><b>★ Highlighted</b> messages matched the search. The others are the conversation around them.</p>""")
    current = None
    for r in rows:
        if r["excerpt"] != current:
            if current is not None:
                out.append("</div>")
            current = r["excerpt"]
            ex = [x for x in rows if x["excerpt"] == current]
            out.append(f'<div class="excerpt"><div class="excerpt-head">Excerpt {current} · {e(r["chat"] or "")} · '
                       f'{e(fmt_dt(ex[0]["date"], False))} – {e(fmt_dt(ex[-1]["date"], False))}</div>')
        att = f'<div class="att">Attachment: {e(r["attachments"])}</div>' if r.get("attachments") else (
            '<div class="att">[Attachment]</div>' if r["has_attachment"] and not r["text"] else "")
        out.append(f'<div class="msg{" match" if r["matched"] else ""}" id="{r["ref"]}"><div class="r">{r["ref"]}</div>'
                   f'<div class="t">{e(fmt_dt(r["date"]))}</div><div><span class="who">{e(r["sender"])}:</span> '
                   f'<span class="body">{e(r["text"] or "")}</span>{att}</div></div>')
    if current is not None:
        out.append("</div>")
    out.append("</section></div></body></html>")
    return "\n".join(out)
