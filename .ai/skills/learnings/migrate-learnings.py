#!/usr/bin/env python3
"""
Migrate learnings from monolithic learnings.yaml to per-session files.

Reads the existing .ai/log/learnings.yaml and splits it into
.ai/log/learnings/{session_id}.yaml files, removing the session_id
field from each document since it's now implicit in the filename.

Usage:
    python migrate-learnings.py [--dry-run] [--delete-old]
"""

import argparse
import sys
from collections import defaultdict
from pathlib import Path


def migrate_learnings(dry_run: bool = False, delete_old: bool = False) -> None:
    """Migrate monolithic learnings.yaml to per-session files."""
    import yaml

    old_path = Path(".ai/log/learnings.yaml")
    new_dir = Path(".ai/log/learnings")

    if not old_path.exists():
        print(f"No existing learnings file at {old_path}", file=sys.stderr)
        sys.exit(0)

    # Group learnings by session_id
    sessions: dict[str, list[dict]] = defaultdict(list)

    with open(old_path) as f:
        for doc in yaml.safe_load_all(f):
            if doc and isinstance(doc, dict):
                session_id = doc.pop("session_id", "unknown")
                sessions[session_id].append(doc)

    print(f"Found {sum(len(v) for v in sessions.values())} learnings across {len(sessions)} sessions")

    if dry_run:
        print("\n[DRY RUN] Would create:")
        for session_id, learnings in sessions.items():
            print(f"  {new_dir / f'{session_id}.yaml'} ({len(learnings)} learnings)")
        return

    # Create the learnings directory
    new_dir.mkdir(parents=True, exist_ok=True)

    # Write per-session files
    for session_id, learnings in sessions.items():
        session_path = new_dir / f"{session_id}.yaml"

        with open(session_path, "w") as f:
            for learning in learnings:
                f.write("---\n")
                for key, value in learning.items():
                    if isinstance(value, str) and ("\n" in value or key == "detail"):
                        f.write(f"{key}: |\n")
                        for line in value.split("\n"):
                            f.write(f"  {line}\n")
                    elif isinstance(value, str) and (":" in value or value.startswith(("'", '"', "[", "{", "-", "*", "&", "!", "|", ">", "%", "@", "`"))):
                        escaped = value.replace("\\", "\\\\").replace('"', '\\"')
                        f.write(f'{key}: "{escaped}"\n')
                    elif isinstance(value, bool):
                        f.write(f"{key}: {str(value).lower()}\n")
                    else:
                        f.write(f"{key}: {value}\n")
                f.write("\n")

        print(f"Created {session_path} ({len(learnings)} learnings)")

    if delete_old:
        old_path.unlink()
        print(f"\nDeleted old file: {old_path}")
    else:
        print(f"\nOld file preserved at: {old_path}")
        print("Run with --delete-old to remove it")


def main():
    parser = argparse.ArgumentParser(
        description="Migrate learnings from monolithic file to per-session files."
    )
    parser.add_argument(
        "--dry-run", "-n",
        action="store_true",
        help="Show what would be done without making changes"
    )
    parser.add_argument(
        "--delete-old",
        action="store_true",
        help="Delete the old learnings.yaml after migration"
    )

    args = parser.parse_args()
    migrate_learnings(dry_run=args.dry_run, delete_old=args.delete_old)


if __name__ == "__main__":
    main()
