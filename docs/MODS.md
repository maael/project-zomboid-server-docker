# Mods

This container downloads Steam Workshop mods automatically on server start
and figures out the correct `Mods=` and `WorkshopItems=` settings for you.

## Quick Setup

### Option A: Steam collection (easiest)

1. Create a [Steam Workshop collection](https://steamcommunity.com/workshop/editcollection/?appid=108600) and add your mods
2. Copy the collection ID from its URL: `steamcommunity.com/sharedfiles/filedetails/?id=3101645832`
3. Set it in `.env`:

```env
MOD_WORKSHOP_COLLECTION_IDS=3101645832
```

### Option B: Mod IDs directly

1. Open a mod's Workshop page; the number after `id=` is its Workshop ID: `https://steamcommunity.com/sharedfiles/filedetails/?id=2503743612`
2. Set them in `.env`:

```env
MOD_WORKSHOP_IDS=2160432461;2685168362;2503743612
```

### 3. Restart

```bash
docker compose down && docker compose up -d
```

That's it. `MOD_NAMES` is **optional** -- the entrypoint scans the downloaded
mods, finds each mod folder (`mod.info`), and writes the `Mods=` line itself.
You can set it explicitly to pin a load order:

```env
MOD_NAMES=SkillRecoveryJournal;MoreDescriptionForTraits
```

## Build 42 mod IDs

Since Build 42, the game identifies mods by the `id=` field inside `mod.info`,
**not** by the mod folder name. Many authors use an id that differs from the
folder name (e.g. "Firearms" has `id=firearmmod`, "DrawOnMap" has
`id=DRAW_ON_MAP`). The entrypoint therefore reads `id=` from each mod's
`mod.info` when auto-deriving `MOD_NAMES`, so the `Mods=` line matches what
the game registers -- without this, affected mods are silently rejected with
`required mod "X" not found` in the server log.

If you pin `MOD_NAMES` yourself, use the `id=` values (either spelling works
for the `entrypoint mods` check: ids and folder names are both accepted).

## Linux Case-Sensitivity Workaround (Build 42 animation bug)

Some Build 42 mods ship animation XML files with mixed-case names (for
example "Guns of Marz" ships `LoadShotgun_HB.xml` inside a `GunsOfMarz`
folder). Modders develop on Windows, where the filesystem is
case-insensitive, so the game works there. On a Linux server, Project
Zomboid build 42 resolves `x_extends` / `x_include` animation references by
**lowercasing the entire path** and opening it directly from disk. On
Windows that still finds the file; on Linux the lowercased path does not
exist, and every referenced animation node fails with a
`FileNotFoundException` in the server log:

```text
AnimNode.Parse threw an exception reading file: .../RackShotgun_HB.xml
PZXmlParserException: ... "steamapps/workshop/content/108600/<id>/mods/gunsofmarz/42.16/media/animsets/player-vehicle/actions/loadshotgun_hb.xml"
java.io.FileNotFoundException: ... loadshotgun_hb.xml (No such file or directory)
```

The affected reload/rack animations silently break, even though the mod
itself loads. This is a game bug, not a container bug -- mods whose XMLs
avoid `x_extends` (even with mixed-case file names) are unaffected.

The entrypoint works around it automatically: on every start it scans the
workshop mods, detects which ones use `x_extends`/`x_include` with
mixed-case paths, and creates **lowercase symlink aliases** for every
uppercase-named file and folder (and for the mod folder itself), so the
game's lowercased lookups resolve. Symlinks only: real files are never
renamed, so Steam re-downloads and mod updates are unaffected, and the
aliases are recreated on every boot (idempotent, self-healing).

You will see a line like this in the logs:

```text
Created 751 case aliases for mod "GunsOfMarz" (...): PZ build 42 resolves x_extends/x_include paths in lowercase, which fails on Linux for mixed-case file names
```

To disable the workaround:

```env
MOD_CASE_ALIASES=false
```

## How It Works

1. `MOD_WORKSHOP_IDS` and `MOD_WORKSHOP_COLLECTION_IDS` are resolved to a list of item IDs (collections via the public Steam page, or the Steam Web API when `STEAM_API_KEY` is set)
2. With `STEAM_USER`/`STEAM_PASS` set, all items are downloaded in a **single steamcmd session** to `<server-files>/steamapps/workshop/content/108600/<id>/`. Without credentials, Steam rejects anonymous downloads, so the **running server downloads them itself** from `WorkshopItems=` and the container **restarts automatically once** downloads finish so the mods load
3. Items already downloaded are skipped unless `MOD_UPDATE_ON_START=true`
4. If `MOD_NAMES` is empty, mod folder names are auto-detected from the downloads
5. The `.ini` is written with `Mods=` and `WorkshopItems=` and the server starts

```text
Resolved workshop collection 3101645832: 3 item(s)
Downloaded workshop mod 2160432461
Downloaded workshop mod 2685168362
Discovered mod "SkillRecoveryJournal" (workshop 2503743612)
Auto-detected mods (MOD_NAMES): SkillRecoveryJournal
```

## Manual (non-Workshop) Mods

Drop unzipped mod folders into `./data/Workshop/` (mounted at
`/home/steam/Zomboid/Workshop`). Any folder containing a `mod.info` file is
auto-detected and added to `Mods=` -- no extra configuration needed.

```bash
unzip my-cool-mod.zip -d data/Workshop/
docker compose up -d
```

## Debugging

List what the container found on disk and check your `MOD_NAMES` against it:

```bash
docker compose exec zomboid /home/steam/entrypoint mods
```

Example output:

```text
Discovered mod "SkillRecoveryJournal" (workshop 2503743612)
Configured MOD_NAMES: SkillRecoveryJournal;TypoMod
WARNING: MOD_NAMES entry "TypoMod" has no mod folder on disk - check the spelling
```

This catches the most common mistake: mod **folder names** differ from
Workshop **titles** (e.g. "Sapph's Cooking" lives in a folder called
`SapphsCooking`).

## Updating Mods

By default already-downloaded mods are skipped to keep restarts fast. To check
for updates every start:

```env
MOD_UPDATE_ON_START=true
```

steamcmd re-downloads each item (unchanged items resolve quickly).

## Troubleshooting

### Mod not showing up in game

- Run `entrypoint mods` to verify the folder was detected and `MOD_NAMES` matches
- Check server logs: `docker compose logs zomboid | grep -i mod`
- Ensure mods are compatible with your game version (B41 vs B42). Mods whose
  `mod.info` declares `versionMax` below the running game version are skipped
  automatically with a warning: the game refuses to load them anyway
  (e.g. a mod capped at `versionMax=42.17` on a 42.20.2 server)
- Mods whose `require=` lists a dependency that is not installed (or whose
  dependency id is a typo) are also rejected by the game; the entrypoint
  prints a `requires "X" which is not installed` warning naming the missing
  dependency
- `AnimNode.Parse threw an exception ... FileNotFoundException` lines are the
  Build 42 case-sensitivity bug described above; the entrypoint's symlink
  aliases fix it automatically on the next start
- Some mods require installation on the client side too
- Clients must subscribe to the same Workshop items on Steam

### Download fails

- `WARNING: workshop item <id> did not download` — if the item is public,
  anonymous workshop downloads may be failing (a known Steam-side issue).
  Set `STEAM_USER`/`STEAM_PASS` in `.env` (account owning Project Zomboid)
  and restart
- Check internet connectivity and Steam rate limits (retry after a while)
- Ensure `UPDATE_ON_START=true` (default) so the base game files are present

### Collection won't resolve

- Collections are resolved from the public Steam community page, so no
  `STEAM_API_KEY` is needed. If you see `could not resolve workshop
  collection`, the server starts without those items; common causes:
- Verify the collection is public (private collections cannot be resolved)
- Steam occasionally blocks scraping; set `STEAM_API_KEY` (free, from
  [steamcommunity.com/dev/apikey](https://steamcommunity.com/dev/apikey)) to
  switch resolution to the Steam Web API instead
- Explicit `MOD_WORKSHOP_IDS` never needs a key: you can paste the item IDs
  (from the collection page, or tools like PZ ID Grabber) to skip collection
  resolution entirely

### Server crashes after adding mods

- Check mod load order (some mods must load first) -- set `MOD_NAMES` explicitly
- Disable all mods, enable one at a time to find the culprit
- Check the in-game console for Lua errors

## Map Mods

Map mods require their map names in `MAP_NAMES`:

```env
MAP_NAMES=Muldraugh, KY;West Point, KY;MyCustomMap
```

The container automatically sets the `Map=` field in the `.ini` based on `MAP_NAMES`.
