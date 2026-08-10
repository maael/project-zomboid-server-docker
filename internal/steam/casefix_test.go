package steam

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/faudil/project-zomboid-server-docker/internal/config"
)

func TestEnsureCaseAliasesCreatesLowercaseAliases(t *testing.T) {
	cfg := testConfig(t)
	base := filepath.Join(cfg.ServerDir, "steamapps/workshop/content/108600/3722134990/mods")
	actions := filepath.Join(base, "GunsOfMarz", "42.16", "media", "AnimSets", "player-vehicle", "actions")
	writeModInfo(t, filepath.Join(actions, "RackShotgun_HB.xml"), `<animNode x_extends="LoadShotgun_HB.xml"></animNode>`)
	writeModInfo(t, filepath.Join(actions, "LoadShotgun_HB.xml"), `<animNode></animNode>`)

	n, err := EnsureCaseAliases(cfg)
	if err != nil {
		t.Fatalf("EnsureCaseAliases: %v", err)
	}
	if n == 0 {
		t.Fatal("EnsureCaseAliases created no aliases")
	}

	// The exact lowercase paths the game resolves must exist and point at the
	// real files: folder alias, dir alias, file alias.
	cases := []struct{ alias, real string }{
		{filepath.Join(base, "gunsofmarz"), filepath.Join(base, "GunsOfMarz")},
		{filepath.Join(base, "GunsOfMarz", "42.16", "media", "animsets"), filepath.Join(base, "GunsOfMarz", "42.16", "media", "AnimSets")},
		{filepath.Join(actions, "loadshotgun_hb.xml"), filepath.Join(actions, "LoadShotgun_HB.xml")},
		{filepath.Join(actions, "rackshotgun_hb.xml"), filepath.Join(actions, "RackShotgun_HB.xml")},
	}
	for _, tc := range cases {
		target, err := os.Readlink(tc.alias)
		if err != nil {
			t.Errorf("alias %s missing: %v", tc.alias, err)
			continue
		}
		if target != tc.real {
			t.Errorf("alias %s -> %s, want %s", tc.alias, target, tc.real)
		}
	}
	// The resolved lowercase path must be traversable and readable.
	data, err := os.ReadFile(filepath.Join(base, "gunsofmarz", "42.16", "media", "animsets", "player-vehicle", "actions", "loadshotgun_hb.xml"))
	if err != nil {
		t.Errorf("reading through aliases: %v", err)
	}
	if string(data) != "<animNode></animNode>" {
		t.Errorf("alias content mismatch: %q", data)
	}

	// Idempotent: a second pass creates nothing new.
	n2, err := EnsureCaseAliases(cfg)
	if err != nil {
		t.Fatalf("EnsureCaseAliases (2nd run): %v", err)
	}
	if n2 != 0 {
		t.Errorf("second run created %d more aliases, want 0", n2)
	}
}

func TestEnsureCaseAliasesSkipsModsWithoutExtends(t *testing.T) {
	cfg := testConfig(t)
	base := filepath.Join(cfg.ServerDir, "steamapps/workshop/content/108600/999/mods")
	// Mixed-case files but no x_extends/x_include: nothing to fix.
	writeModInfo(t, filepath.Join(base, "BigCamelMod", "media", "SomeFile.TXT"), "data")
	writeModInfo(t, filepath.Join(base, "BigCamelMod", "media", "plain.xml"), "<animNode></animNode>")

	n, err := EnsureCaseAliases(cfg)
	if err != nil {
		t.Fatalf("EnsureCaseAliases: %v", err)
	}
	if n != 0 {
		t.Errorf("created %d aliases for a mod without x_extends, want 0", n)
	}
	if _, err := os.Lstat(filepath.Join(base, "bigcamelmod")); err == nil {
		t.Error("folder alias must not exist for a mod without x_extends")
	}
}

func TestEnsureCaseAliasesSkipsNameCollisions(t *testing.T) {
	cfg := testConfig(t)
	base := filepath.Join(cfg.ServerDir, "steamapps/workshop/content/108600/999/mods")
	actions := filepath.Join(base, "MixedMod", "media", "AnimSets")
	writeModInfo(t, filepath.Join(actions, "RackShotgun_HB.xml"), `<animNode x_extends="LoadShotgun_HB.xml"></animNode>`)
	// A real lowercase file with the same name already exists: the alias must
	// not overwrite it, and it must stay a regular file.
	lower := filepath.Join(actions, "loadshotgun_hb.xml")
	writeModInfo(t, lower, "real-lowercase")

	if _, err := EnsureCaseAliases(cfg); err != nil {
		t.Fatalf("EnsureCaseAliases: %v", err)
	}
	fi, err := os.Lstat(lower)
	if err != nil {
		t.Fatalf("lowercase file gone: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("existing lowercase file was replaced by a symlink")
	}
}

func TestEnsureCaseAliasesKeepsDiscoveryIntact(t *testing.T) {
	cfg := testConfig(t)
	base := filepath.Join(cfg.ServerDir, "steamapps/workshop/content/108600/3722134990/mods")
	writeModInfo(t, filepath.Join(base, "GunsOfMarz", "42.16", "media", "AnimSets", "RackShotgun_HB.xml"),
		`<animNode x_extends="LoadShotgun_HB.xml"></animNode>`)
	writeModInfo(t, filepath.Join(base, "GunsOfMarz", "mod.info"), "id=MarzGuns\n")

	if _, err := EnsureCaseAliases(cfg); err != nil {
		t.Fatalf("EnsureCaseAliases: %v", err)
	}

	// The folder alias is a symlink: discovery must not see a second mod.
	names := DiscoverModNames(cfg)
	if len(names) != 1 || names[0] != "MarzGuns" {
		t.Errorf("DiscoverModNames = %v, want [MarzGuns]", names)
	}
	if got := ModCountOnDisk(cfg); got != 1 {
		t.Errorf("ModCountOnDisk = %d, want 1 (symlink folder must not be counted)", got)
	}
}

func TestEnsureCaseAliasesDisabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.ModCaseAliases = false
	base := filepath.Join(cfg.ServerDir, "steamapps/workshop/content/108600/999/mods")
	writeModInfo(t, filepath.Join(base, "GunsOfMarz", "media", "AnimSets", "RackShotgun_HB.xml"),
		`<animNode x_extends="LoadShotgun_HB.xml"></animNode>`)

	n, err := EnsureCaseAliases(cfg)
	if err != nil {
		t.Fatalf("EnsureCaseAliases: %v", err)
	}
	if n != 0 {
		t.Errorf("created %d aliases with MOD_CASE_ALIASES=false, want 0", n)
	}
	if _, err := os.Lstat(filepath.Join(base, "gunsofmarz")); err == nil {
		t.Error("folder alias must not exist when disabled")
	}
}

func TestEnsureCaseAliasesConfigDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	if !cfg.ModCaseAliases {
		t.Error("MOD_CASE_ALIASES should default to true")
	}
}
