package dist_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kfet/distkit"
	"github.com/kfet/distkit/installsh"
	"github.com/kfet/slack-acp/internal/dist"
	"github.com/kfet/slack-acp/internal/installsvc"
)

// repoFile resolves a path at the repository root from this package's dir.
func repoFile(name string) string { return filepath.Join("..", "..", name) }

func TestConfig(t *testing.T) {
	cfg := dist.Config("1.2.3")
	if cfg.Repo != "kfet/slack-acp" {
		t.Errorf("Repo = %q", cfg.Repo)
	}
	if cfg.Binary != "slack-acp" || cfg.AssetStem != "slack-acp" {
		t.Errorf("Binary/AssetStem = %q/%q", cfg.Binary, cfg.AssetStem)
	}
	if cfg.Version != "1.2.3" {
		t.Errorf("Version = %q", cfg.Version)
	}
	// The hint must name the service unit internal/installsvc actually
	// writes for THIS platform, and must be a restart: slack-acp has no
	// supervisor and no SIGHUP handler, so `systemctl --user reload` (or a
	// systemctl hint on a Mac) would fail on a fleet host.
	home, _ := os.UserHomeDir()
	want := installsvc.RestartHint(runtime.GOOS, installsvc.DefaultUser(home))
	if cfg.RestartHint != want {
		t.Errorf("RestartHint = %q, want %q", cfg.RestartHint, want)
	}
	if !strings.Contains(cfg.RestartHint, "restart") && !strings.Contains(cfg.RestartHint, "kickstart") {
		t.Errorf("RestartHint %q is neither a restart nor a kickstart", cfg.RestartHint)
	}
	// Checksum verification and Homebrew handling are on: both are load
	// -bearing on production fleet hosts and must never be silently
	// disabled by an edit to Config.
	if cfg.SkipChecksums {
		t.Error("SkipChecksums must stay false: unverified downloads land on fleet hosts")
	}
	if cfg.DisableBrew {
		t.Error("DisableBrew must stay false: a brew keg swap is reverted by the next brew upgrade")
	}
}

// TestInstallShSpecMatchesConfig keeps install.sh.json and dist.Config from
// disagreeing about where releases come from or how assets are named — the
// installer and the self-updater must resolve the same bytes.
func TestInstallShSpecMatchesConfig(t *testing.T) {
	spec, err := installsh.LoadSpec(repoFile("install.sh.json"))
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	want := installsh.FromConfig(dist.Config("1.2.3"))
	if spec.Repo != want.Repo {
		t.Errorf("spec repo = %q, config repo = %q", spec.Repo, want.Repo)
	}
	if spec.Binary != want.Binary {
		t.Errorf("spec binary = %q, config binary = %q", spec.Binary, want.Binary)
	}
	// Both empty means both take distkit's defaults; any other combination
	// is a divergence.
	if spec.AssetStem != "" && spec.AssetStem != want.Binary {
		t.Errorf("spec asset_stem = %q, want %q or empty", spec.AssetStem, want.Binary)
	}
	if spec.AssetTemplate != want.AssetTemplate {
		t.Errorf("spec asset_template = %q, config = %q", spec.AssetTemplate, want.AssetTemplate)
	}
	if spec.ArmSuffix != want.ArmSuffix {
		t.Errorf("spec arm_suffix = %q, config = %q", spec.ArmSuffix, want.ArmSuffix)
	}
	if spec.NoChecksums {
		t.Error("install.sh.json must not disable checksum verification")
	}
	if spec.ChecksumsAsset != want.ChecksumsAsset {
		t.Errorf("spec checksums_asset = %q, config = %q", spec.ChecksumsAsset, want.ChecksumsAsset)
	}
}

// TestAssetNamingMatchesGoReleaser pins the asset name the update path and
// the installer resolve to the one the release workflow actually uploads. A
// rename in .goreleaser.yaml would otherwise be invisible until every host
// failed to update.
func TestAssetNamingMatchesGoReleaser(t *testing.T) {
	data, err := os.ReadFile(repoFile(".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	// GoReleaser's Go-template spelling of distkit's {stem}-{os}-{arch},
	// with the GOARM suffix appended for the 32-bit ARM build.
	const want = `name_template: "slack-acp-{{ .Os }}-{{ .Arch }}{{ if .Arm }}v{{ .Arm }}{{ end }}"`
	if !strings.Contains(string(data), want) {
		t.Errorf(".goreleaser.yaml no longer contains\n  %s\nasset naming has drifted from distkit.DefaultAssetTemplate (%q) + ArmSuffix armv6",
			want, distkit.DefaultAssetTemplate)
	}
	if !strings.Contains(string(data), "name_template: checksums.txt") {
		t.Errorf(".goreleaser.yaml no longer publishes %q, which the update path verifies against", distkit.DefaultChecksums)
	}
}

// TestDevBuildsAreNotRefused documents a deliberate property: the Makefile
// stamps an untagged build "<version>-dev+<sha>[.dirty]", which distkit does
// NOT classify as a placeholder dev version (only bare "dev", "unknown",
// "snapshot", … are). So a host running a `make deploy` pre-release build can
// still `slack-acp update` its way back onto a real release — which is what
// an operator wants — while a plain `dev` build is refused.
func TestDevBuildsAreNotRefused(t *testing.T) {
	if !distkit.IsDevBuild("dev") {
		t.Error("bare dev version should be refused by distkit")
	}
	if distkit.IsDevBuild("0.5.0-dev+abc1234.dirty") {
		t.Error("a Makefile-stamped deploy build is no longer updatable; the rollback path off a hand-deployed binary is gone")
	}
}

// TestInstallShIsNotDrifted fails when the checked-in install.sh no longer
// matches what the distkit template produces — a hand edit, or a template
// change picked up by a dependency bump. Regenerate with `make install.sh`.
func TestInstallShIsNotDrifted(t *testing.T) {
	spec, err := installsh.LoadSpec(repoFile("install.sh.json"))
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if err := installsh.CheckDrift(repoFile("install.sh"), spec); err != nil {
		t.Fatal(err)
	}
}
