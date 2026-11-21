#!/usr/bin/env python3
"""
Watershed - E22-900T22S basic UART sanity check
---------------------------------------------------
Sends the module's "read ambient noise RSSI" command (from the Ebyte
E22 register manual: C0 C1 C2 C3 + start address + read length).
This should get *some* response regardless of what LoRa air-interface
parameters the far-end transmitter uses - it only proves the UART
wiring, bridge jumper, and baud rate are actually correct.
"""
import serial
import time
import sys

PORT = "/dev/ttyS0"
BAUD = 9600

def main():
    ser = serial.Serial(port=PORT, baudrate=BAUD, parity=serial.PARITY_NONE,
                         stopbits=serial.STOPBITS_ONE, bytesize=serial.EIGHTBITS,
                         timeout=1)
    ser.reset_input_buffer()

    cmd = bytes([0xC0, 0xC1, 0xC2, 0xC3, 0x00, 0x01])
    print(f"sending: {cmd.hex(' ')}")
    ser.write(cmd)
    ser.flush()
    time.sleep(0.3)

    resp = ser.read(64)
    if resp:
        print(f"got {len(resp)} bytes back: {resp.hex(' ')}")
    else:
        print("no response at all")

    ser.close()

if __name__ == "__main__":
    main()
