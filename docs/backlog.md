# Backlog

Things deliberately not done, with the reason.

## Remote agent hosts

- **The per-conversation cwd and staged attachments are created on THIS host,
  not the agent's.** Same bug `poe-acp` fixed in v0.68.1: with an agent spawned
  over ssh (`--agent-cmd "ssh -T box fir --mode acp"`) the relay sends a `cwd`
  on `session/new` that exists only on its own disk, and the agent — which
  takes `cwd` as-is, with no stat and no mkdir — silently falls back to `$HOME`.
  Every conversation then shares one directory, staged input attachments are
  absent, and files the agent writes for upload cannot be read back. Nothing
  errors anywhere; the failure is entirely invisible.

  The primitive is already shared: `acp-kit/remotefs` (`Mkdir` / `Push` /
  `Fetch`, ssh+tar, BatchMode, argv only, bounded, remote paths quoted;
  `remotefs.Local` is the no-op for a local agent). Adopt the same operator
  key, `agent_ssh_host`; provision before session ACQUISITION (`session/load`
  and `session/resume` carry the cwd too, not just `session/new`); and fail
  session creation loudly rather than falling through to a wrong cwd, because
  falling through is what makes the bug invisible.
