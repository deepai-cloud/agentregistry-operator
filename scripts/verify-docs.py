#!/usr/bin/env python3
"""Catch broken local documentation links without contacting external websites."""
from pathlib import Path
import re
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
files = [ROOT / "README.md", ROOT / "charts/agentregistry-operator/README.md"]
files += list((ROOT / "docs").rglob("*.md")) + list((ROOT / "examples").rglob("*.md"))
errors = []
for document in files:
    for target in re.findall(r"\]\(([^)]+)\)", document.read_text()):
        target = target.split(' "', 1)[0].strip("<>")
        url = urlsplit(target)
        if url.scheme or url.netloc or not url.path:
            continue
        if not (document.parent / unquote(url.path)).exists():
            errors.append(f"{document.relative_to(ROOT)}: missing link {target}")
if errors:
    raise SystemExit("\n".join(errors))
print(f"Documentation links verified in {len(files)} files")
