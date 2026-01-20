#!/usr/bin/env python3
"""
Extract learnings from Claude Code session transcripts.

Uses headless Claude (Sonnet) to analyze full session context and extract
project-relevant learnings. No separate API key required - uses existing
Claude Code auth.

Usage:
    python extract-learnings.py [--session SESSION_ID] [--dry-run] [--apply]

Hooks:
    Designed to be called from PreCompact, Stop, or git pre-push hooks.
"""

import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
from datetime import datetime
from pathlib import Path
from typing import Iterator


EXTRACTION_PROMPT = """\
Analyze this session transcript to extract learnings that will help future agents work better on this project.

Quality bar: Only extract insights that would meaningfully change how a future agent approaches work.

EXTRACT:
- **Project gotchas** - Non-obvious things that would trip up a new agent
- **Critical distinctions** - Important differences that affect decisions (e.g., "use X not Y for this")
- **Architectural insights** - How the system works in ways the code doesn't make obvious
- **Discovered rules** - Mistakes that should become rules to prevent recurrence
- **Documentation gaps** - Places where existing rules/guides/hints were NOT followed, leading to wasted effort. This reveals docs that need strengthening, clarifying, or promoting.

SKIP:
- General programming techniques (obvious to any competent agent)
- Implementation details specific to this session
- Things obvious from reading the code or docs
- Generic truisms ("consistency matters", "test before committing")

For documentation gaps, note:
- What guidance existed but wasn't followed?
- Why might it have been missed? (buried, unclear, too weak)
- How should it be strengthened?

Ask: "Would a future agent benefit from knowing this BEFORE they encounter it?"
If the answer is "they'd figure it out quickly anyway" - skip it.

Return learnings as JSON. If nothing meets the bar, return an empty array.
"""

LEARNINGS_SCHEMA = {
    "type": "object",
    "properties": {
        "learnings": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "type": {
                        "type": "string",
                        "enum": ["gotcha", "distinction", "architecture", "rule", "documentation_gap"]
                    },
                    "summary": {
                        "type": "string",
                        "description": "One-line summary of the learning"
                    },
                    "detail": {
                        "type": "string",
                        "description": "Fuller explanation with context"
                    },
                    "actionable": {
                        "type": "boolean",
                        "description": "Whether this can be applied now"
                    }
                },
                "required": ["type", "summary", "detail", "actionable"]
            }
        }
    },
    "required": ["learnings"]
}


def get_project_sessions_path() -> Path:
    """Convert current working directory to Claude Code's session storage path."""
    cwd = os.getcwd()
    project_dir_name = cwd.replace("/", "-")
    if not project_dir_name.startswith("-"):
        project_dir_name = "-" + project_dir_name
    return Path.home() / ".claude" / "projects" / project_dir_name


def get_extraction_state_path() -> Path:
    """Get path to extraction state file."""
    return Path(".ai/log/extraction-state.json")


def load_extraction_state(session_id: str) -> dict | None:
    """Load extraction state for a session. Returns None if no prior extraction."""
    state_path = get_extraction_state_path()
    if not state_path.exists():
        return None
    try:
        with open(state_path) as f:
            state = json.load(f)
        if state.get("session_id") == session_id:
            return state
    except (json.JSONDecodeError, IOError):
        pass
    return None


def save_extraction_state(session_id: str, last_timestamp: str, last_line: int) -> None:
    """Save extraction state for incremental extraction."""
    state_path = get_extraction_state_path()
    state_path.parent.mkdir(parents=True, exist_ok=True)
    state = {
        "session_id": session_id,
        "last_extracted_timestamp": last_timestamp,
        "last_extracted_line": last_line,
    }
    with open(state_path, "w") as f:
        json.dump(state, f, indent=2)


def iter_session_files(sessions_path: Path, session_id: str | None = None) -> Iterator[Path]:
    """Iterate over session JSONL files, optionally filtering by session ID."""
    if not sessions_path.exists():
        return
    pattern = f"{session_id}.jsonl" if session_id else "*.jsonl"
    yield from sorted(sessions_path.glob(pattern), key=lambda p: p.stat().st_mtime, reverse=True)


def format_content_block(block: dict) -> str:
    """Format a single content block for the transcript."""
    block_type = block.get("type", "unknown")

    if block_type == "text":
        return block.get("text", "")

    elif block_type == "thinking":
        thinking = block.get("thinking", "")
        return f"<thinking>\n{thinking}\n</thinking>"

    elif block_type == "tool_use":
        tool_name = block.get("name", "unknown")
        tool_input = block.get("input", {})
        # Truncate large inputs
        input_str = json.dumps(tool_input, indent=2)
        if len(input_str) > 500:
            input_str = input_str[:500] + "\n... [truncated]"
        return f"<tool_use name=\"{tool_name}\">\n{input_str}\n</tool_use>"

    elif block_type == "tool_result":
        content = block.get("content", "")
        if isinstance(content, list):
            content = "\n".join(
                item.get("text", str(item)) if isinstance(item, dict) else str(item)
                for item in content
            )
        is_error = block.get("is_error", False)
        tag = "tool_error" if is_error else "tool_result"
        # Truncate very long results
        if len(str(content)) > 1000:
            content = str(content)[:1000] + "\n... [truncated]"
        return f"<{tag}>\n{content}\n</{tag}>"

    else:
        return f"<{block_type}>{json.dumps(block)}</{block_type}>"


def format_message(entry: dict) -> str | None:
    """Format a JSONL entry into readable transcript format."""
    entry_type = entry.get("type")
    message = entry.get("message", {})
    timestamp = entry.get("timestamp", "")

    if entry_type == "user":
        content = message.get("content", "")
        if isinstance(content, list):
            parts = []
            for block in content:
                if isinstance(block, dict):
                    if block.get("type") == "text":
                        parts.append(block.get("text", ""))
                    elif block.get("type") == "tool_result":
                        parts.append(format_content_block(block))
                else:
                    parts.append(str(block))
            content = "\n".join(parts)
        return f"## User [{timestamp}]\n\n{content}"

    elif entry_type == "assistant":
        content = message.get("content", [])
        model = message.get("model", "unknown")

        if isinstance(content, str):
            return f"## Assistant [{model}] [{timestamp}]\n\n{content}"

        parts = []
        for block in content:
            if isinstance(block, dict):
                formatted = format_content_block(block)
                if formatted:
                    parts.append(formatted)

        if parts:
            return f"## Assistant [{model}] [{timestamp}]\n\n" + "\n\n".join(parts)

    elif entry_type == "summary":
        summary = entry.get("summary", "")
        return f"## Summary [{timestamp}]\n\n{summary}"

    return None


def session_to_transcript(
    session_file: Path,
    max_chars: int = 200000,
    after_timestamp: str | None = None,
    after_line: int = 0,
) -> tuple[str, str | None, int]:
    """Convert a session JSONL file to a readable transcript.

    Args:
        session_file: Path to session JSONL file
        max_chars: Maximum characters in transcript
        after_timestamp: Only include entries after this timestamp (for incremental)
        after_line: Skip this many lines (for incremental)

    Returns:
        Tuple of (transcript, last_timestamp, last_line_number)
    """
    parts = []
    total_chars = 0
    last_timestamp = None
    line_number = 0
    included_count = 0

    with open(session_file) as f:
        for line in f:
            line_number += 1

            # Skip lines we've already processed
            if line_number <= after_line:
                continue

            try:
                entry = json.loads(line.strip())
            except json.JSONDecodeError:
                continue

            # Track timestamp for state
            entry_timestamp = entry.get("timestamp")
            if entry_timestamp:
                last_timestamp = entry_timestamp

            # Skip entries before our cutoff timestamp (belt and suspenders with line number)
            if after_timestamp and entry_timestamp and entry_timestamp <= after_timestamp:
                continue

            formatted = format_message(entry)
            if formatted:
                if total_chars + len(formatted) > max_chars:
                    parts.append("\n... [transcript truncated due to length]")
                    break
                parts.append(formatted)
                total_chars += len(formatted)
                included_count += 1

    transcript = "\n\n---\n\n".join(parts) if parts else ""
    return transcript, last_timestamp, line_number


def find_claude_cli() -> str:
    """Find the claude CLI executable."""
    # Check common locations
    candidates = [
        Path.home() / ".claude" / "local" / "claude",
        Path("/usr/local/bin/claude"),
        Path("/opt/homebrew/bin/claude"),
    ]

    for candidate in candidates:
        if candidate.exists() and candidate.is_file():
            return str(candidate)

    # Fall back to PATH lookup
    import shutil
    claude_path = shutil.which("claude")
    if claude_path:
        return claude_path

    raise FileNotFoundError(
        "Could not find claude CLI. Expected at ~/.claude/local/claude or in PATH."
    )


def extract_with_claude(transcript_path: Path, session_id: str, model: str = "claude-sonnet-4-5-20250929") -> str:
    """
    Spawn headless Claude to extract learnings from transcript.

    Uses the user's existing Claude Code auth - no separate API key needed.
    MCP servers are disabled via --strict-mcp-config to reduce startup overhead.
    """
    claude_cli = find_claude_cli()
    prompt = f"Read the session transcript at {transcript_path} and extract learnings."

    cmd = [
        claude_cli, "-p", prompt,
        "--model", model,
        "--append-system-prompt", EXTRACTION_PROMPT,
        "--allowedTools", "Read",
        "--output-format", "json",
        "--json-schema", json.dumps(LEARNINGS_SCHEMA),
        "--no-session-persistence",
        "--mcp-config", '{"mcpServers":{}}',  # Empty config - no MCP servers
        "--strict-mcp-config",  # Ignore project MCP configs
    ]

    result = subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        timeout=300,  # 5 minute timeout
    )

    if result.returncode != 0:
        raise RuntimeError(f"Claude extraction failed: {result.stderr}")

    return result.stdout.strip()


def parse_learnings(raw_output: str) -> list[dict] | None:
    """Parse JSON learnings from Claude's output."""
    content = raw_output.strip()

    # With --output-format json and --json-schema, output has "structured_output" field
    try:
        output = json.loads(content)

        # Handle --output-format json wrapper with --json-schema
        if isinstance(output, dict) and "structured_output" in output:
            structured = output["structured_output"]
            if isinstance(structured, dict) and "learnings" in structured:
                learnings = structured["learnings"]
                if isinstance(learnings, list):
                    if len(learnings) == 0:
                        return None  # No learnings
                    return learnings

        # Fallback: check "result" field (older format)
        if isinstance(output, dict) and "result" in output:
            result = output["result"]
            if isinstance(result, dict) and "learnings" in result:
                learnings = result["learnings"]
                if isinstance(learnings, list):
                    if len(learnings) == 0:
                        return None
                    return learnings

    except json.JSONDecodeError:
        pass

    # Return raw if parsing fails
    return [{"type": "raw", "content": raw_output}]


def get_session_log_path(session_id: str) -> Path:
    """Get the per-session learnings file path."""
    return Path(".ai/log/learnings") / f"{session_id}.yaml"


def load_existing_learnings(log_path: Path) -> set[str]:
    """Load existing summaries from log for deduplication."""
    existing = set()
    if not log_path.exists():
        return existing

    try:
        import yaml
        with open(log_path) as f:
            for doc in yaml.safe_load_all(f):
                if doc and isinstance(doc, dict):
                    summary = doc.get("summary", "")
                    if summary:
                        existing.add(summary)
    except Exception:
        pass  # If we can't read existing log, proceed without dedupe

    return existing


def append_to_log(learnings: list[dict], log_path: Path) -> int:
    """Append learnings to the session log file, deduplicating by summary.

    Returns the number of new learnings appended.
    """
    log_path.parent.mkdir(parents=True, exist_ok=True)

    # Load existing learnings for deduplication
    existing = load_existing_learnings(log_path)

    timestamp = datetime.now().isoformat()
    new_count = 0

    with open(log_path, "a") as f:
        for learning in learnings:
            summary = learning.get("summary", "")

            # Skip duplicates
            if summary in existing:
                continue

            entry = {
                "timestamp": timestamp,
                **learning
            }
            # Write as YAML document
            f.write("---\n")
            for key, value in entry.items():
                if isinstance(value, str) and ("\n" in value or key == "detail"):
                    # Multi-line or detail field: use block scalar for readability
                    f.write(f"{key}: |\n")
                    for line in value.split("\n"):
                        f.write(f"  {line}\n")
                elif isinstance(value, str) and (":" in value or value.startswith(("'", '"', "[", "{", "-", "*", "&", "!", "|", ">", "%", "@", "`"))):
                    # String with special chars: quote it
                    escaped = value.replace("\\", "\\\\").replace('"', '\\"')
                    f.write(f'{key}: "{escaped}"\n')
                elif isinstance(value, bool):
                    f.write(f"{key}: {str(value).lower()}\n")
                else:
                    f.write(f"{key}: {value}\n")
            f.write("\n")
            new_count += 1

    return new_count


def main():
    parser = argparse.ArgumentParser(
        description="Extract learnings from Claude Code session transcripts."
    )
    parser.add_argument(
        "--session", "-s",
        help="Specific session ID (default: latest session)"
    )
    parser.add_argument(
        "--dry-run", "-n",
        action="store_true",
        help="Print transcript without extracting"
    )
    parser.add_argument(
        "--model", "-m",
        default="claude-sonnet-4-5-20250929",
        help="Model for extraction (default: sonnet)"
    )
    parser.add_argument(
        "--output", "-o",
        help="Output file (default: .ai/log/learnings.yaml)"
    )
    parser.add_argument(
        "--apply",
        action="store_true",
        help="Apply actionable learnings to project files"
    )
    parser.add_argument(
        "--recommend",
        action="store_true",
        help="Print recommendations without applying"
    )
    parser.add_argument(
        "--full",
        action="store_true",
        help="Force full extraction, ignoring incremental state"
    )

    args = parser.parse_args()

    sessions_path = get_project_sessions_path()

    if not sessions_path.exists():
        print(f"No sessions found at: {sessions_path}", file=sys.stderr)
        sys.exit(1)

    session_files = list(iter_session_files(sessions_path, args.session))

    if not session_files:
        print("No session files found.", file=sys.stderr)
        sys.exit(1)

    # Process latest session only
    session_file = session_files[0]
    session_id = session_file.stem

    print(f"Processing session: {session_id}...", file=sys.stderr)

    # Load extraction state for incremental extraction (unless --full)
    if args.full:
        state = None
        print("Full extraction (--full flag)...", file=sys.stderr)
    else:
        state = load_extraction_state(session_id)
        if state:
            print(f"Incremental extraction from line {state.get('last_extracted_line', 0)}...", file=sys.stderr)
        else:
            print("Full extraction (no prior state)...", file=sys.stderr)

    after_timestamp = state.get("last_extracted_timestamp") if state else None
    after_line = state.get("last_extracted_line", 0) if state else 0

    # Convert session to transcript (incremental if state exists)
    transcript, last_timestamp, last_line = session_to_transcript(
        session_file,
        after_timestamp=after_timestamp,
        after_line=after_line,
    )

    if not transcript.strip():
        print("No new content since last extraction.", file=sys.stderr)
        sys.exit(0)

    if args.dry_run:
        print(transcript)
        sys.exit(0)

    # Write transcript to project temp file (must be in project dir for headless Claude access)
    transcript_path = Path(".ai/log/.transcript-temp.md")
    transcript_path.parent.mkdir(parents=True, exist_ok=True)
    transcript_path.write_text(transcript)

    try:
        # Extract learnings with headless Claude
        print("Extracting learnings with Claude...", file=sys.stderr)
        raw_output = extract_with_claude(transcript_path, session_id, args.model)

        learnings = parse_learnings(raw_output)

        if learnings is None:
            print("No significant learnings from this session.", file=sys.stderr)
            sys.exit(0)

        # Output learnings
        if args.recommend or not args.apply:
            print("\n# Extracted Learnings\n")
            for learning in learnings:
                print(f"## {learning.get('type', 'unknown').upper()}: {learning.get('summary', 'No summary')}")
                print(f"\n{learning.get('detail', '')}")
                print(f"\n**Actionable:** {'Yes' if learning.get('actionable') else 'No'}")
                print()

        # Append to log (with deduplication)
        log_path = Path(args.output) if args.output else get_session_log_path(session_id)
        new_count = append_to_log(learnings, log_path)
        skipped = len(learnings) - new_count
        if new_count > 0:
            print(f"Appended {new_count} new learnings to: {log_path}", file=sys.stderr)
        if skipped > 0:
            print(f"Skipped {skipped} duplicate learnings.", file=sys.stderr)

        # Save extraction state for incremental extraction next time
        if last_timestamp:
            save_extraction_state(session_id, last_timestamp, last_line)
            print(f"Saved extraction state (line {last_line}).", file=sys.stderr)

        # Apply if requested
        if args.apply:
            print("\n--apply not yet implemented. Use recommendations above.", file=sys.stderr)

    finally:
        # Clean up temp file
        transcript_path.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
