#!/usr/bin/env python3
"""Extract example responses from archived Twitter docs into testdata/reference.

dev.twitter.com (mid 2010 to early 2011) pages show real API output in
syntax-highlighted <pre> blocks labelled JSON / XML / RSS / Atom. Each block
is saved verbatim (tags stripped, entities decoded) as
testdata/reference/<method>.<format>, and SOURCES.md records where each
came from. Blocks that do not parse (truncated examples) are kept with a
.broken suffix for reference but are not used by tests.

Usage: python3 scripts/extract_refs.py
"""
import html, json, os, re, sys
import xml.dom.minidom

ROOT = os.path.join(os.path.dirname(__file__), "..", "testdata")
RAW = os.path.join(ROOT, "wayback", "raw")
OUT = os.path.join(ROOT, "reference")

def blocks(s):
    out = []
    for m in re.finditer(r'<pre ondblclick[^>]*>(.*?)</pre>', s, re.S):
        code = html.unescape(re.sub(r"<[^>]+>", "", m.group(1))).strip()
        # Labels on the page are unreliable; classify by content.
        head = code[:300]
        if code.startswith(("[", "{", '"')) or code in ("true", "false"):
            label = "json"
        elif "<rss" in head:
            label = "rss"
        elif "<feed" in head:
            label = "atom"
        elif code.startswith("<"):
            label = "xml"
        else:
            label = "unknown"
        if label == "json":
            code = repair_json(code)
            try:
                # The docs also printed "\n" escapes as raw newlines. Parse
                # leniently and re-serialise (key order preserved).
                code = json.dumps(json.loads(code, strict=False), indent=2, ensure_ascii=False)
            except Exception:
                pass
        out.append((label, code))
    return out

def repair_json(code):
    """The docs rendered JSON strings unescaped, so the "source" anchor's
    quotes break parsing. Re-escape quotes inside source values only."""
    def fix(m):
        inner = m.group(2).replace('\\"', '"').replace('"', '\\"')
        return m.group(1) + '"' + inner + '"' + m.group(3)
    code = re.sub(r'("source"\s*:\s*)"(<a .*?<\\?/a>)"(\s*,?\s*$)', fix, code, flags=re.M)
    # Elided items ("...") between array elements.
    code = re.sub(r',\s*\.\.\.[^\n]*\n(\s*[\[{])', r',\n\1', code)
    code = re.sub(r',\s*\.\.\.[^\n\]}]*(\s*[\]}])', r'\1', code)
    code = re.sub(r',(\s*[\]}])', r'\1', code)  # trailing commas left by elision
    return code

def valid(fmt, code):
    try:
        if fmt == "json":
            json.loads(code)
        else:
            xml.dom.minidom.parseString(code.encode())
        return True
    except Exception:
        return False

def main():
    os.makedirs(OUT, exist_ok=True)
    idx_path = os.path.join(ROOT, "wayback", "index.json")
    idx = json.load(open(idx_path)) if os.path.exists(idx_path) else {}
    sources = []
    for f in sorted(os.listdir(RAW)):
        if not f.startswith(("doc-get-", "doc-post-", "doc-GET-", "doc-POST-")):
            continue
        key = f[:-5]
        s = open(os.path.join(RAW, f), encoding="utf-8", errors="replace").read()
        seen = {}
        for label, code in blocks(s):
            if label == "unknown" or not code:
                continue
            name = key.lower().replace("doc-", "", 1)
            n = seen.get(label, 0)
            seen[label] = n + 1
            fname = name + ("" if n == 0 else "-%d" % n) + "." + label
            if not valid(label if label == "json" else "xml", code):
                fname += ".broken"
            open(os.path.join(OUT, fname), "w").write(code + "\n")
            src = idx.get(key, {})
            sources.append("| `%s` | https://web.archive.org/web/%s/%s |" % (fname, src.get("timestamp", "?"), src.get("url", "?")))
            print(fname)
    with open(os.path.join(OUT, "SOURCES.md"), "w") as fh:
        fh.write("# Reference samples\n\nExtracted by `scripts/extract_refs.py` from archived dev.twitter.com pages.\n"
                 "JSON is re-serialised after a lenient parse (the docs showed escapes unescaped); XML is verbatim.\n"
                 "Files ending `.broken` did not parse (truncated doc examples) and are not used by tests.\n\n"
                 "| File | Archived page |\n|---|---|\n")
        fh.write("\n".join(sources) + "\n")

if __name__ == "__main__":
    main()
