package steam

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/faudil/project-zomboid-server-docker/internal/config"
)

const steamcmdPath = "/home/steam/steamcmd/steamcmd.sh"

// depotDownloaderPath is the DepotDownloader binary installed in the image.
// It replaces SteamCMD's app_update for downloading the server files.
const depotDownloaderPath = "/usr/local/bin/depotdownloader"

// workshopAppID is the Steam Workshop app id for Project Zomboid.
const workshopAppID = "108600"

// collectionAPI is the Steam Web API endpoint used to resolve workshop
// collections. Overridable in tests.
var collectionAPI = "https://api.steampowered.com/ISteamRemoteStorage/GetCollectionDetails/v1/"

// runSteamCmdCapture runs steamcmd, streaming output to stdout while keeping
// a copy for failure detection. steamcmd exits 0 even when commands fail, so
// the captured output is the only reliable signal. Overridable in tests.
var runSteamCmdCapture = runSteamCmdCaptureImpl

func runSteamCmdCaptureImpl(args ...string) (string, error) {
	cmd := exec.Command(steamcmdPath, args...)
	cmd.Dir = filepath.Dir(steamcmdPath)
	cmd.Env = append(os.Environ(), "HOME=/home/steam")

	var buf bytes.Buffer
	out := io.MultiWriter(os.Stdout, &buf)
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	return buf.String(), err
}

// runDepotDownloader runs DepotDownloader, streaming output to stdout while
// keeping a copy for failure detection. Unlike steamcmd it exits non-zero on
// failure, but the output still carries the useful error context.
// Overridable in tests.
var runDepotDownloader = runDepotDownloaderImpl

func runDepotDownloaderImpl(args ...string) (string, error) {
	cmd := exec.Command(depotDownloaderPath, args...)
	// The self-contained .NET build runs without ICU in globalization-invariant
	// mode, avoiding an extra ~200MB of locale packages in the image.
	cmd.Env = append(os.Environ(), "HOME=/home/steam", "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")

	var buf bytes.Buffer
	out := io.MultiWriter(os.Stdout, &buf)
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	return buf.String(), err
}

// steamLoginArgs builds the steamcmd login arguments. Real credentials are
// used when STEAM_USER is set; anonymous otherwise.
func steamLoginArgs(cfg *config.ServerConfig) []string {
	if cfg.SteamUser == "" {
		return []string{"+login", "anonymous"}
	}
	args := []string{}
	if cfg.SteamGuardCode != "" {
		args = append(args, "+set_steam_guard_code", cfg.SteamGuardCode)
	}
	return append(args, "+login", cfg.SteamUser, cfg.SteamPass)
}

func startScriptPath(cfg *config.ServerConfig) string {
	return cfg.ServerDir + "/start-server.sh"
}

// fixExecutableBits restores the execute permission on the server binaries.
// DepotDownloader extracts files without preserving the Steam depot's
// executable bits, so jre64/bin/java and ProjectZomboid64 land as mode 0644.
// Project Zomboid's start-server.sh only prints "Only 64bit is supported"
// when jre64/bin/java cannot be executed, so without this fix every start
// fails with that misleading message and exits 0.
func fixExecutableBits(cfg *config.ServerConfig) {
	files := []string{
		startScriptPath(cfg),
		filepath.Join(cfg.ServerDir, "ProjectZomboid64"),
	}

	// jre64/bin and linux64 contents are all executables or shared libraries;
	// shared libraries do not need +x, but applying it matches the depot.
	for _, dir := range []string{
		filepath.Join(cfg.ServerDir, "jre64", "bin"),
		filepath.Join(cfg.ServerDir, "linux64"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}

	for _, f := range files {
		if err := os.Chmod(f, 0755); err != nil && !os.IsNotExist(err) {
			fmt.Printf("WARNING: could not set executable bit on %s: %v\n", f, err)
		}
	}
}

const maxUpdateAttempts = 6

var updateRetryDelay = 60 * time.Second

// installArgs builds the DepotDownloader invocation for the server files.
// Anonymous by default; STEAM_USER/STEAM_PASS are passed through when set.
func installArgs(cfg *config.ServerConfig) []string {
	args := []string{
		"-app", cfg.SteamAppID,
		"-dir", cfg.ServerDir,
		"-validate",
	}
	if cfg.ServerBranch != "" {
		args = append(args, "-branch", cfg.ServerBranch)
	}
	if cfg.SteamUser != "" {
		args = append(args, "-username", cfg.SteamUser, "-password", cfg.SteamPass)
	}
	return args
}

// depotPermanentFailure reports whether DepotDownloader output describes a
// problem retrying cannot fix (e.g. bad credentials).
func depotPermanentFailure(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{
		"password was incorrect",
		"invalid password",
		"steam guard",
		"two-factor",
		"not valid for this account",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// InstallOrUpdate downloads the dedicated server files with DepotDownloader,
// which uses the same Steam3 protocol as steamcmd but downloads anonymously
// reliably (steamcmd's app_update is intermittently rejected by Steam's
// backend). Retries with a backoff as a safety net; permanent failures such
// as bad credentials fail immediately.
func InstallOrUpdate(cfg *config.ServerConfig) error {
	if !cfg.UpdateOnStart {
		if _, err := os.Stat(startScriptPath(cfg)); err == nil {
			fixExecutableBits(cfg)
			return nil
		}
	}

	if cfg.SteamUser != "" && cfg.SteamPass == "" {
		return fmt.Errorf("STEAM_PASS is required when STEAM_USER is set (DepotDownloader would otherwise prompt for a password and hang)")
	}

	args := installArgs(cfg)

	for attempt := 1; attempt <= maxUpdateAttempts; attempt++ {
		if attempt > 1 {
			fmt.Printf("Server download failed, retrying in 60s (attempt %d/%d)...\n", attempt, maxUpdateAttempts)
			time.Sleep(updateRetryDelay)
		}

		output, err := runInstallAttempt(cfg, args)
		if err == nil {
			fixExecutableBits(cfg)
			return nil
		}
		fmt.Printf("Server download attempt %d/%d failed: %v\n", attempt, maxUpdateAttempts, err)

		// Don't burn the remaining attempts on a problem retrying cannot fix.
		if depotPermanentFailure(output) {
			return err
		}
	}

	return fmt.Errorf("could not download app %s after %d attempts", cfg.SteamAppID, maxUpdateAttempts)
}

func runInstallAttempt(cfg *config.ServerConfig, args []string) (string, error) {
	output, err := runDepotDownloader(args...)
	if err != nil {
		return output, fmt.Errorf("DepotDownloader failed: %w", err)
	}

	if _, err := os.Stat(startScriptPath(cfg)); err != nil {
		return output, fmt.Errorf("server files were not installed (start-server.sh missing)")
	}

	return output, nil
}

// steamFailure returns the first steamcmd error line found in the output,
// or an empty string when the run looks successful.
func steamFailure(output string) string {
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		for _, marker := range []string{
			"failed to install app",
			"download item",
			"no subscription",
			"missing file permissions",
			"missing configuration",
			"access denied",
			"invalid password",
			"two-factor code required",
			"steam guard code is incorrect",
			"password incorrect",
		} {
			if strings.Contains(lower, marker) {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

func workshopDir(cfg *config.ServerConfig) string {
	return filepath.Join(cfg.ServerDir, "steamapps", "workshop", "content", workshopAppID)
}

// ResolveModWorkshopIDs returns the full list of workshop item IDs to
// download: explicit MOD_WORKSHOP_IDS plus items resolved from
// MOD_WORKSHOP_COLLECTION_IDS. Collections that cannot be resolved only log a
// warning so an explicit ID list keeps working.
func ResolveModWorkshopIDs(cfg *config.ServerConfig) []string {
	ids := splitIDs(cfg.ModWorkshopIDs)

	if cfg.ModWorkshopCollection != "" {
		for _, collID := range splitIDs(cfg.ModWorkshopCollection) {
			items, err := resolveCollection(cfg, collID)
			if err != nil {
				fmt.Printf("WARNING: could not resolve workshop collection %s: %v\n", collID, err)
				continue
			}
			fmt.Printf("Resolved workshop collection %s: %d item(s)\n", collID, len(items))
			ids = append(ids, items...)
		}
	}

	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func splitIDs(raw string) []string {
	if raw == "" {
		return nil
	}
	return config.ParseList(raw)
}

// collectionPageURL is the public Steam community page of a workshop
// collection. It is scraped when no STEAM_API_KEY is configured; the Web API
// (collectionAPI) needs a key but the page does not. Overridable in tests.
var collectionPageURL = "https://steamcommunity.com/sharedfiles/filedetails/?id="

// resolveCollection fetches the item IDs of a Steam workshop collection.
// With a STEAM_API_KEY the Web API is used; without one the public collection
// page is scraped instead, so collections work with no key at all.
func resolveCollection(cfg *config.ServerConfig, collectionID string) ([]string, error) {
	if cfg.SteamAPIKey != "" {
		return resolveCollectionAPI(cfg, collectionID)
	}
	return resolveCollectionKeyless(cfg, collectionID)
}

// resolveCollectionAPI resolves a collection through the Steam Web API.
// GetCollectionDetails requires an API key: without one Steam returns 400.
func resolveCollectionAPI(cfg *config.ServerConfig, collectionID string) ([]string, error) {
	form := url.Values{}
	form.Set("collectioncount", "1")
	form.Set("publishedfileids", collectionID)

	req, err := http.NewRequest(http.MethodPost, collectionAPI, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	q := req.URL.Query()
	q.Set("key", cfg.SteamAPIKey)
	req.URL.RawQuery = q.Encode()

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Steam API returned status %d", resp.StatusCode)
	}

	var out struct {
		Response struct {
			Result []struct {
				PublishedFileID string `json:"publishedfileid"`
				Children        []struct {
					PublishedFileID string `json:"publishedfileid"`
				} `json:"children"`
			} `json:"result"`
		} `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding Steam API response: %w", err)
	}

	if len(out.Response.Result) == 0 || out.Response.Result[0].PublishedFileID == "" {
		return nil, fmt.Errorf("collection %s not found", collectionID)
	}

	var ids []string
	for _, child := range out.Response.Result[0].Children {
		if child.PublishedFileID != "" {
			ids = append(ids, child.PublishedFileID)
		}
	}
	return ids, nil
}

// resolveCollectionKeyless scrapes the public Steam community page of a
// collection. The item list is server-rendered into a collectionChildren
// block, so no API key is needed. Returns an error for private, empty or
// restructured pages.
func resolveCollectionKeyless(cfg *config.ServerConfig, collectionID string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, collectionPageURL+collectionID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("collection page returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading collection page: %w", err)
	}

	ids, err := parseCollectionPage(string(body))
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("collection %s is empty or private", collectionID)
	}
	return ids, nil
}

// parseCollectionPage extracts the item IDs of a workshop collection from its
// community page HTML: the links inside the collectionChildren block, deduped
// while preserving order.
func parseCollectionPage(page string) ([]string, error) {
	start := strings.Index(page, `<div class="collectionChildren">`)
	if start == -1 {
		return nil, fmt.Errorf("collection page has no collectionChildren section")
	}
	end := strings.Index(page[start:], `<div style="clear: left">`)
	if end == -1 {
		return nil, fmt.Errorf("collection page children section is truncated")
	}
	section := page[start : start+end]

	re := regexp.MustCompile(`sharedfiles/filedetails/\?id=(\d+)`)
	matches := re.FindAllStringSubmatch(section, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("collection page children section has no items")
	}

	seen := make(map[string]struct{}, len(matches))
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		id := m[1]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// DownloadWorkshopItems downloads all workshop items in a single steamcmd
// session. Items already on disk are skipped unless ModUpdateOnStart is set.
// steamcmd's exit code does not reflect per-item failures, so each item's
// presence is verified afterwards.
//
// With an anonymous Steam session nothing is downloaded: Steam rejects
// anonymous workshop_download_item requests silently since 2024. The running
// PZ server downloads the items itself from WorkshopItems=, so the entrypoint
// relies on that path (and restarts once to load them, see WaitForModDownloads).
// STEAM_USER/STEAM_PASS makes the pre-download work here instead.
func DownloadWorkshopItems(cfg *config.ServerConfig, ids []string) error {
	dir := workshopDir(cfg)
	var toDownload []string

	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(dir, id)); err == nil && !cfg.ModUpdateOnStart {
			fmt.Printf("Workshop mod %s already downloaded\n", id)
			continue
		}
		toDownload = append(toDownload, id)
	}

	if len(toDownload) == 0 {
		return nil
	}

	if cfg.SteamUser == "" {
		if !cfg.UseSteam {
			fmt.Println("WARNING: workshop mods cannot be downloaded: the server runs with -nosteam (no Steam, no anonymous steamcmd downloads). Set STEAM_USER/STEAM_PASS or enable Steam.")
		} else {
			fmt.Println("Workshop mods not pre-downloaded (anonymous steamcmd downloads are rejected by Steam); the running server will download them from WorkshopItems= and the container will restart once to load them")
		}
		return nil
	}

	args := []string{
		"+@NoPromptForPassword", "1",
		"+force_install_dir", cfg.ServerDir,
	}
	args = append(args, steamLoginArgs(cfg)...)
	args = append(args, workshopBatchArgs(cfg, toDownload)...)

	// Downloads are intermittently rate-limited by Steam; retry the batch once
	// before giving up on this start. Output is captured because steamcmd
	// exits 0 even when a workshop item fails to download.
	for attempt := 1; attempt <= 2; attempt++ {
		output, err := runSteamCmdCapture(args...)
		if err != nil {
			if attempt == 2 {
				fmt.Printf("WARNING: workshop mod download failed: %v\n", err)
				return nil
			}
			fmt.Println("Workshop mod download failed (Steam intermittently rate-limits downloads), retrying in 60s...")
			time.Sleep(updateRetryDelay)
			continue
		}
		if msg := steamFailure(output); msg != "" {
			if attempt == 2 {
				fmt.Printf("WARNING: workshop mod download reported an error: %s\n", msg)
				return nil
			}
			fmt.Println("Workshop mod download reported an error, retrying in 60s...")
			time.Sleep(updateRetryDelay)
			continue
		}
		break
	}

	for _, id := range toDownload {
		if _, err := os.Stat(filepath.Join(dir, id)); err != nil {
			fmt.Printf("WARNING: workshop item %s did not download (private, region-locked, invalid ID, or Steam-side download failure). Check the ID and the account's access to the item\n", id)
		} else {
			fmt.Printf("Downloaded workshop mod %s\n", id)
		}
	}

	return nil
}

// Polling knobs for WaitForModDownloads, overridable in tests.
var (
	modPollInterval  = 15 * time.Second
	modNoGrowthPolls = 6 // 90s without new items = downloads finished
	modWaitMax       = 30 * time.Minute
)

// ModCountOnDisk returns how many mod folders are currently on disk. Used to
// detect whether new mods appeared since the entrypoint wrote the ini.
func ModCountOnDisk(cfg *config.ServerConfig) int {
	return len(scanModFolders(cfg))
}

// WaitForModDownloads blocks until the running PZ server has downloaded the
// workshop items into the workshop dir (or a timeout elapses). It returns
// true when at least one item appeared on disk, meaning the container should
// restart once so Mods= can be populated and the mods loaded.
func WaitForModDownloads(cfg *config.ServerConfig, ids []string) bool {
	dir := workshopDir(cfg)
	deadline := time.Now().Add(modWaitMax)

	prev := -1
	stable := 0
	for {
		count := 0
		for _, id := range ids {
			if _, err := os.Stat(filepath.Join(dir, id)); err == nil {
				count++
			}
		}

		if count >= len(ids) {
			return count > 0
		}
		if count == prev {
			stable++
		} else {
			stable = 0
		}
		// Downloads finished when the on-disk count stops growing.
		if count > 0 && stable >= modNoGrowthPolls {
			return true
		}
		if time.Now().After(deadline) {
			return count > 0
		}

		prev = count
		time.Sleep(modPollInterval)
	}
}

// workshopBatchArgs builds the workshop_download_item commands for a single
// steamcmd session.
func workshopBatchArgs(cfg *config.ServerConfig, ids []string) []string {
	args := []string{}
	for _, id := range ids {
		args = append(args, fmt.Sprintf("workshop_download_item %s %s", workshopAppID, id))
	}
	return append(args, "+quit")
}

// modOnDisk describes a mod folder detected on disk. name is the identity the
// PZ build-42 server registers mods under (the id= field of mod.info, falling
// back to the folder name), which is what Mods= must contain.
type modOnDisk struct {
	name     string
	folder   string
	source   string
	outdated bool
	require  []string
}

// scanModFolders finds every mod folder on disk and maps its identity (mod
// name for Mods=) to its source (workshop item ID or "manual").
func scanModFolders(cfg *config.ServerConfig) map[string]modOnDisk {
	gameVer, hasGameVer := serverGameVersion(cfg)
	mods := make(map[string]modOnDisk)
	add := func(dir, folder, source string) {
		m := resolveModIdentity(dir, folder, gameVer, hasGameVer)
		m.source = source
		if _, ok := mods[m.name]; !ok {
			mods[m.name] = m
		}
	}

	itemDirs, err := os.ReadDir(workshopDir(cfg))
	if err == nil {
		for _, item := range itemDirs {
			if !item.IsDir() {
				continue
			}
			source := "workshop " + item.Name()
			itemPath := filepath.Join(workshopDir(cfg), item.Name())
			// Standard layout: <item>/mods/<ModName>/. Build 42 mods are
			// versioned: <item>/mods/<ModName>/<build>/mod.info.
			if sub, err := os.ReadDir(filepath.Join(itemPath, "mods")); err == nil {
				for _, d := range sub {
					if d.IsDir() && modDirHasModInfo(filepath.Join(itemPath, "mods", d.Name())) {
						add(filepath.Join(itemPath, "mods", d.Name()), d.Name(), source)
					}
				}
			}
			// Legacy layout: <item>/<ModName>/
			if sub, err := os.ReadDir(itemPath); err == nil {
				for _, d := range sub {
					if d.IsDir() && d.Name() != "mods" && modDirHasModInfo(filepath.Join(itemPath, d.Name())) {
						add(filepath.Join(itemPath, d.Name()), d.Name(), source)
					}
				}
			}
		}
	}

	// Manual mods dropped into <DATA_DIR>/Workshop/
	manualDir := filepath.Join(cfg.DataDir, "Workshop")
	if sub, err := os.ReadDir(manualDir); err == nil {
		for _, d := range sub {
			if d.IsDir() && hasModInfo(filepath.Join(manualDir, d.Name())) {
				add(filepath.Join(manualDir, d.Name()), d.Name(), "manual")
			}
		}
	}

	return mods
}

func hasModInfo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "mod.info"))
	return err == nil
}

// modDirHasModInfo reports whether dir contains a mod.info directly (b41
// layout) or in an immediate subdirectory, which is how b42 workshop mods
// are downloaded: <ModName>/<build>/mod.info.
func modDirHasModInfo(dir string) bool {
	if hasModInfo(dir) {
		return true
	}
	sub, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, d := range sub {
		if d.IsDir() && hasModInfo(filepath.Join(dir, d.Name())) {
			return true
		}
	}
	return false
}

// modVariant is one mod.info of a mod folder: the folder's own mod.info (b41
// layout) or the build-42 variant in an immediate subdirectory.
type modVariant struct {
	path       string
	buildDir   string
	versionMin string
	versionMax string
	require    []string
}

// parseRequire splits a mod.info require= value into mod ids, matching the
// game: comma-separated, trimmed, and leading backslashes stripped.
func parseRequire(raw string) []string {
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(strings.TrimPrefix(entry, "\\"))
		if entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// modVariants returns every mod.info of a mod folder (direct + one per
// immediate subdirectory) with its version constraints.
func modVariants(dir string) []modVariant {
	var variants []modVariant
	add := func(path, buildDir string) {
		info := readModInfo(path)
		variants = append(variants, modVariant{
			path:       path,
			buildDir:   buildDir,
			versionMin: strings.TrimSpace(info["versionMin"]),
			versionMax: strings.TrimSpace(info["versionMax"]),
			require:    parseRequire(info["require"]),
		})
	}
	if hasModInfo(dir) {
		add(filepath.Join(dir, "mod.info"), "")
	}
	sub, err := os.ReadDir(dir)
	if err == nil {
		for _, d := range sub {
			if d.IsDir() && hasModInfo(filepath.Join(dir, d.Name())) {
				add(filepath.Join(dir, d.Name(), "mod.info"), d.Name())
			}
		}
	}
	return variants
}

// readModInfo parses the key=value fields of a mod.info file.
func readModInfo(path string) map[string]string {
	out := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		if _, ok := out[key]; !ok {
			out[key] = strings.TrimSpace(line[eq+1:])
		}
	}
	return out
}

// gameVersion is a Project Zomboid build version such as 42.20.2.
type gameVersion struct {
	major, minor, patch int
}

// parseGameVersion parses "42", "42.20" or "42.20.2". Returns false for
// empty or malformed values.
func parseGameVersion(s string) (gameVersion, bool) {
	var v gameVersion
	if s == "" {
		return v, false
	}
	parts := strings.Split(s, ".")
	for i := 0; i < 3; i++ {
		n := 0
		if i < len(parts) {
			val := parts[i]
			if val == "" {
				return v, false
			}
			for _, c := range val {
				if c < '0' || c > '9' {
					return v, false
				}
			}
			n = atoi(val)
		}
		switch i {
		case 0:
			v.major = n
		case 1:
			v.minor = n
		case 2:
			v.patch = n
		}
	}
	return v, true
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// before reports whether v is strictly older than o.
func (v gameVersion) before(o gameVersion) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}

// compatible reports whether the mod.info version constraints allow gameVer.
func (v modVariant) compatible(gameVer gameVersion) bool {
	if v.versionMin != "" {
		if min, ok := parseGameVersion(v.versionMin); ok && gameVer.before(min) {
			return false
		}
	}
	if v.versionMax != "" {
		if max, ok := parseGameVersion(v.versionMax); ok && max.before(gameVer) {
			return false
		}
	}
	return true
}

// consoleVersionPath is the server console log inside the data dir. The game
// prints its own build there (version=42.20.2), which tells us which mod.info
// variant the running server will pick.
func consoleVersionPath(cfg *config.ServerConfig) string {
	return filepath.Join(cfg.DataDir, "server-console.txt")
}

// serverGameVersion reads the game version from the last server boot logged
// in the console. Returns false when the server has never been started or the
// line cannot be parsed.
func serverGameVersion(cfg *config.ServerConfig) (gameVersion, bool) {
	data, err := os.ReadFile(consoleVersionPath(cfg))
	if err != nil {
		return gameVersion{}, false
	}
	re := regexp.MustCompile(`version=(\d+\.\d+(?:\.\d+)?)`)
	matches := re.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		return gameVersion{}, false
	}
	// The console spans many boots; the last line wins.
	return parseGameVersion(matches[len(matches)-1][1])
}

// selectModVariant picks the mod.info variant the PZ server will use for the
// running game version: the versioned variant with the highest versionMin
// whose range contains gameVer. Without a known game version the newest
// variant (highest versionMin) is preferred, and the direct b41-style
// mod.info is the last resort. The second return value is false when no
// variant matches the running game version (the mod is outdated).
func selectModVariant(variants []modVariant, gameVer gameVersion, hasGameVer bool) (modVariant, bool) {
	if len(variants) == 0 {
		return modVariant{}, false
	}
	best := -1
	for i := range variants {
		if hasGameVer && !variants[i].compatible(gameVer) {
			continue
		}
		if best == -1 || variantNewer(variants[i], variants[best]) {
			best = i
		}
	}
	if best != -1 {
		return variants[best], true
	}
	// No variant matches the running version: fall back to the newest so the
	// id is still reported, and let the caller warn about incompatibility.
	newest := 0
	for i := range variants {
		if variantNewer(variants[i], variants[newest]) {
			newest = i
		}
	}
	return variants[newest], false
}

// variantNewer prefers versioned variants over the direct b41-style mod.info,
// and among versioned ones the highest versionMin. Ties fall back to a stable
// path comparison.
func variantNewer(a, b modVariant) bool {
	aDir := a.buildDir
	bDir := b.buildDir
	if aDir != bDir {
		if aDir == "" {
			return false
		}
		if bDir == "" {
			return true
		}
		av, aok := parseGameVersion(aDir)
		bv, bok := parseGameVersion(bDir)
		if aok && bok {
			return bv.before(av)
		}
		if aok != bok {
			return aok
		}
	}
	am, aok := parseGameVersion(a.versionMin)
	bm, bok := parseGameVersion(b.versionMin)
	if aok && bok && !am.equal(bm) {
		return bm.before(am)
	}
	return a.path > b.path
}

func (v gameVersion) equal(o gameVersion) bool {
	return v.major == o.major && v.minor == o.minor && v.patch == o.patch
}

// resolveModIdentity determines the Mods= identity of a mod folder: the id=
// field of the mod.info variant matching the running game version, falling
// back to the folder name when mod.info has no usable id (b41-style mods).
func resolveModIdentity(dir, folder string, gameVer gameVersion, hasGameVer bool) modOnDisk {
	variants := modVariants(dir)
	chosen, compatible := selectModVariant(variants, gameVer, hasGameVer)

	name := folder
	if chosen.path != "" {
		if id := strings.TrimSpace(readModInfo(chosen.path)["id"]); id != "" {
			name = id
		}
	}

	return modOnDisk{
		name:     name,
		folder:   folder,
		outdated: len(variants) > 0 && !compatible,
		require:  chosen.require,
	}
}

// DiscoverModNames returns the sorted names of all mods found on disk that
// are compatible with the running game version, logging where each one was
// found. Build 42 mods are registered by the id= field of their mod.info
// (which may differ from the folder name); b41-style mods without an id keep
// the folder name. Mods whose version constraints exclude the running game
// version are skipped with a warning: the game refuses to load them anyway.
func DiscoverModNames(cfg *config.ServerConfig) []string {
	mods := scanModFolders(cfg)

	names := make([]string, 0, len(mods))
	for name, m := range mods {
		where := m.source
		if m.folder != name {
			where += ", folder " + m.folder
		}
		fmt.Printf("Discovered mod %q (%s)\n", name, where)
		if m.outdated {
			gameVer, hasGameVer := serverGameVersion(cfg)
			suffix := ""
			if hasGameVer {
				suffix = fmt.Sprintf(" for game version %d.%d.%d", gameVer.major, gameVer.minor, gameVer.patch)
			}
			fmt.Printf("WARNING: mod %q is not compatible%s (its mod.info version limits exclude it); the game will not load it - check for an update or remove it from the collection\n", name, suffix)
			continue
		}
		names = append(names, name)
	}

	// A mod whose require= references a mod that is not installed is rejected
	// by the game (isAvailableRequired); surface that here so it is not a
	// silent failure. Only warn for mods that are otherwise loadable.
	for _, m := range mods {
		if m.outdated {
			continue
		}
		for _, req := range m.require {
			if _, ok := mods[req]; ok {
				continue
			}
			matched := false
			for _, other := range mods {
				if other.folder == req {
					matched = true
					break
				}
			}
			if !matched {
				fmt.Printf("WARNING: mod %q requires %q which is not installed; the game will not load it - add the dependency to the collection or remove the mod\n", m.name, req)
			}
		}
	}

	sort.Strings(names)
	return names
}

// WarnMissingMods logs a warning for every configured MOD_NAMES entry that
// has no matching mod on disk. Entries are matched against both the build-42
// id= identity and the folder name, so either spelling works.
func WarnMissingMods(cfg *config.ServerConfig) {
	if cfg.ModNames == "" {
		return
	}
	onDisk := scanModFolders(cfg)
	for _, entry := range strings.Split(cfg.ModNames, ";") {
		if _, ok := onDisk[entry]; ok {
			continue
		}
		matched := false
		for _, m := range onDisk {
			if m.folder == entry {
				matched = true
				break
			}
		}
		if !matched {
			fmt.Printf("WARNING: MOD_NAMES entry %q has no mod on disk - check the spelling (mod names are the id= field of mod.info in build 42, not the folder name or workshop title)\n", entry)
		}
	}
}
