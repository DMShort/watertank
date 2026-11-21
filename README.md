# Watershed

LoRa water-tank monitoring: an ESP32 tank node (ultrasonic level, water
temperature, supply voltage) reporting to a Raspberry Pi base station
(Mosquitto + Home Assistant).

| Folder | What's in it |
|---|---|
| `firmware/tank_node/` | Tank node firmware (Heltec WiFi LoRa 32 V2) and wiring schematics |
| `tools/WatershedTankTool/` | Windows tool: find the node over USB, set up the tank, test it, install firmware updates |
| `basestation/` | Pi base station: Docker compose, Mosquitto config, LoRa HAT test scripts |
| `.github/workflows/release.yml` | Builds a release (firmware + tool) when a version tag is pushed |

Releasing a new version is described in [RELEASING.md](RELEASING.md).
