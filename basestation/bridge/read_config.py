#!/usr/bin/env python3
"""
Watershed - E22-900T22S register read (config mode required: M0=GND, M1=3.3V)
"""
import serial
import time

PORT = "/dev/ttyS0"
BAUD = 9600  # config-mode command baud is fixed at 9600 regardless of the UART data rate register

def main():
    ser = serial.Serial(port=PORT, baudrate=BAUD, parity=serial.PARITY_NONE,
                         stopbits=serial.STOPBITS_ONE, bytesize=serial.EIGHTBITS,
                         timeout=1)
    ser.reset_input_buffer()

    # Read 9 registers starting at address 0x00: ADDH, ADDL, NETID, REG0, REG1, REG2, REG3, +2 spare
    cmd = bytes([0xC1, 0x00, 0x09])
    print(f"sending: {cmd.hex(' ')}")
    ser.write(cmd)
    ser.flush()
    time.sleep(0.3)

    resp = ser.read(64)
    if resp:
        print(f"got {len(resp)} bytes: {resp.hex(' ')}")
        if len(resp) >= 3 and resp[0] == 0xC1:
            data = resp[3:]
            labels = ["ADDH", "ADDL", "NETID", "REG0(uart+air)", "REG1(subpkt+power)", "REG2(channel)", "REG3(flags)"]
            for i, b in enumerate(data):
                label = labels[i] if i < len(labels) else f"byte{i}"
                print(f"  [{i}] {label} = 0x{b:02X} ({b:08b})")
            if len(data) >= 6:
                channel = data[5]
                print(f"\n  => channel = {channel} => frequency = {850.125 + channel} MHz")
            if len(data) >= 4:
                reg0 = data[3]
                print(f"  => air rate bits = {reg0 & 0b111:03b}")
            if len(data) >= 7:
                reg3 = data[6]
                fixed_mode = (reg3 >> 6) & 1
                print(f"  => REG3 fixed-point-transmission bit = {fixed_mode} (0=transparent/default, 1=fixed point)")
                rssi_enable = (reg3 >> 7) & 1
                print(f"  => REG3 RSSI-enable bit = {rssi_enable}")
    else:
        print("no response - config mode may not be active (check M1=3.3V, M0=GND)")

    ser.close()

if __name__ == "__main__":
    main()
