#!/usr/bin/env python3
"""
Watershed - E22-900T22S raw listener (diagnostic only)
--------------------------------------------------------
Opens the LoRa HAT's serial port and prints every raw byte that
arrives, with a timestamp. Not the real bridge - just answers one
question: is anything coming through the radio link at all, given
the node and this HAT are two completely different LoRa stacks
(RadioLib driving an SX1276 directly vs. Ebyte's own firmware
wrapping an SX1262) that need matching air-interface parameters
(frequency, bandwidth, spreading factor, coding rate, sync word)
before they can hear each other.
"""
import serial
import sys
from datetime import datetime

PORT = "/dev/ttyS0"
BAUD = 9600

def main():
    try:
        ser = serial.Serial(port=PORT, baudrate=BAUD, parity=serial.PARITY_NONE,
                             stopbits=serial.STOPBITS_ONE, bytesize=serial.EIGHTBITS,
                             timeout=1)
    except Exception as e:
        print(f"Could not open {PORT}: {e}", file=sys.stderr)
        sys.exit(1)

    print(f"Listening on {PORT} at {BAUD} baud. Ctrl+C to stop.")
    try:
        while True:
            data = ser.read(64)
            if data:
                ts = datetime.now().strftime("%H:%M:%S.%f")[:-3]
                hexstr = data.hex(" ")
                try:
                    ascii_repr = data.decode("ascii", errors="replace")
                except Exception:
                    ascii_repr = "?"
                print(f"[{ts}] {len(data):2d} bytes  hex: {hexstr}  ascii: {ascii_repr!r}")
    except KeyboardInterrupt:
        print("\nstopped")
    finally:
        ser.close()

if __name__ == "__main__":
    main()
