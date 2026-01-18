#!/bin/bash
# Install git hooks by symlinking from src/hooks/ to .git/hooks/
#
# Usage: ./src/hooks/install.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
GIT_HOOKS_DIR="$PROJECT_ROOT/.git/hooks"

if [ ! -d "$GIT_HOOKS_DIR" ]; then
    echo "Error: .git/hooks directory not found. Is this a git repo?" >&2
    exit 1
fi

# Find all hook files (exclude install.sh and README)
for hook in "$SCRIPT_DIR"/*; do
    hook_name="$(basename "$hook")"

    # Skip non-hook files
    case "$hook_name" in
        install.sh|README.md|*.sample)
            continue
            ;;
    esac

    target="$GIT_HOOKS_DIR/$hook_name"

    # Remove existing hook if present
    if [ -e "$target" ] || [ -L "$target" ]; then
        rm "$target"
        echo "Removed existing $hook_name"
    fi

    # Create relative symlink
    ln -s "../../src/hooks/$hook_name" "$target"
    echo "Installed $hook_name"
done

echo "Done."
