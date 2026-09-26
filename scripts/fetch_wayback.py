#!/usr/bin/env python3
"""Fetch archived 2009-2010 Twitter API docs from the Wayback Machine.

For each apiwiki method page, pick the latest 200 snapshot, download the raw
page (id_ flag), and extract <pre> example blocks into testdata/wayback/.
Usage: python3 scripts/fetch_wayback.py
"""
import html, json, os, re, sys, time, urllib.parse, urllib.request

OUT = os.path.join(os.path.dirname(__file__), "..", "testdata", "wayback")
RAW = os.path.join(OUT, "raw")
PATTERNS = [
    "apiwiki.twitter.com/Twitter-REST-API-Method*",
    "apiwiki.twitter.com/Twitter-Search-API-Method*",
    "apiwiki.twitter.com/Return-Values*",
    "apiwiki.twitter.com/HTTP-Response-Codes-and-Errors*",
    "apiwiki.twitter.com/Rate-limiting*",
    "apiwiki.twitter.com/Authentication*",
    "apiwiki.twitter.com/OAuth-FAQ*",
    "apiwiki.twitter.com/Things-Every-Developer-Should-Know*",
]

def get(url, tries=6):
    for i in range(tries):
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "mockingbird-research/1.0"})
            with urllib.request.urlopen(req, timeout=60) as r:
                b = r.read()
            if b"Temporarily Offline" in b[:2000]:
                raise IOError("archive offline")
            return b
        except Exception as e:
            print("  retry", i, url[:100], e, file=sys.stderr)
            time.sleep(3 + i * 4)
    return None

def slug(u):
    p = urllib.parse.unquote(urllib.parse.urlsplit(u).path).strip("/")
    p = p.replace(" ", "-").replace(":", "").replace(" ", "-")
    return re.sub(r"[^A-Za-z0-9._-]+", "-", p) or "index"

def main():
    os.makedirs(RAW, exist_ok=True)
    latest = {}
    for pat in PATTERNS:
        cdx = "https://web.archive.org/cdx/search/cdx?" + urllib.parse.urlencode(
            {"url": pat, "output": "json", "filter": "statuscode:200", "fl": "original,timestamp"})
        b = get(cdx)
        if not b:
            print("cdx failed", pat, file=sys.stderr); continue
        rows = json.loads(b or b"[]")[1:]
        for orig, ts in rows:
            if "SearchFor=" in orig or "%E2%97%8F" in orig:
                continue
            key = slug(orig.split("?")[0])
            if "mode=print" in orig:  # prefer print views, they have less chrome
                ts = ts + "p"
            if key not in latest or ts > latest[key][1]:
                latest[key] = (orig, ts)
    index = {}
    for key, (orig, ts) in sorted(latest.items()):
        dest = os.path.join(RAW, key + ".html")
        if not os.path.exists(dest):
            b = get("https://web.archive.org/web/%sid_/%s" % (ts.rstrip("p"), orig))
            if not b:
                continue
            open(dest, "wb").write(b)
        s = open(dest, encoding="utf-8", errors="replace").read()
        pres = [html.unescape(re.sub(r"<[^>]+>", "", p)) for p in re.findall(r"<pre[^>]*>(.*?)</pre>", s, re.S)]
        text = html.unescape(re.sub(r"<[^>]+>", "", re.sub(r"<script.*?</script>|<style.*?</style>", "", s, flags=re.S)))
        text = re.sub(r"\n\s*\n+", "\n", text)
        open(os.path.join(OUT, key + ".txt"), "w").write(text)
        if pres:
            open(os.path.join(OUT, key + ".pre.txt"), "w").write("\n\n=====\n\n".join(pres))
        index[key] = {"url": orig, "timestamp": ts.rstrip("p"), "examples": len(pres)}
        print(key, ts, len(pres))
    json.dump(index, open(os.path.join(OUT, "index.json"), "w"), indent=1, sort_keys=True)

if __name__ == "__main__":
    main()
