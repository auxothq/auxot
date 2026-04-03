#!/usr/bin/env bash
# Strip optional outer markdown fence (e.g. ```mdx ... ```) from generated changelog MDX.
# Astro requires frontmatter --- to be the first bytes of the file.
set -euo pipefail
f="${1:?usage: $0 FILE.mdx}"
[[ -f "$f" ]] || exit 0
python3 -c '
import sys
path = sys.argv[1]
with open(path, encoding="utf-8") as fp:
    lines = fp.readlines()
if lines and lines[0].lstrip().startswith("```"):
    lines = lines[1:]
if lines and lines[-1].strip() == "```":
    lines = lines[:-1]
with open(path, "w", encoding="utf-8") as fp:
    fp.writelines(lines)
' "$f"
