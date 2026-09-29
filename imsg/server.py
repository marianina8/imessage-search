"""Local web app for searching the message index and summarizing results with Claude.

Runs on http://127.0.0.1:8765 only (never exposed to your network).

    python -m imsg.server --open     # also opens the browser (setup wizard if no index yet)
    python -m imsg.server --port 9000 --index data/index.sqlite

AI summaries need ANTHROPIC_API_KEY in the environment (or in a .env file at the
repo root). Only the messages shown in your current results are sent.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import secrets
import socket
import sqlite3
import threading
import urllib.error
import webbrowser
import urllib.request
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from . import build_index, macos, report

ROOT = Path(__file__).resolve().parent.parent
STATIC = ROOT / "web"
DEFAULT_INDEX = ROOT / "data" / "index.sqlite"
DEFAULT_MODEL = "claude-sonnet-5-5"
MAX_SUMMARY_CHARS = 120_000
DEMO = False  # set by imsg.demo: fictional data, setup wizard disabled  # keep prompts comfortably inside the context window


def load_dotenv():
    env = ROOT / ".env"
    if env.exists():
        for line in env.read_text().splitlines():
            line = line.strip()
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                os.environ.setdefault(k.strip(), v.strip().strip('"').strip("'"))


# --------------------------------------------------------------------------- #
# Queries
# --------------------------------------------------------------------------- #
def fts_query(q: str, mode: str) -> str:
    """Turn user input into a safe FTS5 MATCH expression."""
    if mode == "phrase":
        return '"' + q.replace('"', " ") + '"'
    terms = re.findall(r'"[^"]+"|\S+', q)
    parts = []
    for t in terms:
        if t.startswith('"') and t.endswith('"') and len(t) > 2:
            parts.append('"' + t[1:-1].replace('"', " ") + '"')
        else:
            t = t.replace('"', "")
            if not t:
                continue
            prefix = t.endswith("*")
            t = t.rstrip("*")
            if t:
                parts.append('"' + t + '"' + ("*" if prefix else ""))
    return (" OR " if mode == "any" else " AND ").join(parts)


def date_to_ts(s: str, end=False):
    if not s:
        return None
    dt = datetime.fromisoformat(s)
    if end and len(s) <= 10:
        dt = dt.replace(hour=23, minute=59, second=59)
    return dt.astimezone().timestamp()


def search(conn, params: dict) -> dict:
    q = (params.get("q") or "").strip()
    mode = params.get("mode") or "all"
    senders = [s for s in params.get("sender_list", []) if s]
    chat = params.get("chat")
    limit = min(int(params.get("limit") or 200), 2000)
    offset = int(params.get("offset") or 0)
    include_reactions = params.get("reactions") == "1"

    where, args = ["m.text != ''"], []
    join = ""
    if q:
        join = "JOIN messages_fts f ON f.rowid = m.id"
        where.append("messages_fts MATCH ?")
        args.append(fts_query(q, mode))
    if senders:
        where.append(f"m.sender IN ({','.join('?' * len(senders))})")
        args += senders
    if chat:
        where.append("m.chat_id = ?")
        args.append(int(chat))
    if (ts := date_to_ts(params.get("from", ""))) is not None:
        where.append("m.ts >= ?")
        args.append(ts)
    if (ts := date_to_ts(params.get("to", ""), end=True)) is not None:
        where.append("m.ts <= ?")
        args.append(ts)
    if not include_reactions:
        where.append("m.is_reaction = 0")

    order = "m.ts DESC" if params.get("sort") != "oldest" else "m.ts ASC"
    base = f"FROM messages m {join} LEFT JOIN chats c ON c.chat_id = m.chat_id WHERE {' AND '.join(where)}"
    try:
        total = conn.execute(f"SELECT COUNT(*) {base}", args).fetchone()[0]
        rows = conn.execute(
            f"SELECT m.id, m.chat_id, c.label AS chat, m.date, m.sender, m.is_from_me, m.text, m.has_attachment "
            f"{base} ORDER BY {order} LIMIT ? OFFSET ?",
            args + [limit, offset],
        ).fetchall()
    except sqlite3.OperationalError as exc:
        return {"error": f"Search syntax problem: {exc}", "total": 0, "results": []}
    return {"total": total, "results": [dict(r) for r in rows]}


def context(conn, message_id: int, n: int = 10) -> list[dict]:
    row = conn.execute("SELECT chat_id, ts FROM messages WHERE id = ?", (message_id,)).fetchone()
    if not row:
        return []
    before = conn.execute(
        "SELECT id, date, sender, is_from_me, text FROM messages WHERE chat_id=? AND ts < ? AND is_reaction=0 "
        "ORDER BY ts DESC LIMIT ?", (row["chat_id"], row["ts"], n)).fetchall()
    after = conn.execute(
        "SELECT id, date, sender, is_from_me, text FROM messages WHERE chat_id=? AND ts >= ? AND is_reaction=0 "
        "ORDER BY ts ASC LIMIT ?", (row["chat_id"], row["ts"], n + 1)).fetchall()
    return [dict(r) for r in reversed(before)] + [dict(r) for r in after]


def senders(conn) -> list[dict]:
    return [dict(r) for r in conn.execute(
        "SELECT sender, COUNT(*) AS n FROM messages WHERE is_reaction=0 GROUP BY sender ORDER BY n DESC")]


def chats(conn) -> list[dict]:
    return [dict(r) for r in conn.execute(
        "SELECT chat_id, label, message_count FROM chats ORDER BY message_count DESC")]


# --------------------------------------------------------------------------- #
# Summaries
# --------------------------------------------------------------------------- #
SYSTEM_PROMPT = (
    "You summarize excerpts from a person's own text-message history for them. "
    "Be factual and neutral. Cite specific messages by their [date] and sender when you make a claim. "
    "Say plainly when the excerpts don't support a conclusion, and don't speculate beyond them. "
    "Format with short headings and bullet points in Markdown."
)


class ClaudeError(Exception):
    pass


def call_claude(system: str, user_msg: str, max_tokens: int = 2500) -> tuple[str, str]:
    """Send one message to the Anthropic API. Returns (text, model)."""
    api_key = os.environ.get("ANTHROPIC_API_KEY")
    if not api_key:
        raise ClaudeError("No Anthropic API key is set. Add one on the search page.")
    model = os.environ.get("ANTHROPIC_MODEL", DEFAULT_MODEL)
    payload = {"model": model, "max_tokens": max_tokens, "system": system,
               "messages": [{"role": "user", "content": user_msg}]}
    req = urllib.request.Request(
        "https://api.anthropic.com/v1/messages",
        data=json.dumps(payload).encode(),
        headers={"x-api-key": api_key, "anthropic-version": "2023-06-01", "content-type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=300) as resp:
            data = json.load(resp)
    except urllib.error.HTTPError as exc:
        raise ClaudeError(f"Anthropic API error {exc.code}: {exc.read().decode()[:500]}") from exc
    except urllib.error.URLError as exc:
        raise ClaudeError(f"Could not reach the Anthropic API: {exc.reason}") from exc
    text = "".join(b.get("text", "") for b in data.get("content", []) if b.get("type") == "text")
    return text, data.get("model", model)


def summarize(conn, body: dict) -> dict:
    api_key = os.environ.get("ANTHROPIC_API_KEY")
    if not api_key:
        return {"error": "ANTHROPIC_API_KEY is not set. Add it to .env (see README)."}

    ids = [int(i) for i in body.get("ids", [])][:2000]
    with_context = bool(body.get("with_context"))
    question = (body.get("question") or "").strip()
    if not ids:
        return {"error": "No messages to summarize."}

    if with_context:
        seen, rows = set(), []
        for i in ids:
            for r in context(conn, i, 3):
                if r["id"] not in seen:
                    seen.add(r["id"])
                    rows.append(r)
    else:
        marks = ",".join("?" * len(ids))
        rows = [dict(r) for r in conn.execute(
            f"SELECT id, date, sender, text FROM messages WHERE id IN ({marks})", ids)]
    rows.sort(key=lambda r: r["date"])

    lines, used, truncated = [], 0, False
    for r in rows:
        line = f"[{r['date'][:16].replace('T', ' ')}] {r['sender']}: {r['text']}"
        if used + len(line) > MAX_SUMMARY_CHARS:
            truncated = True
            break
        lines.append(line)
        used += len(line) + 1

    task = question or "Summarize the main topics, key facts, decisions, and any notable changes over time."
    user_msg = (
        f"Search filters used: {json.dumps(body.get('filters', {}))}\n\n"
        f"<messages>\n" + "\n".join(lines) + "\n</messages>\n\n"
        f"Task: {task}"
    )
    try:
        text, model = call_claude(SYSTEM_PROMPT, user_msg, 2500)
    except ClaudeError as exc:
        return {"error": str(exc)}
    return {"summary": text, "messages_sent": len(lines), "truncated": truncated, "model": model}


# --------------------------------------------------------------------------- #
# Export
# --------------------------------------------------------------------------- #
EXPORTS: dict[str, dict] = {}  # token -> {"html": str, "csv": bytes, "name": str}


def create_export(conn, body: dict) -> dict:
    ids = [int(i) for i in body.get("ids", [])]
    if not ids:
        return {"error": "Search first, then export the results."}
    context_n = max(0, min(int(body.get("context", 5)), 25))
    rows = report.collect(conn, ids, context_n)
    if not rows:
        return {"error": "Nothing to export."}
    info = {
        "title": (body.get("title") or "Text messages").strip()[:150],
        "prepared_by": (body.get("prepared_by") or "").strip()[:100],
        "context_n": context_n,
        "filters": body.get("filters") or {},
        "chats": {c["chat_id"]: c["label"] for c in chats(conn)},
        "meta": report.meta(conn),
        "question": (body.get("question") or "").strip(),
    }
    if body.get("include_summary"):
        transcript, truncated = report.summary_input(rows)
        focus = f"\n\nFocus especially on: {info['question']}" if info["question"] else ""
        try:
            text, model = call_claude(
                report.REPORT_SYSTEM_PROMPT,
                f"<messages>\n{transcript}\n</messages>{focus}", max_tokens=4000)
        except ClaudeError as exc:
            return {"error": f"The report wasn't created because the AI summary failed: {exc}"}
        info.update(summary=text, model=model, summary_truncated=truncated,
                    summary_count=transcript.count("\n") + 1)
    token = secrets.token_urlsafe(12)
    EXPORTS[token] = {
        "html": report.to_html(rows, info, token),
        "csv": report.to_csv(rows),
        "name": re.sub(r"[^\w\- ]+", "", info["title"]).strip() or "messages",
    }
    while len(EXPORTS) > 20:  # keep memory bounded
        EXPORTS.pop(next(iter(EXPORTS)))
    return {"token": token, "count": len(rows)}


# --------------------------------------------------------------------------- #
# Setup wizard: background build job
# --------------------------------------------------------------------------- #
class BuildJob:
    def __init__(self):
        self.lock = threading.Lock()
        self.state = {"state": "idle"}
        self.thread: threading.Thread | None = None

    def get(self) -> dict:
        with self.lock:
            return dict(self.state)

    def set(self, **kw):
        with self.lock:
            self.state.update(kw)

    def start(self, index_path: Path, body: dict) -> dict:
        if self.thread and self.thread.is_alive():
            return {"error": "Already building."}
        self.state = {"state": "running", "stage": "Getting your messages…", "done": 0, "total": 0}
        self.thread = threading.Thread(target=self._run, args=(index_path, body), daemon=True)
        self.thread.start()
        return {"ok": True}

    def _run(self, index_path: Path, body: dict):
        data_dir = index_path.parent
        try:
            now = datetime.now().astimezone().strftime("%B %-d, %Y at %-I:%M %p %Z")
            if body.get("source") == "backup":
                self.set(stage="Unlocking and copying messages from the iPhone backup…")
                got = macos.extract_from_backup(body.get("backup_id", ""), data_dir, body.get("password", ""))
                info = next((b for b in macos.list_backups() if b["id"] == body.get("backup_id")), {})
                made = datetime.fromisoformat(info["date"]).strftime("%B %-d, %Y at %-I:%M %p") if info.get("date") else "unknown date"
                desc = (f"Local (Finder) backup of the iPhone \"{info.get('device', 'iPhone')}\" made {made}"
                        f"{' (encrypted)' if info.get('encrypted') else ''}. Messages database extracted {now}.")
            else:
                self.set(stage="Copying messages from the Messages app…")
                got = macos.copy_from_mac(data_dir)
                desc = f"Messages app database on this Mac (~/Library/Messages/chat.db), copied {now}."
            self.set(stage="Reading and indexing messages…")
            result = build_index.build(
                got["db"], index_path, [], (body.get("me") or "Me").strip() or "Me", True,
                ios_contacts=got["contacts"],
                progress=lambda done, total: self.set(done=done, total=total),
                source_desc=desc,
            )
            (data_dir / "setup.json").write_text(json.dumps({
                "source": body.get("source"), "backup_id": body.get("backup_id"),
                "me": body.get("me"), "built": datetime.now().astimezone().isoformat(),
            }))
            self.set(state="done", stage="Done", result=result)
        except macos.WrongPassword as exc:
            self.set(state="failed", error=str(exc), needs_password=True)
        except Exception as exc:  # show anything else in the wizard
            self.set(state="failed", error=f"{type(exc).__name__}: {exc}")


BUILD_JOB = BuildJob()
BACKUP_JOB = macos.BackupJob()


def save_api_key(key: str) -> dict:
    key = key.strip()
    if not key.startswith("sk-"):
        return {"error": "That doesn't look like an Anthropic API key (it should start with sk-ant-)."}
    env = ROOT / ".env"
    lines = [l for l in (env.read_text().splitlines() if env.exists() else []) if not l.startswith("ANTHROPIC_API_KEY=")]
    lines.insert(0, f"ANTHROPIC_API_KEY={key}")
    env.write_text("\n".join(lines) + "\n")
    os.chmod(env, 0o600)
    os.environ["ANTHROPIC_API_KEY"] = key
    return {"ok": True}


def setup_status(index_path: Path) -> dict:
    info = {}
    if (index_path.parent / "setup.json").exists():
        try:
            info = json.loads((index_path.parent / "setup.json").read_text())
        except ValueError:
            pass
    return {
        "platform_mac": macos.IS_MAC,
        "fda": macos.full_disk_access(),
        "host_app": macos.host_app(),
        "can_relaunch": macos.IS_MAC and macos.host_app() == "Terminal",
        "index_ready": index_path.exists(),
        "last_setup": info,
        "me_default": info.get("me") or macos.full_name(),
        "ai": bool(os.environ.get("ANTHROPIC_API_KEY")),
        "backup_tool": BACKUP_JOB.available(),
    }


# --------------------------------------------------------------------------- #
# HTTP
# --------------------------------------------------------------------------- #
class Handler(BaseHTTPRequestHandler):
    index_path: Path = DEFAULT_INDEX
    server_ref: ThreadingHTTPServer | None = None

    def log_message(self, fmt, *args):  # quieter console
        pass

    def db(self):
        conn = sqlite3.connect(f"file:{self.index_path}?mode=ro", uri=True)
        conn.row_factory = sqlite3.Row
        return conn

    def send_json(self, obj, status=200):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def send_file(self, name):
        data = (STATIC / name).read_bytes()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def redirect(self, where):
        self.send_response(302)
        self.send_header("Location", where)
        self.end_headers()

    def body(self) -> dict:
        length = int(self.headers.get("Content-Length", 0))
        return json.loads(self.rfile.read(length) or b"{}")

    def same_origin(self) -> bool:
        # Block other websites from driving this local API from your browser.
        origin = self.headers.get("Origin")
        host = self.headers.get("Host", "")
        return origin is None or origin.endswith("//" + host)

    def do_GET(self):
        url = urlparse(self.path)
        raw = parse_qs(url.query)
        qs = {k: v[-1] for k, v in raw.items()}
        qs["sender_list"] = raw.get("sender", [])
        path = url.path

        if path in ("/", "/index.html"):
            return self.redirect("/setup") if not self.index_path.exists() else self.send_file("index.html")
        if path == "/setup":
            return self.redirect("/") if DEMO else self.send_file("setup.html")
        if path.startswith("/report/"):
            token = path[len("/report/"):]
            as_csv = token.endswith(".csv")
            exp = EXPORTS.get(token[:-4] if as_csv else token)
            if not exp:
                return self.send_json({"error": "This report has expired. Export again from the search page."}, 404)
            data = exp["csv"] if as_csv else exp["html"].encode()
            self.send_response(200)
            if as_csv:
                self.send_header("Content-Type", "text/csv; charset=utf-8")
                self.send_header("Content-Disposition", f'attachment; filename="{exp["name"]}.csv"')
            else:
                self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(data)
            return

        # Setup endpoints work before an index exists.
        if path == "/api/setup/status":
            return self.send_json(setup_status(self.index_path))
        if path == "/api/setup/devices":
            return self.send_json({"devices": macos.connected_devices(), "backups": macos.list_backups(),
                                   "backup_job": BACKUP_JOB.status()})
        if path == "/api/setup/mac":
            return self.send_json(macos.mac_messages_stats())
        if path == "/api/setup/build":
            return self.send_json(BUILD_JOB.get())

        if not self.index_path.exists():
            return self.send_json({"error": "No index yet. Open /setup."}, 404)
        with self.db() as conn:
            if path == "/api/search":
                return self.send_json(search(conn, qs))
            if path == "/api/context":
                return self.send_json(context(conn, int(qs["id"]), int(qs.get("n", 10))))
            if path == "/api/meta":
                info = setup_status(self.index_path)
                return self.send_json({
                    "senders": senders(conn), "chats": chats(conn),
                    "ai": bool(os.environ.get("ANTHROPIC_API_KEY")), "last_setup": info["last_setup"],
                    "me_name": report.meta(conn).get("me_name", ""), "demo": DEMO,
                })
        self.send_json({"error": "not found"}, 404)

    def do_POST(self):
        if not self.same_origin():
            return self.send_json({"error": "forbidden"}, 403)
        path = urlparse(self.path).path
        body = self.body()
        if path == "/api/summarize":
            with self.db() as conn:
                return self.send_json(summarize(conn, body))
        if path == "/api/export":
            with self.db() as conn:
                return self.send_json(create_export(conn, body))
        if path == "/api/setup/open":
            return self.send_json({"ok": macos.open_target(body.get("target", ""))})
        if path == "/api/setup/backup":
            return self.send_json(BACKUP_JOB.start(body.get("serial", "")))
        if path == "/api/setup/build":
            return self.send_json(BUILD_JOB.start(self.index_path, body))
        if path == "/api/setup/apikey":
            return self.send_json(save_api_key(body.get("key", "")))
        if path == "/api/setup/relaunch":
            ok = macos.relaunch_terminal(macos.find_launcher(ROOT))
            if ok:
                self.index_path.parent.mkdir(parents=True, exist_ok=True)
                (self.index_path.parent / ".resume-setup").touch()
            self.send_json({"ok": ok})
            if ok:  # exit so Terminal can quit without asking about a running process
                threading.Timer(0.5, lambda: os._exit(0)).start()
            return
        self.send_json({"error": "not found"}, 404)


def port_in_use(port: int) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        return s.connect_ex(("127.0.0.1", port)) == 0


def main(argv=None):
    ap = argparse.ArgumentParser(description="Local message search UI")
    ap.add_argument("--index", type=Path, default=DEFAULT_INDEX)
    ap.add_argument("--port", type=int, default=8765)
    ap.add_argument("--open", action="store_true", help="open the browser once the server is up")
    args = ap.parse_args(argv)
    load_dotenv()

    url = f"http://127.0.0.1:{args.port}/"
    if port_in_use(args.port):
        print(f"iMessage Search is already running. Opening {url}")
        if args.open:
            webbrowser.open(url)
        return

    Handler.index_path = args.index.resolve()
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print("=" * 60)
    print(f"  iMessage Search{' DEMO (fictional messages)' if DEMO else ''} is running at {url}")
    print("  Keep this window open while you use it.")
    print("  To stop: close this window, or press Ctrl+C.")
    print("=" * 60)
    if not args.index.exists():
        print("No search index yet, so the setup wizard will open.")
    resume = args.index.parent / ".resume-setup"
    target = url
    if resume.exists():  # coming back from the Full Disk Access restart
        resume.unlink()
        target = url + "setup#resume"
    elif not args.index.exists():
        target = url + "setup"
    if args.open:
        threading.Timer(0.8, lambda: webbrowser.open(target)).start()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
