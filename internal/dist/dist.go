// Package dist holds the one definition of how slack-acp is distributed:
// which GitHub repo its releases come from, how its assets are named, and
// how a host recycles the service after a binary swap.
//
// Both consumers read it from here, so they cannot drift apart: the `update`
// subcommand (distkit.Main) and the generated root install.sh, whose spec is
// checked against this config by TestInstallShSpecMatchesConfig.
package dist

import (
	"os"
	"runtime"

	"github.com/kfet/distkit"
	"github.com/kfet/slack-acp/internal/installsvc"
)

// Repo is the GitHub owner/name releases are published to.
const Repo = "kfet/slack-acp"

// Binary is the installed file name.
const Binary = "slack-acp"

// RestartHint is printed after a successful `update` so the operator knows
// how to make the swapped binary actually run — the rename replaces the
// directory entry, but the live process keeps the old inode mapped until it
// re-execs. The command comes from installsvc, which owns the unit it has to
// match, and is platform-aware (systemd user unit on Linux, launchd agent on
// macOS).
func RestartHint() string {
	home, _ := os.UserHomeDir()
	return installsvc.RestartHint(runtime.GOOS, installsvc.DefaultUser(home))
}

// Config is the distkit configuration for this binary. version is the
// compiled-in version (main.version, set via -ldflags).
func Config(version string) distkit.Config {
	return distkit.Config{
		Repo:        Repo,
		Binary:      Binary,
		AssetStem:   Binary,
		Version:     version,
		RestartHint: RestartHint(),
	}
}
