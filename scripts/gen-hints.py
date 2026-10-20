#!/usr/bin/env python3
"""Build portal/hints/hints.json from the diagnosis-hints markdown.

Usage: scripts/gen-hints.py <hints.md> [out.json]

Each "## slug" section has "- Trigger:", "- Meaning:", "- Check ...:" (a list
of `commands`), "- Likely causes:" (a list) and "- Note:" (note titles).
Only read-only commands are kept (kubectl get/describe/logs/top/events,
helm get/list/status/history); anything else is dropped here and the Go
tests check the result again.
"""
import json, re, sys

READONLY = re.compile(r"^(kubectl( -n \S+| --namespace \S+| -A)* (get|describe|logs|top|events|auth can-i|version|api-resources|explain)|helm (get|list|status|history))\b")

def parse(text):
    out, cur, field = [], None, None
    for line in text.splitlines():
        m = re.match(r"^## ([a-z0-9-]+)\s*$", line)
        if m:
            cur = {"slug": m.group(1), "trigger": "", "meaning": "", "check": [], "causes": [], "notes": []}
            out.append(cur); field = None
            continue
        if line.startswith("## "):
            cur = None; continue
        if cur is None:
            continue
        m = re.match(r"^- (Trigger|Meaning|Check[^:]*|Likely causes|Note):\s*(.*)$", line)
        if m:
            key = m.group(1).split(" ")[0].lower()
            field = {"trigger": "trigger", "meaning": "meaning", "check": "check", "likely": "causes", "note": "notes"}[key]
            rest = m.group(2).strip()
            if field in ("trigger", "meaning"):
                cur[field] = rest
            elif field == "notes":
                cur["notes"] = [n.strip() for n in rest.split(";") if n.strip()]
            continue
        m = re.match(r"^  - (.*)$", line)
        if m and field in ("check", "causes"):
            item = m.group(1).strip()
            if field == "check":
                c = re.match(r"^`([^`]+)`", item)
                if c and READONLY.match(c.group(1)):
                    cur["check"].append(c.group(1))
            else:
                cur["causes"].append(item)
    return out

if __name__ == "__main__":
    src = sys.argv[1]
    dst = sys.argv[2] if len(sys.argv) > 2 else "portal/hints/hints.json"
    entries = parse(open(src, encoding="utf-8").read())
    with open(dst, "w", encoding="utf-8") as f:
        json.dump(entries, f, indent=1, ensure_ascii=False); f.write("\n")
    print(len(entries), "entries ->", dst)
