#!/usr/bin/env bash
# shellcheck disable=SC2015  # `check && ok || bad` is the assertion idiom; ok/bad never fail
# converge_render.sh — offline tests for scripts/converge.sh. Synthetic
# fixtures only (test/fixtures/bots); never reads the real fleet registry.
#
#   1. merge: distro.json <- bot file (deep merge, null unsets, arrays replace,
#      bot key order kept, managed:false skips the merge)
#   2. render: golden files for config / execstart / unit / plist
#      (UPDATE=1 rewrites them)
#   3. byte-compat: the unit and plist match `slack-acp install-service`
#   4. validation, relay filter, semver/require/resolve, plan-stale
#   5. fake-root converge: dry run, --apply (checksum-verified atomic binary
#      swap from a file:// release, config, unit, restart via a systemctl
#      stub), idempotence, checksum mismatch leaves the binary untouched
#   6. check-no-leak.sh: skip without a registry, fail on a planted id
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CV="$ROOT/scripts/converge.sh"
FIX="$ROOT/test/fixtures/bots"
GOLD="$ROOT/test/golden"
export FLEET_BOTS_DIR="$FIX"
unset FLEET_DISTRO FLEET_LOCK
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
pass=0; fail=0
ok()  { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $*" >&2; }
eq()  { [ "$2" = "$3" ] && ok || bad "$1: got [$2] want [$3]"; }

# -- 1. merge -----------------------------------------------------------------
s=$("$CV" spec bot-a)
eq "merge: default binary"       "$(jq -r .binary <<<"$s")" "~/.local/bin/slack-acp"
eq "merge: default supervisor"   "$(jq -r .supervisor <<<"$s")" "systemd-user"
eq "merge: bot config kept"      "$(jq -c .config.agent_cmd <<<"$s")" '["/home/user-a/.local/bin/fir","--mode","acp"]'
eq "merge: bot key order first"  "$(jq -r 'keys_unsorted[0]' <<<"$s")" "name"
s=$("$CV" spec bot-b)
eq "merge: array replaced"       "$(jq -c .config.agent_cmd <<<"$s")" '["claude-code","--acp"]'
eq "merge: null unsets default"  "$(jq -r 'has("require")' <<<"$s")" "false"
eq "merge: nested default kept"  "$(jq -r .service.config_path <<<"$s")" "~/.config/slack-acp/config.json"
eq "merge: nested bot added"     "$(jq -r .service.state_dir <<<"$s")" "~/Library/slack-acp/state"
s=$("$CV" spec bot-d)
eq "merge: config null unsets"   "$(jq -r '.config | has("agent_cmd")' <<<"$s")" "false"
eq "merge: systemd override"     "$(jq -r .systemd.restart_sec <<<"$s")" "5s"
eq "merge: systemd default kept" "$(jq -r .systemd.restart <<<"$s")" "on-failure"
mkdir -p "$T/unmanaged"
jq '. + {managed: false} | del(.binary)' "$FIX/bot-a.json" >"$T/unmanaged/bot-u.json"
eq "merge: managed:false skips"  "$(FLEET_BOTS_DIR=$T/unmanaged "$CV" spec bot-u | jq -r '.binary // "none"')" "none"

# -- 2. golden renders ----------------------------------------------------------
render_all() { # <bot> <home> <artefacts...>
  local bot=$1 home=$2 a; shift 2
  for a in "$@"; do
    out=$("$CV" render "$bot" "$a" "$home") || { bad "render $bot $a failed"; continue; }
    if [ "${UPDATE:-0}" = 1 ]; then printf '%s\n' "$out" >"$GOLD/$bot.$a"; ok; continue; fi
    [ "$out" = "$(cat "$GOLD/$bot.$a" 2>/dev/null)" ] && ok \
      || bad "render $bot $a differs from golden: $(diff <(printf '%s\n' "$out") "$GOLD/$bot.$a" | head -5)"
  done
}
render_all bot-a /home/user-a config execstart unit
render_all bot-b /Users/user-b config execstart plist
render_all bot-d /home/user-d config execstart unit

# -- 3. byte-compat with install-service ----------------------------------------
SA="$T/slack-acp"
if (cd "$ROOT" && go build -o "$SA" ./cmd/slack-acp) 2>"$T/build.err"; then
  want=$("$SA" install-service --dry-run --goos linux --binary /home/user-a/.local/bin/slack-acp \
          --config /home/user-a/.config/slack-acp/config.json --env /home/user-a/.config/slack-acp/env 2>/dev/null | sed "/^# would write/d")
  eq "install-service compat: unit" "$("$CV" render bot-a unit /home/user-a)" "$want"
  want=$(HOME=/Users/user-b USER=user-b "$SA" install-service --dry-run --goos darwin \
          --binary /opt/homebrew/bin/slack-acp --config /Users/user-b/.config/slack-acp/config.json \
          --env /Users/user-b/.config/slack-acp/env 2>/dev/null | sed "/^# would write/d")
  jq 'del(.service.state_dir)' "$FIX/bot-b.json" >"$T/unmanaged/bot-b2.json"
  got=$(FLEET_BOTS_DIR=$T/unmanaged "$CV" render bot-b2 plist /Users/user-b)
  eq "install-service compat: plist" "$got" "$want"
else
  bad "go build: $(cat "$T/build.err")"
fi

# -- 4. validation, relay filter, semver ----------------------------------------
"$CV" render bot-c unit >/dev/null 2>&1 && bad "zulip-acp bot accepted" || ok
mkdir -p "$T/badspec"
for kv in '.binary="~/bin/x;rm -rf ~"' '.credentials.env_file="~/a b"' '.service.config_path="~/$(id)"' \
          '.supervisor="runit"' '.platform="plan9/mips"' '.require={"fir":">=1.0.0"}' \
          '.require={"slack_acp":"newest"}' '.unit="a%h"' '.config=[]'; do
  jq "$kv" "$FIX/bot-a.json" >"$T/badspec/bot-x.json"
  FLEET_BOTS_DIR=$T/badspec "$CV" render bot-x unit >/dev/null 2>&1 && bad "accepted bad spec: $kv" || ok
done
eq "semver-cmp"            "$("$CV" semver-cmp 0.9.0 0.10.0)" "-1"
eq "semver-sat dev build"  "$("$CV" semver-sat 0.11.1-dev+abc '>=0.11.0')" "no"
eq "semver-sat ~>"         "$("$CV" semver-sat 0.11.9 '~>0.11.0')" "yes"
eq "resolve honours bot-d" "$("$CV" resolve slack_acp 0.10.0 0.11.0 0.11.4 0.12.0 2>/dev/null)" "0.11.4"
eq "plan-stale old image"  "$("$CV" plan-stale 1 0.11.2 0.11.1)" "stale|running image is 0.11.1, wanted 0.11.2"
eq "plan-stale unreadable" "$("$CV" plan-stale 1 0.11.2 '')" "current|running version unreadable"

# -- 5. fake-root converge ---------------------------------------------------
R="$T/root"; REL="$T/rel"; mkdir -p "$R/.local/bin" "$R/.config/slack-acp" "$R/proc" "$REL/v0.11.2"
printf 'SLACK_BOT_TOKEN=xoxb-test\nSLACK_APP_TOKEN=xapp-test\n' >"$R/.config/slack-acp/env"
mkbin() { printf '#!/bin/sh\n[ "$1" = --version ] && echo %s\n' "$1" >"$2"; chmod +x "$2"; }
mkbin 0.11.1 "$R/.local/bin/slack-acp"
mkbin 0.11.2 "$REL/v0.11.2/slack-acp-linux-amd64"
(cd "$REL/v0.11.2" && sha256sum slack-acp-linux-amd64 >checksums.txt)
printf '{"slack_acp":"0.11.2"}\n' >"$T/lock"
# systemctl stub: records calls; "restart" bumps the pid. MainPID points at a
# fake /proc entry whose exe is the installed binary.
cat >"$R/.local/bin/systemctl" <<STUB
#!/bin/sh
echo "\$*" >>"$R/systemctl.log"
case "\$*" in
  *"show -p MainPID"*) cat "$R/pid" 2>/dev/null || echo 0 ;;
  *is-active*) [ -f "$R/pid" ] && echo active || echo inactive ;;
  *restart*) n=\$(( \$(cat "$R/pid" 2>/dev/null || echo 100) + 1 )); echo \$n >"$R/pid"
             mkdir -p "$R/proc/\$n"; ln -sf "$R/.local/bin/slack-acp" "$R/proc/\$n/exe" ;;
esac
exit 0
STUB
chmod +x "$R/.local/bin/systemctl"
echo 100 >"$R/pid"; mkdir -p "$R/proc/100"; cp -p "$R/.local/bin/slack-acp" "$R/proc/old-image"
ln -sf "$R/proc/old-image" "$R/proc/100/exe"
mv "$R/proc/old-image" "$R/proc/slack-acp.running"; ln -sf "$R/proc/slack-acp.running" "$R/proc/100/exe"
export FLEET_LOCK="$T/lock" SLACK_ACP_RELEASE_BASE="file://$REL" SETTLE_WAIT=0

out=$("$CV" bot-a --target-root "$R" 2>&1)
grep -q "slack-acp: 0.11.1 → 0.11.2" <<<"$out" && ok || bad "dry run: no binary change: $out"
grep -q "would restart" <<<"$out" && ok || bad "dry run: no restart planned: $out"
[ ! -f "$R/.config/slack-acp/config.json" ] && ok || bad "dry run wrote config"
eq "dry run kept binary" "$("$R/.local/bin/slack-acp" --version)" "0.11.1"

# checksum mismatch: nothing swapped
cp "$REL/v0.11.2/checksums.txt" "$T/sums"
sed -i 's/^[0-9a-f]\{4\}/0000/' "$REL/v0.11.2/checksums.txt"
"$CV" bot-a --target-root "$R" --apply >"$T/out" 2>&1 && bad "apply passed with a bad checksum" || ok
grep -q "sha256 mismatch" "$T/out" && ok || bad "no sha256 mismatch message: $(cat "$T/out")"
eq "bad checksum kept binary" "$("$R/.local/bin/slack-acp" --version)" "0.11.1"
cp "$T/sums" "$REL/v0.11.2/checksums.txt"

out=$("$CV" bot-a --target-root "$R" --apply 2>&1) || bad "apply failed: $out"
eq "apply swapped binary" "$("$R/.local/bin/slack-acp" --version)" "0.11.2"
ls "$R/.local/bin/slack-acp.bak-"* >/dev/null 2>&1 && ok || bad "no binary backup"
ls "$R/.local/bin/.slack-acp.new."* >/dev/null 2>&1 && bad "temp binary left behind" || ok
eq "apply wrote config" "$(jq -c . "$R/.config/slack-acp/config.json")" "$(jq -c .config "$FIX/bot-a.json")"
eq "apply wrote unit" "$(cat "$R/.config/systemd/user/slack-acp.service")" "$("$CV" render bot-a unit "$R")"
eq "new config is 0600" "$(stat -c %a "$R/.config/slack-acp/config.json")" "600"
grep -q "daemon-reload" "$R/systemctl.log" && ok || bad "unit change without daemon-reload"
grep -q "restart slack-acp.service" "$R/systemctl.log" && ok || bad "no restart"
grep -q "converged (" <<<"$out" && ok || bad "apply did not report converged: $out"

out=$("$CV" bot-a --target-root "$R" 2>&1)
grep -q "already converged" <<<"$out" && ok || bad "second run not idempotent: $out"

# config written without a trailing newline (as slack-acp init does) is not a change
printf '%s' "$(jq . "$R/.config/slack-acp/config.json")" >"$R/c" && mv "$R/c" "$R/.config/slack-acp/config.json"
out=$("$CV" bot-a --target-root "$R" 2>&1)
grep -q "already converged" <<<"$out" && ok || bad "missing trailing newline counted as a change: $out"

# a running image older than the lock is stale even when the files match
n=$(cat "$R/pid"); cp -p "$R/.local/bin/slack-acp.bak-"* "$R/proc/slack-acp.old"
ln -sf "$R/proc/slack-acp.old" "$R/proc/$n/exe"
out=$("$CV" bot-a --target-root "$R" 2>&1)
grep -q "restart needed despite matching files" <<<"$out" && ok || bad "stale image not detected: $out"

# a missing env file aborts --apply before anything is written
rm "$R/.config/slack-acp/env"
"$CV" bot-a --target-root "$R" --apply >"$T/out" 2>&1 && bad "apply without env file passed" || ok
grep -q "missing or empty" "$T/out" && ok || bad "no env-file message: $(cat "$T/out")"

# -- 6. check-no-leak ---------------------------------------------------------
out=$(FLEET_BOTS_DIR="$T/none" "$ROOT/scripts/check-no-leak.sh" 2>&1)
grep -q "skipped" <<<"$out" && ok || bad "no-leak did not skip without a registry: $out"
G="$T/gitrepo"; mkdir -p "$G/scripts" "$T/reg"
cp "$ROOT/scripts/check-no-leak.sh" "$G/scripts/"
printf '{"name":"bot-z","host":"host-z","slack":{"team_id":"T0LEAKED1"}}\n' >"$T/reg/bot-z.json"
( cd "$G" && git init -q && echo "nothing here" >README && git add -A && git -c user.name=t -c user.email=t@example.invalid commit -qm init )
FLEET_BOTS_DIR="$T/reg" "$G/scripts/check-no-leak.sh" >/dev/null 2>&1 && ok || bad "no-leak failed on a clean tree"
( cd "$G" && echo "team T0LEAKED1" >>README && git add -A )
FLEET_BOTS_DIR="$T/reg" "$G/scripts/check-no-leak.sh" >/dev/null 2>&1 && bad "no-leak missed a Slack team id" || ok

echo "converge_render: $pass passed, $fail failed"
[ "$fail" = 0 ]
