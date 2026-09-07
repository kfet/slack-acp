package dist_test

import (
	"os"
	"os/exec"
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

// TestDeployBuildsAreRefusedButCanBePinned documents the contract for the
// binary `make deploy` ships. The Makefile stamps an untagged build
// "<version>-dev+<sha>[.dirty]", which distkit classifies as a working-tree
// build and refuses to self-update off `latest` — correct, because that same
// version string is also what a developer's own ./bin/slack-acp carries, and
// silently renaming a release binary over someone's working tree is exactly
// what the guard exists to prevent.
//
// The rollback path off a hand-deployed binary is not gone, it is explicit:
// `slack-acp update -version vX.Y.Z` names one release instead of chasing
// latest, and gets past the guard. The refusal message says so.
func TestDeployBuildsAreRefusedButCanBePinned(t *testing.T) {
	if !distkit.IsDevBuild("dev") {
		t.Error("bare dev version should be refused by distkit")
	}
	if !distkit.IsDevBuild("0.5.0-dev+abc1234.dirty") {
		t.Error("a Makefile-stamped deploy build must be treated as a working-tree build; " +
			"appending a commit sha must not disarm the guard")
	}
	if !distkit.IsDevBuild("0.5.0-dev+abc1234") {
		t.Error("a clean Makefile-stamped deploy build must also be refused")
	}
	// A real release — tagged, with published assets — still updates.
	if distkit.IsDevBuild("0.5.0") {
		t.Error("a release tag must never be mistaken for a dev build")
	}
	if distkit.IsDevBuild("0.5.0-rc1") {
		t.Error("a prerelease is a real tag with real assets and must still update")
	}
}

// TestInstallShRefusesATraversingVersion is a regression test for the path
// traversal fixed in distkit v0.1.6. VERSION is pasted into two URL paths, so
// "../../other/repo/releases/download/v1" used to walk out of this repo and
// install ANOTHER project's binary — and because checksums.txt was fetched
// from that same traversed location, it verified against itself and printed
// "checksum ok".
//
// TestInstallShIsNotDrifted cannot catch a regression here: it only proves
// install.sh matches whatever the template currently produces, so an upstream
// change that dropped the guard would regenerate cleanly and pass. This runs
// the installer instead and asserts the refusal, which is the actual
// contract.
//
// PATH is replaced with a directory holding only shims that scream, so the
// test also proves the refusal happens BEFORE any network access — a guard
// that rejects a bad tag after downloading with it is not a guard.
func TestInstallShRefusesATraversingVersion(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh: %v", err)
	}
	realPATH := os.Getenv("PATH")
	shim := t.TempDir()
	for _, name := range []string{"curl", "wget"} {
		script := "#!/bin/sh\necho \"NETWORK ATTEMPTED: " + name + " $*\" >&2\nexit 99\n"
		if err := os.WriteFile(filepath.Join(shim, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write %s shim: %v", name, err)
		}
	}

	for _, version := range []string{
		"../../kfet/poe-acp/releases/download/v1",
		"release/v1",
		"v1.2.3 ; rm -rf /",
		"-oops",
		// ".." carries no slash, so a slash-only check lets it through —
		// but every URL normaliser still reads it as "up one", which turns
		// the API's /releases/tags/<tag> into the releases LIST endpoint
		// and resolves a release nobody asked for.
		"..",
		".",
		".v1",
	} {
		cmd := exec.Command("sh", repoFile("install.sh"))
		cmd.Env = append(os.Environ(),
			"PATH="+shim+string(os.PathListSeparator)+realPATH,
			"VERSION="+version,
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("VERSION=%q was accepted; install.sh has no traversal guard:\n%s", version, out)
			continue
		}
		if !strings.Contains(string(out), "bad VERSION") {
			t.Errorf("VERSION=%q: want a 'bad VERSION' refusal, got:\n%s", version, out)
		}
		if strings.Contains(string(out), "NETWORK ATTEMPTED") {
			t.Errorf("VERSION=%q was rejected only after fetching with it:\n%s", version, out)
		}
	}
}

// TestInstallShAcceptsAReleaseTag is the other half: the guard must reject a
// traversing tag without also rejecting the ordinary ones, or every install
// breaks. A real tag gets past validation and reaches the download, where the
// screaming shim stops it — reaching the network IS the pass condition here.
//
// The empty string is in the list deliberately: `curl … | sh` with no VERSION
// set leaves it empty, the script defaults it to "latest", and the guard must
// let that through rather than refusing the overwhelmingly common install.
func TestInstallShAcceptsAReleaseTag(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh: %v", err)
	}
	realPATH := os.Getenv("PATH")
	shim := t.TempDir()
	for _, name := range []string{"curl", "wget"} {
		script := "#!/bin/sh\necho \"NETWORK ATTEMPTED: " + name + " $*\" >&2\nexit 99\n"
		if err := os.WriteFile(filepath.Join(shim, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write %s shim: %v", name, err)
		}
	}

	for _, version := range []string{"v0.6.0", "0.6.0", "v1.2.3-rc1", "latest", ""} {
		cmd := exec.Command("sh", repoFile("install.sh"))
		cmd.Env = append(os.Environ(),
			"PATH="+shim+string(os.PathListSeparator)+realPATH,
			"VERSION="+version,
		)
		out, _ := cmd.CombinedOutput()
		if strings.Contains(string(out), "bad VERSION") {
			t.Errorf("VERSION=%q is an ordinary release tag and must be accepted:\n%s", version, out)
		}
		// Asserting only the absence of a refusal would pass vacuously if
		// install.sh died before ever reaching the guard — a syntax error
		// or a botched regeneration would look like success. Requiring
		// that a fetch was actually attempted proves the tag got all the
		// way through validation.
		if !strings.Contains(string(out), "NETWORK ATTEMPTED") {
			t.Errorf("VERSION=%q never reached a download; install.sh failed before the guard:\n%s", version, out)
		}
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
