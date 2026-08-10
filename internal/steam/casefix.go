package steam

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/faudil/project-zomboid-server-docker/internal/config"
)

// EnsureCaseAliases creates lowercase symlink aliases for workshop mod files
// whose names (or folder names) contain uppercase characters.
//
// Why this is needed: Project Zomboid build 42 resolves x_extends and
// x_include animation XML references by lowercasing the entire resolved path
// (ZomboidFileSystem.getRelativeFile) and opening it directly on disk. On
// Windows - where modders develop - the filesystem is case-insensitive, so
// mixed-case mods like "GunsOfMarz/.../LoadShotgun_HB.xml" work. On a Linux
// dedicated server the lowercased path does not exist and every referenced
// file throws FileNotFoundException, silently dropping the anim nodes.
//
// The aliases are symlinks only: real files are never renamed or moved, so
// Steam re-downloads and mod updates are unaffected. The pass is idempotent
// and re-run on every boot, so it self-heals after Steam replaces an item.
// Only mods whose XMLs actually use x_extends/x_include with uppercase path
// components get aliases; everything else is left untouched.
//
// Set MOD_CASE_ALIASES=false to disable the workaround entirely.
func EnsureCaseAliases(cfg *config.ServerConfig) (int, error) {
	if !cfg.ModCaseAliases {
		fmt.Println("MOD_CASE_ALIASES=false: lowercase symlink aliases for workshop mods are disabled")
		return 0, nil
	}

	total := 0
	itemDirs, err := os.ReadDir(workshopDir(cfg))
	if err == nil {
		for _, item := range itemDirs {
			if !item.IsDir() {
				continue
			}
			itemPath := filepath.Join(workshopDir(cfg), item.Name())
			// Standard layout: <item>/mods/<ModName>/
			if sub, err := os.ReadDir(filepath.Join(itemPath, "mods")); err == nil {
				for _, d := range sub {
					if d.IsDir() {
						n := ensureModCaseAliases(filepath.Join(itemPath, "mods", d.Name()), d.Name(), filepath.Join(itemPath, "mods"))
						total += n
					}
				}
			}
			// Legacy layout: <item>/<ModName>/
			if sub, err := os.ReadDir(itemPath); err == nil {
				for _, d := range sub {
					if d.IsDir() && d.Name() != "mods" {
						n := ensureModCaseAliases(filepath.Join(itemPath, d.Name()), d.Name(), itemPath)
						total += n
					}
				}
			}
		}
	}

	// Manual mods dropped into <DATA_DIR>/Workshop/
	manualDir := filepath.Join(cfg.DataDir, "Workshop")
	if sub, err := os.ReadDir(manualDir); err == nil {
		for _, d := range sub {
			if d.IsDir() {
				n := ensureModCaseAliases(filepath.Join(manualDir, d.Name()), d.Name(), manualDir)
				total += n
			}
		}
	}

	if total > 0 {
		fmt.Printf("Created %d lowercase symlink aliases for Linux case-sensitive mod paths\n", total)
	}
	return total, nil
}

// ensureModCaseAliases creates lowercase aliases for one mod folder when its
// XMLs reference other files via x_extends/x_include and the mod tree has
// mixed-case names (the only situation the build-42 lowercase resolution
// breaks). Returns the number of symlinks created.
func ensureModCaseAliases(modDir, modName, parentDir string) int {
	if !modNeedsCaseAliases(modDir) {
		return 0
	}

	created := 0
	if hasUppercase(modName) {
		alias := filepath.Join(parentDir, strings.ToLower(modName))
		if createAlias(alias, modDir) {
			created++
		}
	}
	created += aliasTree(modDir)
	if created > 0 {
		fmt.Printf("Created %d case aliases for mod %q (%s): PZ build 42 resolves x_extends/x_include paths in lowercase, which fails on Linux for mixed-case file names\n", created, modName, modDir)
	}
	return created
}

// modNeedsCaseAliases walks the mod tree once and reports whether any path
// component is mixed-case AND any .xml file references another file via
// x_extends/x_include. Both conditions are required for the lowercase
// resolution bug to bite; the walk exits early once both are seen.
func modNeedsCaseAliases(modDir string) bool {
	hasUpper := false
	usesExtends := false
	filepath.WalkDir(modDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !hasUpper && hasUppercase(d.Name()) {
			hasUpper = true
		}
		if !usesExtends && !d.IsDir() && strings.HasSuffix(strings.ToLower(path), ".xml") {
			if data, err := os.ReadFile(path); err == nil &&
				(strings.Contains(string(data), "x_extends") || strings.Contains(string(data), "x_include")) {
				usesExtends = true
			}
		}
		if hasUpper && usesExtends {
			return filepath.SkipAll
		}
		return nil
	})
	return hasUpper && usesExtends
}

// aliasTree creates a lowercase symlink for every file and directory in the
// tree whose name contains uppercase characters. It walks the real tree only
// (never recursing through symlinks), so aliases never alias aliases.
func aliasTree(root string) int {
	return aliasTreeDir(root)
}

func aliasTreeDir(dir string) int {
	created := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		name := e.Name()
		if !hasUppercase(name) {
			if e.IsDir() {
				created += aliasTreeDir(filepath.Join(dir, name))
			}
			continue
		}
		real := filepath.Join(dir, name)
		alias := filepath.Join(dir, strings.ToLower(name))
		if createAlias(alias, real) {
			created++
		}
		if e.IsDir() {
			created += aliasTreeDir(real)
		}
	}
	return created
}

// createAlias symlinks alias -> real, skipping anything that already exists
// at alias (a previous alias or a genuine lowercase-named file, in which case
// the lowercase path already resolves). Returns true when a symlink was made.
func createAlias(alias, real string) bool {
	if _, err := os.Lstat(alias); err == nil {
		return false
	}
	if err := os.Symlink(real, alias); err != nil {
		fmt.Printf("WARNING: could not create case alias %s -> %s: %v\n", alias, real, err)
		return false
	}
	return true
}

// hasUppercase reports whether s contains any uppercase ASCII letter.
func hasUppercase(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) && r < 128 {
			return true
		}
	}
	return false
}
