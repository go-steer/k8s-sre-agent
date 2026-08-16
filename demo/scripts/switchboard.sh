#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Run switchboard against a real Slack workspace, outbound only.
#
#   ./scripts/switchboard.sh          # foreground; ^C to stop
#
# monitor.sh starts this itself when scripts/slack.env is present, so you only
# need to run it directly if you want switchboard in its own terminal — which
# is worth doing the first time, because its startup log is where a bad token
# or an unreachable channel says so.
#
# Credentials come from scripts/slack.env via common.sh, and reach switchboard
# through the environment. Nothing here puts a token on a command line.

source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

need_bin switchboard

slack_configured ||
  die "no Slack credentials — put SWITCHBOARD_SLACK_APP_TOKEN and SWITCHBOARD_SLACK_BOT_TOKEN in $SLACK_ENV (see slack.env.example)"

# The allowlist is not optional here even though switchboard treats it as such.
# Empty means the ingress may post into any conversation the bot can reach, and
# switchboard says so in a startup warning; a demo that can post anywhere is a
# demo one typo away from posting somewhere real.
[[ -n "${SLACK_CONVERSATION:-}" ]] ||
  die "SLACK_CONVERSATION is unset — set it in $SLACK_ENV to the channel ID (Slack: channel name → View channel details → the C… id at the bottom), and invite the bot with /invite @switchboard"

say "switchboard: Slack ingress on $INGRESS_ADDR, posting only to $SLACK_CONVERSATION"
exec "$BIN/switchboard" serve \
  --platform slack \
  --ingress-addr "$INGRESS_ADDR" \
  --ingress-allow "$SLACK_CONVERSATION"
