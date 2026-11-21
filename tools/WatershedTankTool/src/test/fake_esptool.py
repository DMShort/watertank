#!/usr/bin/env python3
"""Pretends to be esptool v4: prints realistic output and 'flashes' by writing the
new firmware version for the fake node to pick up."""
import sys, time, os
args = sys.argv[1:]
port = args[args.index("--port")+1]; baud = args[args.index("--baud")+1]
pairs = args[args.index("-z")+1:]
open("/home/claude/e2e/esptool_calls.txt","a").write(" ".join(args)+"\n")
if os.path.exists("/home/claude/e2e/noconnect"):
    print("Connecting........_____....._____"); print("A fatal error occurred: Failed to connect to ESP32: No serial data received."); sys.exit(2)
if os.path.exists("/home/claude/e2e/fail_once") and baud == "921600":
    print("esptool.py v4.8.1"); print("Serial port " + port); print("Connecting....")
    print("A fatal error occurred: Timed out waiting for packet header"); sys.exit(2)
print("esptool.py v4.8.1"); print("Serial port " + port); print("Connecting....")
print("Chip is ESP32-D0WDQ6 (revision v1.0)"); print("Features: WiFi, BT, Dual Core, 240MHz")
print("Changing baud rate to " + baud); print("Changed."); print("Configuring flash size...")
for i in range(0, len(pairs), 2):
    off = int(pairs[i], 16); path = pairs[i+1]; size = os.path.getsize(path)
    print(f"Flash will be erased from 0x{off:08x} to 0x{off+size:08x}...")
    print(f"Compressed {size} bytes to {size//2}...")
    for pct in (0, 25, 50, 75, 100):
        print(f"Writing at 0x{off + size*pct//100:08x}... ({pct} %)", end="\r" if pct < 100 else "\n", flush=True); time.sleep(0.15)
    print(f"Wrote {size} bytes ({size//2} compressed) at 0x{off:08x} in 0.6 seconds"); print("Hash of data verified.")
    if off == 0x10000:
        data = open(path, "rb").read()
        ver = data[16:32].split(b"\0")[0].decode(errors="ignore") or "test"
        open("/home/claude/sim/fwver.txt","w").write(ver)
print("Leaving..."); print("Hard resetting via RTS pin...")
