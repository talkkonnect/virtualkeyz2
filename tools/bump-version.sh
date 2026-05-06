#!/usr/bin/env bash
# Bump SoftwareVersion by 0.01, set SoftwareReleaseUTC to now (UTC), and prepend a changelog line.
# Usage: ./tools/bump-version.sh "Description of this code change"
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
MSG="${*:-Code change}"
if [[ -z "${MSG// /}" ]]; then
  echo "Usage: $0 \"Description of change\"" >&2
  exit 1
fi

export BUMP_ROOT="$ROOT"
export BUMP_MSG="$MSG"

python3 << 'PY'
import os, pathlib, re, datetime

root = pathlib.Path(os.environ["BUMP_ROOT"])
msg = os.environ["BUMP_MSG"].replace("\n", " ").strip()

ver_go = root / "internal" / "app" / "virtualkeyz2.go"
text = ver_go.read_text(encoding="utf-8")
m = re.search(r'SoftwareVersion\s*=\s*"([0-9.]+)"', text)
if not m:
    raise SystemExit("Could not find SoftwareVersion in internal/app/virtualkeyz2.go")
old = m.group(1)
try:
    v = round(float(old) + 0.01, 2)
except ValueError:
    raise SystemExit(f"Bad version: {old!r}")
new_ver = f"{v:.2f}"
now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
text = re.sub(
    r'(SoftwareVersion\s*=\s*")[^"]+(")',
    rf"\g<1>{new_ver}\2",
    text,
    count=1,
)
text = re.sub(
    r'(SoftwareReleaseUTC\s*=\s*")[^"]+(")',
    rf"\g<1>{now}\2",
    text,
    count=1,
)
ver_go.write_text(text, encoding="utf-8")

cl = root / "changelog.txt"
lines = cl.read_text(encoding="utf-8").splitlines(keepends=True)
entry = f"[{new_ver}] {now}  {msg}\n"
i = 0
while i < len(lines) and (lines[i].strip() == "" or lines[i].lstrip().startswith("#")):
    i += 1
lines.insert(i, entry)
cl.write_text("".join(lines), encoding="utf-8")

print(f"Bumped {old!r} -> {new_ver!r}, release {now}")
print(f"Updated {ver_go} and {cl}")
PY
