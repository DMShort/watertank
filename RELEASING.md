# Releasing a new version (Dave)

**Pushing to `main` publishes a release.** GitHub builds the firmware and the
tool, and Dad's Watershed Tank Tool picks it up. He never needs the Arduino IDE.

```
you: edit + bench test → commit → push to main
GitHub Actions (about 3–10 min): next version (v0.4.1 → v0.4.2), compile firmware + build tool + bundle esptool → Release
Dad's tool: on start (and every 6 h) updates itself → shows "Install firmware v0.4.2" on the node
```

Dad downloads the tool once from **https://github.com/DMShort/watertank/releases/latest**.

## One-time setup

1. **Create the GitHub repo from this folder.** Easiest with GitHub Desktop:
   - *File → Add local repository → `E:\WaterTank`*.
   - When it says this isn't a repository yet, choose *create a repository*. The included `.gitignore` already keeps out the exe, logs, Home Assistant data and the Ebyte manual.
   - *Publish repository*. **Untick "Keep this code private"** (recommended, see below).

   Or from a terminal in `E:\WaterTank`:
   ```
   git init -b main
   git add .
   git commit -m "Watershed: node firmware v0.3, tank tool, release workflow"
   git remote add origin https://github.com/<you>/watershed.git
   git push -u origin main
   ```
2. **Match the build to your Arduino IDE.** Open `.github/workflows/release.yml`:
   - Set `ESP32_CORE` to the esp32 version in *Boards Manager*. It's currently 3.3.13.
   - Optionally pin libraries, e.g. `"RadioLib@7.1.2"`, to what *Library Manager* shows.
3. Send Dad the release link above. Don't send the .exe in your folder: that's a development build with no update source.

### Public or private repo?
- **Public (recommended):** works with nothing to set up on Dad's PC. Keep Wi-Fi passwords, MQTT logins and so on out of the code. `.gitignore` already excludes `basestation/homeassistant/`.
- **Private:** create a *fine-grained personal access token* on GitHub. Give it access to only this repo, with *Contents: Read-only*. Put this next to Dad's exe as `update.json`:
  ```json
  { "repo": "<you>/watershed", "token": "github_pat_..." }
  ```

## Every release

1. Bench-test your change on your own node as usual (Arduino IDE upload).
2. Commit with a message Dad will understand, e.g. *"Fix no-echo reading when the tank is full"*. The release notes are the commit messages since the last release.
3. Push to `main`. That's it.

- **What triggers a release:** changes under `firmware/`, `tools/WatershedTankTool/` or the workflow. Docs and `basestation/` changes don't.
- **To push without releasing:** put `[skip ci]` in the commit message.
- **For a bigger version jump:** tag it yourself. The tag message becomes the notes. Releases after that carry on from the new number.
  ```
  git tag -a v0.5.0 -m "Adds supply-voltage alarm"
  git push origin v0.5.0
  ```
- **Check the build:** watch the repo's **Actions** tab. A red ✕ means nothing was published, and Dad stays on the previous release. Click in to see why. Releases run one at a time, in order.

## Quick test builds without a release
In the Arduino IDE: *Sketch → Export Compiled Binary*. Send Dad `firmware/tank_node/build/<board>/tank_node.ino.bin`. In the tool: *Firmware & updates → Flash a test build*. Only the app is replaced, so his tank settings are kept. Next time he clicks *Install firmware*, he goes back to the latest release.

## What's in a release
| File | Purpose |
|---|---|
| `WatershedTankTool.exe` | the tool (updates itself) |
| `fw_bootloader.bin` `fw_partitions.bin` `fw_boot_app0.bin` `fw_app.bin` | firmware, flashed at 0x1000 / 0x8000 / 0xE000 / 0x10000 |
| `esptool.exe` | Espressif's official flasher (v4.8.1), downloaded by the tool when first needed |
| `manifest.json` | versions and SHA-256 hashes. The tool refuses any download that doesn't match. |

The firmware is never flashed as one merged image. That would overwrite 0x9000–0xDFFF, where the node keeps its ID, tank shape and calibration.
