#!/usr/bin/env bash
# Runner flavor catalog: one directory per flavor under images/, each with a
# flavor.json ({"label", "base", "default"?}) and optionally its own Dockerfile.
#   flavors.sh list                       flavor names, default first
#   flavors.sh matrix                     JSON array for the CI build matrix
#   flavors.sh get <flavor> <key>         base | label | dockerfile | default
# IMAGES_DIR overrides the catalog directory (tests). Needs python3.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
IMAGES_DIR="${IMAGES_DIR:-$ROOT/images}"
export IMAGES_DIR
exec python3 - "$@" <<'PY'
import json, os, re, sys

images = os.environ["IMAGES_DIR"]
name_re = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$")  # same rule as the controller

def load():
    flavors = []
    for entry in sorted(os.listdir(images)):
        path = os.path.join(images, entry, "flavor.json")
        if not os.path.isfile(path):
            continue
        if not name_re.match(entry):
            sys.exit(f"flavors: {entry}: name must match {name_re.pattern}")
        with open(path) as f:
            try:
                meta = json.load(f)
            except ValueError as e:
                sys.exit(f"flavors: {path}: {e}")
        for key in ("label", "base"):
            if not isinstance(meta.get(key), str) or not meta[key]:
                sys.exit(f"flavors: {path}: missing \"{key}\"")
        own = os.path.isfile(os.path.join(images, entry, "Dockerfile"))
        flavors.append({
            "name": f"runner-{entry}", "image": "runner", "flavor": entry, "label": meta["label"],
            "base": meta["base"], "dockerfile": f"images/{entry}/Dockerfile" if own else "images/Dockerfile",
            "default": bool(meta.get("default", False)), "tag_prefix": f"{entry}-",
        })
    defaults = [f for f in flavors if f["default"]]
    if len(defaults) != 1:
        sys.exit(f"flavors: exactly one flavor must have \"default\": true (found {len(defaults)})")
    flavors.sort(key=lambda f: (not f["default"], f["flavor"]))
    return flavors

args = sys.argv[1:]
cmd = args[0] if args else "list"
flavors = load()
if cmd == "list":
    print("\n".join(f["flavor"] for f in flavors))
elif cmd == "matrix":
    print(json.dumps(flavors))
elif cmd == "get" and len(args) == 3:
    match = [f for f in flavors if f["flavor"] == args[1]]
    if not match:
        sys.exit(f"flavors: no flavor named {args[1]}")
    value = match[0].get(args[2])
    if value is None:
        sys.exit(f"flavors: unknown key {args[2]}")
    print(json.dumps(value) if isinstance(value, bool) else value)
else:
    sys.exit("usage: flavors.sh list | matrix | get <flavor> <base|label|dockerfile|default>")
PY
