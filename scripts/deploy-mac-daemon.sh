#!/bin/bash
# Build + install the Mac daemon locally.
#
# Usage: ./scripts/deploy-mac-daemon.sh
#
# Required env:
#   CRM_MAC_CODESIGN_IDENTITY=<identity>
#       Local self-signed Code Signing certificate to sign with.
#       Using a stable cert keeps the designated requirement constant
#       across rebuilds, which lets FDA grants survive (Contacts
#       grants re-prompt regardless — TCC quirk).
#       Set to "-" to force ad-hoc signing.
#
# Behavior:
#   - First install (no bundle at install path): refuses with a
#     message explaining that pairing requires interactive input
#     (--pi-url / --pair / --hostname). Build is skipped in that
#     case because the operator needs to invoke `crm-mac install`
#     manually anyway.
#   - Upgrade: runs `make mac-daemon` then `crm-mac install --upgrade`
#     from the freshly-built build-dir binary.
#   - Kickstarts the running daemon at the end so any new TCC state
#     takes effect on the next tick.
set -e

if [ -z "${CRM_MAC_CODESIGN_IDENTITY:-}" ]; then
    echo "Error: CRM_MAC_CODESIGN_IDENTITY is not set." >&2
    echo "" >&2
    echo "Export the name of your local self-signed Code Signing" >&2
    echo "certificate, or set it to \"-\" to force ad-hoc signing:" >&2
    echo "" >&2
    echo "  export CRM_MAC_CODESIGN_IDENTITY=\"My Local Code Signing\"" >&2
    echo "  ./scripts/deploy-mac-daemon.sh" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
INSTALL_BUNDLE="$HOME/Library/Application Support/crm-mac/crm-mac.app"
BUILD_BUNDLE="$PROJECT_DIR/mac-daemon/.build/release/crm-mac.app"
BUILD_BINARY="$BUILD_BUNDLE/Contents/MacOS/crm-mac"
DAEMON_LABEL="xyz.spengrah.crm-mac"

cd "$PROJECT_DIR"

echo "=== Mac daemon deploy ==="
echo ""

if [ ! -e "$INSTALL_BUNDLE" ]; then
    echo "No existing install at $INSTALL_BUNDLE."
    echo ""
    echo "First install requires interactive pairing. Mint a token on the Pi:"
    echo "  ssh <pi-host> 'crm-admin --mint-pairing-token'"
    echo ""
    echo "Then build and run install manually:"
    echo "  make mac-daemon"
    echo "  $BUILD_BINARY install \\"
    echo "    --pi-url   <pi-url> \\"
    echo "    --pair     <pairing-token> \\"
    echo "    --hostname <label>"
    exit 1
fi

echo "Existing install detected at $INSTALL_BUNDLE — running upgrade."
echo ""

echo "=== Building ==="
make mac-daemon
echo ""

if [ ! -x "$BUILD_BINARY" ]; then
    echo "Error: build did not produce $BUILD_BINARY" >&2
    exit 1
fi

echo "=== Installing (upgrade) ==="
"$BUILD_BINARY" install --upgrade
echo ""

# `install --upgrade` copies a correctly-substituted bundle into the install
# path, but `SMAppService.register()` reads the CALLING binary's containing-
# bundle plist — which is the build-dir bundle, whose plist still has the
# literal `__INSTALL_PREFIX__` placeholder (it's the operator's install path
# that gets baked in only at install-time-rendering of the install-path
# bundle's plist). launchd then tries to exec the literal placeholder path
# and crashes with EX_CONFIG, with the daemon visibly "Lost" on the Pi.
#
# Defeat the cache by booting out the stale registration and re-registering
# from the INSTALLED binary, whose containing-bundle plist already has the
# real path substituted in.
INSTALLED_BIN="$INSTALL_BUNDLE/Contents/MacOS/crm-mac"
echo "=== Re-registering from installed binary (SMAppService cache workaround) ==="
launchctl bootout "gui/$(id -u)/$DAEMON_LABEL" 2>/dev/null || true
sleep 1
"$INSTALLED_BIN" install --register-only
echo ""

echo "=== Verifying ==="
sleep 2
program="$(launchctl print "gui/$(id -u)/$DAEMON_LABEL" 2>/dev/null | awk -F'= ' '/^\tprogram /{print $2}')"
case "$program" in
    "")
        echo "Daemon: NOT found in launchd" >&2
        exit 1
        ;;
    *__INSTALL_PREFIX__*)
        echo "Daemon: SMAppService still has __INSTALL_PREFIX__ cached at $program" >&2
        echo "(the bootout+register-from-installed dance did not take effect)" >&2
        exit 1
        ;;
    *)
        echo "Daemon registered: $program"
        ;;
esac

echo ""
echo "=== Mac daemon deploy complete ==="
