#!/bin/bash
# Test systemd service files

set -e

echo "Testing systemd service files..."
echo ""

# Test each service file
for file in infra/personalcrm-*.service infra/personalcrm.target; do
    if [ -f "$file" ]; then
        echo "Validating $file..."
        systemd-analyze verify "$file" || echo "⚠ Warning: $file has issues"
    fi
done

echo ""

# Test installation script
if command -v shellcheck >/dev/null 2>&1; then
    echo "Running shellcheck on install script..."
    shellcheck infra/install-systemd.sh
    echo "✓ Shellcheck passed"
else
    echo "⚠ Warning: shellcheck not installed, skipping script validation"
    echo "  Install with: sudo apt install shellcheck"
fi

echo ""
echo "✓ Validation complete"
