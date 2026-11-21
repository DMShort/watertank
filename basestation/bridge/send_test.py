#!/usr/bin/env python3
"""
Watershed - E22-900T22S transmit test (normal mode: M0=GND, M1=GND)
Sends a marker message once a second for the given duration, so it's
broadcasting continuously while the ESP32 node cycles through its
listen sweep on the other end.
"""
import serial
import time
import sys

PORT = "/dev/ttyS0"
BAUD = 9600
DURATION_S = 40

def main():
    ser = serial.Serial(port=PORT, baudrate=BAUD, parity=serial.PARITY_NONE,
                         stopbits=serial.STOPBITS_ONE, bytesize=serial.EIGHTBITS,
                         timeout=1)
    print(f"Sending marker every 1s for {DURATION_S}s...")
    end = time.time() + DURATION_S
    n = 0
    while time.time() < end:
        msg = f"PISWEEP{n}".encode("ascii")
        ser.write(msg)
        ser.flush()
        print(f"sent: {msg}")
        n += 1
        time.sleep(1)
    ser.close()
    print("done")

if __name__ == "__main__":
    main()
