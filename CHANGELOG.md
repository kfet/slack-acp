# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

## [0.1.0] - 2026-05-06

### Added
- Initial v0: Socket Mode Slack client, ACP agent process wrapper,
  per-thread session routing, throttled streaming message updates,
  in-flight prompt cancellation on follow-up, per-session cwd.
- Permission policies: `allow-all`, `read-only`, `deny-all`.
- User and channel allowlists.
- JSON config (DisallowUnknownFields) + env overrides for tokens.
