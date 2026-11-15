#!/system/bin/sh
# wg-user install-time hook (KernelSU / Magisk "modern" customize.sh path).
#
# IMPORTANT — do NOT add an install.sh to this module. Both KernelSU and
# Magisk treat the presence of install.sh as a "legacy module":
#
#   if is_legacy_script; then                     # install.sh exists
#       unzip -oj "$ZIPFILE" module.prop install.sh uninstall.sh \
#                'common/*' -d $TMPDIR            # ^ -j FLATTENS common/abi.sh
#       . $TMPDIR/install.sh                      # runs from /dev/tmp
#   else                                          # customize.sh exists
#       unzip -o "$ZIPFILE" -x 'META-INF/*' -d $MODPATH   # extracts EVERYTHING
#       . $MODPATH/customize.sh                   # runs from the module root
#   fi
#
# The legacy path never extracts bin/, etc/, or webroot/. This module needs
# all of those, so it uses the modern customize.sh path exclusively.
#
# In the modern path the following are guaranteed:
#   - MODPATH is exported by the installer (e.g. /data/adb/modules_update/<id>)
#   - every file from the zip is already at $MODPATH/<path>
#   - we are sourced, not exec'd, so errexit would propagate to the parent
#
# CRITICAL: do NOT use `set -e` here. Sourced scripts share the parent
# shell; enabling errexit would cause the parent installer to abort on
# the first non-zero command (e.g. its own `rmdir -p $MODPATH` against
# the non-empty module dir). Verified by simulation: case A (with set -e)
# exits 1 after a non-zero rmdir; case B (without) exits 0. Handle errors
# explicitly with explicit checks instead.

trace() { printf 'wg-user install: %s\n' "$*" >&2; }

# Resolve the module root. In the modern path the installer exports
# MODPATH. (When sourced, $0 is the parent shell's name — NOT the script
# path — so the $0 absolute-path branch is just a defensive fallback.)
MODPATH="${MODPATH:-}"
if [ -z "$MODPATH" ] || [ ! -f "$MODPATH/module.prop" ]; then
    case "$0" in
        /*)
            cand="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
            [ -f "$cand/module.prop" ] && MODPATH="$cand"
            ;;
    esac
fi
[ -n "$MODPATH" ] || MODPATH="$(pwd)"

# Drop a trailing slash. "$MODPATH"/* in park_tunnel_conf is a case pattern,
# and "/data/x/mod/" would not match "/data/x/mod/etc/tunnel.conf", which made
# an in-tree config look like a file in the installed module dir.
while [ -n "${MODPATH%/}" ] && [ "${MODPATH%/}" != "$MODPATH" ]; do
    MODPATH="${MODPATH%/}"
done

if [ ! -f "$MODPATH/module.prop" ]; then
    trace "FATAL: cannot locate module root"
    trace "  MODPATH='${MODPATH:-}' \$0='$0' pwd='$(pwd)'"
    # `exit` from a sourced script terminates the parent installer; that
    # is the right behaviour for an unrecoverable error.
    exit 1
fi

cd "$MODPATH"
trace "module root: $MODPATH"

. "$MODPATH/common/util.sh"
. "$MODPATH/common/abi.sh"

# One binary per ABI is all 0.3.x ships: wstunnel runs in-process, so there is
# no separate wireproxy/wstunnel to select. Failure is fatal — without
# bin/wg-user the module cannot start at all.
if ! select_binary "$MODPATH" "$MODPATH/bin/wg-user"; then
    log_err "failed to select the wg-user binary"
    exit 1
fi

# ---------------------------------------------------------------------------
# Restore the executable bits.
#
# KernelSU's ksud extracts the zip without applying the Unix mode stored in
# the archive, so every file lands as 0644. service.sh is then exec'd at boot
# and dies with "can't execute: Permission denied", and wg-user cannot be
# invoked from service.sh either. The module then never starts, with no log
# to explain it.
#
# chmod (not the installer's set_perm helper) so this also works under Magisk,
# APatch and a manual . customize.sh run.
# ---------------------------------------------------------------------------
fix_mode() {
    [ -e "$1" ] || return 0
    chown 0:0 "$1" 2>/dev/null || true
    chmod "$2" "$1" 2>/dev/null || true
}

for f in wg-user service.sh customize.sh uninstall.sh; do
    fix_mode "$MODPATH/$f" 0755
done
[ -d "$MODPATH/bin" ] && chmod -R 0755 "$MODPATH/bin" 2>/dev/null

trace "permissions: $(ls -l "$MODPATH/service.sh" 2>/dev/null | awk '{print $1}') service.sh"

# ---------------------------------------------------------------------------
# Config handling.
#
# The distributable zip intentionally carries only the *.example configs —
# a real one contains WireGuard private keys and a server address. On
# reinstall we keep whatever the device already has; on a fresh install we
# seed from the example so the module has a parseable file to start from.
#
# 0.3.0 merged the old etc/tunnel.conf into etc/wireproxy.conf's [Transport]
# section, so upgrades are migrated in place after seeding.
# ---------------------------------------------------------------------------
seed_config() {
    name="$1"
    [ -f "$MODPATH/etc/$name" ] && return 0
    if [ -f "/data/adb/modules/wg-user/etc/$name" ]; then
        cp -f "/data/adb/modules/wg-user/etc/$name" "$MODPATH/etc/$name" 2>/dev/null \
            && log_info "kept existing etc/$name" \
            && return 0
    fi
    if [ -f "$MODPATH/etc/$name.example" ]; then
        cp -f "$MODPATH/etc/$name.example" "$MODPATH/etc/$name" 2>/dev/null \
            && log_warn "no existing etc/$name — seeded from the example; edit it before use"
    fi
}

# old_conf_value <file> <key> — echo the first "key = value" of a 0.2.x
# tunnel.conf.
#
# The old reader matched keys at the start of a line, case-insensitively,
# wherever they sat in the file (section headers were decoration), and kept
# the first occurrence. Like the old reader this keeps the value to end of
# line — strip_comment() removes the comment where trailing text would make
# the new parser reject the value.
old_conf_value() {
    [ -f "$1" ] || return 0
    awk -v want="$2" '
        {
            line = $0
            sub(/\r$/, "", line)
            sub(/^[[:space:]]+/, "", line)
            if (line == "") next
            if (substr(line, 1, 1) == "#" || substr(line, 1, 1) == ";") next
            if (substr(line, 1, 1) == "[") next
            eq = index(line, "=")
            if (eq == 0) next
            key = substr(line, 1, eq - 1)
            sub(/[[:space:]]+$/, "", key)
            if (tolower(key) != tolower(want)) next
            val = substr(line, eq + 1)
            sub(/^[[:space:]]+/, "", val)
            sub(/[[:space:]]+$/, "", val)
            print val
            exit
        }
    ' "$1"
}

# strip_comment <value> — drop a trailing "# ..." comment and the surrounding
# blanks. Used for fields whose old "everything to end of line" value would be
# rejected by the stricter new parser (mode, URLs, host:port); never used for
# usernames/passwords, where '#' can be part of the secret.
strip_comment() {
    printf '%s\n' "$1" | awk '{
        sub(/#.*/, "")
        sub(/^[[:space:]]+/, "")
        sub(/[[:space:]]+$/, "")
        print
    }'
}

# park_tunnel_conf <old-file> — keep the old file for its comments, never
# delete it. A copy that lives in the installed module dir (a manual
# reinstall over the same tree) is renamed next to itself.
park_tunnel_conf() {
    old="$1"
    case "$old" in
        "$MODPATH"/*)
            if mv -f "$old" "$old.migrated" 2>/dev/null; then
                chmod 0600 "$old.migrated" 2>/dev/null || true
            else
                log_warn "could not rename $old to $old.migrated"
            fi
            ;;
        *)
            # The live module dir is replaced wholesale on the next boot;
            # renaming its config now would break a rollback. Keep a copy of
            # the comments in the new module instead.
            if cp -f "$old" "$MODPATH/etc/tunnel.conf.migrated" 2>/dev/null; then
                chmod 0600 "$MODPATH/etc/tunnel.conf.migrated" 2>/dev/null || true
            else
                log_warn "could not save a copy of $old"
            fi
            log_info "left $old in place (the module dir is replaced at reboot)"
            ;;
    esac
}

# The [Transport] writer. Values arrive through the environment so a password
# never shows up in `ps`. The section is emitted as-is, and no value is ever
# followed by a comment on the same line: strip_comment() removes only "#",
# while go-ini also strips ";" in an unquoted value, so keeping comments out is
# belt-and-braces rather than a fix for a semantic difference. Credentials are
# copied verbatim (never through strip_comment), so a '#' or ';' in a password
# survives.
MIGRATE_AWK='
function emit_transport() {
    print ""
    print "# Added by customize.sh: the old etc/tunnel.conf, folded into this file."
    print "# The reference for every key here is etc/wireproxy.conf.example."
    print "[Transport]"
    print "Mode = " mode
    if (url != "") print "URL = " url
    if (user != "") print "Username = " user
    if (pass != "") print "Password = " pass
    if (insecure != "") print "Insecure = " insecure
    print ""
    inserted = 1
}

BEGIN {
    mode = ENVIRON["MIG_MODE"]
    url = ENVIRON["MIG_URL"]
    user = ENVIRON["MIG_USER"]
    pass = ENVIRON["MIG_PASS"]
    insecure = ENVIRON["MIG_INSECURE"]
    endpoint = ENVIRON["MIG_ENDPOINT"]
    section = ""
    inserted = 0
}

{
    line = $0
    sub(/\r$/, "", line)
    trimmed = line
    sub(/^[[:space:]]+/, "", trimmed)
    sub(/[[:space:]]+$/, "", trimmed)

    if (substr(trimmed, 1, 1) == "[") {
        name = trimmed
        sub(/^\[/, "", name)
        sub(/\].*$/, "", name)
        section = tolower(name)
        if (!inserted && section == "peer") emit_transport()
        print line
        next
    }

    if (endpoint != "" && section == "peer") {
        eq = index(trimmed, "=")
        if (eq > 0) {
            key = substr(trimmed, 1, eq - 1)
            sub(/[[:space:]]+$/, "", key)
            if (tolower(key) == "endpoint") {
                print "Endpoint = " endpoint
                next
            }
        }
    }

    print line
}

END { if (!inserted) emit_transport() }
'

# migrate_tunnel_conf — fold a 0.2.x etc/tunnel.conf into [Transport].
#
# Old shape (flat key = value, first occurrence wins):
#     mode = off|wstunnel
#     [wstunnel] remote, local-udp, remote-udp, username, password, insecure
# New shape ([Transport] in etc/wireproxy.conf):
#     Mode, URL, Username, Password, Insecure
# The names are not copies: remote becomes URL, and mode moves into the
# section. [Transport] also accepts Path/Keepalive, which 0.2.x had no knob
# for, so their defaults are what the old code used.
#
# [Peer] Endpoint is the dangerous part. 0.2.x read it as the LOCAL UDP
# listener (local-udp) and passed the server-side delivery target (remote-udp)
# on wstunnel's command line. The new in-process transport takes the delivery
# target from [Peer] Endpoint, so in wstunnel mode Endpoint must become the old
# remote-udp, whose default was 127.0.0.1:51820. Skipping that rewrite tells
# the server to deliver WG traffic to whatever the old local-udp port was on
# the server, and the tunnel silently never handshakes. In off mode Endpoint is
# normally the real WireGuard server address and is left alone — except when a
# wstunnel config is degraded to off (unknown mode, bad scheme, missing URL):
# Endpoint is still the old local-udp listener then, so the migration warns and
# the user has to point it at the real server.
migrate_tunnel_conf() {
    old=""
    for cand in "$MODPATH/etc/tunnel.conf" "/data/adb/modules/wg-user/etc/tunnel.conf"; do
        if [ -f "$cand" ]; then
            old="$cand"
            break
        fi
    done
    [ -n "$old" ] || return 0

    new="$MODPATH/etc/wireproxy.conf"
    if [ ! -f "$new" ]; then
        log_warn "found $old but no etc/wireproxy.conf; leaving it alone"
        return 0
    fi
    if grep -qi '^[[:space:]]*\[transport\]' "$new" 2>/dev/null; then
        log_info "etc/wireproxy.conf already has [Transport]; not migrating again"
        park_tunnel_conf "$old"
        return 0
    fi

    # mode: 0.2.x treated anything that was not "off"/"wstunnel" as off (the
    # deleted tunnel.conf.example documented that), but internal/config rejects
    # unknown modes, so "mode = on" would brick the module on upgrade.
    mode_raw=$(strip_comment "$(old_conf_value "$old" mode)")
    case "$(printf '%s' "$mode_raw" | tr '[:upper:]' '[:lower:]')" in
        ""|off)   mode=off ;;
        wstunnel) mode=wstunnel ;;
        *)
            log_warn "etc/tunnel.conf: mode='$mode_raw' is neither 'off' nor 'wstunnel'; migrating as 'off'. With the transport disabled, [Peer] Endpoint must be the real WireGuard server address: the old Endpoint was the local UDP listener used by 0.2.0 (local-udp), so the migrated file parses but would send UDP to the phone."
            mode=off
            ;;
    esac

    # remote: only ws:// and wss:// are supported now (WebTransport/wts:// is
    # an explicit non-goal). A URL with any other scheme is dropped and the
    # transport disabled, instead of producing a config the daemon refuses.
    url=$(strip_comment "$(old_conf_value "$old" remote)")
    if [ -n "$url" ]; then
        case "$(printf '%s' "$url" | tr '[:upper:]' '[:lower:]')" in
            ws://*|wss://*) ;;
            *)
                log_warn "etc/tunnel.conf: remote='$url' is not ws:// or wss:// (WebTransport is not supported); migrating as 'off' — see docs/TUNNEL-SERVER.md. With the transport disabled, [Peer] Endpoint must be the real WireGuard server address: the old Endpoint was the local UDP listener (local-udp), so the migrated file parses but would send UDP to the phone."
                mode=off
                url=""
                ;;
        esac
    fi

    # A wstunnel mode without a URL is rejected by internal/config ("[Transport]
    # URL is required when Mode = \"wstunnel\""), i.e. the install would succeed
    # and the daemon would refuse the config on every boot. Degrade exactly like
    # the bad-scheme case instead.
    if [ "$mode" = wstunnel ] && [ -z "$url" ]; then
        log_warn "etc/tunnel.conf: mode = wstunnel but no 'remote' URL; migrating as 'off'. With the transport disabled, [Peer] Endpoint must be the real WireGuard server address: the old Endpoint was the local UDP listener (local-udp), so the migrated file parses but would send UDP to the phone."
        mode=off
    fi

    # wstunnel mode carries exactly one [Peer] (internal/config: "transport mode
    # \"wstunnel\" requires exactly one [Peer]"). The rewrite below touches
    # Endpoint in every peer block, so a 0.2.x file with several peers would
    # migrate "successfully" and then exit 2 on every boot, with no install-time
    # signal. Which peer to keep is the user's call, so warn rather than pick:
    # the daemon will keep refusing the file until the extras are removed.
    if [ "$mode" = wstunnel ]; then
        peers=$(grep -ci '^[[:space:]]*\[peer\]' "$new" 2>/dev/null || true)
        if [ "${peers:-0}" -gt 1 ]; then
            log_warn "etc/wireproxy.conf has $peers [Peer] sections; wstunnel mode supports exactly one peer, and the daemon will refuse to start (exit 2) until the extra ones are removed. See docs/CONFIG.md — [Peer]."
        fi
    fi

    user=$(old_conf_value "$old" username)
    pass=$(old_conf_value "$old" password)

    # insecure: reproduce 0.2.x faithfully for a single key. It decided with
    #   grep -Eqi '^[[:space:]]*insecure[[:space:]]*=[[:space:]]*(false|no|0)'
    # and only added --tls-verify-certificate when that matched. The pattern is
    # prefix-anchored, so "falsey"/"none"/"nope"/"0.1" also counted as
    # verification ON; true, yes, on, off, a typo or an empty value all meant
    # "no flag", and wstunnel's default is verification OFF (config.rs: "the
    # client will happily connect to any server with self-signed certificate").
    # internal/config defaults Insecure to false = verify, so the migration
    # translates the old rule by prefix, not by boolean: false*/no*/0* ->
    # Insecure = false, every other non-empty value (and an absent/empty key)
    # -> Insecure = true. Ineffective when the transport is off (no TLS), so
    # the key is only written for Mode = wstunnel.
    # Caveat: old_conf_value keeps the first occurrence of a duplicate key, while
    # 0.2.x's grep matched any matching line, so the two can disagree when the
    # old file repeats the key.
    insecure=""
    if [ "$mode" = wstunnel ]; then
        insecure_raw=$(strip_comment "$(old_conf_value "$old" insecure)")
        case "$(printf '%s' "$insecure_raw" | tr '[:upper:]' '[:lower:]')" in
            false*|no*|0*) insecure=false ;;
            *)
                insecure=true
                if [ -z "$insecure_raw" ]; then
                    log_warn "etc/tunnel.conf has no 'insecure' value (key absent or empty); 0.2.0 did not verify the TLS certificate then, so Insecure = true is written. Set 'Insecure = false' in etc/wireproxy.conf to enable verification."
                fi
                ;;
        esac
    fi

    endpoint=""
    # host:port copied verbatim could carry a trailing comment
    # ("127.0.0.1:51820 # server side"), which is not a valid endpoint.
    remote_udp=$(strip_comment "$(old_conf_value "$old" remote-udp)")
    if [ "$mode" = wstunnel ]; then
        endpoint="${remote_udp:-127.0.0.1:51820}"
    elif [ -n "$remote_udp" ]; then
        log_warn "etc/tunnel.conf: remote-udp ignored; the migration runs with Mode = $mode, and remote-udp only applies to wstunnel"
    fi

    tmp="$new.migrating.$$"
    # umask 077: the temp copy holds WireGuard keys and must never exist with
    # the default 0644 for even a moment.
    if ! ( umask 077
           MIG_MODE="$mode" MIG_URL="$url" MIG_USER="$user" MIG_PASS="$pass" \
               MIG_INSECURE="$insecure" MIG_ENDPOINT="$endpoint" \
               awk "$MIGRATE_AWK" "$new" > "$tmp" ); then
        rm -f "$tmp"
        log_err "failed to rewrite etc/wireproxy.conf from $old"
        return 1
    fi
    chmod 0600 "$tmp" 2>/dev/null || true
    if ! mv -f "$tmp" "$new"; then
        rm -f "$tmp"
        log_err "failed to replace etc/wireproxy.conf"
        return 1
    fi
    chmod 0600 "$new" 2>/dev/null || true

    log_info "migrated $old into etc/wireproxy.conf ([Transport] Mode = $mode)"
    park_tunnel_conf "$old"
}

mkdir -p "$MODPATH/etc" 2>/dev/null
seed_config wireproxy.conf
# An attempted migration that fails must abort the install: carrying on would
# leave Mode = off with the old Endpoint = 127.0.0.1:<local-udp>, i.e. WG UDP
# pointed back at the phone. Aborting keeps the working 0.2.x module installed.
# "No tunnel.conf to migrate" returns 0 above and stays silent.
if ! migrate_tunnel_conf; then
    log_err "config migration failed; aborting the install"
    exit 1
fi
[ -d "$MODPATH/etc" ] && chmod 0700 "$MODPATH/etc" 2>/dev/null
[ -f "$MODPATH/etc/wireproxy.conf" ] && chmod 0600 "$MODPATH/etc/wireproxy.conf" 2>/dev/null
[ -f "$MODPATH/etc/tunnel.conf.migrated" ] && chmod 0600 "$MODPATH/etc/tunnel.conf.migrated" 2>/dev/null

log_info "Installed wg-user for $(getprop ro.product.cpu.abi)"
