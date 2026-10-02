#!/usr/bin/env bash
# converge.sh — the only sanctioned way to change a slack-acp bot host.
#
# Reads distro.json <- $BOTS_DIR/<bot>.json (deep-merged; the shared fleet
# registry, entries with "relay": "slack-acp") + dist.lock and makes the
# target host match. Version moves go through the lock, never through a
# hand-typed command on a host.
#
# Usage:
#   converge.sh <bot>                          dry-run (default): show what would change
#   converge.sh <bot> --apply                  actually converge the host
#   converge.sh <bot> --local                  act on THIS machine directly (no ssh);
#                                              refuses unless the spec host is us
#   converge.sh <bot> --force-local            --local without the "is this us" check
#   converge.sh <bot> --target-root DIR        act on a local fake host rooted at DIR
#                                              (no ssh; for testing). DIR/proc stands in
#                                              for /proc so a test can model a running image
#   converge.sh --tot                          resolve the newest release that satisfies
#                                              EVERY spec's `require` and rewrite
#                                              dist.lock; NEVER converges
#   converge.sh spec <bot>                     print the merged spec (distro <- bot)
#   converge.sh render <bot> <artefact> [home] render one artefact to stdout
#                                              artefact: config | execstart | unit | plist
#                                              home: the target $HOME (default /home/user)
#   converge.sh plan-stale <running> <want> <running_ver>
#                                              print whether the RUNNING image is stale
#   converge.sh semver-cmp <a> <b>             print -1|0|1 (test hook)
#   converge.sh semver-sat <version> <constraint>
#                                              print yes/no, exit 0/1 (test hook)
#   converge.sh resolve <component> <ver...>   newest version satisfying every spec's
#                                              require.<component> (test hook)
#   converge.sh self-host <host>               is <host> THIS machine? exit 0/1 (test hook)
#
# What converge manages on a host: the slack-acp binary (at the locked
# version), config.json, and the supervisor definition — a systemd user unit
# (supervisor "systemd-user") or a launchd LaunchAgent plist ("launchd").
# It does NOT manage the agent (fir, claude-code, ...): a host often runs
# other relays too, and the agent belongs to whichever converge owns it.
#
# Binary swap: the release asset and checksums.txt are downloaded on the host,
# the sha256 is verified, and the file is written to a temp name in the SAME
# directory and renamed over the binary. rename(2) replaces the directory
# entry while the running process keeps its old inode, so there is no ETXTBSY
# and no stop-before-copy.
#
# Recycle: slack-acp has no graceful reload (it handles SIGTERM only). Any
# change is applied by a restart: `systemctl --user restart` or
# `launchctl kickstart -k`. Socket Mode reconnects in seconds; an in-flight
# turn on that bot is dropped, so run converge from outside that bot.
#
# Rendering matches `slack-acp install-service` byte for byte (absolute paths,
# the target's $HOME substituted for ~), so a host set up by install-service
# converges with no unit change.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
# ONE bot registry for the whole relay fleet, shared with poe-acp, zulip-acp
# and fleet.sh. Only entries with .relay == slack-acp are ours.
BOTS_DIR="${FLEET_BOTS_DIR:-$HOME/sync/shared/fleet/bots}"
RELAY=slack-acp
# bot_specs: our relay's spec files, one per line.
bot_specs() {
  local f
  for f in "$BOTS_DIR"/*.json; do
    [ -f "$f" ] && [ "$(jq -r '.relay // empty' "$f")" = "$RELAY" ] && echo "$f"
  done
  return 0
}
# distro.json (next to dist.lock) holds the defaults shared by every bot of
# this relay; a bot file holds only what differs. merged_spec deep-merges
# distro <- bot: the bot wins, objects merge recursively, any other value
# (arrays included) is replaced wholesale, and a bot null unsets a default.
# Merged objects keep the BOT's key order with default-only keys appended,
# so a bot that needs a rendered key order (config.json) can restate keys.
# Bots with "managed": false are tracked, not distro instances: no merge.
DISTRO="${FLEET_DISTRO:-$REPO_ROOT/distro.json}"
MERGE_JQ='def dmerge($d; $b):
  if ($d|type) == "object" and ($b|type) == "object" then
    (reduce ($b|keys_unsorted[]) as $k ({};
       .[$k] = (if ($d|has($k)) then dmerge($d[$k]; $b[$k]) else $b[$k] end))) as $r
    | reduce ($d|keys_unsorted[]) as $k ($r; if has($k) then . else .[$k] = $d[$k] end)
  else $b end;
def prune: if type == "object" then with_entries(select(.value != null) | .value |= prune) else . end;
if $bot[0].managed == false then $bot[0] else (dmerge($distro[0]; $bot[0]) | prune) end'
TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT
# merged_spec <bot-file> — print the path of the bot's merged spec.
merged_spec() {
  local out
  out=$(mktemp "$TMPD/spec.XXXXXX")
  if [ -f "$DISTRO" ]; then
    jq -n --slurpfile distro "$DISTRO" --slurpfile bot "$1" "$MERGE_JQ" >"$out" \
      || die "cannot merge $DISTRO <- $1"
  else
    cp "$1" "$out"
  fi
  echo "$out"
}
# bot_spec <name>: path of that bot's MERGED spec; dies unless it is ours.
bot_spec() {
  local spec="$BOTS_DIR/$1.json" r
  [ -f "$spec" ] || die "no such bot spec: $spec"
  r=$(jq -r '.relay // empty' "$spec")
  [ "$r" = "$RELAY" ] || die "$1 is a ${r:-relay-less} bot, not $RELAY: $spec"
  merged_spec "$spec"
}
LOCK="${FLEET_LOCK:-$REPO_ROOT/dist.lock}"
STAMP=$(date +%Y%m%d-%H%M%S)

SLACK_ACP_REPO=kfet/slack-acp
# Where release assets are fetched from on the host. Overridable so a test
# can point it at a file:// directory.
RELEASE_BASE="${SLACK_ACP_RELEASE_BASE:-https://github.com/$SLACK_ACP_REPO/releases/download}"
# How long (seconds) to wait for "slack: connected" in the log after a restart.
CONNECT_WAIT=${CONNECT_WAIT:-30}
case "$CONNECT_WAIT" in ''|*[!0-9]*) echo "converge: error: CONNECT_WAIT must be a whole number of seconds" >&2; exit 1 ;; esac

die()  { echo "converge: error: $*" >&2; exit 1; }
note() { echo "  $*"; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"; }

need jq

# ---------------------------------------------------------------------------
# Semantic version comparison
#
# Hand-written on purpose. `sort -V` is not a semver comparator: it happily
# orders `v0.9.0` after `v0.10.0` only by accident of the leading `v`, and it
# ranks `0.31.3-dev+abc` ABOVE `0.31.3`, which is exactly backwards — a dev
# build is NOT the release. Shelling out to python/node is worse: converge
# must run on a bare host with nothing but coreutils, jq and ssh.
#
# The rule that matters: only a clean X.Y.Z is a release. Any prerelease or
# build-metadata suffix (`-dev+sha`, `-rc1`, `.dirty`) satisfies NO constraint,
# so a dev binary can never be locked into the fleet by accident.
# ---------------------------------------------------------------------------

# is_release_version <s> — true iff s is exactly X.Y.Z, all numeric.
is_release_version() {
  local v=$1 a b c d
  case "$v" in ''|*[!0-9.]*|.*|*.) return 1 ;; esac
  IFS=. read -r a b c d <<<"$v"
  [ -n "$a" ] && [ -n "$b" ] && [ -n "$c" ] && [ -z "${d:-}" ]
}

# ver_cmp <a> <b> — print -1, 0 or 1. Both must be release versions.
ver_cmp() {
  local a1 a2 a3 b1 b2 b3 x y i
  is_release_version "$1" || die "not a release version: $1"
  is_release_version "$2" || die "not a release version: $2"
  IFS=. read -r a1 a2 a3 <<<"$1"
  IFS=. read -r b1 b2 b3 <<<"$2"
  for i in 1 2 3; do
    case $i in
      1) x=$a1; y=$b1 ;;
      2) x=$a2; y=$b2 ;;
      3) x=$a3; y=$b3 ;;
    esac
    # 10# forces decimal: an 08 segment is not octal here.
    if [ "$((10#$x))" -lt "$((10#$y))" ]; then echo -1; return 0; fi
    if [ "$((10#$x))" -gt "$((10#$y))" ]; then echo 1; return 0; fi
  done
  echo 0
}

# constraint_terms <expr> — normalise a constraint into "<op> <version>" lines,
# or fail. A constraint is whitespace-separated terms, ANDed:
#   >=X.Y.Z  <=X.Y.Z  >X.Y.Z  <X.Y.Z  =X.Y.Z  X.Y.Z (exact)  ~>X.Y.Z
# ~>X.Y.Z is the pessimistic operator: >=X.Y.Z and <X.(Y+1).0.
constraint_terms() {
  local expr=$1 term op want out=""
  for term in $expr; do
    case "$term" in
      '>='*)  op=ge; want=${term#>=} ;;
      '<='*)  op=le; want=${term#<=} ;;
      '~>'*)  op=tw; want=${term#'~>'} ;;
      '>'*)   op=gt; want=${term#>} ;;
      '<'*)   op=lt; want=${term#<} ;;
      '='*)   op=eq; want=${term#=} ;;
      *)      op=eq; want=$term ;;
    esac
    is_release_version "$want" || return 1
    out+="$op $want"$'\n'
  done
  # A constraint that yielded no terms — empty, or nothing but whitespace — is
  # a failure, never a vacuous truth: it would otherwise be satisfied by every
  # version, which is the opposite of what writing it down meant.
  [ -n "$out" ] || return 1
  printf '%s' "$out"
}

# valid_constraint <expr>
valid_constraint() { constraint_terms "$1" >/dev/null 2>&1; }

# ver_satisfies <version> <constraint> — true iff version meets every term.
# A non-release version (prerelease / build metadata) satisfies nothing.
ver_satisfies() {
  local v=$1 expr=$2 terms op want c w1 w2
  is_release_version "$v" || return 1
  terms=$(constraint_terms "$expr") || return 1
  while read -r op want; do
    [ -n "$op" ] || continue
    c=$(ver_cmp "$v" "$want")
    case "$op" in
      ge) [ "$c" -ge 0 ] || return 1 ;;
      gt) [ "$c" -gt 0 ] || return 1 ;;
      le) [ "$c" -le 0 ] || return 1 ;;
      lt) [ "$c" -lt 0 ] || return 1 ;;
      eq) [ "$c" -eq 0 ] || return 1 ;;
      tw) [ "$c" -ge 0 ] || return 1
          IFS=. read -r w1 w2 _ <<<"$want"
          [ "$(ver_cmp "$v" "$((10#$w1)).$((10#$w2 + 1)).0")" -lt 0 ] || return 1 ;;
    esac
  done <<<"$terms"
  return 0
}

# ---------------------------------------------------------------------------
# require blocks: the constraint side of constraint-vs-resolution
#
# A spec's `require` is OPTIONAL and names components, not hosts:
#   "require": { "slack_acp": ">=0.11.0" }
# A spec without one behaves exactly as before.
# ---------------------------------------------------------------------------
REQUIRE_COMPONENTS="slack_acp"

# spec_require <spec-file> <component> — the constraint, or empty.
spec_require() { jq -r --arg k "$2" '.require[$k] // empty' "$1"; }

# all_requires <component> — "<bot>\t<constraint>" for every spec that has one.
all_requires() {
  local f bot c
  for f in $(bot_specs); do
    [ -f "$f" ] || continue
    bot=$(basename "$f" .json)
    c=$(spec_require "$(merged_spec "$f")" "$1")
    [ -n "$c" ] && printf '%s\t%s\n' "$bot" "$c"
  done
  return 0
}

# resolve_satisfying <component> <version...> — the NEWEST given version that
# satisfies every spec's constraint for that component. Empty if none does.
resolve_satisfying() {
  local comp=$1 v best="" reqs c
  shift
  reqs=$(all_requires "$comp")
  for v in "$@"; do
    is_release_version "$v" || continue
    if [ -n "$reqs" ]; then
      while IFS=$'\t' read -r _ c; do
        [ -n "$c" ] || continue
        ver_satisfies "$v" "$c" || continue 2
      done <<<"$reqs"
    fi
    if [ -z "$best" ] || [ "$(ver_cmp "$v" "$best")" -gt 0 ]; then best=$v; fi
  done
  printf '%s\n' "$best"
}

resolve_component() {
  local comp=$1 label=$2 picked newest reqs
  shift 2
  [ $# -gt 0 ] || die "could not list any $label release (rate-limited? is gh logged in?)"
  newest=$(printf '%s\n' "$@" | tail -1)
  picked=$(resolve_satisfying "$comp" "$@")
  if [ -z "$picked" ]; then
    reqs=$(all_requires "$comp" | sed 's/^/     /')
    die "no $label release satisfies every spec's require.$comp (newest available: $newest). Constraints:
$reqs"
  fi
  [ "$picked" = "$newest" ] || note "$label: held at $picked by a spec constraint (newest release is $newest)" >&2
  printf '%s\n' "$picked"
}


# ---------------------------------------------------------------------------
# Spec access ($SPEC is set in main)
# ---------------------------------------------------------------------------
SPEC=""
SPEC_SRC=""
# THOME: the target's $HOME, substituted for a leading ~ in every rendered path.
THOME=/home/user

jqs() { jq -r "$1" "$SPEC"; }
skey() { jq -r --arg k "$1" '.service[$k] // empty' "$SPEC"; }

# validate_spec — refuse a spec whose strings would break out of where they
# are interpolated (a remote bash payload, a unit line, a plist <string>).
# Runs ONCE, at top level: a check inside $(...) would die in a subshell.
validate_spec() {
  local k v
  for k in .name .host .unit .binary .platform .credentials.env_file \
           .service.config_path .service.state_dir .launchd.label .launchd.path \
           .systemd.env_path .systemd.restart .systemd.restart_sec .systemd.timeout_stop_sec; do
    v=$(jq -r "$k // empty" "$SPEC")
    case "$v" in
      *'"'*|*'$'*|*'`'*|*';'*|*'&'*|*'|'*|*"'"*|*'<'*|*'>'*|*'\'*|*' '*|*'
'*) die "$SPEC_SRC: $k must not contain whitespace, a quote, \$, backtick, ;, &, |, <, > or \\ (got: $v)" ;;
    esac
  done
  # Only env_path may carry a systemd % specifier (%h).
  for k in .name .unit .binary .credentials.env_file .service.config_path .service.state_dir; do
    case "$(jq -r "$k // empty" "$SPEC")" in
      *'%'*) die "$SPEC_SRC: $k must not contain % (systemd specifier)" ;;
    esac
  done
  case "$(jqs '.supervisor // empty')" in
    systemd-user|launchd) : ;;
    *) die "$SPEC_SRC: .supervisor must be systemd-user or launchd (got: $(jqs '.supervisor // "absent"'))" ;;
  esac
  for k in .host .binary .credentials.env_file .service.config_path .platform; do
    [ -n "$(jq -r "$k // empty" "$SPEC")" ] || die "$SPEC_SRC: $k is required (in the bot file or distro.json)"
  done
  case "$(jqs '.platform')" in
    linux/amd64|linux/arm64|linux/armv6|darwin/amd64|darwin/arm64) : ;;
    *) die "$SPEC_SRC: unsupported .platform $(jqs '.platform')" ;;
  esac
  [ "$(jq -r '.config | type' "$SPEC")" = object ] || die "$SPEC_SRC: .config must be an object"
  validate_require
}

# validate_require — the `require` block, if present, must be an object whose
# keys are known components and whose values are parseable constraints.
validate_require() {
  local t k c
  t=$(jq -r 'if has("require") then (.require | type) else "absent" end' "$SPEC")
  case "$t" in
    absent|null) return 0 ;;
    object) : ;;
    *) die "$SPEC_SRC: .require must be an object like {\"slack_acp\": \">=0.11.0\"} (got $t)" ;;
  esac
  while IFS= read -r k; do
    case " $REQUIRE_COMPONENTS " in
      *" $k "*) : ;;
      *) die "$SPEC_SRC: unknown .require key \"$k\" (known: $REQUIRE_COMPONENTS)" ;;
    esac
    [ "$(jq -r --arg k "$k" '.require[$k] | type' "$SPEC")" = string ] \
      || die "$SPEC_SRC: .require.$k must be a string constraint (e.g. \">=0.11.0\")"
    c=$(jq -r --arg k "$k" '.require[$k]' "$SPEC")
    valid_constraint "$c" \
      || die "$SPEC_SRC: invalid .require.$k constraint \"$c\""
  done < <(jq -r '.require | keys[]' "$SPEC")
}

# enforce_require <bot> <host> <component> <locked-version>
enforce_require() {
  local bot=$1 host=$2 comp=$3 locked=$4 c
  c=$(spec_require "$SPEC" "$comp")
  [ -n "$c" ] || return 0
  ver_satisfies "$locked" "$c" && return 0
  die "$host ($bot): dist.lock has $comp $locked, which does not satisfy $SPEC_SRC require.$comp \"$c\". Re-resolve with \`scripts/converge.sh --tot\`. Nothing was changed on $host."
}

# p_abs <spec-path> — ~/x -> $THOME/x
p_abs() { printf '%s' "${1/#\~/$THOME}"; }

# ---------------------------------------------------------------------------
# Renderers — byte-compatible with internal/installsvc (install-service).
# ---------------------------------------------------------------------------
render_config() { jq '.config' "$SPEC"; }

render_execstart() {
  local out v
  out="$(p_abs "$(jqs '.binary')") --config $(p_abs "$(skey config_path)")"
  v=$(skey state_dir); [ -n "$v" ] && out+=" --state-dir $(p_abs "$v")"
  printf '%s\n' "$out"
}

render_unit() {
  local v
  cat <<'EOF'
[Unit]
Description=slack-acp
After=network-online.target
Wants=network-online.target

[Service]
EOF
  v=$(jqs '.systemd.env_path // empty'); [ -n "$v" ] && echo "Environment=PATH=$v"
  echo "EnvironmentFile=$(p_abs "$(jqs '.credentials.env_file')")"
  echo "ExecStart=$(render_execstart)"
  echo "Restart=$(jqs '.systemd.restart // "on-failure"')"
  echo "RestartSec=$(jqs '.systemd.restart_sec // "2s"')"
  v=$(jqs '.systemd.timeout_stop_sec // empty'); [ -n "$v" ] && echo "TimeoutStopSec=$v"
  cat <<'EOF'

[Install]
WantedBy=default.target
EOF
}

# launchd_label — the LaunchAgent Label (= plist basename).
launchd_label() {
  local l
  l=$(jqs '.launchd.label // empty')
  [ -n "$l" ] || l="dev.${THOME##*/}.slack-acp"
  printf '%s\n' "$l"
}

xml_escape() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g' -e "s/'/\&apos;/g"; }

render_plist() {
  local label path logs exec
  label=$(launchd_label)
  path=$(jqs '.launchd.path // empty')
  [ -n "$path" ] || path="$THOME/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
  path=$(p_abs "$path")
  logs="$THOME/Library/Logs"
  exec="set -a; . '$(p_abs "$(jqs '.credentials.env_file')")'; set +a; exec '$(p_abs "$(jqs '.binary')")' --config '$(p_abs "$(skey config_path)")'"
  local v; v=$(skey state_dir); [ -n "$v" ] && exec+=" --state-dir '$(p_abs "$v")'"
  cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$(printf '%s' "$label" | xml_escape)</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>$(printf '%s' "$exec" | xml_escape)</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>$(printf '%s' "$path" | xml_escape)</string>
    <key>HOME</key><string>$(printf '%s' "$THOME" | xml_escape)</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$(printf '%s' "$logs" | xml_escape)/slack-acp.out.log</string>
  <key>StandardErrorPath</key><string>$(printf '%s' "$logs" | xml_escape)/slack-acp.err.log</string>
</dict>
</plist>
EOF
}

# supervisor_file — spec-style path of the unit / plist on the target.
supervisor_file() {
  case "$(jqs '.supervisor')" in
    systemd-user) printf '~/.config/systemd/user/%s.service\n' "$(jqs '.unit // "slack-acp"')" ;;
    launchd)      printf '~/Library/LaunchAgents/%s.plist\n' "$(launchd_label)" ;;
  esac
}
render_supervisor() {
  case "$(jqs '.supervisor')" in
    systemd-user) render_unit ;;
    launchd)      render_plist ;;
  esac
}

# ---------------------------------------------------------------------------
# Remote execution (ssh by host alias, or a local fake root for testing)
# ---------------------------------------------------------------------------
HOST=""
TARGET_ROOT=""
LOCAL=0
FORCE_LOCAL=0
PROC_ROOT=/proc

# host_is_self <spec-host> — is the spec's host THIS machine? An unknown
# answer is NO (ssh is used). Sets SELF_WHY.
SELF_WHY=""
host_is_self() {
  local host=$1 real ip mine
  SELF_WHY=""
  real=$(ssh -G "$host" 2>/dev/null | awk '/^hostname /{print $2; exit}' || true)
  [ -n "$real" ] || real="$host"
  case "$real" in
    localhost|127.0.0.1|::1) SELF_WHY="$host -> $real is the loopback"; return 0 ;;
  esac
  if [ "$real" = "$(hostname 2>/dev/null)" ] || [ "$real" = "$(hostname -f 2>/dev/null)" ]; then
    SELF_WHY="$host -> $real matches this machine's hostname"
    return 0
  fi
  ip=$(getent hosts "$real" 2>/dev/null | awk '{print $1; exit}' || true)
  [ -n "$ip" ] || return 1
  mine=$( { tailscale ip 2>/dev/null || true
            ip -o addr show 2>/dev/null | awk '{print $4}' | cut -d/ -f1
            ifconfig 2>/dev/null | awk '/inet /{print $2}'
          } | sort -u | tr '\n' ' ')
  [ -n "$mine" ] || return 1
  if printf '%s' "$mine" | grep -qw -- "$ip"; then
    SELF_WHY="$host -> $real ($ip) is an address of this machine"
    return 0
  fi
  return 1
}

rsh() { # run a shell command on the target; stdin is forwarded
  if [ "$LOCAL" = 1 ]; then
    bash -lc "$1"
  elif [ -n "$TARGET_ROOT" ]; then
    HOME="$TARGET_ROOT" PATH="$TARGET_ROOT/.local/bin:$PATH" bash -c "$1"
  else
    # A LOGIN BASH on the far side (~/.local/bin on PATH, never zsh), the
    # payload single-quoted with sed. BatchMode: never stop for a prompt.
    local esc
    esc=$(printf '%s' "$1" | sed "s/'/'\\\\''/g")
    # shellcheck disable=SC2029  # remote-side expansion is intended
    ssh -o BatchMode=yes -o ConnectTimeout=10 "$HOST" "bash -lc '$esc'"
  fi
}

# rcat <abs-path> — print remote file; empty if missing; die on transport error
rcat() {
  local rc=0
  rsh "p='$1'; if [ -f \"\$p\" ]; then cat \"\$p\"; else exit 42; fi" || rc=$?
  case "$rc" in 0|42) : ;; *) die "failed reading $1 (rc=$rc — ssh/transport error?)" ;; esac
}

# rwrite <abs-path> <local-content-file> — backup, then temp + rename
rwrite() {
  rsh "set -e; p='$1'; mkdir -p \"\$(dirname \"\$p\")\"; \
       [ -f \"\$p\" ] && cp -p \"\$p\" \"\$p.bak-$STAMP\"; \
       t=\$(mktemp \"\$p.new.XXXXXX\"); cat > \"\$t\"; \
       [ -f \"\$p\" ] && chmod --reference=\"\$p\" \"\$t\" 2>/dev/null || chmod 644 \"\$t\"; \
       mv -f \"\$t\" \"\$p\"" <"$2"
}

# diff_text <label> <abs-path> <desired-file> — byte diff; 0 same, 1 differs.
diff_text() {
  local have
  have=$(mktemp "$TMPD/have.XXXXXX")
  rcat "$2" >"$have"
  diff -u --label "$1 (current)" --label "$1 (desired)" "$have" "$3"
}

# diff_json <label> <abs-path> <desired-file> — compare as JSON values (key
# order kept, whitespace and a trailing newline ignored): slack-acp init
# writes config.json without one, and that is not a change.
diff_json() {
  local have a b
  have=$(mktemp "$TMPD/have.XXXXXX")
  rcat "$2" >"$have"
  if [ ! -s "$have" ]; then
    diff -u --label "$1 (current)" --label "$1 (desired)" "$have" "$3"; return
  fi
  a=$(mktemp "$TMPD/a.XXXXXX"); b=$(mktemp "$TMPD/b.XXXXXX")
  jq . "$have" >"$a" 2>/dev/null || cp "$have" "$a"
  jq . "$3" >"$b"
  diff -u --label "$1 (current)" --label "$1 (desired)" "$a" "$b"
}

# ---------------------------------------------------------------------------
# Running state
# ---------------------------------------------------------------------------
SA_EXE=slack-acp

# probe_state — prints "<running>|<pid>|<version>"; version is that of the
# image the pid EXECUTES ($PROC_ROOT/<pid>/exe), empty when unknowable
# (macOS has no /proc).
probe_state() {
  local sup unit label
  sup=$(jqs '.supervisor')
  if [ -n "$TARGET_ROOT" ] && [ ! -x "$TARGET_ROOT/.local/bin/systemctl" ] \
     && [ ! -x "$TARGET_ROOT/.local/bin/launchctl" ]; then
    echo '0|0|'; return 0
  fi
  unit=$(jqs '.unit // "slack-acp"'); label=$(launchd_label)
  rsh "sup='$sup'; u='$unit'; label='$label'; proc='$PROC_ROOT'; zaname='$SA_EXE'
$(cat <<'EOS'
pid=0; run=0
if [ "$sup" = systemd-user ]; then
  pid=$(systemctl --user show -p MainPID --value "$u" 2>/dev/null || echo 0)
  act=$(systemctl --user is-active "$u" 2>/dev/null || true)
  case "$pid" in ''|*[!0-9]*) pid=0 ;; esac
  [ "$act" = active ] && [ "$pid" -gt 0 ] && run=1
else
  pid=$(launchctl print "gui/$(id -u)/$label" 2>/dev/null | awk '$1=="pid" && $2=="=" {print $3; exit}')
  case "$pid" in ''|*[!0-9]*) pid=0 ;; esac
  [ "$pid" -gt 0 ] && run=1
fi
ver=''
if [ "$run" = 1 ]; then
  e=$(readlink "$proc/$pid/exe" 2>/dev/null || true); e=${e% (deleted)}; e=${e##*/}
  case "$e" in "$zaname"|"$zaname".*) ver=$("$proc/$pid/exe" --version 2>/dev/null | head -1 || true) ;; esac
fi
printf '%s|%s|%s\n' "$run" "$pid" "$ver"
EOS
)"
}

# stale_decision <running> <want> <running_ver> — is the live process still
# executing an old image? "stale|why" or "current|why". An unreadable version
# is never evidence of staleness.
stale_decision() {
  local running=$1 want=$2 ver=$3
  if [ "$running" != 1 ]; then echo "current|not running"; return 0; fi
  if [ -z "$ver" ]; then echo "current|running version unreadable"; return 0; fi
  if [ "$ver" = "$want" ]; then echo "current|running image is $want"; return 0; fi
  echo "stale|running image is $ver, wanted $want"
}

# ---------------------------------------------------------------------------
# Binary installation: download + sha256 verify + temp-in-same-dir + rename.
# ---------------------------------------------------------------------------
install_binary() { # <abs-binary> <want-version> <platform>
  local b=$1 want=$2 platform=$3 asset
  asset="slack-acp-${platform%%/*}-${platform##*/}"
  note "downloading $asset v$want + checksums.txt, verifying sha256, atomic rename"
  rsh "set -e; b='$b'; want='$want'; asset='$asset'; base='$RELEASE_BASE'; stamp='$STAMP'
$(cat <<'EOS'
d=$(dirname "$b"); mkdir -p "$d"
t=$(mktemp "$d/.slack-acp.new.XXXXXX"); c=$(mktemp)
trap 'rm -f "$t" "$c"' EXIT
curl -fsSL "$base/v$want/$asset" -o "$t"
curl -fsSL "$base/v$want/checksums.txt" -o "$c"
w=$(awk -v n="$asset" '$2==n || $2=="*"n {print $1; exit}' "$c")
[ -n "$w" ] || { echo "no checksum for $asset in checksums.txt" >&2; exit 3; }
h=$( { sha256sum "$t" 2>/dev/null || shasum -a 256 "$t"; } | awk '{print $1}')
[ "$h" = "$w" ] || { echo "sha256 mismatch for $asset: got $h, want $w" >&2; exit 4; }
chmod 755 "$t"
got=$("$t" --version 2>/dev/null | head -1 || true)
[ "$got" = "$want" ] || { echo "downloaded binary reports '$got', wanted $want" >&2; exit 5; }
[ -f "$b" ] && cp -p "$b" "$b.bak-$stamp"
mv -f "$t" "$b"
EOS
)" || die "binary install failed on $HOST"
}

# ---------------------------------------------------------------------------
# Recycle + verify
# ---------------------------------------------------------------------------
restart_service() { # <sup_changed>
  local changed=$1 unit label
  unit=$(jqs '.unit // "slack-acp"'); label=$(launchd_label)
  case "$(jqs '.supervisor')" in
    systemd-user)
      if [ "$changed" = 1 ]; then
        rsh "systemctl --user daemon-reload && systemctl --user enable $unit.service >/dev/null 2>&1; systemctl --user restart $unit.service"
      else
        rsh "systemctl --user restart $unit.service"
      fi ;;
    launchd)
      if [ "$changed" = 1 ] || ! rsh "launchctl print gui/\$(id -u)/$label >/dev/null 2>&1"; then
        rsh "launchctl bootout gui/\$(id -u)/$label 2>/dev/null; launchctl bootstrap gui/\$(id -u) \"\$HOME/Library/LaunchAgents/$label.plist\""
      else
        rsh "launchctl kickstart -k gui/\$(id -u)/$label"
      fi ;;
  esac
}

# wait_connected <since-epoch> — poll the log for Socket Mode's handshake.
wait_connected() {
  local since=$1 unit i
  unit=$(jqs '.unit // "slack-acp"')
  for i in $(seq 1 "$CONNECT_WAIT"); do
    case "$(jqs '.supervisor')" in
      systemd-user)
        rsh "journalctl --user -u $unit.service --since @$since --no-pager -o cat 2>/dev/null | grep -q 'slack: connected'" && return 0 ;;
      launchd)
        rsh "tail -n 200 \"\$HOME/Library/Logs/slack-acp.err.log\" \"\$HOME/Library/Logs/slack-acp.out.log\" 2>/dev/null | grep -q 'slack: connected'" && return 0 ;;
    esac
    sleep 1
  done
  return 1
}

# ---------------------------------------------------------------------------
# Converge
# ---------------------------------------------------------------------------
converge() {
  local bot=$1 apply=$2 changes=0 sup_changed=0 want host binary cur

  [ -f "$LOCK" ] || die "missing $LOCK"
  want=$(jq -r '.slack_acp' "$LOCK")
  is_release_version "$want" || die "$LOCK: .slack_acp is not a release version: $want"

  host=$(jqs '.host'); binary=$(jqs '.binary')
  SA_EXE=${binary##*/}
  HOST="$host"

  echo "== converge $bot (host=$host supervisor=$(jqs '.supervisor'))$([ -n "$TARGET_ROOT" ] && echo " [fake root: $TARGET_ROOT]")$([ "$LOCAL" = 1 ] && echo " [local]")"
  [ "$apply" = 1 ] || echo "== DRY RUN — no changes will be made (use --apply)"

  enforce_require "$bot" "$host" slack_acp "$want"

  [ "$LOCAL" = 1 ] && [ -n "$TARGET_ROOT" ] && die "--local and --target-root are mutually exclusive"
  if [ "$LOCAL" = 1 ] && [ "$FORCE_LOCAL" != 1 ]; then
    if host_is_self "$host"; then
      echo "== --local: confirmed $SELF_WHY"
    else
      die "--local refused: cannot confirm spec host '$host' is this machine. Re-run with --force-local if you are certain."
    fi
  fi
  if [ "$LOCAL" != 1 ] && [ -z "$TARGET_ROOT" ] && host_is_self "$host"; then
    LOCAL=1
    echo "== local target: $SELF_WHY — no ssh"
  fi

  rsh true || die "cannot reach target ($host)"
  if [ -n "$TARGET_ROOT" ]; then THOME=$TARGET_ROOT; else THOME=$(rsh 'printf %s "$HOME"'); fi
  [ -n "$THOME" ] || die "cannot read \$HOME on $host"

  local envf; envf=$(p_abs "$(jqs '.credentials.env_file')")
  if [ "$apply" = 1 ] && [ -z "$(rcat "$envf")" ]; then
    die "$envf is missing or empty on $host — create it first (SLACK_BOT_TOKEN + SLACK_APP_TOKEN, mode 0600); the service will not start without it"
  fi

  # -- 1. binary -------------------------------------------------------------
  local b; b=$(p_abs "$binary")
  cur=$(rsh "'$b' --version 2>/dev/null | head -1 || true")
  if [ "$cur" = "$want" ]; then
    note "slack-acp $cur ✓"
  else
    changes=$((changes + 1))
    note "slack-acp: ${cur:-missing} → $want"
    if [ "$apply" = 1 ]; then
      install_binary "$b" "$want" "$(jqs '.platform')"
      cur=$(rsh "'$b' --version 2>/dev/null | head -1 || true")
      [ "$cur" = "$want" ] || die "slack-acp still ${cur:-missing} after install (wanted $want)"
      note "slack-acp now $cur ✓"
    fi
  fi

  # -- 2. config.json --------------------------------------------------------
  local cfg want_file
  cfg=$(p_abs "$(skey config_path)")
  want_file=$(mktemp "$TMPD/want.XXXXXX"); render_config >"$want_file"
  if diff_json "config.json" "$cfg" "$want_file"; then
    note "config $cfg ✓"
  else
    changes=$((changes + 1))
    if [ "$apply" = 1 ]; then rwrite "$cfg" "$want_file"; note "config written (backup: $cfg.bak-$STAMP)"; fi
  fi

  # -- 3. supervisor definition ---------------------------------------------
  local sfile
  sfile=$(p_abs "$(supervisor_file)")
  want_file=$(mktemp "$TMPD/want.XXXXXX"); render_supervisor >"$want_file"
  if diff_text "$(jqs '.supervisor')" "$sfile" "$want_file"; then
    note "supervisor $sfile ✓"
  else
    changes=$((changes + 1)); sup_changed=1
    if [ "$apply" = 1 ]; then rwrite "$sfile" "$want_file"; note "supervisor written (backup: $sfile.bak-$STAMP)"; fi
  fi

  # -- 4. running image -----------------------------------------------------
  local state run_before pid_before ver_before stale
  state=$(probe_state)
  IFS='|' read -r run_before pid_before ver_before <<<"$state"
  stale=$(stale_decision "$run_before" "$want" "$ver_before")
  [ "${stale%%|*}" = stale ] && note "running: ${stale#*|} — restart needed despite matching files"
  [ "$run_before" = 1 ] || { note "service is not running"; changes=$((changes + 1)); }

  if [ "$changes" = 0 ] && [ "${stale%%|*}" != stale ]; then
    echo "== $bot: already converged, nothing to do${ver_before:+ (running $ver_before)}"
    return 0
  fi
  if [ "$apply" != 1 ]; then
    echo "== $bot: $changes change(s) pending, would restart the service (dry run; re-run with --apply)"
    return 0
  fi
  if [ -n "$TARGET_ROOT" ] && [ ! -x "$TARGET_ROOT/.local/bin/systemctl" ] && [ ! -x "$TARGET_ROOT/.local/bin/launchctl" ]; then
    echo "== $bot: applied to fake root; no supervisor stub, skipping restart/verify"
    return 0
  fi

  local since run pid ver i up=0
  since=$(date +%s)
  note "restarting (slack-acp has no graceful reload; Socket Mode reconnects)"
  restart_service "$sup_changed"
  for i in $(seq 1 10); do
    IFS='|' read -r run pid ver <<<"$(probe_state)"
    [ "$run" = 1 ] && { up=1; break; }
    sleep 1
  done
  [ "$up" = 1 ] || die "service not running after restart"
  if [ "$run_before" = 1 ] && [ "$pid" = "$pid_before" ]; then
    die "pid $pid_before did not move across the restart"
  fi
  if [ -n "$ver" ] && [ "$ver" != "$want" ]; then
    die "restarted service runs $ver, wanted $want"
  fi
  note "running pid ${pid_before} → ${pid}${ver:+, image $ver}"
  if [ -z "$TARGET_ROOT" ]; then
    if wait_connected "$since"; then
      note "Slack Socket Mode connected ✓"
    else
      die "no 'slack: connected' in the log ${CONNECT_WAIT}s after restart — check tokens and the journal"
    fi
  fi
  echo "== $bot: converged ($changes change(s) applied)"
}

# ---------------------------------------------------------------------------
# --tot: resolve the newest acceptable release ONCE into dist.lock, then STOP.
# ---------------------------------------------------------------------------
list_tags_slack_acp() {
  # gh first (token, drafts/prereleases excluded); git ls-remote fallback.
  local tags=""
  if command -v gh >/dev/null 2>&1; then
    tags=$(gh release list --repo "$SLACK_ACP_REPO" --limit 200 \
             --json tagName,isDraft,isPrerelease \
             -q '.[] | select(.isDraft == false and .isPrerelease == false) | .tagName' \
           2>/dev/null || true)
  fi
  if [ -z "$tags" ]; then
    tags=$(git ls-remote --tags "https://github.com/$SLACK_ACP_REPO" \
             | awk '{print $2}' | sed 's|^refs/tags/||' | grep -v '\^{}$' || true)
  fi
  printf '%s\n' "$tags" | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sed 's/^v//' | sort -V
}

tot() {
  local old new f
  old=$( [ -f "$LOCK" ] && jq -r '.slack_acp' "$LOCK" || echo none)
  for f in $(bot_specs); do
    SPEC=$(merged_spec "$f"); SPEC_SRC=$f; validate_require
  done
  SPEC=""
  echo "== tot: resolving the newest slack-acp release that satisfies every spec's require"
  # shellcheck disable=SC2046  # word splitting of the version list is intended
  new=$(resolve_component slack_acp slack-acp $(list_tags_slack_acp))
  [ -n "$new" ] || die "could not resolve the latest slack-acp release"
  if [ "$old" = "$new" ]; then echo "== tot: lock already at $new, nothing moved"; return 0; fi
  note "slack-acp: $old → $new"
  jq -n --arg v "$new" --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{slack_acp: $v, resolved_at: $at}' >"$LOCK"
  echo "== tot: dist.lock rewritten. Review the diff, commit it, then converge each bot:"
  for f in $(bot_specs); do
    echo "   scripts/converge.sh $(basename "$f" .json) --apply"
  done
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
usage() {
  sed -n '/^# Usage:/,/^# What converge/p' "$0" | sed '$d; s/^# \{0,1\}//' >&2
  exit 1
}

[ $# -ge 1 ] || usage

case "$1" in
  --tot)
    [ $# -eq 1 ] || die "--tot takes no other arguments (tot never converges)"
    tot ;;
  spec)
    [ $# -eq 2 ] || usage
    jq . "$(bot_spec "$2")" ;;
  render)
    [ $# -eq 3 ] || [ $# -eq 4 ] || usage
    SPEC=$(bot_spec "$2"); SPEC_SRC="$BOTS_DIR/$2.json"
    validate_spec
    THOME=${4:-/home/user}
    case "$3" in
      config)    render_config ;;
      execstart) render_execstart ;;
      unit)      render_unit ;;
      plist)     render_plist ;;
      *)         usage ;;
    esac ;;
  plan-stale)
    [ $# -eq 4 ] || usage
    stale_decision "$2" "$3" "$4" ;;
  semver-cmp)
    [ $# -eq 3 ] || usage
    ver_cmp "$2" "$3" ;;
  semver-sat)
    [ $# -eq 3 ] || usage
    if ver_satisfies "$2" "$3"; then echo yes; else echo no; exit 1; fi ;;
  self-host)
    [ $# -eq 2 ] || usage
    if host_is_self "$2"; then echo "self: $SELF_WHY"; else echo "remote"; exit 1; fi ;;
  resolve)
    [ $# -ge 2 ] || usage
    COMP=$2; shift 2
    resolve_component "$COMP" "$COMP" "$@" ;;
  -*) usage ;;
  *)
    BOT=$1; shift
    SPEC=$(bot_spec "$BOT"); SPEC_SRC="$BOTS_DIR/$BOT.json"
    validate_spec
    APPLY=0
    while [ $# -gt 0 ]; do
      case "$1" in
        --apply) APPLY=1; shift ;;
        --dry-run) APPLY=0; shift ;;
        --target-root)
          [ $# -ge 2 ] || usage
          TARGET_ROOT=$(cd "$2" && pwd) || die "bad --target-root"
          PROC_ROOT="$TARGET_ROOT/proc"; shift 2 ;;
        --local) LOCAL=1; shift ;;
        --force-local) LOCAL=1; FORCE_LOCAL=1; shift ;;
        *) usage ;;
      esac
    done
    converge "$BOT" "$APPLY" ;;
esac
