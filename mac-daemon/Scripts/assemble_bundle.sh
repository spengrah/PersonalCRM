#!/usr/bin/env bash
# Assemble crm-mac.app from a built Mach-O + Info.plist source.
#
# Usage: assemble_bundle.sh <macho_path> <bundle_path> <info_plist_source>
#
# Inputs:
#   - <macho_path>        Path to the release Mach-O (e.g. .build/release/crm-mac).
#   - <bundle_path>       Final bundle path (e.g. .build/release/crm-mac.app).
#                         If present, removed and replaced.
#   - <info_plist_source> Path to the canonical Info.plist
#                         (mac-daemon/Sources/crm-mac/Info.plist).
#
# Optional:
#   CRM_MAC_CODESIGN_IDENTITY=<identity>
#       Sign the bundle with a local codesigning identity instead of
#       ad-hoc signing. **Local self-signed Code Signing certificate
#       only** — the script unconditionally appends --timestamp=none
#       whenever this is set, which would silently strip the trusted
#       timestamp from a real Apple Developer ID signature. A stable
#       self-signed cert keeps the designated requirement stable
#       across rebuilds, which lets FDA grants survive local
#       development rebuilds (Contacts grants still re-prompt — TCC
#       Contacts subsystem binds to CDHash regardless of DR).
#
#   CRM_BUILD_SHA=<git-sha>
#       If non-empty, stamp the assembled bundle's Contents/Info.plist
#       with a CRMBuildSHA key carrying this value, BEFORE the codesign
#       pass (so the key is sealed). `make mac-daemon` sets this to
#       `git rev-parse HEAD`; the mac-deploy reconcile flow reads the
#       key back from the installed bundle to decide whether a rebuild
#       is needed. When unset, no key is written (the byte-identity
#       parity test relies on this to stay deterministic).
#
# Output: a fully-assembled, codesigned crm-mac.app bundle at
# <bundle_path> with:
#
#   crm-mac.app/Contents/
#     Info.plist                          <- byte-identical copy of <info_plist_source>
#     MacOS/crm-mac                       <- copy of <macho_path>, chmod +x
#     Library/LaunchAgents/
#       xyz.spengrah.crm-mac.plist        <- LaunchAgent plist with
#                                            __INSTALL_PREFIX__ placeholder
#
# The LaunchAgents plist uses __INSTALL_PREFIX__ as a placeholder for
# the install-time bundle path (the operator's
# `~/Library/Application Support/crm-mac/crm-mac.app`). At install
# time the Swift installer substitutes the placeholder with the real
# install path before SMAppService registers the bundle. The
# build-time artifact is never registered with launchd from the
# .build/release/ directory itself.
#
# Logs + config-dir paths in the LaunchAgent plist use the build-time
# $HOME directly; they're never read by macOS from the build artifact.
#
# This script intentionally uses ONLY tools that ship with the Xcode
# Command Line Tools (no full Xcode required): mkdir, cp, chmod,
# plutil, codesign. No xcodebuild, no swiftc invocation, no lipo.
#
# Two-pass codesign:
#   1. Inner Mach-O signed with explicit --identifier xyz.spengrah.crm-mac.
#   2. Outer bundle seal with the same identifier (no --deep; the
#      inner binary was signed first).
#
# Verifications:
#   - codesign --verify --strict --deep on the bundle (--deep is
#     Apple-recommended for VERIFICATION only).
#   - Inner Mach-O Identifier= line matches xyz.spengrah.crm-mac.
#   - Outer bundle Identifier= line matches xyz.spengrah.crm-mac.
#   - Certificate-backed signatures do not fall back to a cdhash DR.
set -euo pipefail

if [ "$#" -ne 3 ]; then
    echo "usage: $0 <macho_path> <bundle_path> <info_plist_source>" >&2
    exit 1
fi

MACHO_PATH="$1"
BUNDLE_PATH="$2"
INFO_PLIST_SOURCE="$3"
BUNDLE_IDENTIFIER="xyz.spengrah.crm-mac"
LAUNCH_AGENTS_PLIST_NAME="${BUNDLE_IDENTIFIER}.plist"
CODE_SIGN_IDENTITY="${CRM_MAC_CODESIGN_IDENTITY:-}"
if [ "${CODE_SIGN_IDENTITY}" = "-" ]; then
    CODE_SIGN_IDENTITY=""
fi

if [ -n "${CODE_SIGN_IDENTITY}" ]; then
    CODESIGN_SIGN_ARGS=(--sign "${CODE_SIGN_IDENTITY}" --timestamp=none)
else
    CODESIGN_SIGN_ARGS=(--sign -)
fi

# Defensive: refuse if no Xcode/CLT is selected. xcode-select -p prints
# the active developer dir on stdout; we only care that it is non-empty.
# Either CLT (/Library/Developer/CommandLineTools) or a full Xcode path
# is fine — the tools we need (codesign, plutil) ship in both.
XCODE_SELECT_PATH="$(xcode-select -p 2>/dev/null || true)"
if [ -z "${XCODE_SELECT_PATH}" ]; then
    echo "FAIL: xcode-select -p returned empty. Install Xcode Command Line Tools (xcode-select --install) or set DEVELOPER_DIR." >&2
    exit 1
fi

if [ ! -x "${MACHO_PATH}" ]; then
    echo "FAIL: Mach-O input '${MACHO_PATH}' does not exist or is not executable" >&2
    exit 1
fi

if [ ! -f "${INFO_PLIST_SOURCE}" ]; then
    echo "FAIL: Info.plist source '${INFO_PLIST_SOURCE}' does not exist" >&2
    exit 1
fi

# Idempotent rebuild: nuke any leftover bundle from a prior run.
rm -rf "${BUNDLE_PATH}"
mkdir -p "${BUNDLE_PATH}/Contents/MacOS" \
    "${BUNDLE_PATH}/Contents/Library/LaunchAgents"

# Stage the Mach-O.
cp "${MACHO_PATH}" "${BUNDLE_PATH}/Contents/MacOS/crm-mac"
chmod +x "${BUNDLE_PATH}/Contents/MacOS/crm-mac"

# Copy Info.plist verbatim from the source tree. This is byte-identical
# to the source file by construction (cp(1) preserves bytes).
cp "${INFO_PLIST_SOURCE}" "${BUNDLE_PATH}/Contents/Info.plist"

# Optionally stamp the build's git SHA into Contents/Info.plist. Inserted
# here, BEFORE the codesign pass below, so the key is under the seal. Uses
# `plutil -replace` (inserts if absent, replaces if present — idempotent;
# unlike `-insert`, which errors on an existing key). When CRM_BUILD_SHA is
# unset/empty the plist is left byte-identical to the source so the
# byte-identity parity test stays deterministic.
if [ -n "${CRM_BUILD_SHA:-}" ]; then
    plutil -replace CRMBuildSHA -string "${CRM_BUILD_SHA}" \
        "${BUNDLE_PATH}/Contents/Info.plist"
fi

# Write the LaunchAgents plist. The binary path contains the
# __INSTALL_PREFIX__ placeholder; the Swift installer substitutes the
# real install-time bundle path at install time. The logs / config-dir
# values use the build-time $HOME directly — the build-time artifact
# is never loaded into launchd from .build/release/ so these values
# are advisory.
BUILD_HOME="${HOME:-/Users/runner}"
cat > "${BUNDLE_PATH}/Contents/Library/LaunchAgents/${LAUNCH_AGENTS_PLIST_NAME}" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${BUNDLE_IDENTIFIER}</string>
    <key>ProgramArguments</key>
    <array>
        <string>__INSTALL_PREFIX__/Contents/MacOS/crm-mac</string>
        <string>daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>Crashed</key>
        <true/>
    </dict>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>${BUILD_HOME}/Library/Logs/crm-mac/stdout.log</string>
    <key>StandardErrorPath</key>
    <string>${BUILD_HOME}/Library/Logs/crm-mac/stderr.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>CRM_MAC_CONFIG_DIR</key>
        <string>${BUILD_HOME}/Library/Application Support/crm-mac</string>
    </dict>
</dict>
</plist>
EOF

# Lint both plists. plutil refuses to lint a malformed plist and exits
# non-zero; -euo pipefail at the top makes that fatal.
plutil -lint "${BUNDLE_PATH}/Contents/Info.plist" >/dev/null
plutil -lint "${BUNDLE_PATH}/Contents/Library/LaunchAgents/${LAUNCH_AGENTS_PLIST_NAME}" >/dev/null

# Two-pass codesign.
codesign --force "${CODESIGN_SIGN_ARGS[@]}" \
    --identifier "${BUNDLE_IDENTIFIER}" \
    "${BUNDLE_PATH}/Contents/MacOS/crm-mac"
codesign --force "${CODESIGN_SIGN_ARGS[@]}" \
    --identifier "${BUNDLE_IDENTIFIER}" \
    "${BUNDLE_PATH}"

# Triple verification.
codesign --verify --strict --deep "${BUNDLE_PATH}"
inner_identifier="$(codesign --display --verbose=2 "${BUNDLE_PATH}/Contents/MacOS/crm-mac" 2>&1 | awk -F= '/^Identifier=/ {print $2}')"
if [ "${inner_identifier}" != "${BUNDLE_IDENTIFIER}" ]; then
    echo "FAIL: inner Mach-O Identifier=${inner_identifier}; expected ${BUNDLE_IDENTIFIER}" >&2
    exit 1
fi
outer_identifier="$(codesign --display --verbose=2 "${BUNDLE_PATH}" 2>&1 | awk -F= '/^Identifier=/ {print $2}')"
if [ "${outer_identifier}" != "${BUNDLE_IDENTIFIER}" ]; then
    echo "FAIL: outer bundle Identifier=${outer_identifier}; expected ${BUNDLE_IDENTIFIER}" >&2
    exit 1
fi

inner_requirement="$(codesign --display -r - "${BUNDLE_PATH}/Contents/MacOS/crm-mac" 2>&1 | sed -n -e 's/^designated => //p' -e 's/^# designated => //p')"
outer_requirement="$(codesign --display -r - "${BUNDLE_PATH}" 2>&1 | sed -n -e 's/^designated => //p' -e 's/^# designated => //p')"

# Fail loudly if the designated-requirement line couldn't be parsed at
# all. An empty value would silently bypass the cdhash check below.
if [ -z "${inner_requirement}" ] || [ -z "${outer_requirement}" ]; then
    echo "FAIL: could not parse designated requirement from codesign output" >&2
    echo "inner: ${inner_requirement}" >&2
    echo "outer: ${outer_requirement}" >&2
    exit 1
fi

if [ -n "${CODE_SIGN_IDENTITY}" ]; then
    if [[ "${inner_requirement}" == *cdhash* ]] || [[ "${outer_requirement}" == *cdhash* ]]; then
        echo "FAIL: certificate-backed signing produced a cdhash designated requirement" >&2
        echo "inner: ${inner_requirement}" >&2
        echo "outer: ${outer_requirement}" >&2
        exit 1
    fi
    echo "signed-with: ${CODE_SIGN_IDENTITY} (certificate-backed; FDA grants persist across rebuilds, Contacts grants do not)"
else
    echo "signed-with: ad-hoc (FDA + Contacts grants both reset on rebuild)"
fi
echo "designated-requirement: ${outer_requirement}"
