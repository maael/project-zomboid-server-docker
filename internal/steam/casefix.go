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
// The returned count is the number of aliases IN USE (created this boot or
// already present from a previous boot). The caller uses it to decide whether
// the anti-cheat checksum must be disabled: the checksum walks the mods'
// media/AnimSets and media/actiongroups dirs, so the aliases make it disagree
// with every client ("File doesn't exist on the client").
//
// Set MOD_CASE_ALIASES=false to disable the workaround entirely.
func EnsureCaseAliases(cfg *config.ServerConfig) (int, error) {
	if !cfg.ModCaseAliases {
		fmt.Println("MOD_CASE_ALIASES=false: lowercase symlink aliases for workshop mods are disabled")
		return 0, nil
	}

	created := 0
	present := 0
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
						c, p := ensureModCaseAliases(filepath.Join(itemPath, "mods", d.Name()), d.Name(), filepath.Join(itemPath, "mods"))
						created += c
						present += p
					}
				}
			}
			// Legacy layout: <item>/<ModName>/
			if sub, err := os.ReadDir(itemPath); err == nil {
				for _, d := range sub {
					if d.IsDir() && d.Name() != "mods" {
						c, p := ensureModCaseAliases(filepath.Join(itemPath, d.Name()), d.Name(), itemPath)
						created += c
						present += p
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
				c, p := ensureModCaseAliases(filepath.Join(manualDir, d.Name()), d.Name(), manualDir)
				created += c
				present += p
			}
		}
	}

	if created > 0 {
		fmt.Printf("Created %d lowercase symlink aliases for Linux case-sensitive mod paths\n", created)
	}
	if present > 0 {
		fmt.Printf("%d lowercase symlink aliases are in use for Linux case-sensitive mod paths\n", present)
	}
	return present, nil
}

// ensureModCaseAliases creates lowercase aliases for one mod folder when its
// XMLs reference other files via x_extends/x_include and the mod tree has
// mixed-case names (the only situation the build-42 lowercase resolution
// breaks). Returns the number of aliases created and the number now present
// (created plus those left over from a previous boot).
func ensureModCaseAliases(modDir, modName, parentDir string) (created, present int) {
	if !modNeedsCaseAliases(modDir) {
		return 0, 0
	}

	if hasUppercase(modName) {
		alias := filepath.Join(parentDir, strings.ToLower(modName))
		if createAlias(alias, modDir) {
			created++
		}
		if _, err := os.Lstat(alias); err == nil {
			present++
		}
	}
	c, p := aliasTree(modDir)
	created += c
	present += p
	if c > 0 {
		fmt.Printf("Created %d case aliases for mod %q (%s): PZ build 42 resolves x_extends/x_include paths in lowercase, which fails on Linux for mixed-case file names\n", c, modName, modDir)
	}
	return created, present
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
// (never recursing through symlinks), so aliases never alias aliases. Returns
// the number of aliases created and the number now present.
func aliasTree(root string) (created, present int) {
	return aliasTreeDir(root)
}

func aliasTreeDir(dir string) (created, present int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		name := e.Name()
		if !hasUppercase(name) {
			if e.IsDir() {
				c, p := aliasTreeDir(filepath.Join(dir, name))
				created += c
				present += p
			}
			continue
		}
		real := filepath.Join(dir, name)
		alias := filepath.Join(dir, strings.ToLower(name))
		if createAlias(alias, real) {
			created++
		}
		if _, err := os.Lstat(alias); err == nil {
			present++
		}
		if e.IsDir() {
			c, p := aliasTreeDir(real)
			created += c
			present += p
		}
	}
	return created, present
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
